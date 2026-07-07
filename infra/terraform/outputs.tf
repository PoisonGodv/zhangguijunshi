output "vpc_id" {
  description = "ID of the created VPC."
  value       = tencentcloud_vpc.main.id
}

output "clb_vip" {
  description = "Public VIP of the CLB — point the domain's A record here."
  value       = tencentcloud_clb_instance.main.clb_vip
}

output "cvm_private_ips" {
  description = "Private IPs of the CVM set."
  value       = tencentcloud_instance.app[*].private_ip
}

output "tdsqlc_vip" {
  description = "TDSQL-C connection VIP (set ZG_POSTGRES_HOST to this)."
  value       = tencentcloud_cynosdb_cluster.main.vip
}

output "redis_ip" {
  description = "Redis connection IP (set ZG_REDIS_HOST to this)."
  value       = tencentcloud_redis_instance.main.ip
}

output "cos_bucket" {
  description = "COS bucket name."
  value       = tencentcloud_cos_bucket.main.bucket
}

output "cls_topic_id" {
  description = "CLS topic id to ship structured logs to."
  value       = tencentcloud_cls_topic.app.id
}
