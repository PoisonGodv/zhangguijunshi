###############################################################################
# Tencent Cloud staging stack (tech plan §1.5 / INFRA-07).
#
# Provisioned: VPC + subnet, a CVM AS-set behind a CLB, COS bucket, TDSQL-C
# (CynosDB) MySQL, Redis, and a CLS logset/topic. The Go monolith runs on the
# CVMs via docker-compose; the CLB fronts :443 -> :8080.
#
# ⚠️ Verify every sku / image / engine version against the current Tencent Cloud
# console before first apply. Field shapes for CynosDB/CLB are the most volatile;
# `terraform plan` will surface any drift.
###############################################################################

locals {
  name_prefix = "${var.project_name}-${var.env}"

  tags = merge(var.common_tags, {
    env    = var.env
    tenant = "platform"
  })
}

provider "tencentcloud" {
  secret_id  = var.tencent_secret_id
  secret_key = var.tencent_secret_key
  region     = var.region
}

############################
# Network foundation
############################

resource "tencentcloud_vpc" "main" {
  name       = "${local.name_prefix}-vpc"
  cidr_block = var.vpc_cidr
  tags       = local.tags
}

resource "tencentcloud_subnet" "app" {
  name              = "${local.name_prefix}-subnet-app"
  vpc_id            = tencentcloud_vpc.main.id
  cidr_block        = var.subnet_cidr
  availability_zone = var.availability_zone
  tags              = local.tags
}

resource "tencentcloud_security_group" "app" {
  name        = "${local.name_prefix}-sg-app"
  description = "App tier: allow 8080 from CLB, 22 from bastion/office only"
  tags        = local.tags
}

resource "tencentcloud_security_group_rule" "allow_http_from_clb" {
  security_group_id = tencentcloud_security_group.app.id
  type              = "ingress"
  cidr_ip           = "0.0.0.0/0" # tightened by CLB as the real source; close in prod
  ip_protocol       = "tcp"
  port_range        = "8080"
  policy            = "accept"
}

resource "tencentcloud_security_group_rule" "allow_ssh" {
  security_group_id = tencentcloud_security_group.app.id
  type              = "ingress"
  cidr_ip           = "0.0.0.0/0" # REPLACE with office/bastion CIDR in prod
  ip_protocol       = "tcp"
  port_range        = "22"
  policy            = "accept"
}

############################
# CLB (front door: 443 -> 8080)
############################

resource "tencentcloud_clb_instance" "main" {
  network_type = "OPEN"
  clb_name     = "${local.name_prefix}-clb"
  vpc_id       = tencentcloud_vpc.main.id
  project_id   = 0
  tags         = local.tags
}

resource "tencentcloud_clb_listener" "https" {
  clb_id            = tencentcloud_clb_instance.main.id
  listener_name     = "${local.name_prefix}-https"
  port              = 443
  protocol          = "HTTPS"
  load_balancing_type = "WRR"
  certificate_mode  = "UNIDIRECTIONAL"
  certificate_id    = var.tls_certificate_id # supply via -var-file
  # TLS cert is managed out-of-band (SSL Certificates Service); wire its id here.
  tags              = local.tags
}

############################
# CVM (Go monolith via docker-compose)
############################

resource "tencentcloud_instance" "app" {
  count             = var.cvm_count
  availability_zone = var.availability_zone
  instance_name     = "${local.name_prefix}-cvm-${count.index + 1}"
  image_id          = var.image_id
  instance_type     = var.cvm_instance_type
  vpc_id            = tencentcloud_vpc.main.id
  subnet_id         = tencentcloud_subnet.app.id
  security_groups   = [tencentcloud_security_group.app.id]
  system_disk_type  = "CLOUD_PREMIUM"
  system_disk_size  = 50

  # The monolith binary + compose are fetched from the Gitea registry / COS and
  # started here. Secrets (DB DSN, AI keys) are injected from SSM by the entrypoint.
  user_data = templatefile("${path.module}/cloud-init.tftpl", {
    env              = var.env
    mysql_host       = tencentcloud_cynosdb_cluster.main.vip
    mysql_password   = var.tdsqlc_password
    redis_host       = tencentcloud_redis_instance.main.ip
    redis_password   = var.redis_password
    cos_bucket       = tencentcloud_cos_bucket.main.bucket
  })

  internet_max_bandwidth_out = 5
  tags                       = local.tags

  lifecycle {
    ignore_changes = [image_id] # image rotation handled outside Terraform
  }
}

# Attach the CVM set to the CLB so :443 fans out to :8080 on each instance.
resource "tencentcloud_clb_attachment" "app" {
  clb_id     = tencentcloud_clb_instance.main.id
  listener_id = tencentcloud_clb_listener.https.id
  dynamic "targets" {
    for_each = tencentcloud_instance.app
    content {
      instance_id = targets.value.id
      port        = 8080
      weight      = 10
    }
  }
}

############################
# COS (object storage; minio locally, COS in cloud)
############################

resource "tencentcloud_cos_bucket" "main" {
  bucket = "${var.project_name}-${var.env}-${var.cos_bucket_suffix}"
  acl    = "private"
  tags   = local.tags
}

############################
# TDSQL-C (CynosDB MySQL) — primary OLTP store
# ⚠️ CynosDB sku/param shapes change often; confirm in console before apply.
############################

resource "tencentcloud_cynosdb_cluster" "main" {
  available_zone   = var.availability_zone
  vpc_id           = tencentcloud_vpc.main.id
  subnet_id        = tencentcloud_subnet.app.id
  db_type          = "MYSQL"
  db_version       = var.tdsqlc_engine_version
  name             = "${local.name_prefix}-tdsqlc"
  password         = var.tdsqlc_password
  pay_mode         = 0            # pay-as-you-go for staging
  storage_pay_mode = 0
  serverless_status = "open"      # serverless scale-to-zero saves staging cost

  instance_machine_group = {
    cpu    = 2
    memory = 4
  }

  tags = local.tags

  lifecycle {
    ignore_changes = [password] # rotated via SSM out-of-band
  }
}

############################
# Redis (session store / budget counter hot cache)
############################

resource "tencentcloud_redis_instance" "main" {
  instance_name     = "${local.name_prefix}-redis"
  type_id           = "8" # Redis 5.0 master-replica, 1GB; 需核实机型与 region
  password          = var.redis_password
  mem_size          = 1024
  vpc_id            = tencentcloud_vpc.main.id
  subnet_id         = tencentcloud_subnet.app.id
  availability_zone = var.availability_zone
  tags              = local.tags
}

############################
# CLS (structured logs; stdout from the Go process)
############################

resource "tencentcloud_cls_logset" "main" {
  logset_name = "${local.name_prefix}-logset"
  tags        = local.tags
}

resource "tencentcloud_cls_topic" "app" {
  logset_id  = tencentcloud_cls_logset.main.id
  topic_name = "${local.name_prefix}-app"
  tags       = local.tags
}
