# Public names of the platform on the management EKS cluster (ADR 0014 in the
# infra repo): an ACM certificate for *.<domain> and the Cloudflare records.
# The domain is registered in Route 53 Domains, and its DNS is on Cloudflare.
#
# A separate root so that only whoever holds a Cloudflare token runs it:
#   CLOUDFLARE_API_TOKEN=... ./tf.sh -chdir=dns init | plan | apply
# The token needs Zone.DNS:Edit on the domain's zone.
#
# Two steps:
#   1. apply with alb_name empty: certificate and its validation record.
#      Put the certificate_arn output into the infra repo
#      (clusters/aws-dev-management/values/baseline.yaml, externalAlb).
#   2. When the ALB exists, set alb_name and apply again: api, app and console
#      records point at it.
#
# Cost: none (ACM public certificates and Cloudflare DNS are free).
# Cleanup: ./tf.sh -chdir=dns destroy (detach the certificate from the ALB first).

terraform {
  required_version = ">= 1.10"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.71"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "5.28.0"
    }
  }
}

variable "region" {
  type    = string
  default = "ap-northeast-2"
}

variable "domain" {
  description = "Domain whose api, app and console names the platform uses."
  type        = string
  default     = "hopcharlie.dev"
}

variable "cloudflare_zone_id" {
  description = "Zone ID of the domain in Cloudflare (Overview page of the zone)."
  type        = string
}

variable "alb_name" {
  description = "Name of the shared ALB of the management cluster (infra baseline externalAlb.groupName). Empty until the ALB exists."
  type        = string
  default     = ""
}

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project   = "hopcharlie"
      Component = "platform-dns"
      ManagedBy = "terraform:platform/deploy/aws/dns"
    }
  }
}

# Reads CLOUDFLARE_API_TOKEN from the environment.
provider "cloudflare" {}

resource "aws_acm_certificate" "platform" {
  domain_name       = "*.${var.domain}"
  validation_method = "DNS"

  lifecycle {
    create_before_destroy = true
  }
}

resource "cloudflare_dns_record" "validation" {
  for_each = {
    for o in aws_acm_certificate.platform.domain_validation_options : o.domain_name => o
  }
  zone_id = var.cloudflare_zone_id
  name    = trimsuffix(each.value.resource_record_name, ".")
  type    = each.value.resource_record_type
  content = trimsuffix(each.value.resource_record_value, ".")
  ttl     = 300
  proxied = false
  comment = "ACM validation for the HopCharlie platform certificate"
}

resource "aws_acm_certificate_validation" "platform" {
  certificate_arn         = aws_acm_certificate.platform.arn
  validation_record_fqdns = [for r in cloudflare_dns_record.validation : r.name]
}

data "aws_lb" "management" {
  count = var.alb_name == "" ? 0 : 1
  name  = var.alb_name
}

# DNS only (not proxied): the ALB holds the certificate, and the event streams
# of the API go straight to it.
resource "cloudflare_dns_record" "platform" {
  for_each = var.alb_name == "" ? toset([]) : toset(["api", "app", "console"])
  zone_id  = var.cloudflare_zone_id
  name     = "${each.key}.${var.domain}"
  type     = "CNAME"
  content  = data.aws_lb.management[0].dns_name
  ttl      = 300
  proxied  = false
  comment  = "HopCharlie platform on the management EKS cluster"
}

output "certificate_arn" {
  description = "Validated certificate for *.<domain>; goes into the infra baseline externalAlb.certificateArn."
  value       = aws_acm_certificate_validation.platform.certificate_arn
}

output "urls" {
  value = { for k, r in cloudflare_dns_record.platform : k => "https://${r.name}" }
}
