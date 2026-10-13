# EKS 클러스터·노드·애드온 (plan 0014). 클러스터 안의 것(LB 컨트롤러·ArgoCD·Fluent Bit·사이트 값)은
# 별도 스택 infra/k8s가 넣는다 — helm·kubernetes provider는 클러스터가 있어야 설정되기 때문이다.

# API 엔드포인트는 공개 + 사설이다. 노드는 사설로 붙고, 운영자는 공개 쪽으로 붙는다(IAM 인증이 막는다).
# 운영자 IP가 회사·집을 오가 CIDR 제한은 두지 않는다(ADR 0007 트레이드오프).
#
# 인증은 액세스 엔트리(API 모드)만 쓴다. aws-auth ConfigMap은 쓰지 않는다.
# 클러스터를 만든 주체(terraform apply를 돌린 운영자)에게만 클러스터 관리자를 준다 — 같은 주체가
# infra/k8s를 apply하고 kubectl을 쓴다. 다른 주체가 필요해지면 aws_eks_access_entry로 더한다.
#
# 기본 애드온(vpc-cni·kube-proxy·coredns)을 EKS가 자기 관리형으로 깔지 않게 하고 아래에서 관리형 애드온으로
# 직접 만든다. 버전이 Terraform에 보이고, 두 방식이 섞이지 않는다.
# tfsec 지적 중 받아들인 것(ADR 0007):
# - 공개 엔드포인트·0.0.0.0/0: 위 설명대로 IAM 인증이 막고, 운영자 IP가 고정되지 않는다.
# - Secret 암호화 미설정: EKS 1.28+는 모든 API 데이터를 AWS 소유 키로 기본 봉투 암호화한다(EKS 사용자 가이드
#   envelope-encryption). 고객 관리 키는 월 1달러 + 요청 과금이라 두지 않는다.
# - 컨트롤 플레인 로그 미설정: 켜고 끄는 측정용 클러스터라 CloudWatch 비용만 늘어난다. 감사가 필요해지면 켠다.
#tfsec:ignore:aws-eks-no-public-cluster-access
#tfsec:ignore:aws-eks-no-public-cluster-access-to-cidr
#tfsec:ignore:aws-eks-encrypt-secrets
#tfsec:ignore:aws-eks-enable-control-plane-logging
resource "aws_eks_cluster" "main" {
  name     = "${local.name_prefix}-eks"
  version  = var.eks_version
  role_arn = aws_iam_role.eks_cluster.arn

  bootstrap_self_managed_addons = false

  access_config {
    authentication_mode                         = "API"
    bootstrap_cluster_creator_admin_permissions = true
  }

  vpc_config {
    subnet_ids              = aws_subnet.app[*].id
    endpoint_private_access = true
    endpoint_public_access  = true
  }

  depends_on = [aws_iam_role_policy_attachment.eks_cluster]

  tags = { Name = "${local.name_prefix}-eks" }
}

# EKS가 만든 클러스터 보안 그룹. 컨트롤 플레인 ENI와 노드(시작 템플릿 없는 관리형 노드 그룹)가 함께 쓰고,
# VPC CNI라 파드도 노드의 SG를 쓴다. 수신 규칙은 security_groups.tf가 더한다.
locals {
  cluster_sg_id = aws_eks_cluster.main.vpc_config[0].cluster_security_group_id
}

# ---- 애드온 ----
# 버전은 클러스터 버전의 기본(default) 애드온 버전이다. 클러스터 버전을 고정했으므로 plan에 보이는 값이 고정값이다.
locals {
  # 노드보다 먼저: 노드가 Ready가 되려면 CNI가, Pod Identity가 동작하려면 에이전트가 있어야 한다.
  addons_before_nodes = ["vpc-cni", "kube-proxy", "eks-pod-identity-agent"]
  # 노드 다음: Deployment라 노드가 없으면 ACTIVE가 되지 않는다. metrics-server는 HPA의 CPU 지표다.
  addons_after_nodes = ["coredns", "metrics-server"]
}

data "aws_eks_addon_version" "this" {
  for_each           = toset(concat(local.addons_before_nodes, local.addons_after_nodes))
  addon_name         = each.value
  kubernetes_version = aws_eks_cluster.main.version
}

resource "aws_eks_addon" "before_nodes" {
  for_each                    = toset(local.addons_before_nodes)
  cluster_name                = aws_eks_cluster.main.name
  addon_name                  = each.value
  addon_version               = data.aws_eks_addon_version.this[each.value].version
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "OVERWRITE"
}

resource "aws_eks_addon" "after_nodes" {
  for_each                    = toset(local.addons_after_nodes)
  cluster_name                = aws_eks_cluster.main.name
  addon_name                  = each.value
  addon_version               = data.aws_eks_addon_version.this[each.value].version
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "OVERWRITE"

  depends_on = [aws_eks_node_group.main]
}

# ---- 노드 ----
# t4g.medium(ARM64, 2 vCPU·4GiB) 2대 고정. 오토스케일러(Karpenter·Cluster Autoscaler)가 없어 최대를 더 둬도 쓰이지 않는다.
# 앱 최대 7개(HPA 6 + surge 1)와 시스템 파드가 2대에 들어가는지는 ADR 0007 "노드 수용"에 계산해 뒀다.
# t4g는 버스트형이다. T4g 기본 크레딧은 unlimited라 크레딧이 떨어져도 기준선(20%)에 묶이지 않는다 —
# 측정 창 9-1에서 describe-instance-credit-specifications로 확인한다.
resource "aws_eks_node_group" "main" {
  cluster_name    = aws_eks_cluster.main.name
  node_group_name = "${local.name_prefix}-nodes"
  node_role_arn   = aws_iam_role.eks_node.arn
  subnet_ids      = aws_subnet.app[*].id
  ami_type        = "AL2023_ARM_64_STANDARD"
  instance_types  = [var.node_instance_type]
  capacity_type   = "ON_DEMAND"

  scaling_config {
    desired_size = var.node_count
    min_size     = var.node_count
    max_size     = var.node_count
  }

  update_config {
    max_unavailable = 1
  }

  depends_on = [
    aws_iam_role_policy_attachment.eks_node,
    aws_eks_addon.before_nodes,
  ]

  tags = { Name = "${local.name_prefix}-nodes" }
}
