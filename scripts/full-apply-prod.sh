#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  ./scripts/full-apply-prod.sh

Recreates linkpulse prod after a full destroy (plan 0014: EKS).

Flow:
  0. preflight: the app image ghcr.io/naerrow/linkpulse:main exists and is public
  1. infra/prod  terraform apply (VPC, RDS, ALB, SQS, EKS cluster + nodes + add-ons, IAM)
  2. kubeconfig  aws eks update-kubeconfig
  3. infra/k8s   terraform apply (kube-prometheus-stack, LB controller, ArgoCD, Image Updater, Fluent Bit,
                 site values, Application)
  4. wait        ArgoCD sync -> Deployment rollout -> target group healthy
  5. check       https://<domain>/healthz

The image no longer depends on the infra: CI pushes to GHCR on every main merge, so there is
no bootstrap image / "wait for CI deploy" step anymore. --trigger-deploy is accepted and ignored.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --trigger-deploy)
      echo "Note: --trigger-deploy is no longer needed (images live in GHCR). Ignoring."
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
  shift
done

for tool in terraform aws kubectl curl; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "$tool is required." >&2
    exit 127
  fi
done

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROD_DIR="$ROOT_DIR/infra/prod"
K8S_DIR="$ROOT_DIR/infra/k8s"
REGION="ap-northeast-2"
IMAGE_REPO="naerrow/linkpulse"

if [[ ! -f "$PROD_DIR/backend.hcl" ]]; then
  echo "Missing infra/prod/backend.hcl. Create it from backend.hcl.example first." >&2
  exit 1
fi

if [[ ! -f "$PROD_DIR/terraform.tfvars" ]]; then
  echo "Missing infra/prod/terraform.tfvars. Create it from terraform.tfvars.example first." >&2
  exit 1
fi

# 관리자 토큰 해시는 plan 0014부터 infra/k8s에 있다. 없으면 관리자 기능이 꺼진 채로 뜬다(fail-closed).
if ! grep -Eq '^admin_token_sha256[[:space:]]*=' "$K8S_DIR/terraform.tfvars" 2>/dev/null; then
  echo "Warning: admin_token_sha256 is not set in infra/k8s/terraform.tfvars. The app will start with admin features OFF."
  echo "         Move admin_token_sha256 from infra/prod/terraform.tfvars (see infra/k8s/terraform.tfvars.example)."
fi

confirm() {
  local expected="$1"
  local prompt="$2"
  local answer

  echo
  read -r -p "$prompt " answer
  if [[ "$answer" != "$expected" ]]; then
    echo "Confirmation did not match. Aborted." >&2
    exit 1
  fi
}

require_default_workspace() {
  local workspace
  workspace="$(terraform workspace show)"
  if [[ "$workspace" != "default" ]]; then
    echo "Refusing to run outside the default Terraform workspace. Current: $workspace" >&2
    exit 1
  fi
}

PROD_PLAN="$(mktemp -t linkpulse-prod-apply.XXXXXX.tfplan)"
K8S_PLAN="$(mktemp -t linkpulse-k8s-apply.XXXXXX.tfplan)"
# shellcheck disable=SC2329 # trap에서 부른다
cleanup() {
  rm -f "$PROD_PLAN" "$K8S_PLAN"
}
trap cleanup EXIT

echo "== linkpulse prod full apply (EKS) =="

echo
echo "Step 0/5: preflight - GHCR image ${IMAGE_REPO}:main must exist and be public."
# 공개 패키지는 익명 토큰으로 매니페스트를 읽을 수 있다. 실패하면 파드가 ImagePullBackOff로 멈춘다.
GHCR_TOKEN="$(curl -fsS "https://ghcr.io/token?scope=repository:${IMAGE_REPO}:pull" | sed -E 's/.*"token":"([^"]+)".*/\1/')" || GHCR_TOKEN=""
if [[ -z "$GHCR_TOKEN" ]] || ! curl -fsS -o /dev/null \
  -H "Authorization: Bearer $GHCR_TOKEN" \
  -H "Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json" \
  "https://ghcr.io/v2/${IMAGE_REPO}/manifests/main"; then
  echo "GHCR image ghcr.io/${IMAGE_REPO}:main is missing or not public." >&2
  echo "Run GitHub Actions 'deploy' on main, then set the package visibility to Public (GitHub -> Packages)." >&2
  exit 1
fi
echo "OK: ghcr.io/${IMAGE_REPO}:main is pullable."

echo
echo "Step 1/5: infra/prod (AWS resources + EKS cluster/nodes). This takes about 15-20 minutes."
cd "$PROD_DIR"
terraform init -input=false -backend-config=backend.hcl
require_default_workspace
terraform plan -input=false -out="$PROD_PLAN"
confirm "apply linkpulse prod" "Type 'apply linkpulse prod' to apply infra/prod:"
terraform apply -input=false "$PROD_PLAN"

CLUSTER_NAME="$(terraform output -raw eks_cluster_name)"
APP_URL="$(terraform output -raw app_url)"

echo
echo "Step 2/5: kubeconfig for $CLUSTER_NAME."
aws eks update-kubeconfig --name "$CLUSTER_NAME" --region "$REGION"
kubectl get nodes

echo
echo "Step 3/5: infra/k8s (controllers, site values, ArgoCD Application)."
cd "$K8S_DIR"
terraform init -input=false -backend-config=../prod/backend.hcl
require_default_workspace
terraform plan -input=false -out="$K8S_PLAN"
confirm "apply linkpulse k8s" "Type 'apply linkpulse k8s' to apply infra/k8s:"
terraform apply -input=false "$K8S_PLAN"

echo
echo "Step 4/5: wait for ArgoCD sync and the app rollout."
# ArgoCD가 git에서 차트를 받아 Deployment를 만들 때까지 기다린다. 그 뒤 rollout이 끝나면 파드가 Ready이고,
# readiness gate 때문에 Ready는 곧 "ALB 타깃 healthy"다.
for attempt in $(seq 1 60); do
  if kubectl -n linkpulse get deploy/linkpulse >/dev/null 2>&1; then
    break
  fi
  echo "Waiting for ArgoCD to create deploy/linkpulse... ($attempt/60)"
  sleep 10
done
kubectl -n linkpulse rollout status deploy/linkpulse --timeout=10m
kubectl -n argocd get application linkpulse -o jsonpath='{.status.sync.status}/{.status.health.status}{"\n"}'
kubectl -n linkpulse get pods -o wide

echo
echo "Step 5/5: health check."
for attempt in $(seq 1 30); do
  if curl -fsS --max-time 5 "$APP_URL/healthz" >/dev/null; then
    echo "Health check passed: $APP_URL/healthz"
    exit 0
  fi
  echo "Waiting for health check... ($attempt/30)"
  sleep 10
done

echo "Health check did not pass within the retry window: $APP_URL/healthz" >&2
exit 1
