###############################################################################
# Input variables. Secrets (secret_id / secret_key / DB passwords) are ALWAYS
# supplied via variables (-var-file / TF_VAR_*) — never hardcoded. In production
# they come from Tencent Cloud SSM + KMS (tech plan §1.5.3), never from this repo.
###############################################################################

variable "tencent_secret_id" {
  description = "Tencent Cloud API secret id (from SSM in prod)."
  type        = string
  sensitive   = true
}

variable "tencent_secret_key" {
  description = "Tencent Cloud API secret key (from SSM in prod)."
  type        = string
  sensitive   = true
}

variable "region" {
  description = "Tencent Cloud region for all resources."
  type        = string
  default     = "ap-guangzhou"
}

variable "env" {
  description = "Deployment environment; tags every resource."
  type        = string
  validation {
    condition     = contains(["staging", "prod"], var.env)
    error_message = "env must be staging or prod (dev uses docker-compose, not Terraform)."
  }
}

variable "project_name" {
  description = "Project tag value."
  type        = string
  default     = "zhanggui-junshi"
}

variable "vpc_cidr" {
  description = "VPC CIDR block."
  type        = string
  default     = "10.4.0.0/16"
}

variable "subnet_cidr" {
  description = "Primary subnet CIDR for the app tier."
  type        = string
  default     = "10.4.1.0/24"
}

variable "availability_zone" {
  description = "Availability zone for single-AZ staging resources."
  type        = string
  default     = "ap-guangzhou-3"
}

variable "cvm_instance_type" {
  description = "CVM instance type running the Go monolith."
  type        = string
  default     = "SA2.MEDIUM4" # 2 vCPU / 4GiB; 需核实机型与可用区
}

variable "cvm_count" {
  description = "Number of CVM instances behind the CLB."
  type        = number
  default     = 2
}

variable "image_id" {
  description = "CVM image id (Ubuntu 22.04 in the target region). Look up with `tencentcloud_images` data source or the console; varies per region."
  type        = string
  default     = "img-22trbn9x" # 需核实: example Guangzhou Ubuntu image id
}

variable "tls_certificate_id" {
  description = "Tencent Cloud SSL certificate id bound to the CLB :443 listener. Managed out-of-band in the SSL Certificates Service."
  type        = string
  default     = ""
}

variable "tdsqlc_engine_version" {
  description = "TDSQL-C MySQL engine version."
  type        = string
  default     = "5.7"
}

variable "tdsqlc_password" {
  description = "TDSQL-C root password (from SSM in prod)."
  type        = string
  sensitive   = true
}

variable "redis_password" {
  description = "Redis password (from SSM in prod)."
  type        = string
  sensitive   = true
}

variable "cos_bucket_suffix" {
  description = "COS bucket suffix (bucket name = <project>-<env>-<suffix>, globally unique)."
  type        = string
  default     = "1300000000"
}

variable "common_tags" {
  description = "Tags applied to every resource."
  type        = map(string)
  default = {
    project = "zhanggui-junshi"
    managed = "terraform"
  }
}
