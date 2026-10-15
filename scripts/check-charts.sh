#!/usr/bin/env bash
# Helm 차트 검사 (plan 0014 단계 2·5, plan 0015 단계 2·4). 로컬과 CI(.github/workflows/ci.yml)가 같은 스크립트를 쓴다.
#
#   1. 앱 차트(deploy/helm/linkpulse): lint → 렌더 → 쿠버네티스 스키마(kubeconform) → 동작 단언
#   2. 알림 규칙·대시보드(앱 차트 files/): promtool 문법·단위 테스트(scripts/rules-test), 대시보드 JSON·PromQL 문법
#   3. 사이트 차트(infra/k8s/charts/site-aws, gitops): lint → 렌더(예시 값) → CRD 스키마
#   4. 사이트 스택 값(infra/k8s/values/*.tftpl): 예시 값으로 채워 고정 버전 차트(kube-prometheus-stack·fluent-bit)를
#      렌더하고 단언한다(네트워크로 차트를 받는다). Alertmanager 설정은 amtool로 검사한다.
#
# 단언은 "되돌리면 장애가 나는 설정"만 고정한다(ADR 0007): HPA면 replicas 없음, preStop sleep,
# readiness=/healthz, 롤링 surge 1·unavailable 0, 연결 예산, Image Updater 값의 이미지 참조.
# 필요: helm, kubeconform, promtool, amtool, jq. CRD 스키마는 저장소의 scripts/schemas를 먼저 보고, 없으면 datreeio/CRDs-catalog에서
# 받는다(네트워크). ImageUpdater는 카탈로그 스키마가 옛 버전이라(spec.namespace 필수) 고정 버전 CRD에서 만든
# 스키마를 둔다. Image Updater 차트 버전을 올리면 다시 만든다:
#   helm pull argo/argocd-image-updater --version <버전> --untar
#   scripts/schemas/crd-to-jsonschema.py argocd-image-updater/templates/crd-imageupdaters.yaml v1alpha1 \
#     scripts/schemas/argocd-image-updater.argoproj.io/imageupdater_v1alpha1.json
# 이 스키마는 모르는 필드를 거부한다(API 서버는 조용히 버려서 오타가 무시된다).
# ServiceMonitor·PrometheusRule도 같은 방법으로 만든다(kube-prometheus-stack 버전을 올리면 다시 만든다):
#   helm pull prometheus-community/kube-prometheus-stack --version <버전> --untar
#   scripts/schemas/crd-to-jsonschema.py kube-prometheus-stack/charts/crds/crds/crd-servicemonitors.yaml v1 \
#     scripts/schemas/monitoring.coreos.com/servicemonitor_v1.json   (prometheusrules도 같다)
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
# 렌더가 대시보드 JSON으로 커져서(plan 0015) 문서 하나만 떼어 본다 — 큰 입력에 grep -q를 걸면 pipefail에서 Broken pipe가 날 수 있다.
HPA=$(printf '%s\n' "$R" | awk '/^kind: HorizontalPodAutoscaler/{f=1} f&&/^---/{f=0} f')
check "HPA min 2·max 6·CPU 60%" 'printf "%s" "$HPA" | grep -q "minReplicas: 2" && printf "%s" "$HPA" | grep -q "maxReplicas: 6" && printf "%s" "$HPA" | grep -q "averageUtilization: 60"'
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

# plan 0015: 지표 포트와 모니터링 CR.
SVC=$(printf '%s\n' "$R" | awk '/^kind: Service$/{f=1} f&&/^---/{f=0} f')
SM=$(printf '%s\n' "$R" | awk '/^kind: ServiceMonitor/{f=1} f&&/^---/{f=0} f')
check "컨테이너 metrics 9090·METRICS_ADDR=:9090" 'printf "%s" "$DEP" | grep -B1 "containerPort: 9090" | grep -q "name: metrics" && printf "%s" "$DEP" | grep -A1 "name: METRICS_ADDR" | grep -q "\":9090\""'
check "Service에 metrics 포트(9090 → metrics), http는 8080 그대로" 'printf "%s" "$SVC" | grep -A2 "name: metrics" | grep -q "port: 9090" && printf "%s" "$SVC" | grep -A2 "name: http" | grep -q "port: 8080"'
check "aws → ServiceMonitor가 metrics 포트 /metrics를 본다(release 라벨 없음)" 'printf "%s" "$SM" | grep -q "port: metrics" && printf "%s" "$SM" | grep -q "path: /metrics" && ! printf "%s" "$SM" | grep -q "release:"'
check "aws → PrometheusRule에 linkpulse 규칙 그룹 4개" '[ "$(printf "%s\n" "$R" | grep -cE "^    - name: linkpulse-(slo|clicks|admin|argocd)$")" -eq 4 ]'
check "aws → 대시보드 ConfigMap 라벨 grafana_dashboard: \"1\"" 'printf "%s" "$R" | grep -B8 "linkpulse.json: |-" | grep -q "grafana_dashboard: \"1\""'
R6=$(helm template linkpulse "$APP" -n linkpulse)
check "기본값(monitoring 끔) → ServiceMonitor·PrometheusRule·대시보드 없음(CRD 없는 사이트)" '! printf "%s" "$R6" | grep -qE "^kind: (ServiceMonitor|PrometheusRule)" && ! printf "%s" "$R6" | grep -q "grafana_dashboard"'

# plan 0016: 런타임 기본값은 차트 ConfigMap에 두고 사이트 ConfigMap이 덮어쓴다(envFrom은 뒤가 이기고 env는 늘 이긴다).
DEFCM=$(printf '%s\n' "$R" | awk '/^# Source: linkpulse\/templates\/defaults-configmap.yaml/{f=1} f&&/^---/{f=0} f')
DEFNAME=$(printf '%s' "$DEFCM" | sed -n 's/^  name: //p' | head -1)
check "기본값 ConfigMap 이름 = <fullname>-defaults" '[ "$DEFNAME" = "linkpulse-defaults" ]'
check "기본값 GOMAXPROCS \"1\"·ACCESS_LOG_SAMPLE \"100\"" 'printf "%s" "$DEFCM" | grep -q "^  GOMAXPROCS: \"1\"$" && printf "%s" "$DEFCM" | grep -q "^  ACCESS_LOG_SAMPLE: \"100\"$"'
check "envFrom 순서: 기본값 → 사이트 ConfigMap → 사이트 Secret" '[ "$(printf "%s" "$DEP" | grep -A6 "envFrom:" | sed -n "s/^ *name: //p" | paste -sd, -)" = "$DEFNAME,linkpulse-site,linkpulse-site-secret" ]'
check "env에 기본값 키 없음(있으면 사이트가 덮어쓸 수 없다)" '! printf "%s" "$DEP" | grep -qE "name: (GOMAXPROCS|ACCESS_LOG_SAMPLE)$"'
R7=$(helm template linkpulse "$APP" -f "$APP/values-aws.yaml" --set runtimeDefaults.GOMAXPROCS=2 -n linkpulse)
sum_of() { printf '%s\n' "$1" | sed -n 's/^ *checksum\/defaults: \([0-9a-f]\{64\}\)$/\1/p'; }
check "파드 템플릿 checksum/defaults가 기본값을 따라 바뀐다(바뀌면 파드가 다시 뜬다)" '[ -n "$(sum_of "$DEP")" ] && [ -n "$(sum_of "$R7")" ] && [ "$(sum_of "$DEP")" != "$(sum_of "$R7")" ]'

echo "== 알림 규칙·대시보드 =="
RULES="$APP/files/rules.yaml"
DASH="$APP/files/dashboard.json"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
check "promtool check rules" 'promtool check rules "$RULES" >/dev/null'
check "promtool test rules(빠른·느린 소진, 저트래픽, 클릭 유실, 발행 실패, 401, ArgoCD·Progressing 제외)" 'promtool test rules scripts/rules-test/linkpulse.test.yaml >/dev/null'
check "대시보드 JSON 유효·uid 고정" '[ "$(jq -r .uid "$DASH")" = "linkpulse-overview" ]'
# 패널 쿼리를 기록 규칙으로 감싸 promtool로 문법만 본다(Grafana 변수는 예시 값으로 바꾼다).
jq '[.panels[].targets[]?.expr] | to_entries | {groups: [{name: "dashboard", rules: [.[] | {record: "dashboard:expr\(.key)", expr: (.value | gsub("\\$__rate_interval"; "1m") | gsub("\\$namespace"; "linkpulse"))}]}]}' "$DASH" >"$TMP/dash-rules.json"
check "대시보드 PromQL 문법" '[ "$(jq ".groups[0].rules | length" "$TMP/dash-rules.json")" -gt 0 ] && promtool check rules "$TMP/dash-rules.json" >/dev/null'
check "시험 알림 규칙(7-2) kubeconform" 'conform <infra/k8s/manifests/test-alert-rule.yaml >/dev/null'

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

echo "== 사이트 스택 값(kube-prometheus-stack·fluent-bit) =="
# infra/k8s/main.tf의 고정 버전을 그대로 쓴다. templatefile 자리(달러 중괄호)는 예시 값으로 채운다.
chart_version() { sed -n "s/^ *$1 *= *\"\([^\"]*\)\".*/\1/p" infra/k8s/main.tf; }
TOPIC=arn:aws:sns:ap-northeast-2:000000000000:linkpulse-prod-prom-alerts
fill() { sed -e "s|\${prom_alerts_topic_arn}|$TOPIC|g" -e 's|\${region}|ap-northeast-2|g' \
  -e 's|\${app_namespace}|linkpulse|g' -e 's|\${log_group_name}|/eks/linkpulse-prod/app|g' "$1"; }
fill infra/k8s/values/kube-prometheus-stack.yaml.tftpl >"$TMP/kps.yaml"
fill infra/k8s/values/fluent-bit.yaml.tftpl >"$TMP/fb.yaml"
check "tftpl 자리가 모두 채워짐(모르는 자리 없음)" '! grep -q "\\\${" "$TMP/kps.yaml" "$TMP/fb.yaml"'
KPS_VER=$(chart_version kube_prometheus_stack)
KPS_ARGS=(kube-prometheus-stack kube-prometheus-stack --repo https://prometheus-community.github.io/helm-charts --version "$KPS_VER"
  -n monitoring -f "$TMP/kps.yaml" --kube-version "$K8S_VERSION")
# 렌더가 커서(수천 줄) 파일로 두고 grep한다. printf | grep -q는 grep이 먼저 끝나면 pipefail에서 Broken pipe로 실패한다.
K="$TMP/kps-render.yaml"
helm template "${KPS_ARGS[@]}" >"$K"
check "kube-prometheus-stack $KPS_VER 렌더" '[ -s "$K" ]'
PROM=$(awk '/^kind: Prometheus$/{f=1} f&&/^---/{f=0} f' "$K")
check "EKS 컨트롤 플레인·kube-proxy ServiceMonitor 없음" '! grep -A3 "^kind: ServiceMonitor" "$K" | grep -qE "name: .*(etcd|controller-manager|scheduler|kube-proxy)"'
check "세 *SelectorNilUsesHelmValues=false → 선택자 {}(모든 네임스페이스의 CR)" '(for k in serviceMonitorSelector podMonitorSelector ruleSelector serviceMonitorNamespaceSelector podMonitorNamespaceSelector ruleNamespaceSelector; do printf "%s" "$PROM" | grep -q "^  $k: {}$" || exit 1; done)'
check "보관 2일·emptyDir(PVC 없음)·자원 요청" 'printf "%s" "$PROM" | grep -q "retention: \"2d\"" && ! printf "%s" "$PROM" | grep -q volumeClaimTemplate && printf "%s" "$PROM" | grep -A2 "requests:" | grep -q "memory: 1Gi"'
check "kubelet /metrics/resource 수집(E1 세 번째 출처)" 'grep -q "path: /metrics/resource$" "$K"'
# ArgoCD 알림이 Progressing을 빼므로(plan 0016) 멈춘 확장·롤링은 이 기본 규칙이 잡는다(런북 P-4).
check "기본 규칙 KubeDeploymentReplicasMismatch·KubeDeploymentRolloutStuck·KubePodNotReady(멈춘 확장·롤링)" 'grep -q "alert: KubeDeploymentReplicasMismatch$" "$K" && grep -q "alert: KubeDeploymentRolloutStuck$" "$K" && grep -q "alert: KubePodNotReady$" "$K"'
check "Alertmanager 서비스 어카운트 = infra/prod Pod Identity 키" 'grep -q "serviceAccountName: alertmanager" "$K" && grep -A3 "alertmanager = {" infra/prod/iam.tf | grep -q "service_account = \"alertmanager\""'
# Alertmanager 설정 Secret을 풀어 amtool로 본다. 템플릿 경로는 임시 디렉터리로 바꾼다.
am_secret() { awk '/^kind: Secret/{f=1} f&&/^---/{f=0} f' "$K" | sed -n "s/^  $1: \"\(.*\)\"$/\1/p" | head -1 | base64 -d; }
am_secret alertmanager.yaml | sed "s|/etc/alertmanager/config/\*.tmpl|$TMP/*.tmpl|" >"$TMP/alertmanager.yaml"
am_secret linkpulse.tmpl >"$TMP/linkpulse.tmpl"
check "Alertmanager 설정 amtool check-config" 'amtool check-config "$TMP/alertmanager.yaml" >/dev/null'
check "SNS 수신기가 prom-alerts 토픽·리전·ASCII 제목" 'grep -q "topic_arn: $TOPIC" "$TMP/alertmanager.yaml" && grep -A1 "sigv4:" "$TMP/alertmanager.yaml" | grep -q "region: ap-northeast-2" && grep -q "subject: .{{ .Status | toUpper }} {{ .CommonLabels.alertname }}." "$TMP/alertmanager.yaml"'
check "Watchdog·InfoInhibitor → null, 그 밖 → slack" '[ "$(amtool config routes test --config.file="$TMP/alertmanager.yaml" alertname=Watchdog)" = null ] && [ "$(amtool config routes test --config.file="$TMP/alertmanager.yaml" alertname=InfoInhibitor)" = null ] && [ "$(amtool config routes test --config.file="$TMP/alertmanager.yaml" alertname=LinkpulseClicksDropped severity=critical)" = slack ]'
check "Chatbot 메시지가 사용자 지정 알림 JSON(version 1.0·source custom·description)" 'amtool template render --template.glob="$TMP/linkpulse.tmpl" --template.text="{{ template \"linkpulse.chatbot\" . }}" | jq -e ".version == \"1.0\" and .source == \"custom\" and (.content.description | length > 0)" >/dev/null'
GPW1=$(sed -n 's/^  admin-password: "\(.*\)"$/\1/p' "$K")
GPW2=$(helm template "${KPS_ARGS[@]}" | sed -n 's/^  admin-password: "\(.*\)"$/\1/p')
check "Grafana 관리자 비밀번호가 고정 문자열(prom-operator)이 아님(렌더마다 난수)" '[ -n "$GPW1" ] && [ "$GPW1" != "$(printf prom-operator | base64)" ] && [ "$GPW1" != "$GPW2" ]'
check "Grafana 익명 Viewer" 'grep -A2 "\[auth.anonymous\]" "$K" | grep -q "enabled = true" && grep -A2 "\[auth.anonymous\]" "$K" | grep -q "org_role = Viewer"'
check "대시보드 사이드카 = 앱 차트 ConfigMap 라벨(grafana_dashboard=1, 모든 네임스페이스)" 'grep -A1 "name: LABEL$" "$K" | grep -q "\"grafana_dashboard\"" && grep -A1 "name: LABEL_VALUE$" "$K" | head -2 | grep -q "\"1\"" && grep -A1 "name: NAMESPACE$" "$K" | grep -q "\"ALL\""'
FB_VER=$(chart_version fluent_bit)
F="$TMP/fb-render.yaml"
helm template fluent-bit fluent-bit --repo https://fluent.github.io/helm-charts --version "$FB_VER" -n logging -f "$TMP/fb.yaml" >"$F"
check "fluent-bit $FB_VER 렌더" '[ -s "$F" ]'
check "Fluent Bit tail: storage.type filesystem·DB·Rotate_Wait 30, Mem_Buf_Limit 없음(G-15)" 'grep -qE "storage.type +filesystem" "$F" && grep -qE "DB +/var/log/flb_kube.db" "$F" && grep -qE "Rotate_Wait +30" "$F" && ! grep -q Mem_Buf_Limit "$F"'
check "Fluent Bit 서비스 storage.path·출력 total_limit_size" 'grep -q "storage.path /var/log/flb-storage/" "$F" && grep -qE "storage.total_limit_size +1G" "$F"'
check "Fluent Bit /var/log 마운트가 쓰기 가능(readOnly 아님)" 'grep -q "mountPath: /var/log$" "$F" && ! grep -A2 "mountPath: /var/log$" "$F" | grep -q "readOnly: true"'

exit $fail
