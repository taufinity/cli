# Manage a Taufinity Studio BigQuery data provider's access boundary declaratively.
#
# The provider shares github.com/taufinity/cli/pkg/studioadmin with the CLI and
# talks to the Studio admin API directly (it does not shell out to the CLI).

terraform {
  required_providers {
    taufinity = {
      source = "taufinity/taufinity"
    }
  }
}

provider "taufinity" {
  # api_url     defaults to $TAUFINITY_API_URL or https://studio.taufinity.io
  # admin_token defaults to $TAUFINITY_ADMIN_TOKEN (sensitive)
  org = "42" # numeric org id (or slug)
}

# Adopt an existing provider:  terraform import taufinity_bq_provider.example <id>
resource "taufinity_bq_provider" "example" {
  name         = "Example BQ"
  endpoint_url = "my-project.my_dataset"
  allowed_tables = [
    "rpt_example_a",
    "rpt_example_b",
  ]
}
