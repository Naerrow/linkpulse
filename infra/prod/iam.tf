# EKS용 IAM (plan 0014). ECS 실행·태스크 역할과 GitHub OIDC 배포 역할을 대체한다.
#
# - 클러스터 역할·노드 역할: EKS가 요구하는 AWS 관리형 정책만 붙인다.
# - 파드 역할: EKS Pod Identity로 서비스 어카운트마다 하나. 신뢰 정책이 네임스페이스·서비스 어카운트
#   세션 태그까지 확인해, 연결(association)을 잘못 만들어도 다른 파드가 이 역할을 받지 못하게 한다.
# - CI(GitHub Actions)에는 AWS 권한이 없다. GHCR에만 푸시하고, 배포는 클러스터 안 ArgoCD가 한다.

data "aws_caller_identity" "current" {}

# ---- 클러스터 역할 ----
data "aws_iam_policy_document" "eks_cluster_assume" {
  statement {
    actions = ["sts:AssumeRole", "sts:TagSession"]
    principals {
      type        = "Service"
      identifiers = ["eks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "eks_cluster" {
  name               = "${local.name_prefix}-eks-cluster"
  assume_role_policy = data.aws_iam_policy_document.eks_cluster_assume.json
}

resource "aws_iam_role_policy_attachment" "eks_cluster" {
  role       = aws_iam_role.eks_cluster.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
}

# ---- 노드 역할 ----
# CNI 정책을 노드 역할에 둔다(aws-node가 노드 자격증명으로 ENI·IP를 붙인다). 파드별 역할로 떼는 것보다
# 단순하고, 노드 2대 구성에서 얻을 것이 적다. ECR 읽기는 EKS 애드온 이미지(ECR에 있다)를 받는 데 쓴다.
data "aws_iam_policy_document" "eks_node_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "eks_node" {
  name               = "${local.name_prefix}-eks-node"
  assume_role_policy = data.aws_iam_policy_document.eks_node_assume.json
}

resource "aws_iam_role_policy_attachment" "eks_node" {
  for_each = toset([
    "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
    "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
    "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
  ])
  role       = aws_iam_role.eks_node.name
  policy_arn = each.value
}

# ---- 파드 역할 (EKS Pod Identity) ----
# 키는 "네임스페이스/서비스 어카운트"다. 차트·헬름 릴리스의 서비스 어카운트 이름과 맞아야 한다
# (deploy/helm/linkpulse values.yaml, infra/k8s의 helm_release).
locals {
  pod_identities = {
    app = {
      namespace       = "linkpulse"
      service_account = "linkpulse"
    }
    lbc = {
      namespace       = "kube-system"
      service_account = "aws-load-balancer-controller"
    }
    fluent_bit = {
      namespace       = "logging"
      service_account = "fluent-bit"
    }
  }
}

# 신뢰 정책: 1차 출처(EKS 사용자 가이드 pod-id-role)의 형태에 세션 태그 조건을 더했다.
data "aws_iam_policy_document" "pod_identity_assume" {
  for_each = local.pod_identities

  statement {
    sid     = "AllowEksAuthToAssumeRoleForPodIdentity"
    actions = ["sts:AssumeRole", "sts:TagSession"]
    principals {
      type        = "Service"
      identifiers = ["pods.eks.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:RequestTag/kubernetes-namespace"
      values   = [each.value.namespace]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:RequestTag/kubernetes-service-account"
      values   = [each.value.service_account]
    }
  }
}

resource "aws_iam_role" "pod" {
  for_each           = local.pod_identities
  name               = "${local.name_prefix}-pod-${replace(each.key, "_", "-")}"
  assume_role_policy = data.aws_iam_policy_document.pod_identity_assume[each.key].json
}

resource "aws_eks_pod_identity_association" "pod" {
  for_each        = local.pod_identities
  cluster_name    = aws_eks_cluster.main.name
  namespace       = each.value.namespace
  service_account = each.value.service_account
  role_arn        = aws_iam_role.pod[each.key].arn
}

# 앱: ECS 태스크 역할과 같은 권한이다.
# - 회전 시크릿 하나: 기동 때 직접 읽고(EKS에는 ECS처럼 넣어 주는 주체가 없다), 인증 실패 뒤 다시 읽는다(Step 2).
#   RDS 관리 시크릿은 AWS 관리형 KMS 키(aws/secretsmanager)라 kms 정책이 필요 없다.
# - 클릭 큐 하나: 보내고(Batcher) 받아서 지운다(Worker). GetQueueAttributes·DLQ 권한은 없다(사람이 CLI로 본다).
data "aws_iam_policy_document" "pod_app" {
  statement {
    sid       = "ReadDBPassword"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [local.rotation_secret_arn]
  }
  statement {
    sid       = "ClickQueueSendReceiveDelete"
    actions   = ["sqs:SendMessage", "sqs:ReceiveMessage", "sqs:DeleteMessage"]
    resources = [aws_sqs_queue.clicks.arn]
  }
}

resource "aws_iam_role_policy" "pod_app" {
  name   = "app"
  role   = aws_iam_role.pod["app"].id
  policy = data.aws_iam_policy_document.pod_app.json
}

# AWS Load Balancer Controller: 공식 정책을 버전째 고정해 둔다(v3.6.0 docs/install/iam_policy.json,
# sha256 16f232c9…a20a6a). 컨트롤러 차트 버전을 올리면 같은 태그의 정책으로 바꾼다.
# 이 클러스터는 ALB를 만들지 않고 TargetGroupBinding으로 타깃만 등록하지만, 컨트롤러가 기동·조정 때
# 부르는 API 범위를 손으로 줄이면 버전마다 깨질 수 있어 공식 정책을 그대로 쓴다(ADR 0007 트레이드오프).
resource "aws_iam_role_policy" "pod_lbc" {
  name   = "aws-load-balancer-controller"
  role   = aws_iam_role.pod["lbc"].id
  policy = file("${path.module}/policies/aws-load-balancer-controller-v3.6.0.json")
}

# Fluent Bit: 앱 로그 그룹 하나에 스트림을 만들고 쓴다. 그룹은 Terraform이 만들므로(logs.tf) 생성 권한은 주지 않는다.
data "aws_iam_policy_document" "pod_fluent_bit" {
  statement {
    sid       = "WriteAppLogs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogStreams"]
    resources = [aws_cloudwatch_log_group.app.arn, "${aws_cloudwatch_log_group.app.arn}:*"]
  }
}

resource "aws_iam_role_policy" "pod_fluent_bit" {
  name   = "fluent-bit"
  role   = aws_iam_role.pod["fluent_bit"].id
  policy = data.aws_iam_policy_document.pod_fluent_bit.json
}
