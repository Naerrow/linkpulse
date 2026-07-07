#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  ./scripts/full-destroy-prod.sh --drop-dev-db
  ./scripts/full-destroy-prod.sh --drop-dev-db --plan-only

Destroys the infra/prod Terraform stack for development cost saving.

Keeps:
  - infra/bootstrap state bucket, because it is a separate Terraform stack
  - domain registration and the existing Route53 hosted zone, because prod only reads the zone

Deletes from infra/prod:
  - ECS, ALB, NAT/EIP, VPC, RDS, CloudWatch logs/alarms, SNS/Chatbot, ECR repo/images, IAM/OIDC resources

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

if ! command -v terraform >/dev/null 2>&1; then
  echo "terraform is required." >&2
  exit 127
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROD_DIR="$ROOT_DIR/infra/prod"

cd "$PROD_DIR"

if [[ ! -f backend.hcl ]]; then
  echo "Missing infra/prod/backend.hcl. Create it from backend.hcl.example first." >&2
  exit 1
fi

if [[ ! -f terraform.tfvars ]]; then
  echo "Missing infra/prod/terraform.tfvars. Create it from terraform.tfvars.example first." >&2
  exit 1
fi

DESTROY_VARS=(
  "-var=db_deletion_protection=false"
  "-var=db_skip_final_snapshot=true"
  "-var=ecr_force_delete=true"
)

state_has() {
  terraform state list 2>/dev/null | grep -qx "$1"
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

echo "== linkpulse prod full destroy =="
echo "This is destructive and intended only for the current no-user development phase."
echo "RDS final snapshot will be skipped, CloudWatch logs will be deleted, and ECR images will be removed."
echo "If this prod stack owns the GitHub OIDC provider, that provider will be deleted too."
echo

terraform init -input=false -backend-config=backend.hcl

WORKSPACE="$(terraform workspace show)"
if [[ "$WORKSPACE" != "default" ]]; then
  echo "Refusing to run outside the default Terraform workspace. Current: $WORKSPACE" >&2
  exit 1
fi

PREP_PLAN="$(mktemp -t linkpulse-prod-destroy-prep.XXXXXX.tfplan)"
DESTROY_PLAN="$(mktemp -t linkpulse-prod-destroy.XXXXXX.tfplan)"
cleanup() {
  rm -f "$PREP_PLAN" "$DESTROY_PLAN"
}
trap cleanup EXIT

PREP_TARGETS=()
if state_has "aws_db_instance.main"; then
  PREP_TARGETS+=("-target=aws_db_instance.main")
fi
if state_has "aws_ecr_repository.app"; then
  PREP_TARGETS+=("-target=aws_ecr_repository.app")
fi

if [[ ${#PREP_TARGETS[@]} -gt 0 ]]; then
  echo
  echo "Step 1/3: plan deletion-prep changes only for RDS/ECR."
  terraform plan -input=false -out="$PREP_PLAN" "${PREP_TARGETS[@]}" "${DESTROY_VARS[@]}"

  if [[ "$PLAN_ONLY" == "true" ]]; then
    echo
    echo "Plan-only mode: prep plan was shown but not applied."
  else
    confirm "prepare destroy" "Type 'prepare destroy' to disable RDS deletion protection / enable ECR force delete:"
    terraform apply -input=false "$PREP_PLAN"
  fi
else
  echo
  echo "Step 1/3: no RDS/ECR resources found in state; skipping deletion-prep."
fi

echo
echo "Step 2/3: plan full prod destroy."
terraform plan -destroy -input=false -out="$DESTROY_PLAN" "${DESTROY_VARS[@]}"

if [[ "$PLAN_ONLY" == "true" ]]; then
  echo
  echo "Plan-only mode complete. No resources were changed."
  exit 0
fi

echo
echo "Step 3/3: apply full prod destroy."
confirm "destroy linkpulse prod" "Type 'destroy linkpulse prod' to delete the infra/prod stack:"
terraform apply -input=false "$DESTROY_PLAN"

echo
echo "Done. infra/prod has been destroyed. infra/bootstrap state bucket was not touched."
