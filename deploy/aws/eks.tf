# The platform on the management EKS cluster (infra repo, ADR 0014). The infra
# repo owns the cluster and deploys the pods with Argo CD; this stack gives
# them an AWS identity. EKS Pod Identity binds the role to the ServiceAccount
# charlie-platform/hopcharlie-platform, and the role has the same reads as the
# host role: the parameters under /hopcharlie and the security view.
#
# Cost: none (IAM roles and Pod Identity associations are free).
# Cleanup: ./tf.sh destroy removes them with the rest of this stack.

resource "aws_iam_role" "platform_pod" {
  name = "${local.name}-platform-pod"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "pods.eks.amazonaws.com" }
      Action    = ["sts:AssumeRole", "sts:TagSession"]
    }]
  })
}

resource "aws_iam_role_policy" "platform_pod_security_read" {
  name   = "read-security-configuration"
  role   = aws_iam_role.platform_pod.id
  policy = local.security_read_policy
}

resource "aws_iam_role_policy" "platform_pod_own_parameters" {
  name   = "read-hopcharlie-parameters"
  role   = aws_iam_role.platform_pod.id
  policy = local.own_parameters_policy
}

resource "aws_eks_pod_identity_association" "platform" {
  cluster_name    = var.management_cluster
  namespace       = "charlie-platform"
  service_account = "hopcharlie-platform"
  role_arn        = aws_iam_role.platform_pod.arn
}
