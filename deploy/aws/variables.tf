variable "region" {
  description = "AWS region. The hackathon account's SCP allows ap-northeast-2 and ap-southeast-2."
  type        = string
  default     = "ap-northeast-2"
}

variable "instance_type" {
  description = "Management host size. The account has 5 on-demand vCPUs in total; t3.small uses 2."
  type        = string
  default     = "t3.small"
}

variable "image_tag" {
  description = "Tag of the platform image in ECR (push-image.sh prints it)."
  type        = string
  default     = "latest"
}

variable "gateway_backends" {
  description = <<-EOT
    GATEWAY_BACKENDS for the traffic gateway: name=url pairs. The defaults are the
    app's addresses inside each cluster over Tailscale; until the clusters exist
    they fail their health checks and the gateway answers 502.
  EOT
  type        = string
  default     = "gke=http://shop.gke.hopcharlie.internal,eks=http://shop.eks.hopcharlie.internal,onprem=http://shop.onprem.hopcharlie.internal"
}

variable "operators" {
  description = "Names that get an operator token (shown in the audit log)."
  type        = list(string)
  default     = ["seohyun", "junseok", "jongwook", "dongyun", "taehoon"]
}

variable "cors_origins" {
  description = "Browser origins allowed to call the API (the console). Empty disables CORS."
  type        = string
  default     = "http://localhost:5173"
}

variable "dsn_host_suffixes" {
  description = "PLATFORM_DSN_HOST_SUFFIXES: database hosts a plan may point at (Tailscale MagicDNS names end in .ts.net)."
  type        = string
  default     = ".ts.net"
}

variable "console_image_tag" {
  description = "Tag of the hopcharlie/console image the host runs (push-console.sh pushes latest)."
  type        = string
  default     = "latest"
}
