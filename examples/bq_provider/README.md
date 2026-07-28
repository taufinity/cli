# taufinity_bq_provider example

```bash
export TAUFINITY_ADMIN_TOKEN=...   # Studio admin token
export TAUFINITY_API_URL=https://studio.taufinity.io

# Create/update:
terraform apply

# Adopt an existing provider instead of creating one:
terraform import taufinity_bq_provider.example <numeric-id>
terraform plan   # should show no changes
```
