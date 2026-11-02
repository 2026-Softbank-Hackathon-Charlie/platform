# This stack's own state lives in the bucket it creates. To rebuild from
# nothing, comment this block out, apply with local state, then run
# `./tf.sh -chdir=state init -migrate-state`.
terraform {
  backend "s3" {
    bucket       = "hopcharlie-tfstate-525091405886-apne2"
    key          = "state/terraform.tfstate"
    region       = "ap-northeast-2"
    encrypt      = true
    use_lockfile = true
  }
}
