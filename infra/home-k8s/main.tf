# 집 k3s 클러스터 안의 사이트 접착제 (plan 0017, ADR 0010). AWS infra/k8s와 같은 자리다.
#
# infra/home(VM)·ansible(k3s·Postgres)·infra/home-aws(Roles Anywhere·집 큐)가 끝난 뒤 apply한다.
# 그 스택들이 만든 값(Postgres IP·ARN·큐 URL)은 원격 state에서 읽어 사이트 ConfigMap·Secret으로 넣는다.
# 앱 자체(deploy/helm/linkpulse + values-home.yaml)는 ArgoCD가 git에서 배포한다. 이 스택은 앱 차트를 모른다.
# AWS와 다른 점: LB 컨트롤러·TargetGroupBinding·Fluent Bit이 없다(공개 노출·CloudWatch 로그는 범위 밖, plan 0017).
#
# 순서(depends_on): kube-prometheus-stack(CRD) → ArgoCD → Image Updater → site(ConfigMap·Secret) → gitops(Application)
#   삭제는 역순이다. Application finalizer는 ArgoCD가 살아 있을 때 풀려야 한다 — destroy 전에
#   kubectl -n argocd delete application linkpulse --wait를 먼저 한다(런북 home-site.md).
# DB 비밀번호·Roles Anywhere 말단 키 Secret은 여기서 만들지 않는다 — Terraform으로 넣으면 S3 state에 평문으로 남는다.
#   apply 뒤 사람이 kubectl로 넣는다. 그 전의 앱 파드 ContainerCreating(마운트할 Secret 없음)은 정상이다.

locals {
  app_namespace = "linkpulse"
  # ansible k3s 역할이 kubeconfig의 default를 이 이름으로 바꾼다. providers.tf가 이 컨텍스트만 쓴다.
  kube_context = "linkpulse-home"

  # AWS(infra/k8s/main.tf chart_versions)와 같은 버전이다. scripts/check-charts.sh가 두 파일의 값이 같은지 본다.
  chart_versions = {
    argo_cd               = "10.10.3"
    argocd_image_updater  = "1.3.2"
    kube_prometheus_stack = "91.8.2"
  }
}

# ---- 앞 스택의 출력 ----
data "terraform_remote_state" "home" {
  backend = "s3"
  config = {
    bucket = var.tfstate_bucket
    key    = "home/terraform.tfstate"
    region = var.region
  }
}

data "terraform_remote_state" "home_aws" {
  backend = "s3"
  config = {
    bucket = var.tfstate_bucket
    key    = "home-aws/terraform.tfstate"
    region = var.region
  }
}

# ---- 네임스페이스 ----
# AWS와 같은 모양으로 Terraform이 만든다(LB 컨트롤러 라벨은 없다). 사람이 먼저 만들면 이 apply가 "already exists"로 실패한다.
resource "kubernetes_namespace_v1" "app" {
  metadata {
    name = local.app_namespace
  }
}

resource "kubernetes_namespace_v1" "monitoring" {
  metadata {
    name = "monitoring"
  }
}

# ---- 관측성 ----
# AWS와 같은 차트·버전이다. 값의 원칙도 같고(컨트롤 플레인 수집 끔·선택자·보관 2일), 알림 수신처만 다르다(values 파일).
resource "helm_release" "kube_prometheus_stack" {
  name       = "kube-prometheus-stack"
  repository = "https://prometheus-community.github.io/helm-charts"
  chart      = "kube-prometheus-stack"
  version    = local.chart_versions.kube_prometheus_stack
  namespace  = kubernetes_namespace_v1.monitoring.metadata[0].name
  wait       = true
  timeout    = 600

  values = [file("${path.module}/values/kube-prometheus-stack.yaml")]
}

# ---- 컨트롤러 ----
# AWS(infra/k8s)와 같은 값이다. 인터넷에 열지 않는다(ClusterIP, kubectl port-forward로 본다).
# 동기화 실패 알림(앱 차트 규칙)이 argocd_app_info를 쓰므로 컨트롤러 지표·ServiceMonitor를 켠다.
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
    controller = {
      metrics = { enabled = true, serviceMonitor = { enabled = true } }
    }
  })]

  # ServiceMonitor CRD가 있어야 ArgoCD 차트가 ServiceMonitor를 렌더한다(infra/k8s와 같은 이유).
  depends_on = [helm_release.kube_prometheus_stack]
}

# GHCR `main` 다이제스트가 바뀌면 Application의 image 파라미터를 바꾼다. GHCR 패키지가 공개라 자격증명이 없다.
resource "helm_release" "argocd_image_updater" {
  name       = "argocd-image-updater"
  repository = "https://argoproj.github.io/argo-helm"
  chart      = "argocd-image-updater"
  version    = local.chart_versions.argocd_image_updater
  namespace  = "argocd"
  wait       = true

  depends_on = [helm_release.argocd]
}

# ---- 사이트 값 ----
# 앱 차트가 envFrom으로 읽는 ConfigMap(linkpulse-site)·Secret(linkpulse-site-secret).
# 계정 ID가 든 값(ARN·큐 URL)과 관리자 해시는 set_sensitive로 넘겨 plan 출력(공개 PR에 붙는다)에서 가린다.
resource "helm_release" "site" {
  name      = "linkpulse-site"
  chart     = "${path.module}/charts/site-home"
  namespace = kubernetes_namespace_v1.app.metadata[0].name
  wait      = true

  values = [yamlencode({
    config = {
      # 공개 주소는 P7이다. 0017은 kubectl port-forward 스모크(localhost:8080)만 한다.
      PUBLIC_BASE_URL   = "http://localhost:8080"
      LOG_LEVEL         = var.log_level
      SHORT_CODE_LENGTH = tostring(var.short_code_length)
      DB_HOST           = data.terraform_remote_state.home.outputs.pg_ip
      DB_PORT           = "5432"
      DB_NAME           = "linkpulse"
      DB_USER           = "linkpulse"
      # 집 Postgres는 자체 서명 인증서다. require는 암호화만 하고 서버 인증서를 검증하지 않는다(AWS와 같은 값).
      DB_SSLMODE = "require"
      # 사람이 kubectl로 넣은 Secret linkpulse-db-password(키 password)를 앱 차트가 이 경로로 마운트한다
      # (values-home.yaml dbPasswordSecret, 차트 mountPath 기본값). 회전 때 kubelet이 파일을 바꾸고 앱이 인증 실패 때 다시 읽는다.
      DB_PASSWORD_SOURCE = "file"
      DB_PASSWORD_FILE   = "/var/run/secrets/linkpulse/password"
      AWS_REGION         = var.region
    }
  })]

  set_sensitive = [
    { name = "config.CLICK_QUEUE_URL", value = data.terraform_remote_state.home_aws.outputs.click_queue_url, type = "string" },
    { name = "config.RA_TRUST_ANCHOR_ARN", value = data.terraform_remote_state.home_aws.outputs.trust_anchor_arn, type = "string" },
    { name = "config.RA_PROFILE_ARN", value = data.terraform_remote_state.home_aws.outputs.profile_arn, type = "string" },
    { name = "config.RA_ROLE_ARN", value = data.terraform_remote_state.home_aws.outputs.role_arn, type = "string" },
    { name = "secret.ADMIN_TOKEN_SHA256", value = lower(var.admin_token_sha256), type = "string" },
  ]
}

# ---- GitOps: Application + ImageUpdater ----
# AWS와 같은 차트(infra/k8s/charts/gitops)에 값 파일만 바꿔 넣는다 — 같은 앱 차트·같은 리비전, 값 파일만 values-home.yaml(판정 (a)).
resource "helm_release" "gitops" {
  name      = "linkpulse-gitops"
  chart     = "${path.module}/../k8s/charts/gitops"
  namespace = "argocd"
  wait      = true

  values = [yamlencode({
    repoURL        = var.git_repo_url
    targetRevision = var.git_revision
    appNamespace   = local.app_namespace
    valueFiles     = ["values-home.yaml"]
  })]

  depends_on = [
    helm_release.site,
    helm_release.argocd,
    helm_release.argocd_image_updater,
    helm_release.kube_prometheus_stack,
  ]
}
