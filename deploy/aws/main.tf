# HopCharlie management host: one EC2 instance running the platform API, the
# traffic gateway, PostgreSQL and Caddy with Docker Compose.
#
# Cost (ap-northeast-2, on-demand, 2026-11):
#   t3.small                 about $0.026/h
#   EBS gp3 20 GiB           about $0.06/day
#   public IPv4 (Elastic IP) $0.005/h
#   ECR, SSM parameters      a few cents
#   total                    about $0.80/day, under $6 until 11/8
#
# Remove everything: ./tf.sh destroy

data "aws_caller_identity" "me" {}

data "aws_vpc" "default" {
  default = true
}

data "aws_subnets" "default" {
  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.default.id]
  }
  filter {
    name   = "default-for-az"
    values = ["true"]
  }
}

data "aws_ssm_parameter" "al2023" {
  name = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
}

locals {
  name   = "hopcharlie"
  prefix = "/hopcharlie"
  # sslip.io resolves api.1-2-3-4.sslip.io to 1.2.3.4, so Caddy can get a
  # Let's Encrypt certificate before the team's Cloudflare domain exists.
  ip_dashed     = replace(aws_eip.host.public_ip, ".", "-")
  api_host      = "api.${local.ip_dashed}.sslip.io"
  app_host      = "app.${local.ip_dashed}.sslip.io"
  console_host  = "console.${local.ip_dashed}.sslip.io"
  image         = "${aws_ecr_repository.platform.repository_url}:${var.image_tag}"
  console_image = "${aws_ecr_repository.console.repository_url}:${var.console_image_tag}"
  # The console on this host always calls the API from its own origin.
  cors_origins = join(",", distinct(compact(concat(split(",", var.cors_origins), ["https://${local.console_host}"]))))
}

# --- image registry ---

resource "aws_ecr_repository" "platform" {
  name                 = "${local.name}/platform"
  image_tag_mutability = "MUTABLE"
  force_delete         = true # destroy must not stop at leftover images

  image_scanning_configuration {
    scan_on_push = true
  }
}

resource "aws_ecr_lifecycle_policy" "platform" {
  repository = aws_ecr_repository.platform.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "keep the last 10 images"
      selection    = { tagStatus = "any", countType = "imageCountMoreThan", countNumber = 10 }
      action       = { type = "expire" }
    }]
  })
}

resource "aws_ecr_repository" "console" {
  name                 = "${local.name}/console"
  image_tag_mutability = "MUTABLE"
  force_delete         = true

  image_scanning_configuration {
    scan_on_push = true
  }
}

resource "aws_ecr_lifecycle_policy" "console" {
  repository = aws_ecr_repository.console.name
  policy     = aws_ecr_lifecycle_policy.platform.policy
}

# --- secrets: generated here, stored as SSM SecureString, read by the host at boot ---

resource "random_password" "db" {
  length  = 32
  special = false
}

resource "random_password" "gateway_admin" {
  length  = 40
  special = false
}

resource "random_password" "operator" {
  for_each = toset(var.operators)
  length   = 40
  special  = false
}

resource "random_password" "planner" {
  length  = 40
  special = false
}

resource "random_password" "viewer" {
  length  = 40
  special = false
}

locals {
  platform_tokens = join(",", concat(
    [for n in var.operators : "${n}:operator:${random_password.operator[n].result}"],
    ["planner:agent:${random_password.planner.result}", "screen:viewer:${random_password.viewer.result}"],
  ))
}

resource "aws_ssm_parameter" "db_password" {
  name  = "${local.prefix}/db-password"
  type  = "SecureString"
  value = random_password.db.result
}

resource "aws_ssm_parameter" "gateway_token" {
  name  = "${local.prefix}/gateway-admin-token"
  type  = "SecureString"
  value = random_password.gateway_admin.result
}

resource "aws_ssm_parameter" "platform_tokens" {
  name  = "${local.prefix}/platform-tokens"
  type  = "SecureString"
  value = local.platform_tokens
}

# --- host settings: files host/sync.sh copies to /opt/hopcharlie ---
#
# The host reads these on every ./redeploy.sh, so changing them never replaces
# the instance (user_data is ignored after the first boot).

locals {
  # Line endings are normalized so a Windows checkout (CRLF) and a Linux one
  # produce the same parameters, and plan shows no change between them.
  host_files = {
    "compose.yaml"           = replace(file("${path.module}/host/compose.yaml"), "\r\n", "\n")
    "compose.tailscale.yaml" = replace(file("${path.module}/host/compose.tailscale.yaml"), "\r\n", "\n")
    "Caddyfile"              = replace(file("${path.module}/host/Caddyfile"), "\r\n", "\n")
    "sync.sh"                = replace(file("${path.module}/host/sync.sh"), "\r\n", "\n")
    "envs.yaml"              = replace(file("${path.module}/../../examples/envs.yaml"), "\r\n", "\n")
    "apps.yaml"              = replace(file("${path.module}/../../examples/apps.yaml"), "\r\n", "\n")
    # Non-secret values for /opt/hopcharlie/.env; sync.sh adds the secrets.
    "settings.env" = join("\n", [
      "IMAGE=${local.image}",
      "CONSOLE_IMAGE=${local.console_image}",
      "REGISTRY=${split("/", aws_ecr_repository.platform.repository_url)[0]}",
      "AWS_REGION=${var.region}",
      "API_HOST=${local.api_host}",
      "APP_HOST=${local.app_host}",
      "CONSOLE_HOST=${local.console_host}",
      "GATEWAY_BACKENDS=${var.gateway_backends}",
      "CORS_ORIGINS=${local.cors_origins}",
      "DSN_HOST_SUFFIXES=${var.dsn_host_suffixes}",
    ])
  }
}

resource "aws_ssm_parameter" "host_file" {
  for_each       = local.host_files
  name           = "${local.prefix}/host/${each.key}"
  type           = "String"
  tier           = "Intelligent-Tiering" # compose.yaml can pass the 4 KB standard limit
  insecure_value = each.value
}

# Team-managed secrets: Terraform only creates them. Set the real values with
# aws ssm put-parameter --overwrite (deploy/aws/README.md); apply never resets them.
resource "aws_ssm_parameter" "team_secret" {
  for_each = {
    "db-passwords" = "# Database passwords plans reference, one KEY=value per line:\n# SRC_PGPASSWORD=...\n# GKE_MOIMROOM_PGPASSWORD=...\n"
    "rclone-env"   = "# rclone remotes, one KEY=value per line:\n# RCLONE_CONFIG_GCS_TYPE=google cloud storage\n"
  }
  name  = "${local.prefix}/${each.key}"
  type  = "SecureString"
  tier  = "Intelligent-Tiering"
  value = each.value

  lifecycle {
    ignore_changes = [value]
  }
}

# --- instance identity: SSM Session Manager instead of SSH, ECR pull, its own parameters ---

resource "aws_iam_role" "host" {
  name = "${local.name}-management-host"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "ssm_core" {
  role       = aws_iam_role.host.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_role_policy_attachment" "ecr_read" {
  role       = aws_iam_role.host.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"
}

locals {
  # Exactly the read calls the AWS collector of GET /v1/security makes
  # (internal/security/aws.go), and nothing else: no write action, and none of
  # the other reads the managed SecurityAudit policy would add. These actions
  # take no resource-level permissions, so the resource is "*".
  # s3control GetPublicAccessBlock is the IAM action s3:GetAccountPublicAccessBlock.
  security_read_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "iam:GetAccountSummary",
        "iam:ListUsers",
        "iam:GetLoginProfile",
        "iam:ListMFADevices",
        "iam:ListAccessKeys",
        "ec2:DescribeSecurityGroups",
        "ec2:GetEbsEncryptionByDefault",
        "cloudtrail:DescribeTrails",
        "cloudtrail:GetTrailStatus",
        "guardduty:ListDetectors",
        "guardduty:GetDetector",
        "guardduty:ListFindings",
        "guardduty:GetFindings",
        "securityhub:DescribeHub",
        "securityhub:GetFindings",
        "s3:GetAccountPublicAccessBlock",
        "sts:GetCallerIdentity",
      ]
      Resource = "*"
    }]
  })

  own_parameters_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath"]
        Resource = "arn:aws:ssm:${var.region}:${data.aws_caller_identity.me.account_id}:parameter${local.prefix}/*"
      },
      {
        Effect    = "Allow"
        Action    = "kms:Decrypt"
        Resource  = "*"
        Condition = { StringEquals = { "kms:ViaService" = "ssm.${var.region}.amazonaws.com" } }
      },
    ]
  })
}

resource "aws_iam_role_policy" "security_read" {
  name   = "read-security-configuration"
  role   = aws_iam_role.host.id
  policy = local.security_read_policy
}

resource "aws_iam_role_policy" "own_parameters" {
  name   = "read-hopcharlie-parameters"
  role   = aws_iam_role.host.id
  policy = local.own_parameters_policy
}

resource "aws_iam_instance_profile" "host" {
  name = "${local.name}-management-host"
  role = aws_iam_role.host.name
}

# --- network: only HTTP(S) in; no SSH ---

resource "aws_security_group" "host" {
  name        = "${local.name}-management-host"
  description = "HopCharlie management host: HTTPS for the API and the app, HTTP for ACME"
  vpc_id      = data.aws_vpc.default.id

  ingress {
    description = "HTTPS (Caddy: API and gateway)"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  ingress {
    description = "HTTP (ACME challenge, redirect to HTTPS)"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_eip" "host" {
  domain = "vpc"
  tags   = { Name = "${local.name}-management-host" }
}

resource "aws_instance" "host" {
  ami                    = data.aws_ssm_parameter.al2023.insecure_value
  instance_type          = var.instance_type
  subnet_id              = sort(data.aws_subnets.default.ids)[0]
  vpc_security_group_ids = [aws_security_group.host.id]
  iam_instance_profile   = aws_iam_instance_profile.host.name

  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 2 # containers on the host may need IMDS through Docker's bridge
  }

  root_block_device {
    volume_type = "gp3"
    volume_size = 20
    encrypted   = true
  }

  # First boot only: installs Docker and runs host/sync.sh from SSM. Settings
  # change through SSM and ./redeploy.sh, so user_data is ignored afterwards
  # and editing it never replaces the host (and its PostgreSQL volume).
  user_data = templatefile("${path.module}/user_data.sh.tftpl", {
    region         = var.region
    prefix         = local.prefix
    compose_plugin = "v5.6.1"
  })
  user_data_replace_on_change = false

  tags = { Name = "${local.name}-management-host" }

  lifecycle {
    # A newer AL2023 AMI or a new bootstrap script must not replace the host mid-demo.
    ignore_changes = [ami, user_data]
  }

  depends_on = [aws_ssm_parameter.db_password, aws_ssm_parameter.gateway_token, aws_ssm_parameter.platform_tokens, aws_ssm_parameter.host_file, aws_ssm_parameter.team_secret]
}

resource "aws_eip_association" "host" {
  instance_id   = aws_instance.host.id
  allocation_id = aws_eip.host.id
}
