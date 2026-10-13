#!/usr/bin/env bash
# Helm 차트 검사 (plan 0014 단계 2·5). 로컬과 CI(.github/workflows/ci.yml)가 같은 스크립트를 쓴다.
#
#   1. 앱 차트(deploy/helm/linkpulse): lint → 렌더 → 쿠버네티스 스키마(kubeconform) → 동작 단언
#   2. 사이트 차트(infra/k8s/charts/site-aws, gitops): lint → 렌더(예시 값) → CRD 스키마
#
# 단언은 "되돌리면 장애가 나는 설정"만 고정한다(ADR 0007): HPA면 replicas 없음, preStop sleep,
# readiness=/healthz, 롤링 surge 1·unavailable 0, 연결 예산, Image Updater 값의 이미지 참조.
# 필요: helm, kubeconform. CRD 스키마는 저장소의 scripts/schemas를 먼저 보고, 없으면 datreeio/CRDs-catalog에서
# 받는다(네트워크). ImageUpdater는 카탈로그 스키마가 옛 버전이라(spec.namespace 필수) 고정 버전 CRD에서 만든
# 스키마를 둔다. Image Updater 차트 버전을 올리면 다시 만든다:
#   helm pull argo/argocd-image-updater --version <버전> --untar
#   scripts/schemas/crd-to-jsonschema.py argocd-image-updater/templates/crd-imageupdaters.yaml v1alpha1 \
#     scripts/schemas/argocd-image-updater.argoproj.io/imageupdater_v1alpha1.json
# 이 스키마는 모르는 필드를 거부한다(API 서버는 조용히 버려서 오타가 무시된다).
#
# 단언은 eval로 돈다(조건식을 이름과 함께 한 줄로 적으려고). 그래서 작은따옴표 안 변수·간접 호출 경고를 끈다.
# shellcheck disable=SC2016,SC2034,SC2329
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

K8S_VERSION="1.37.0" # infra/prod variables.tf eks_version과 맞춘다
APP=deploy/helm/linkpulse
LOCAL_SCHEMAS='scripts/schemas/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
CRD_SCHEMAS='https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'

fail=0
check() {
  if eval "$2"; then echo "✅ $1"; else echo "❌ $1"; fail=1; fi
}
# kubeconform: 내장 스키마 + CRD 카탈로그. 스키마가 없는 종류도 실패로 본다(-ignore-missing-schemas 없음).
conform() {
  kubeconform -strict -summary -kubernetes-version "$K8S_VERSION" \
    -schema-location default -schema-location "$LOCAL_SCHEMAS" -schema-location "$CRD_SCHEMAS"
}

echo "== 앱 차트 =="
check "helm lint" 'helm lint "$APP" -f "$APP/values-aws.yaml" >/dev/null'
R=$(helm template linkpulse "$APP" -f "$APP/values-aws.yaml" -n linkpulse)
check "kubeconform" 'printf "%s\n" "$R" | conform'
DEP=$(printf '%s\n' "$R" | awk '/^kind: Deployment/{f=1} f&&/^---/{f=0} f')
check "HPA 켜짐 → Deployment에 replicas 없음" '! printf "%s" "$DEP" | grep -q "^  replicas:"'
check "HPA min 2·max 6·CPU 60%" 'printf "%s" "$R" | grep -q "minReplicas: 2" && printf "%s" "$R" | grep -q "maxReplicas: 6" && printf "%s" "$R" | grep -q "averageUtilization: 60"'
check "preStop이 sleep 10(exec 아님)" 'printf "%s" "$DEP" | grep -A5 "preStop:" | grep -q "sleep:" && printf "%s" "$DEP" | grep -A5 "preStop:" | grep -q "seconds: 10" && ! printf "%s" "$DEP" | grep -A5 "preStop:" | grep -q "exec:"'
check "readiness가 /healthz" 'printf "%s" "$DEP" | grep -A2 "readinessProbe:" | grep -q "path: /healthz"'
check "runAsNonRoot에 숫자 UID(이미지 USER가 이름이라 없으면 컨테이너가 안 뜬다)" 'printf "%s" "$DEP" | grep -q "runAsNonRoot: true" && printf "%s" "$DEP" | grep -q "runAsUser: 65532"'
check "어떤 프로브도 /readyz를 보지 않음" '! printf "%s" "$DEP" | grep -q "path: /readyz"'
check "롤링 maxSurge 1·maxUnavailable 0" 'printf "%s" "$DEP" | grep -q "maxSurge: 1" && printf "%s" "$DEP" | grep -q "maxUnavailable: 0"'
check "종료 유예 45초" 'printf "%s" "$DEP" | grep -q "terminationGracePeriodSeconds: 45"'
check "DB_MAX_OPEN_CONNS=11" 'printf "%s" "$DEP" | grep -A1 "name: DB_MAX_OPEN_CONNS" | grep -q "\"11\""'
check "기본 이미지 ghcr.io/naerrow/linkpulse:main" 'printf "%s" "$DEP" | grep -q "image: \"ghcr.io/naerrow/linkpulse:main\""'
check "aws 사이트는 비밀번호 파일 마운트 없음" '! printf "%s" "$DEP" | grep -q "db-password"'
R2=$(helm template linkpulse "$APP" --set autoscaling.enabled=false -n linkpulse)
check "HPA 끔 → replicas 렌더·HPA 없음" 'printf "%s" "$R2" | grep -q "^  replicas: 2" && ! printf "%s" "$R2" | grep -q "HorizontalPodAutoscaler"'
R3=$(helm template linkpulse "$APP" --set image.tag=main@sha256:0123abcd -n linkpulse)
check "Image Updater 값(tag@digest) → repo:tag@digest" 'printf "%s" "$R3" | grep -q "image: \"ghcr.io/naerrow/linkpulse:main@sha256:0123abcd\""'
R4=$(helm template linkpulse "$APP" --set image.tag=sha256:0123abcd -n linkpulse)
check "다이제스트만 → repo@digest" 'printf "%s" "$R4" | grep -q "image: \"ghcr.io/naerrow/linkpulse@sha256:0123abcd\""'
R5=$(helm template linkpulse "$APP" --set dbPasswordSecret.name=db-pw -n linkpulse)
check "file 출처(P6) → Secret을 디렉터리로 마운트(subPath 없음)" 'printf "%s" "$R5" | grep -q "secretName: db-pw" && printf "%s" "$R5" | grep -q "mountPath: /var/run/secrets/linkpulse" && ! printf "%s" "$R5" | grep -q subPath'

echo "== 사이트 차트 =="
SITE=infra/k8s/charts/site-aws
SITE_SET=(--set targetGroupBinding.targetGroupARN=arn:aws:elasticloadbalancing:ap-northeast-2:000000000000:targetgroup/linkpulse-prod-tg/0123456789abcdef
  --set targetGroupBinding.vpcID=vpc-0123456789abcdef0 --set config.DB_HOST=db.example.internal)
check "site-aws helm lint" 'helm lint "$SITE" "${SITE_SET[@]}" >/dev/null'
S=$(helm template linkpulse-site "$SITE" "${SITE_SET[@]}" -n linkpulse)
check "site-aws kubeconform(TGB CRD 포함)" 'printf "%s\n" "$S" | conform'
check "TGB가 networking 없이 ip 타입" 'printf "%s" "$S" | grep -q "targetType: ip" && ! printf "%s" "$S" | grep -q "networking:"'
check "빈 비밀은 Secret에 넣지 않음" '! printf "%s" "$S" | grep -q "ADMIN_TOKEN_SHA256"'
S2=$(helm template linkpulse-site "$SITE" "${SITE_SET[@]}" --set secret.ADMIN_TOKEN_SHA256=abc --set 'secret.RATE_LIMIT_EXEMPT_IPS=198.51.100.7\,203.0.113.9' -n linkpulse)
check "쉼표 이스케이프한 예외 IP가 한 값으로 들어감" 'printf "%s" "$S2" | grep -q "RATE_LIMIT_EXEMPT_IPS: \"198.51.100.7,203.0.113.9\""'
check "TGB ARN이 없으면 렌더 실패" '! helm template x "$SITE" --set targetGroupBinding.vpcID=vpc-1 >/dev/null 2>&1'

GITOPS=infra/k8s/charts/gitops
check "gitops helm lint" 'helm lint "$GITOPS" >/dev/null'
G=$(helm template linkpulse-gitops "$GITOPS" -n argocd)
check "gitops kubeconform(Application·ImageUpdater CRD)" 'printf "%s\n" "$G" | conform'
check "Application에 finalizer·자동 동기화" 'printf "%s" "$G" | grep -q "resources-finalizer.argocd.argoproj.io" && printf "%s" "$G" | grep -q "selfHeal: true"'
check "ImageUpdater가 digest·argocd write-back" 'printf "%s" "$G" | grep -q "updateStrategy: digest" && printf "%s" "$G" | grep -q "method: argocd"'

exit $fail
