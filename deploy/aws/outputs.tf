output "api_url" {
  description = "Platform API (charlie login --server ...)."
  value       = "https://${local.api_host}"
}

output "app_url" {
  description = "User traffic through the gateway."
  value       = "https://${local.app_host}"
}

output "ecr_repository_url" {
  value = aws_ecr_repository.platform.repository_url
}

output "instance_id" {
  value = aws_instance.host.id
}

output "shell" {
  description = "Open a shell on the host (needs the Session Manager plugin)."
  value       = "aws ssm start-session --region ${var.region} --target ${aws_instance.host.id}"
}

output "operator_tokens" {
  description = "One operator token per name. Hand each person only their own."
  value       = { for n in var.operators : n => random_password.operator[n].result }
  sensitive   = true
}

output "planner_token" {
  description = "Agent token for the AI planner (can submit plans, cannot approve)."
  value       = random_password.planner.result
  sensitive   = true
}

output "viewer_token" {
  description = "Read-only token (demo screen, judges)."
  value       = random_password.viewer.result
  sensitive   = true
}
