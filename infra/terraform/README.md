# Terraform — Tencent Cloud staging stack (INFRA-07)

IaC for the staging cloud resources of the 掌柜军师 backend: **CVM + CLB + COS +
TDSQL-C (CynosDB) + Redis + CLS**, on a dedicated VPC. The Go modular monolith
runs on the CVMs (docker-compose), the CLB fronts `:443 → :8080`, logs ship to
CLS. Production is the same module with prod var-files; dev never touches
Terraform (it uses `infra/compose`).

## Files

| File | Purpose |
|---|---|
| `versions.tf` | Terraform ≥ 1.5 + tencentcloud provider pin, COS remote state backend |
| `variables.tf` | All inputs; secrets are `sensitive`, supplied via `-var-file` / `TF_VAR_*` |
| `main.tf` | Provider + VPC/subnet + the six resource types + cloud-init boot |
| `cloud-init.tftpl` | CVM bootstrap (docker run the monolith, inject secrets via `.env`) |
| `outputs.tf` | VIPs / connection info |
| `staging.tfvars.example` | Copy → `staging.tfvars`, fill placeholders, never commit |

## First-time apply

```bash
cd infra/terraform

# 1. Backend (remote state in COS). Bucket must already exist.
terraform init \
  -backend-config="bucket=zgjs-tfstate-<acctid>" \
  -backend-config="prefix=terraform/staging" \
  -backend-config="region=ap-guangzhou"

# 2. Review the plan.
terraform plan -var-file=staging.tfvars -out=tfplan

# 3. Apply (interactive yes).
terraform apply tfplan
```

## Verification status (this commit)

This box has **no `terraform` binary**, so the `.tf` here is authored against the
`tencentcloud` provider docs but is **not yet `terraform validate`d locally**.
Before the first real apply, on a machine with Terraform:

```bash
terraform init -upgrade        # pulls provider, may surface API/field drift
terraform fmt -check -diff
terraform validate
terraform plan -var-file=staging.tfvars
```

These four commands are the gate for "staging 资源可起" (WBS M0-S2 INFRA-07 DoD).

### Things most likely to need adjustment at first `plan`

- **`tencentcloud_cynosdb_cluster`** — sku / `serverless_status` / machine-group
  field shapes change between provider versions; confirm against the console.
- **`image_id`** — varies per region; look up with a `tencentcloud_images` data
  source or the console (the default is a Guangzhou Ubuntu placeholder).
- **`type_id`** for Redis and **`cvm_instance_type`** — confirm the sku exists in
  the target zone.
- **`tls_certificate_id`** — bind a certificate created out-of-band in the SSL
  Certificates Service.

## Secrets

Never put real credentials in these files. In production the four secrets
(`tencent_secret_*`, `tdsqlc_password`, `redis_password`) are resolved from
Tencent Cloud **SSM + KMS** at deploy time (tech plan §1.5.3). `*.tfvars` is in
`.gitignore`; only `*.tfvars.example` is committed.
