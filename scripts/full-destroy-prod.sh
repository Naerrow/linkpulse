#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  ./scripts/full-destroy-prod.sh --drop-dev-db
  ./scripts/full-destroy-prod.sh --drop-dev-db --plan-only

Destroys linkpulse prod for development cost saving (plan 0014: EKS). Order matters:

  1. in-cluster finalizers: delete the ArgoCD Application and the TargetGroupBinding while their
     controllers (ArgoCD, LB controller) are still alive, and verify none remain
  2. infra/k8s   terraform destroy (Helm releases, namespaces)
  3. infra/prod  terraform destroy (EKS, ALB, NAT/EIP, VPC, RDS, CloudWatch logs/alarms, SNS/Chatbot, IAM)

Keeps:
  - infra/bootstrap state bucket, because it is a separate Terraform stack
  - domain registration and the existing Route53 hosted zone, because prod only reads the zone
  - GHCR images (not AWS)

The --drop-dev-db flag is required because this deletes the RDS database without a final snapshot.
EOF
}

DROP_DEV_DB=false
PLAN_ONLY=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --drop-dev-db)
      DROP_DEV_DB=true
      ;;
    --plan-only)
      PLAN_ONLY=true
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

if [[ "$DROP_DEV_DB" != "true" ]]; then
  echo "Refusing to continue: pass --drop-dev-db to acknowledge RDS data loss." >&2
  exit 2
fi

for tool in terraform aws kubectl; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "$tool is required." >&2
    exit 127
  fi
done

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROD_DIR="$ROOT_DIR/infra/prod"
K8S_DIR="$ROOT_DIR/infra/k8s"
REGION="ap-northeast-2"
CLUSTER_NAME="linkpulse-prod-eks"

if [[ ! -f "$PROD_DIR/backend.hcl" ]]; then
  echo "Missing infra/prod/backend.hcl. Create it from backend.hcl.example first." >&2
  exit 1
fi

if [[ ! -f "$PROD_DIR/terraform.tfvars" ]]; then
  echo "Missing infra/prod/terraform.tfvars. Create it from terraform.tfvars.example first." >&2
  exit 1
fi

DESTROY_VARS=(
  "-var=db_deletion_protection=false"
  "-var=db_skip_final_snapshot=true"
)

STATE_LIST=""

load_state_list() {
  local output

  if ! output="$(terraform state list 2>&1)"; then
    echo "Failed to read Terraform state; refusing to skip deletion-prep." >&2
    echo "$output" >&2
    exit 1
  fi

  STATE_LIST="$output"
}

state_has() {
  grep -Fxq -- "$1" <<<"$STATE_LIST"
}

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

PREP_PLAN="$(mktemp -t linkpulse-prod-destroy-prep.XXXXXX.tfplan)"
DESTROY_PLAN="$(mktemp -t linkpulse-prod-destroy.XXXXXX.tfplan)"
K8S_DESTROY_PLAN="$(mktemp -t linkpulse-k8s-destroy.XXXXXX.tfplan)"
cleanup() {
  rm -f "$PREP_PLAN" "$DESTROY_PLAN" "$K8S_DESTROY_PLAN"
}
trap cleanup EXIT

echo "== linkpulse prod full destroy (EKS) =="
echo "This is destructive and intended only for the current no-user development phase."
echo "RDS final snapshot will be skipped and CloudWatch logs will be deleted."

CLUSTER_EXISTS=false
if aws eks describe-cluster --name "$CLUSTER_NAME" --region "$REGION" >/dev/null 2>&1; then
  CLUSTER_EXISTS=true
fi

echo
if [[ "$CLUSTER_EXISTS" != "true" ]]; then
  echo "Step 1/3: EKS cluster $CLUSTER_NAME not found; skipping in-cluster cleanup."
elif [[ "$PLAN_ONLY" == "true" ]]; then
  echo "Step 1/3: plan-only - would delete Application linkpulse and TargetGroupBinding(s) first,"
  echo "          and Prometheus/Alertmanager CRs too if they carry finalizers."
else
  echo "Step 1/3: delete finalizer-bearing objects while their controllers are alive."
  # Application(resources-finalizer)은 ArgoCD가, TGB는 LB 컨트롤러가 살아 있어야 지워진다. terraform destroy가
  # 컨트롤러 릴리스를 먼저 또는 동시에 지우면 finalizer에 걸려 네임스페이스·destroy가 멈춘다(ADR 0007).
  aws eks update-kubeconfig --name "$CLUSTER_NAME" --region "$REGION"
  kubectl -n argocd delete application linkpulse --ignore-not-found --wait --timeout=5m
  kubectl -n linkpulse delete targetgroupbindings.elbv2.k8s.aws --all --ignore-not-found --wait --timeout=5m
  LEFT_APPS="$(kubectl -n argocd get applications.argoproj.io -o name 2>/dev/null || true)"
  LEFT_TGBS="$(kubectl -n linkpulse get targetgroupbindings.elbv2.k8s.aws -o name 2>/dev/null || true)"
  if [[ -n "$LEFT_APPS" || -n "$LEFT_TGBS" ]]; then
    echo "Finalizer-bearing objects remain; refusing to destroy controllers under them:" >&2
    echo "$LEFT_APPS $LEFT_TGBS" >&2
    echo "See docs/runbooks/alarm-response.md (EKS: stuck finalizer)." >&2
    exit 1
  fi
  echo "OK: no Application / TargetGroupBinding left."
  # plan 0015: Prometheus·Alertmanager CR에 finalizer가 있으면 operator가 살아 있을 때 지운다. 고정 버전(operator v0.94.1)은
  # 기본으로 넣지 않는 것으로 보지만, 들어가 있으면 helm uninstall이 operator와 CR을 함께 지울 때 CR이 남아 네임스페이스 삭제가 멈춘다.
  # 없으면 아무것도 하지 않는다(흐름은 그대로다).
  if kubectl api-resources --api-group=monitoring.coreos.com -o name 2>/dev/null | grep -qx 'prometheuses.monitoring.coreos.com'; then
    MON_KINDS="prometheuses.monitoring.coreos.com,alertmanagers.monitoring.coreos.com"
    FIN_CRS="$(kubectl -n monitoring get "$MON_KINDS" -o jsonpath='{range .items[?(@.metadata.finalizers)]}{.kind}/{.metadata.name}{" "}{end}' 2>/dev/null || true)"
    if [[ -n "$FIN_CRS" ]]; then
      echo "Monitoring CRs with finalizers: $FIN_CRS- deleting while the operator is alive."
      kubectl -n monitoring delete "$MON_KINDS" --all --ignore-not-found --wait --timeout=5m
      if [[ -n "$(kubectl -n monitoring get "$MON_KINDS" -o name 2>/dev/null || true)" ]]; then
        echo "Monitoring CRs remain; refusing to destroy the operator under them." >&2
        echo "See docs/runbooks/alarm-response.md (EKS: stuck finalizer)." >&2
        exit 1
      fi
    else
      echo "OK: no finalizer on Prometheus / Alertmanager CRs."
    fi
  fi
fi

echo
echo "Step 2/3: infra/k8s destroy."
cd "$K8S_DIR"
terraform init -input=false -backend-config=../prod/backend.hcl
require_default_workspace
load_state_list
if [[ -z "$STATE_LIST" ]]; then
  echo "infra/k8s state is empty; skipping."
elif [[ "$CLUSTER_EXISTS" != "true" ]]; then
  # 클러스터가 먼저 사라졌다면 helm·kubernetes provider가 붙을 곳이 없다. 클러스터 안의 것은 클러스터와 함께 사라졌다.
  echo "infra/k8s has state but the cluster is gone. Remove the stale state by hand:" >&2
  echo "  (cd infra/k8s && terraform state list | xargs -n1 terraform state rm)" >&2
  exit 1
else
  terraform plan -destroy -input=false -out="$K8S_DESTROY_PLAN"
  if [[ "$PLAN_ONLY" == "true" ]]; then
    echo "Plan-only mode: infra/k8s destroy plan was shown but not applied."
  else
    confirm "destroy linkpulse k8s" "Type 'destroy linkpulse k8s' to delete the infra/k8s stack:"
    terraform apply -input=false "$K8S_DESTROY_PLAN"
  fi
fi

echo
echo "Step 3/3: infra/prod destroy."
cd "$PROD_DIR"
terraform init -input=false -backend-config=backend.hcl
require_default_workspace
load_state_list

if state_has "aws_db_instance.main"; then
  echo
  echo "Deletion-prep: plan RDS changes only (deletion protection off, skip final snapshot)."
  terraform plan -input=false -out="$PREP_PLAN" -target=aws_db_instance.main "${DESTROY_VARS[@]}"

  if [[ "$PLAN_ONLY" == "true" ]]; then
    echo
    echo "Plan-only mode: prep plan was shown but not applied."
  else
    confirm "prepare destroy" "Type 'prepare destroy' to disable RDS deletion protection:"
    terraform apply -input=false "$PREP_PLAN"
  fi
else
  echo "No RDS instance in state; skipping deletion-prep."
fi

echo
echo "Plan full prod destroy."
terraform plan -destroy -input=false -out="$DESTROY_PLAN" "${DESTROY_VARS[@]}"

if [[ "$PLAN_ONLY" == "true" ]]; then
  echo
  echo "Plan-only mode complete. No resources were changed."
  exit 0
fi

confirm "destroy linkpulse prod" "Type 'destroy linkpulse prod' to delete the infra/prod stack:"
terraform apply -input=false "$DESTROY_PLAN"

echo
echo "Done. infra/k8s and infra/prod have been destroyed. infra/bootstrap state bucket was not touched."
echo "Verify: (cd infra/prod && terraform state list | wc -l) and (cd infra/k8s && terraform state list | wc -l) are both 0."
