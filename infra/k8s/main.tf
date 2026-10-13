# 클러스터 안의 사이트 접착제 (plan 0014).
#
# infra/prod(AWS 리소스)가 끝난 뒤 apply한다. AWS가 만든 값(RDS 주소·시크릿 ARN·큐 URL·타깃 그룹 ARN)은 apply마다
# 바뀌어 git(차트)에 넣을 수 없으니, 여기서 이름으로 찾아 클러스터 안 ConfigMap·Secret·TargetGroupBinding으로 넣는다.
# 앱 자체(deploy/helm/linkpulse)는 ArgoCD가 git에서 배포한다. 이 스택은 앱 차트를 모른다.
#
# 순서(depends_on, ADR 0007·docs/runbooks/alarm-response.md):
#   생성  컨트롤러(LBC·ArgoCD·Image Updater, wait) → site(ConfigMap·Secret·TGB) → gitops(Application·ImageUpdater)
#   삭제  역순. TGB·Application에는 finalizer가 있어 각 컨트롤러가 살아 있을 때 지워져야 한다.
#         scripts/full-destroy-prod.sh가 terraform destroy 전에 둘을 kubectl delete --wait로 먼저 지운다.

locals {
  name_prefix   = "${var.project}-${var.environment}"
  cluster_name  = "${local.name_prefix}-eks"
  app_namespace = "linkpulse"

  # 차트 버전 고정(2026-10-13 확인). 올릴 때는 values 형식과(LBC는) 같은 태그의 IAM 정책을 함께 확인한다.
  chart_versions = {
    aws_load_balancer_controller = "3.6.0"   # 앱 v3.6.0 — infra/prod/policies/aws-load-balancer-controller-v3.6.0.json
    argo_cd                      = "10.10.3" # 앱 v3.5.5
    argocd_image_updater         = "1.3.2"   # 앱 v1.3.1 — 설정이 ImageUpdater CR이다(charts/gitops)
    fluent_bit                   = "0.58.3"  # 앱 5.1.3 — EKS Pod Identity 자격증명 지원
  }
}

# ---- infra/prod가 만든 것 (이름으로 찾는다) ----
data "aws_lb_target_group" "app" {
  name = "${local.name_prefix}-tg"
}

data "aws_db_instance" "main" {
  db_instance_identifier = "${local.name_prefix}-pg"
}

data "aws_sqs_queue" "clicks" {
  name = "${local.name_prefix}-clicks"
}

data "aws_cloudwatch_log_group" "app" {
  name = "/eks/${local.name_prefix}/app"
}

# ---- 네임스페이스 ----
# 앱 네임스페이스는 helm의 create_namespace로 만들지 않는다 — 라벨을 붙이지 못한다.
# 이 라벨이 있어야 LB 컨트롤러가 파드에 readiness gate를 넣고, 롤링 때 새 파드가 ALB에서 healthy가 된 뒤에 옛 파드가 빠진다.
resource "kubernetes_namespace_v1" "app" {
  metadata {
    name = local.app_namespace
    labels = {
      "elbv2.k8s.aws/pod-readiness-gate-inject" = "enabled"
    }
  }
}

resource "kubernetes_namespace_v1" "logging" {
  metadata {
    name = "logging"
  }
}

# ---- 컨트롤러 ----
# AWS Load Balancer Controller: ALB를 만들지 않는다. 기존 타깃 그룹에 파드 IP만 등록한다(TargetGroupBinding).
# 리전·VPC를 명시한다 — 파드에서 IMDS로 알아내지 못할 수 있다. 서비스 어카운트 이름은 Pod Identity 연결(infra/prod iam.tf)의 키다.
# Service 변경 웹훅은 끈다: LoadBalancer Service를 쓰지 않고, 켜 두면 클러스터의 모든 Service 생성이 이 웹훅을 지난다.
resource "helm_release" "aws_load_balancer_controller" {
  name       = "aws-load-balancer-controller"
  repository = "https://aws.github.io/eks-charts"
  chart      = "aws-load-balancer-controller"
  version    = local.chart_versions.aws_load_balancer_controller
  namespace  = "kube-system"
  wait       = true

  values = [yamlencode({
    clusterName                 = local.cluster_name
    region                      = var.region
    vpcId                       = data.aws_eks_cluster.main.vpc_config[0].vpc_id
    serviceAccount              = { name = "aws-load-balancer-controller" }
    enableServiceMutatorWebhook = false
  })]
}

# ArgoCD: 인터넷에 열지 않는다(ClusterIP). 운영자는 kubectl port-forward로 본다.
# 쓰지 않는 구성 요소는 끈다(dex·notifications, ApplicationSet은 0개) — 노드 2대의 파드 수 한도(노드당 17개) 때문이다.
resource "helm_release" "argocd" {
  name             = "argocd"
  repository       = "https://argoproj.github.io/argo-helm"
  chart            = "argo-cd"
  version          = local.chart_versions.argo_cd
  namespace        = "argocd"
  create_namespace = true
  wait             = true

  values = [yamlencode({
    dex            = { enabled = false }
    notifications  = { enabled = false }
    applicationSet = { replicas = 0 }
  })]
}

# Image Updater: GHCR의 `main` 태그 다이제스트가 바뀌면 ArgoCD Application의 image 파라미터를 바꾼다(git에 쓰지 않는다).
# GHCR 패키지가 공개라 레지스트리 자격증명이 없다. 설정(ImageUpdater CR)은 charts/gitops에 있다.
resource "helm_release" "argocd_image_updater" {
  name       = "argocd-image-updater"
  repository = "https://argoproj.github.io/argo-helm"
  chart      = "argocd-image-updater"
  version    = local.chart_versions.argocd_image_updater
  namespace  = "argocd"
  wait       = true

  depends_on = [helm_release.argocd]
}

# Fluent Bit: 앱 파드의 stdout(JSON)만 CloudWatch Logs로 보낸다. 다른 네임스페이스·노드 로그는 보내지 않는다(비용).
# kubernetes 필터의 Merge_Log가 앱 JSON을 최상위 필드로 풀어 ECS 시절 Logs Insights 쿼리(msg·task_id 등)가 그대로 돈다.
# 로그 그룹은 infra/prod가 만든다(auto_create_group 끔 — IAM에 생성 권한이 없다).
resource "helm_release" "fluent_bit" {
  name       = "fluent-bit"
  repository = "https://fluent.github.io/helm-charts"
  chart      = "fluent-bit"
  version    = local.chart_versions.fluent_bit
  namespace  = kubernetes_namespace_v1.logging.metadata[0].name
  wait       = true

  values = [yamlencode({
    serviceAccount = { name = "fluent-bit" }
    resources = {
      requests = { cpu = "50m", memory = "64Mi" }
      limits   = { memory = "128Mi" }
    }
    config = {
      inputs  = <<-EOT
        [INPUT]
            Name              tail
            Path              /var/log/containers/*_${local.app_namespace}_*.log
            multiline.parser  cri
            Tag               kube.*
            Mem_Buf_Limit     5MB
            Skip_Long_Lines   On
      EOT
      filters = <<-EOT
        [FILTER]
            Name         kubernetes
            Match        kube.*
            Merge_Log    On
            Keep_Log     Off
            Labels       Off
            Annotations  Off
      EOT
      outputs = <<-EOT
        [OUTPUT]
            Name               cloudwatch_logs
            Match              kube.*
            region             ${var.region}
            log_group_name     ${data.aws_cloudwatch_log_group.app.name}
            log_stream_prefix  pod-
            auto_create_group  false
      EOT
    }
  })]
}

# ---- 사이트 값 + 타깃 그룹 연결 ----
# 앱 차트가 envFrom으로 읽는 ConfigMap(linkpulse-site)·Secret(linkpulse-site-secret)과 TargetGroupBinding.
# 비밀 두 개는 set_sensitive로 넘겨 plan 출력(공개 PR에 붙는다)에서 가린다. 쉼표는 helm --set 구분자라 이스케이프한다.
resource "helm_release" "site" {
  name      = "linkpulse-site"
  chart     = "${path.module}/charts/site-aws"
  namespace = kubernetes_namespace_v1.app.metadata[0].name
  wait      = true

  values = [yamlencode({
    config = {
      PUBLIC_BASE_URL   = "https://${var.domain_name}"
      LOG_LEVEL         = var.log_level
      SHORT_CODE_LENGTH = tostring(var.short_code_length)
      DB_HOST           = data.aws_db_instance.main.address
      DB_PORT           = tostring(data.aws_db_instance.main.port)
      DB_NAME           = data.aws_db_instance.main.db_name
      DB_USER           = data.aws_db_instance.main.master_username
      DB_SSLMODE        = "require"
      # 앱이 기동 때 Secrets Manager에서 직접 읽고, 인증 실패 때 다시 읽는다(Step 2). ECS의 시크릿 주입을 대신한다.
      DB_PASSWORD_SOURCE = "aws"
      DB_SECRET_ARN      = data.aws_db_instance.main.master_user_secret[0].secret_arn
      # SDK 리전은 ARN에서 온다. 이 값은 ARN 리전과의 교차검사용이다(app/internal/config).
      AWS_REGION      = var.region
      CLICK_QUEUE_URL = data.aws_sqs_queue.clicks.url
    }
    targetGroupBinding = {
      targetGroupARN = data.aws_lb_target_group.app.arn
      vpcID          = data.aws_eks_cluster.main.vpc_config[0].vpc_id
      serviceName    = "linkpulse"
      servicePort    = 8080
    }
  })]

  set_sensitive = [
    { name = "secret.ADMIN_TOKEN_SHA256", value = lower(var.admin_token_sha256), type = "string" },
    { name = "secret.RATE_LIMIT_EXEMPT_IPS", value = replace(var.rate_limit_exempt_ips, ",", "\\,"), type = "string" },
  ]

  # TGB는 LBC의 CRD·검증 웹훅이 준비된 뒤에 들어가야 하고, 삭제 때는 LBC가 살아 있어야 finalizer가 풀린다.
  depends_on = [helm_release.aws_load_balancer_controller]
}

# ---- GitOps: Application + ImageUpdater ----
# site 다음에 넣는다 — 파드가 생기기 전에 TGB가 있어야 LB 컨트롤러가 readiness gate를 넣는다.
# 삭제 때는 이것이 가장 먼저 지워진다(Application finalizer가 앱 리소스를 ArgoCD로 지운다).
resource "helm_release" "gitops" {
  name      = "linkpulse-gitops"
  chart     = "${path.module}/charts/gitops"
  namespace = "argocd"
  wait      = true

  values = [yamlencode({
    repoURL        = var.git_repo_url
    targetRevision = var.git_revision
    appNamespace   = local.app_namespace
  })]

  depends_on = [
    helm_release.site,
    helm_release.argocd,
    helm_release.argocd_image_updater,
  ]
}
