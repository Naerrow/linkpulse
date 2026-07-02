#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  ./scripts/full-apply-prod.sh
  ./scripts/full-apply-prod.sh --trigger-deploy
  ./scripts/full-apply-prod.sh --no-scale

Recreates the infra/prod Terraform stack after a full destroy.

Flow:
  1. terraform apply with service_desired_count=0 and image_tag=bootstrap
  2. trigger or wait for the GitHub Actions deploy workflow to build/push/deploy the real image
  3. terraform apply again with the normal desired count from terraform.tfvars
  4. check /healthz

Options:
  --trigger-deploy  Run 'gh workflow run deploy.yml --ref main' after infra creation.
  --no-scale        Stop after step 1. Use this if you want to deploy/scale manually.
EOF
}

TRIGGER_DEPLOY=false
NO_SCALE=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --trigger-deploy)
      TRIGGER_DEPLOY=true
      ;;
    --no-scale)
      NO_SCALE=true
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

if ! command -v terraform >/dev/null 2>&1; then
  echo "terraform is required." >&2
  exit 127
fi

if [[ "$TRIGGER_DEPLOY" == "true" ]] && ! command -v gh >/dev/null 2>&1; then
  echo "gh is required when --trigger-deploy is used." >&2
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

echo "== linkpulse prod full apply =="
echo "This recreates infra/prod. It starts ECS at desired_count=0 until the real image is deployed."
echo

terraform init -input=false -backend-config=backend.hcl

WORKSPACE="$(terraform workspace show)"
if [[ "$WORKSPACE" != "default" ]]; then
  echo "Refusing to run outside the default Terraform workspace. Current: $WORKSPACE" >&2
  exit 1
fi

INFRA_PLAN="$(mktemp -t linkpulse-prod-apply-infra.XXXXXX.tfplan)"
SCALE_PLAN="$(mktemp -t linkpulse-prod-apply-scale.XXXXXX.tfplan)"
cleanup() {
  rm -f "$INFRA_PLAN" "$SCALE_PLAN"
}
trap cleanup EXIT

echo
echo "Step 1/4: plan infra creation with ECS desired_count=0."
terraform plan -input=false -out="$INFRA_PLAN" \
  -var=service_desired_count=0 \
  -var=image_tag=bootstrap \
  -var=ecr_force_delete=false

confirm "apply linkpulse prod" "Type 'apply linkpulse prod' to create infra/prod with ECS scaled to 0:"
terraform apply -input=false "$INFRA_PLAN"

echo
echo "Terraform outputs for GitHub Actions variables:"
terraform output github_actions_role_arn
terraform output ecr_repository_url
terraform output ecr_repository_name
terraform output ecs_cluster_name
terraform output ecs_service_name
terraform output ecs_task_definition_family
echo "Compare these with GitHub repo Variables if this is the first recreate or the AWS account changed."

if [[ "$NO_SCALE" == "true" ]]; then
  echo
  echo "Stopped after infra creation because --no-scale was provided."
  echo "Next: run the GitHub Actions deploy workflow, then run terraform apply with the normal desired count."
  exit 0
fi

echo
echo "Step 2/4: deploy the real app image."
if [[ "$TRIGGER_DEPLOY" == "true" ]]; then
  (cd "$ROOT_DIR" && gh workflow run deploy.yml --ref main)
  echo "GitHub Actions deploy workflow was triggered. Wait until it succeeds before continuing."
else
  echo "Trigger GitHub Actions deploy now:"
  echo "  GitHub -> Actions -> deploy -> Run workflow -> branch main"
fi

echo
read -r -p "Press Enter only after the deploy workflow has succeeded."

echo
echo "Step 3/4: plan scale-up with the normal desired count from terraform.tfvars/defaults."
terraform plan -input=false -out="$SCALE_PLAN" -var=ecr_force_delete=false

confirm "scale linkpulse prod" "Type 'scale linkpulse prod' to apply the normal desired count:"
terraform apply -input=false "$SCALE_PLAN"

echo
echo "Step 4/4: health check."
APP_URL="$(terraform output -raw app_url)"

if command -v curl >/dev/null 2>&1; then
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
fi

echo "curl is not installed. Check manually: $APP_URL/healthz"
