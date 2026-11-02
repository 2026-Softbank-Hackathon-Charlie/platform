# Shared state: every teammate with access to the hackathon account plans and
# applies against the same state, and S3 lock files keep two applies from
# running at once. The bucket comes from ./state.
terraform {
  backend "s3" {
    bucket       = "hopcharlie-tfstate-525091405886-apne2"
    key          = "platform/management-host/terraform.tfstate"
    region       = "ap-northeast-2"
    encrypt      = true
    use_lockfile = true
  }
}
