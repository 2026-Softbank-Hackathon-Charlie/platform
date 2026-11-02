terraform {
  required_version = ">= 1.9"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.71"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.9"
    }
  }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project   = "hopcharlie"
      Component = "management-host"
      ManagedBy = "terraform:platform/deploy/aws" # ECR rejects parentheses in tag values
    }
  }
}
