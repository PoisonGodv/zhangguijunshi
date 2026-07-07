###############################################################################
# Terraform + provider version pins (tech plan §1.5 / INFRA-07).
#
# ⚠️ Verify the provider's latest published version before first apply:
#   https://registry.terraform.io/providers/tencentcloudstack/tencentcloud/latest
###############################################################################

terraform {
  required_version = ">= 1.5.0"

  required_providers {
    tencentcloud = {
      source  = "tencentcloudstack/tencentcloud"
      version = "~> 1.61" # 需核实最新稳定版
    }
  }

  # Remote state lives in COS so the team shares one state. Backend config is
  # partially populated from variables via terraform init -backend-config.
  backend "cos" {
    # prefix / region / bucket are supplied at `terraform init` time
    # (see README) to avoid baking state-bucket credentials into this file.
  }
}
