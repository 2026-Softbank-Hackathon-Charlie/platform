# Terraform state bucket for deploy/aws, shared by the whole team.
#
# The management host's state holds the generated API tokens and the DB
# password, so the bucket is private, encrypted, versioned and HTTPS only.
# Locking uses S3 lock files (Terraform 1.10+), so no DynamoDB table.
#
# Cost: a few KB of state, well under a cent a month.
# This stack's own state lives in the same bucket (see backend.tf).

terraform {
  required_version = ">= 1.10"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.71"
    }
  }
}

variable "region" {
  type    = string
  default = "ap-northeast-2"
}

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project   = "hopcharlie"
      Component = "terraform-state"
      ManagedBy = "terraform:platform/deploy/aws/state"
    }
  }
}

data "aws_caller_identity" "me" {}

locals {
  bucket = "hopcharlie-tfstate-${data.aws_caller_identity.me.account_id}-apne2"
}

resource "aws_s3_bucket" "state" {
  bucket = local.bucket

  # Losing this bucket loses track of every resource in deploy/aws.
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_versioning" "state" {
  bucket = aws_s3_bucket.state.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "state" {
  bucket = aws_s3_bucket.state.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "state" {
  bucket                  = aws_s3_bucket.state.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "state" {
  bucket = aws_s3_bucket.state.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_policy" "state" {
  bucket = aws_s3_bucket.state.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "DenyInsecureTransport"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:*"
      Resource  = [aws_s3_bucket.state.arn, "${aws_s3_bucket.state.arn}/*"]
      Condition = { Bool = { "aws:SecureTransport" = "false" } }
    }]
  })
  depends_on = [aws_s3_bucket_public_access_block.state]
}

resource "aws_s3_bucket_lifecycle_configuration" "state" {
  bucket = aws_s3_bucket.state.id
  rule {
    id     = "expire-old-state-versions"
    status = "Enabled"
    filter {}
    noncurrent_version_expiration {
      noncurrent_days = 90
    }
  }
}

output "bucket" {
  value = aws_s3_bucket.state.bucket
}
