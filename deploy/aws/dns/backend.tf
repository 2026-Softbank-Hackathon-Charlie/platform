# Same state bucket as the rest of deploy/aws (see ../state).
terraform {
  backend "s3" {
    bucket       = "hopcharlie-tfstate-525091405886-apne2"
    key          = "platform/dns/terraform.tfstate"
    region       = "ap-northeast-2"
    encrypt      = true
    use_lockfile = true
  }
}
