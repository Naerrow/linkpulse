#!/usr/bin/env bash
# plan 0012 — capacity.js 한 번의 실행을 분 단위·단계 단위 표(Markdown)로 모은다.
#
# 사용법: load/k6/collect-metrics.sh <시작_epoch> <k6_csv(.gz 가능)> [MAX_RPS] [LOG_GROUP]
#   시작_epoch: capacity.js setup이 찍은 "시나리오 시작 … epoch=" 값(정각)
#   MAX_RPS:    k6 실행 때 준 값(기본 1600). 단계표를 capacity.js와 같은 규칙으로 자른다.
#   LOG_GROUP:  앱 로그 그룹(기본 /ecs/<PREFIX>-app — 0012·0013 재수집). EKS(plan 0014)는 /eks/linkpulse-prod/app.
#
# EKS 실행이면 record-pods.sh가 CSV 옆에 남긴 <이름>-pods.tsv·-hpa.tsv를 읽어 "파드·HPA" 절을 덧붙인다.
# 그 파일이 없으면(0012·0013) 출력은 예전과 같다. ECS CPU 열은 EKS에서 비어 나온다(Container Insights를 켜지 않았다).
#
# plan 0015: 클러스터 안 Prometheus(monitoring/kube-prometheus-stack-prometheus)가 있으면 kubectl port-forward를 백그라운드로
# 열어(끝날 때 닫는다) Prometheus 절·카운터 정합성·로그 유실(파드·분당 줄 수) 표를 덧붙인다. 원본 응답은 <이름>-prom.json·
# -lines.json으로 남기고, 클러스터가 없으면(destroy 뒤 재수집) 그것을 다시 쓴다. 둘 다 없으면(0012~0014) 이 절을 건너뛴다.
#
# plan 0016: 로그 양(앱 로그 그룹 IncomingBytes·Logs Insights "http request" 줄 수)과 Fluent Bit CPU·ArgoCD health를 "로그 양·
# Fluent Bit·ArgoCD" 절로 덧붙인다(요청 로그 표본화·ArgoCD 알림 판정). 그 자료가 하나도 없으면(0012~0015 재수집) 이 절을 건너뛴다.
# START_OFFSET_S로 비킨 실행(정각 + 30초)은 직전 정각(epoch − 30)을 시작으로 준다 — CloudWatch가 벽시계 분 단위로만 주고,
# 유지 구간 2분 30초가 벽시계 두 분을 그대로 덮는다(load/k6/README.md 0016 창 절차).
#
# 실행이 끝나고 3분쯤 뒤, destroy 전에 돌린다 — 지표·로그가 늦게 들어오고, destroy하면 로그 그룹이 사라진다.
# 읽기 전용 AWS 호출만 쓴다(describe-load-balancers, get-metric-data, logs start-query/get-query-results).
# Prometheus에는 읽기(query·query_range)만 한다.
#
# 단계 i(0부터)는 [시작 + 3i분, 시작 + 3i+3분)이다. 첫 1분은 30초 램프가 섞여 빼고,
# 유지 구간의 온전한 2분(3i+1, 3i+2)만 단계 값으로 쓴다.
set -euo pipefail

START=${1:?시작 epoch가 필요하다}
CSV=${2:?k6 CSV 경로가 필요하다}
MAX_RPS=${3:-1600}
REGION=${AWS_REGION:-ap-northeast-2}
PREFIX=${NAME_PREFIX:-linkpulse-prod}
LOG_GROUP=${4:-/ecs/$PREFIX-app}
# 파드 CPU 요청(차트 values.yaml resources.requests.cpu, 밀리코어). 파드 CPU %의 분모다 — HPA도 같은 분모를 쓴다.
POD_CPU_REQUEST_M=${POD_CPU_REQUEST_M:-250}

if [[ ! $START =~ ^[0-9]+$ ]]; then
  echo "시작 epoch가 숫자가 아니다: $START" >&2
  exit 1
fi
if (( START % 60 != 0 )); then
  echo "시작 epoch가 정각이 아니다: $START" >&2
  exit 1
fi
[[ -r $CSV ]] || { echo "k6 CSV를 읽을 수 없다: $CSV" >&2; exit 1; }

# capacity.js의 ALL_RATES와 같아야 한다.
STAGES=()
for r in 50 100 200 400 800 1600 3200 6400; do
  if (( r <= MAX_RPS )); then STAGES+=("$r"); fi
done
if (( ${#STAGES[@]} == 0 )); then
  echo "MAX_RPS=$MAX_RPS 이하 단계가 없다" >&2
  exit 1
fi
END=$(( START + ${#STAGES[@]} * 180 ))
STAGES_JSON=$(printf '%s\n' "${STAGES[@]}" | jq -sc .)

TMP=$(mktemp -d)
PF_PID=""
trap 'rm -rf "$TMP"; if [[ -n $PF_PID ]]; then kill "$PF_PID" 2>/dev/null || true; fi' EXIT

# ---- CloudWatch 지표(1분 단위) ----
# ALB 지표 차원은 ARN 끝부분(app/<이름>/<id>)이다. 계정 ID가 든 ARN 전체는 찍지 않는다.
ALB_ARN=$(aws elbv2 describe-load-balancers --region "$REGION" --names "$PREFIX-alb" \
  --query 'LoadBalancers[0].LoadBalancerArn' --output text)
ALB_DIM=${ALB_ARN#*:loadbalancer/}

jq -n --arg alb "$ALB_DIM" --arg p "$PREFIX" --arg lg "$LOG_GROUP" '
  def m(id; ns; name; stat; dims):
    {Id: id, MetricStat: {Metric: {Namespace: ns, MetricName: name, Dimensions: dims}, Period: 60, Stat: stat}};
  [{Name: "LoadBalancer", Value: $alb}] as $alb
  | [{Name: "ClusterName", Value: "\($p)-cluster"}, {Name: "ServiceName", Value: "\($p)-app"}] as $ecs
  | [{Name: "DBInstanceIdentifier", Value: "\($p)-pg"}] as $rds
  | [{Name: "QueueName", Value: "\($p)-clicks"}] as $sqs
  | [
      m("alb_req"; "AWS/ApplicationELB"; "RequestCount"; "Sum"; $alb),
      m("alb_p95"; "AWS/ApplicationELB"; "TargetResponseTime"; "p95"; $alb),
      m("alb_p99"; "AWS/ApplicationELB"; "TargetResponseTime"; "p99"; $alb),
      m("tgt_5xx"; "AWS/ApplicationELB"; "HTTPCode_Target_5XX_Count"; "Sum"; $alb),
      m("elb_5xx"; "AWS/ApplicationELB"; "HTTPCode_ELB_5XX_Count"; "Sum"; $alb),
      m("ecs_cpu"; "AWS/ECS"; "CPUUtilization"; "Average"; $ecs),
      m("ecs_cpu_max"; "AWS/ECS"; "CPUUtilization"; "Maximum"; $ecs),
      m("ecs_mem"; "AWS/ECS"; "MemoryUtilization"; "Average"; $ecs),
      m("rds_cpu"; "AWS/RDS"; "CPUUtilization"; "Average"; $rds),
      m("rds_conn"; "AWS/RDS"; "DatabaseConnections"; "Maximum"; $rds),
      m("rds_surplus"; "AWS/RDS"; "CPUSurplusCreditsCharged"; "Sum"; $rds),
      m("rds_wlat"; "AWS/RDS"; "WriteLatency"; "Average"; $rds),
      m("rds_rlat"; "AWS/RDS"; "ReadLatency"; "Average"; $rds),
      m("sqs_age"; "AWS/SQS"; "ApproximateAgeOfOldestMessage"; "Maximum"; $sqs),
      m("sqs_sent"; "AWS/SQS"; "NumberOfMessagesSent"; "Sum"; $sqs),
      m("log_bytes"; "AWS/Logs"; "IncomingBytes"; "Sum"; [{Name: "LogGroupName", Value: $lg}])
    ]' > "$TMP/queries.json"

iso() { jq -rn --argjson t "$1" '$t | todate'; }
aws cloudwatch get-metric-data --region "$REGION" \
  --metric-data-queries "file://$TMP/queries.json" \
  --start-time "$(iso "$START")" --end-time "$(iso "$END")" \
  --scan-by TimestampAscending --output json > "$TMP/cw.json"
# 원본 응답을 CSV 옆에 남긴다 — 표를 다시 만들거나 숫자를 대조할 때 다시 조회하지 않아도 된다(destroy 뒤에도).
RAW=${CSV%.csv*}
cp "$TMP/cw.json" "$RAW-cw.json"

# ---- Logs Insights(앱 로그) ----
# insights <쿼리> <출력 파일> [끝 epoch] — 끝을 주지 않으면 단계 구간의 끝(END)까지다.
insights() {
  local qid status
  qid=$(aws logs start-query --region "$REGION" --log-group-name "$LOG_GROUP" \
    --start-time "$START" --end-time "${3:-$END}" --query-string "$1" --query queryId --output text)
  for _ in $(seq 90); do
    aws logs get-query-results --region "$REGION" --query-id "$qid" --output json > "$2"
    status=$(jq -r .status "$2")
    case $status in
      Complete) return 0 ;;
      Failed | Cancelled | Timeout | Unknown) echo "Logs Insights 쿼리 $status" >&2; exit 1 ;;
    esac
    sleep 2
  done
  echo "Logs Insights 쿼리가 3분 안에 끝나지 않았다" >&2
  exit 1
}
# wait_count·wait_ms는 줄마다 직전 줄 이후 증가분이라 합친다. tasks는 그 분에 로그를 남긴 태스크 수다.
# 별칭을 원래 필드 이름(wait_count)과 같게 두면 Logs Insights가 결과에서 그 열을 빼 버린다 — 다른 이름을 쓴다.
insights 'filter msg = "db pool"
  | stats sum(wait_count) as waits, sum(wait_ms) as waited_ms, max(in_use) as in_use_max,
          avg(in_use) as in_use_avg, count_distinct(task_id) as tasks by bin(1m)' "$TMP/pool.json"
# 클릭 실패: 0012의 동기 UPDATE 실패와 0013의 발행·반영 실패를 함께 센다(같은 스크립트로 두 측정을 다시 수집할 수 있게).
insights 'filter msg in ["클릭 집계 실패", "클릭 발행 실패", "클릭 반영 실패"] | stats count(*) as n by bin(1m)' "$TMP/fail.json"
# plan 0013 클릭 파이프라인 통계(10초마다 증가분). 0012 실행에는 없어서 열이 비어 나온다.
insights 'filter msg = "click queue"
  | stats sum(recorded_clicks) as rec, sum(dropped_clicks) as drp, sum(applied_clicks) as app,
          max(pending_codes) as pend by bin(1m)' "$TMP/cq.json"
# 정합성(plan 0013 판정 (b)): 시작부터 지금까지 합. 수집은 실행이 끝나고 큐가 빈 뒤에 하므로 끝을 지금으로 둔다.
insights 'filter msg = "click queue"
  | stats sum(recorded_clicks) as rec, sum(dropped_clicks) as drp, sum(applied_clicks) as app,
          sum(unknown_clicks) as unk, sum(duplicate_batches) as dups, sum(apply_errors) as aerr' "$TMP/cons.json" "$(date +%s)"
# 요청 로그 줄 수(plan 0016 판정 (d)). 표본화하면 성공·빠른 요청은 1/N만 남는다 — 요청 수는 지표로 본다.
insights 'filter msg = "http request" | stats count(*) as n by bin(1m)' "$TMP/req.json"
cp "$TMP/pool.json" "$RAW-pool.json"
cp "$TMP/req.json" "$RAW-req.json"
cp "$TMP/fail.json" "$RAW-fail.json"
cp "$TMP/cq.json" "$RAW-cq.json"
cp "$TMP/cons.json" "$RAW-cons.json"

# ---- Prometheus(plan 0015) ----
PROM_RAW="$RAW-prom.json"
LINES_RAW="$RAW-lines.json"
PROM=""
if kubectl -n monitoring get svc kube-prometheus-stack-prometheus >/dev/null 2>&1; then
  PF_PORT=${PF_PORT:-19090}
  kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus "$PF_PORT:9090" >/dev/null 2>&1 &
  PF_PID=$!
  PROM="http://127.0.0.1:$PF_PORT"
  ready=false
  for _ in $(seq 30); do
    if curl -fsS "$PROM/-/ready" >/dev/null 2>&1; then ready=true; break; fi
    sleep 1
  done
  [[ $ready == true ]] || { echo "Prometheus port-forward가 30초 안에 준비되지 않았다" >&2; exit 1; }
fi
if [[ -n $PROM ]]; then
  # range는 분 끝(t+60)에 [1m] 창으로 평가한다 — 그 값이 분 t의 값이다.
  # 정합성은 drain이 끝난 고정 시각(T1)에 한 번 묻는다: teardown의 "DB 클릭 합계" 줄 시각(DB 합계가 멈춘 때) + 긁기 두 번(30초).
  # 이 시각은 HPA 축소(부하가 떨어지고 약 5분 뒤)보다 앞이라 늘어난 파드가 아직 있고, 파드가 지워진 뒤 다시 수집해도 답이 같다.
  # 그 줄이 없으면 지금 시각을 쓴다.
  T1=$(date +%s)
  LOG_TS=$(grep 'DB 클릭 합계 = ' "$RAW.log" 2>/dev/null | tail -1 | grep -o 'time="[^"]*"' | cut -d'"' -f2 || true)
  LOG_EPOCH=$(jq -rn --arg t "$LOG_TS" '$t | capture("^(?<dt>[0-9-]{10}T[0-9:]{8})(?<tz>Z|[+-][0-9]{2}:[0-9]{2})$")? as $c
    | ($c.dt + "Z" | fromdateiso8601)
      - (if $c.tz == "Z" then 0 else (($c.tz[1:3] | tonumber) * 3600 + ($c.tz[4:6] | tonumber) * 60) * (if $c.tz[0:1] == "-" then -1 else 1 end) end)')
  if [[ $LOG_EPOCH =~ ^[0-9]+$ ]] && (( LOG_EPOCH + 30 < T1 )); then T1=$(( LOG_EPOCH + 30 )); fi
  D=$(( T1 - START ))
  NSEL='namespace="linkpulse"'
  SLI='route=~"GET /\\{code\\}|GET /api/links/\\{code\\}"'
  prom_range() {
    curl -fsS --get "$PROM/api/v1/query_range" --data-urlencode "query=$1" \
      --data-urlencode "start=$(( START + 60 ))" --data-urlencode "end=$END" --data-urlencode step=60
  }
  prom_at() { curl -fsS --get "$PROM/api/v1/query" --data-urlencode "query=$1" --data-urlencode "time=$T1"; }
  pod_cpu() { echo "sum by (pod) (rate(container_cpu_usage_seconds_total{$NSEL,container=\"app\",metrics_path=\"$1\"}[1m]))"; }
  PROC="sum by (pod) (rate(process_cpu_seconds_total{$NSEL}[1m]))"
  BUCKET="linkpulse_http_request_duration_seconds_bucket{$NSEL,$SLI}"
  # 카운터 증가분(정수): 구간 끝의 최댓값 − 구간 시작 값. 실행 중에 새로 뜬 파드는 시작 값이 없어 0으로 본다.
  # increase()는 외삽해서 정수가 맞지 않아 쓰지 않는다. 리셋(재시작)·사라진 파드는 이 식이 다루지 못해 아래에서 따로 센다.
  inc() { echo "sum(max_over_time($1{$NSEL}[${D}s] @ $T1) - (($1{$NSEL} @ $START) * 1 or max_over_time($1{$NSEL}[${D}s] @ $T1) * 0))"; }
  RANGE=(
    rps "sum(rate(linkpulse_http_requests_total{$NSEL}[1m]))"
    p95 "histogram_quantile(0.95, sum by (le) (rate(${BUCKET}[1m])))"
    p99 "histogram_quantile(0.99, sum by (le) (rate(${BUCKET}[1m])))"
    slow "1 - sum(rate(linkpulse_http_request_duration_seconds_bucket{$NSEL,$SLI,le=\"0.1\"}[1m])) / sum(rate(linkpulse_http_request_duration_seconds_count{$NSEL,$SLI}[1m]))"
    e5xx "sum(increase(linkpulse_http_requests_total{$NSEL,code=~\"5..\"}[1m]))"
    rpc "sum(rate(linkpulse_http_requests_total{$NSEL}[1m])) / sum(rate(linkpulse_http_connections_opened_total{$NSEL}[1m]))"
    pods "count(kube_pod_status_ready{$NSEL,condition=\"true\"} == 1)"
    cad_avg "avg($(pod_cpu /metrics/cadvisor))"
    cad_max "max($(pod_cpu /metrics/cadvisor))"
    res_avg "avg($(pod_cpu /metrics/resource))"
    res_max "max($(pod_cpu /metrics/resource))"
    proc_avg "avg($PROC)"
    proc_max "max($PROC)"
    throttle "sum(rate(container_cpu_cfs_throttled_periods_total{$NSEL,container=\"app\"}[1m])) / sum(rate(container_cpu_cfs_periods_total{$NSEL,container=\"app\"}[1m]))"
    gmp_min "min(go_sched_gomaxprocs_threads{$NSEL})"
    gmp_max "max(go_sched_gomaxprocs_threads{$NSEL})"
    # 파드가 그 분 전체에 살아 있었는가: 직전 긁기를 포함한 75초 창의 up 5번(15초 간격)이 모두 1.
    alive "min_over_time(up{$NSEL}[75s]) and count_over_time(up{$NSEL}[75s]) >= 5"
    # plan 0016: Fluent Bit CPU(노드마다 파드 하나 — 합과 가장 바쁜 파드), 표본과 무관하게 늘 남는 요청 수(4xx·5xx + 100ms 초과),
    # ArgoCD health(그 분에 보인 상태마다 시계열 하나)와 LinkpulseArgoCDAppUnhealthy(pending 포함) 시계열 수.
    # 늘 남는 요청의 항마다 "or vector(0)"을 붙인다 — 4xx·5xx 시계열은 그 코드가 처음 나올 때 생겨서, 없으면 합 전체가 빈다
    # (-prom.json에는 합만 남아 destroy 뒤 되살릴 수 없다). 5xx이면서 100ms를 넘는 요청은 두 번 세어진다(드물다).
    fb_cpu "sum(rate(container_cpu_usage_seconds_total{namespace=\"logging\",container=\"fluent-bit\",metrics_path=\"/metrics/cadvisor\"}[1m]))"
    fb_max "max(sum by (pod) (rate(container_cpu_usage_seconds_total{namespace=\"logging\",container=\"fluent-bit\",metrics_path=\"/metrics/cadvisor\"}[1m])))"
    req_total "sum(increase(linkpulse_http_requests_total{$NSEL}[1m]))"
    req_kept "(sum(increase(linkpulse_http_requests_total{$NSEL,code=~\"[45]..\"}[1m])) or vector(0)) + (sum(increase(linkpulse_http_request_duration_seconds_count{$NSEL}[1m])) or vector(0)) - (sum(increase(linkpulse_http_request_duration_seconds_bucket{$NSEL,le=\"0.1\"}[1m])) or vector(0))"
    argo_health "max by (name, health_status) (max_over_time(argocd_app_info[1m]))"
    argo_alert "count(max_over_time(ALERTS{alertname=\"LinkpulseArgoCDAppUnhealthy\"}[1m]))"
  )
  AT=(
    recorded "$(inc linkpulse_clicks_recorded_total)"
    dropped "$(inc linkpulse_clicks_dropped_total)"
    applied "$(inc linkpulse_clicks_applied_total)"
    unknown "$(inc linkpulse_clicks_unknown_total)"
    dups "$(inc linkpulse_clicks_duplicate_batches_total)"
    aerr "$(inc linkpulse_clicks_apply_errors_total)"
    # 재시작하면 모든 카운터가 함께 0이 된다 — 기록·반영 두 카운터의 리셋으로 본다(이름 정규식으로 묶으면 이름이 빠져 라벨이 겹친다).
    resets "sum(resets(linkpulse_clicks_recorded_total{$NSEL}[${D}s] @ $T1)) + sum(resets(linkpulse_clicks_applied_total{$NSEL}[${D}s] @ $T1))"
    gone "count(last_over_time(linkpulse_clicks_recorded_total{$NSEL}[${D}s] @ $T1)) - (count(linkpulse_clicks_recorded_total{$NSEL} @ $T1) or vector(0))"
  )
  mkdir -p "$TMP/prom"
  for (( i = 0; i < ${#RANGE[@]}; i += 2 )); do prom_range "${RANGE[i+1]}" > "$TMP/prom/${RANGE[i]}.json"; done
  for (( i = 0; i < ${#AT[@]}; i += 2 )); do prom_at "${AT[i+1]}" > "$TMP/prom/${AT[i]}.json"; done
  (cd "$TMP/prom" && jq -n 'reduce inputs as $r ({}; . + {(input_filename | ltrimstr("./") | rtrimstr(".json")): $r})' ./*.json) > "$PROM_RAW"
  # 로그 유실 검사(판정 (c)): 파드·분마다 "click queue"·"db pool" 줄 수. 10초 tick이라 각각 6줄이어야 한다.
  insights 'filter msg in ["click queue", "db pool"] | stats count(*) as n by bin(1m), task_id, msg' "$LINES_RAW"
fi

# ---- 파드·HPA 기록(EKS, record-pods.sh) — 분마다 모은다. 없으면 빈 객체다. ----
# pods.tsv: epoch, 파드, CPU(밀리코어), 메모리(MiB) / hpa.tsv: epoch, 현재 파드 수, 목표 파드 수, HPA CPU %
PODS_FILE="$RAW-pods.tsv"
HPA_FILE="$RAW-hpa.tsv"
if [[ -r $PODS_FILE ]]; then
  awk -F'\t' '{ m = $1 - $1 % 60; k = m SUBSEP $2; n[m]++; sum[m] += $3; if ($3 > mx[m]) mx[m] = $3
                 if (!(k in seen)) { seen[k] = 1; pods[m]++ } }
    END { for (m in n) printf "{\"t\":%d,\"pods\":%d,\"avg\":%.1f,\"max\":%d}\n", m, pods[m], sum[m] / n[m], mx[m] }' "$PODS_FILE" \
    | jq -s 'map({key: (.t | tostring), value: .}) | from_entries' > "$TMP/pods.json"
else
  echo '{}' > "$TMP/pods.json"
fi
if [[ -r $HPA_FILE ]]; then
  awk -F'\t' '{ m = $1 - $1 % 60; if ($2 > cur[m]) cur[m] = $2; if ($3 > des[m]) des[m] = $3; if ($4 > util[m]) util[m] = $4; seen[m] = 1 }
    END { for (m in seen) printf "{\"t\":%d,\"cur\":%d,\"des\":%d,\"util\":%d}\n", m, cur[m], des[m], util[m] }' "$HPA_FILE" \
    | jq -s 'map({key: (.t | tostring), value: .}) | from_entries' > "$TMP/hpa.json"
else
  echo '{}' > "$TMP/hpa.json"
fi

# teardown이 찍은 DB 합계(k6 실행 로그가 CSV와 같은 이름으로 옆에 있을 때).
DB_TOTAL=$(grep -o 'DB 클릭 합계 = [0-9]*' "$RAW.log" 2>/dev/null | grep -o '[0-9]*$' | tail -1 || true)

# ---- k6 CSV: 분마다 클라이언트 요청 수와 dropped_iterations ----
# 앞 세 열(metric_name, timestamp, metric_value)만 쓴다. 인용부호가 들어갈 수 있는 열보다 앞이라 쉼표로 잘라도 안전하다.
if [[ $CSV == *.gz ]]; then reader=(gzip -dc "$CSV"); else reader=(cat "$CSV"); fi
"${reader[@]}" | awk -F, -v s="$START" -v e="$END" '
  $1 == "http_reqs" || $1 == "dropped_iterations" {
    t = int($2); if (t < s || t >= e) next
    m = t - t % 60; seen[m] = 1
    if ($1 == "http_reqs") reqs[m]++; else dropped[m] += $3
  }
  END { for (m in seen) printf "{\"t\":%d,\"reqs\":%d,\"dropped\":%d}\n", m, reqs[m], dropped[m] }' \
  | jq -s 'map({key: (.t | tostring), value: .}) | from_entries' > "$TMP/k6.json"

# ---- 합쳐서 표로 찍는다 ----
echo "구간 $(iso "$START") ~ $(iso "$END"), 단계 ${STAGES[*]} rps"
echo
jq -nr --argjson start "$START" --argjson stages "$STAGES_JSON" --arg dbtotal "${DB_TOTAL:-}" \
  --slurpfile cw "$TMP/cw.json" --slurpfile pool "$TMP/pool.json" \
  --slurpfile fail "$TMP/fail.json" --slurpfile k6 "$TMP/k6.json" \
  --slurpfile cq "$TMP/cq.json" --slurpfile cons "$TMP/cons.json" '
  # CloudWatch 시각. CLI가 로컬 시간대로 줄 수 있다("2026-10-12T09:37:00+09:00") — 오프셋을 빼서 UTC epoch로 맞춘다.
  def cwts:
    if type == "number" then floor | tostring
    else capture("^(?<dt>[0-9-]{10}T[0-9:]{8})(\\.[0-9]+)?(?<tz>Z|[+-][0-9]{2}:[0-9]{2})$") as $c
      | ($c.dt + "Z" | fromdateiso8601)
        - (if $c.tz == "Z" then 0
           else ($c.tz[1:3] | tonumber) * 3600 + ($c.tz[4:6] | tonumber) * 60
                | if $c.tz[0:1] == "-" then -. else . end end)
      | tostring
    end;
  def bints: .[0:10] + "T" + .[11:19] + "Z" | fromdateiso8601 | tostring; # "2026-10-08 12:34:00.000"
  def byminute: map({key: (.["bin(1m)"] | bints), value: (map_values(tonumber? // .))}) | from_entries;
  def rows($r): $r | map(map({key: .field, value: .value}) | from_entries);
  def mul($k): if . == null then null else . * $k end;
  def div($d): if . == null or $d == null or $d == 0 then null else . / $d end;
  def r1: if . == null then null else . * 10 | round / 10 end;
  def show: if . == null then "-" else tostring end;
  def maxn: map(select(. != null)) | max;
  def avgn: map(select(. != null)) | if length == 0 then null else add / length end;
  def sumn: map(select(. != null)) | if length == 0 then null else add end;

  ($cw[0].MetricDataResults | map({key: .Id,
     value: ([.Timestamps, .Values] | transpose | map({key: (.[0] | cwts), value: .[1]}) | from_entries)})
   | from_entries) as $m
  | (rows($pool[0].results) | byminute) as $p
  | (rows($fail[0].results) | byminute) as $f
  | (rows($cq[0].results) | byminute) as $q
  | (rows($cons[0].results) | first // {} | map_values(tonumber? // .)) as $c
  | $k6[0] as $k

  # 한 분의 값. 건수 지표는 데이터가 없으면 0이다(CloudWatch는 0건인 분을 빼고 준다).
  # 풀·클릭 실패는 그 분에 "db pool" 로그가 있을 때만 0으로 본다 — 로그가 아직 안 들어온 것과 0건을 구분한다.
  | def minute($s; $t):
      ($m.alb_req[$t] // 0) as $req
      | { stage: $s, t: ($t | tonumber),
          k6_rps: (($k[$t].reqs // 0) / 60), dropped: ($k[$t].dropped // 0),
          alb_rps: ($req / 60), alb_req: $req,
          p95: ($m.alb_p95[$t] | mul(1000)), p99: ($m.alb_p99[$t] | mul(1000)),
          tgt5xx: ($m.tgt_5xx[$t] // 0), elb5xx: ($m.elb_5xx[$t] // 0),
          ecs_cpu: $m.ecs_cpu[$t], ecs_cpu_max: $m.ecs_cpu_max[$t], ecs_mem: $m.ecs_mem[$t],
          rds_cpu: $m.rds_cpu[$t], rds_conn: $m.rds_conn[$t], rds_surplus: $m.rds_surplus[$t],
          wlat: ($m.rds_wlat[$t] | mul(1000)), rlat: ($m.rds_rlat[$t] | mul(1000)),
          wait_count: $p[$t].waits, wait_ms: $p[$t].waited_ms,
          in_use_max: $p[$t].in_use_max, in_use_avg: $p[$t].in_use_avg,
          # 연결 보유 시간(ms) = 태스크당 평균 in_use ÷ 태스크당 처리량 (Little의 법칙)
          hold_ms: ($p[$t].in_use_avg | div(($req / 60) | div($p[$t].tasks)) | mul(1000)),
          click_fail: (if $p[$t] then ($f[$t].n // 0) else null end),
          sqs_age: $m.sqs_age[$t], sqs_sent: $m.sqs_sent[$t],
          pending_max: $q[$t].pend, dropped_clicks: $q[$t].drp };

  [ range(0; $stages | length) as $i | range(1; 3) as $j
    | minute($stages[$i]; ($start + ($i * 3 + $j) * 60 | tostring)) ] as $rows

  | "### 분 단위 (램프가 섞인 분 제외)",
    "",
    "| 단계 rps | 분(UTC) | k6 rps | dropped | ALB rps | p95 ms | p99 ms | 타깃 5xx | ELB 5xx | ECS CPU 평균/최대 % | ECS 메모리 % | RDS CPU % | DB 연결 | 초과 크레딧 과금 | RDS 쓰기/읽기 지연 ms | 풀 대기 건 | 풀 대기 ms | in_use 최대/평균 | 연결 보유 ms | 클릭 실패 | SQS 최고 대기 s | SQS 보낸 메시지 | 대기 코드 최대 | 버린 클릭 |",
    "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |",
    ($rows[] | "| \(.stage) | \(.t | todate | .[11:16]) | \(.k6_rps | r1) | \(.dropped) | \(.alb_rps | r1) | \(.p95 | r1 | show) | \(.p99 | r1 | show) | \(.tgt5xx) | \(.elb5xx) | \(.ecs_cpu | r1 | show)/\(.ecs_cpu_max | r1 | show) | \(.ecs_mem | r1 | show) | \(.rds_cpu | r1 | show) | \(.rds_conn | show) | \(.rds_surplus | r1 | show) | \(.wlat | r1 | show)/\(.rlat | r1 | show) | \(.wait_count | show) | \(.wait_ms | show) | \(.in_use_max | show)/\(.in_use_avg | r1 | show) | \(.hold_ms | r1 | show) | \(.click_fail | show) | \(.sqs_age | show) | \(.sqs_sent | show) | \(.pending_max | show) | \(.dropped_clicks | show) |"),
    "",
    "### 단계 단위 (유지 구간 2분 — 처리량·CPU는 평균, p95·p99·연결·in_use는 두 분 중 큰 값, 건수는 합)",
    "",
    "| 단계 rps | ALB rps (목표 대비 %) | k6 rps | dropped | p95 ms | p99 ms | 5xx % (타깃+ELB) | ELB 5xx | ECS CPU 평균/최대 % | RDS CPU % | DB 연결 | 초과 크레딧 과금 | 풀 대기 건/ms | in_use 최대 | 연결 보유 ms | 클릭 실패 | SQS 최고 대기 s | 대기 코드 최대 | 버린 클릭 |",
    "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |",
    ($rows | group_by(.stage)[] | . as $g
     | ($g | map(.alb_req) | add) as $req
     | ($g | map(.tgt5xx + .elb5xx) | add) as $e5
     | "| \($g[0].stage) | \($g | map(.alb_rps) | avgn | r1) (\($g | map(.alb_rps) | avgn | div($g[0].stage) | mul(100) | r1 | show)) | \($g | map(.k6_rps) | avgn | r1) | \($g | map(.dropped) | add) | \($g | map(.p95) | maxn | r1 | show) | \($g | map(.p99) | maxn | r1 | show) | \($e5 | div($req) | mul(100) | r1 | show) | \($g | map(.elb5xx) | add) | \($g | map(.ecs_cpu) | avgn | r1 | show)/\($g | map(.ecs_cpu_max) | maxn | r1 | show) | \($g | map(.rds_cpu) | avgn | r1 | show) | \($g | map(.rds_conn) | maxn | show) | \($g | map(.rds_surplus) | sumn | r1 | show) | \($g | map(.wait_count) | sumn | show)/\($g | map(.wait_ms) | sumn | show) | \($g | map(.in_use_max) | maxn | show) | \($g | map(.hold_ms) | avgn | r1 | show) | \($g | map(.click_fail) | sumn | show) | \($g | map(.sqs_age) | maxn | show) | \($g | map(.pending_max) | maxn | show) | \($g | map(.dropped_clicks) | sumn | show) |"),
    "",
    "### 정합성 (plan 0013 판정 (b) — 실행 시작부터 수집 시각까지, click queue 통계 합)",
    "",
    (if $c.rec == null then "click queue 통계가 없다(0012처럼 동기 집계 실행이면 정상)."
     else "- recorded \($c.rec) − dropped \($c.drp) = **\($c.rec - $c.drp)**",
          "- applied \($c.app) + unknown \($c.unk) = **\($c.app + $c.unk)** (위와 같아야 한다. 중복 배치 \($c.dups), 반영 실패 \($c.aerr))",
          "- DB 합계(teardown) = **\(if $dbtotal == "" then "-" else $dbtotal end)** (applied와 같아야 한다)"
     end)
'

# ---- 파드·HPA (EKS) — 기록 파일이 있을 때만 덧붙인다 ----
if [[ -r $PODS_FILE || -r $HPA_FILE ]]; then
  jq -nr --argjson start "$START" --argjson stages "$STAGES_JSON" --argjson req "$POD_CPU_REQUEST_M" \
    --slurpfile pods "$TMP/pods.json" --slurpfile hpa "$TMP/hpa.json" --slurpfile pool "$TMP/pool.json" '
    def bints: .[0:10] + "T" + .[11:19] + "Z" | fromdateiso8601 | tostring;
    def rows($r): $r | map(map({key: .field, value: .value}) | from_entries);
    def byminute: map({key: (.["bin(1m)"] | bints), value: (map_values(tonumber? // .))}) | from_entries;
    def r1: if . == null then null else . * 10 | round / 10 end;
    def pct: if . == null then null else . / $req * 100 end;
    def show: if . == null then "-" else tostring end;
    def maxn: map(select(. != null)) | max;
    def avgn: map(select(. != null)) | if length == 0 then null else add / length end;
    (rows($pool[0].results) | byminute) as $p
    | $pods[0] as $k | $hpa[0] as $h
    | [ range(0; $stages | length) as $i | range(1; 3) as $j
        | ($start + ($i * 3 + $j) * 60 | tostring) as $t
        | { stage: $stages[$i], t: ($t | tonumber),
            log_pods: $p[$t].tasks, top_pods: $k[$t].pods,
            cpu_avg: ($k[$t].avg | pct), cpu_max: ($k[$t].max | pct),
            hpa_cur: $h[$t].cur, hpa_des: $h[$t].des, hpa_util: $h[$t].util } ] as $rows
    | "",
      "### 파드·HPA (EKS — record-pods.sh 30초 기록, 파드 CPU %는 요청 \($req)m 대비)",
      "",
      "| 단계 rps | 분(UTC) | 파드 수(로그 task_id) | 파드 수(top) | 파드 CPU 평균/최대 % | HPA 현재/목표 | HPA CPU % |",
      "| --- | --- | --- | --- | --- | --- | --- |",
      ($rows[] | "| \(.stage) | \(.t | todate | .[11:16]) | \(.log_pods | show) | \(.top_pods | show) | \(.cpu_avg | r1 | show)/\(.cpu_max | r1 | show) | \(.hpa_cur | show)/\(.hpa_des | show) | \(.hpa_util | show) |"),
      "",
      "| 단계 rps | 파드 수 최대(로그) | 파드 CPU 평균/최대 % | HPA 목표 최대 | HPA CPU % 최대 |",
      "| --- | --- | --- | --- | --- |",
      ($rows | group_by(.stage)[] | . as $g
       | "| \($g[0].stage) | \($g | map(.log_pods) | maxn | show) | \($g | map(.cpu_avg) | avgn | r1 | show)/\($g | map(.cpu_max) | maxn | r1 | show) | \($g | map(.hpa_des) | maxn | show) | \($g | map(.hpa_util) | maxn | show) |")
  '
fi

# ---- Prometheus(plan 0015) — 원본 응답이 있을 때만 덧붙인다 ----
if [[ -r $PROM_RAW ]]; then
  [[ -r $LINES_RAW ]] || echo '{"results":[]}' > "$LINES_RAW"
  jq -nr --argjson start "$START" --argjson stages "$STAGES_JSON" --arg dbtotal "${DB_TOTAL:-}" \
    --slurpfile prom "$PROM_RAW" --slurpfile lines "$LINES_RAW" '
    def num: if . == null then null elif type == "number" then . elif . == "NaN" or . == "+Inf" or . == "-Inf" then null else tonumber end;
    def byminute: map({key: ((.[0] | floor) - 60 | tostring), value: (.[1] | num)}) | from_entries; # 분 끝 평가 → 그 분
    def series($r): ($r.data.result[0].values // []) | byminute;
    def scalar($r): ($r.data.result[0].value[1] // null) | num;
    def bints: .[0:10] + "T" + .[11:19] + "Z" | fromdateiso8601 | tostring;
    def r1: if . == null then null else . * 10 | round / 10 end;
    def mul($k): if . == null then null else . * $k end;
    def div($d): if . == null or $d == null or $d == 0 then null else . / $d end;
    def show: if . == null then "-" else tostring end;
    def maxn: map(select(. != null)) | max;
    def minn: map(select(. != null)) | min;
    def avgn: map(select(. != null)) | if length == 0 then null else add / length end;
    def sumn: map(select(. != null)) | if length == 0 then null else add end;
    $prom[0] as $P
    | ([$P | keys[] | select(. != "alive" and . != "argo_health")] | map({key: ., value: series($P[.])}) | from_entries) as $s
    | ($P.alive.data.result // [] | map({pod: .metric.pod, v: (.values | byminute)})) as $alive
    | ($lines[0].results | map(map({key: .field, value: .value}) | from_entries)
       | map({t: (.["bin(1m)"] | bints), pod: .task_id, msg: .msg, n: (.n | tonumber)})) as $L
    | [ range(0; $stages | length) as $i | range(1; 3) as $j
        | ($start + ($i * 3 + $j) * 60 | tostring) as $t
        | { stage: $stages[$i], t: ($t | tonumber),
            rps: $s.rps[$t], p95: ($s.p95[$t] | mul(1000)), p99: ($s.p99[$t] | mul(1000)), slow: ($s.slow[$t] | mul(100)),
            e5xx: $s.e5xx[$t], rpc: $s.rpc[$t], pods: $s.pods[$t],
            cad_avg: ($s.cad_avg[$t] | mul(1000)), cad_max: ($s.cad_max[$t] | mul(1000)),
            res_avg: ($s.res_avg[$t] | mul(1000)), res_max: ($s.res_max[$t] | mul(1000)),
            proc_avg: ($s.proc_avg[$t] | mul(1000)), proc_max: ($s.proc_max[$t] | mul(1000)),
            throttle: ($s.throttle[$t] | mul(100)),
            # 로그 유실: 그 분 전체에 살아 있던 파드마다 두 종류의 줄 수.
            lines: [ $alive[] | select(.v[$t] == 1) | .pod as $pod
                     | { pod: $pod,
                         cq: ([$L[] | select(.t == $t and .pod == $pod and .msg == "click queue") | .n] | add // 0),
                         dp: ([$L[] | select(.t == $t and .pod == $pod and .msg == "db pool") | .n] | add // 0) } ] } ] as $rows
    | "",
      "### Prometheus — 분 단위 (plan 0015. 앱 지표는 리다이렉트·링크 조회 경로, CPU는 파드당 밀리코어 — 한도 250m)",
      "",
      "| 단계 rps | 분(UTC) | 앱 rps | 앱 p95/p99 ms | 100ms 초과 % | 앱 5xx | 연결당 요청 | 파드(Ready) | CPU cAdvisor 평균/최대 | CPU kubelet resource 평균/최대 | CPU 앱 process 평균/최대 | CFS 스로틀 % |",
      "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |",
      ($rows[] | "| \(.stage) | \(.t | todate | .[11:16]) | \(.rps | r1 | show) | \(.p95 | r1 | show)/\(.p99 | r1 | show) | \(.slow | r1 | show) | \(.e5xx | round? // null | show) | \(.rpc | r1 | show) | \(.pods | show) | \(.cad_avg | r1 | show)/\(.cad_max | r1 | show) | \(.res_avg | r1 | show)/\(.res_max | r1 | show) | \(.proc_avg | r1 | show)/\(.proc_max | r1 | show) | \(.throttle | r1 | show) |"),
      "",
      "| 단계 rps | 앱 rps | 앱 p95/p99 ms(큰 값) | 100ms 초과 % | 앱 5xx | 연결당 요청 | 파드 최대 | CPU 평균 cAdvisor/resource/process | process ÷ cAdvisor | CPU 최대(cAdvisor) | CFS 스로틀 % |",
      "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |",
      ($rows | group_by(.stage)[] | . as $g
       | "| \($g[0].stage) | \($g | map(.rps) | avgn | r1 | show) | \($g | map(.p95) | maxn | r1 | show)/\($g | map(.p99) | maxn | r1 | show) | \($g | map(.slow) | avgn | r1 | show) | \($g | map(.e5xx) | sumn | round? // null | show) | \($g | map(.rpc) | avgn | r1 | show) | \($g | map(.pods) | maxn | show) | \($g | map(.cad_avg) | avgn | r1 | show)/\($g | map(.res_avg) | avgn | r1 | show)/\($g | map(.proc_avg) | avgn | r1 | show) | \($g | map(.proc_avg) | avgn | div($g | map(.cad_avg) | avgn) | mul(100) | round? // null | show)% | \($g | map(.cad_max) | maxn | r1 | show) | \($g | map(.throttle) | avgn | r1 | show) |"),
      "",
      "GOMAXPROCS(실행 동안, go_sched_gomaxprocs_threads): \([$s.gmp_min[]] | minn | show) ~ \([$s.gmp_max[]] | maxn | show)",
      "",
      "### 정합성 — Prometheus 카운터 (plan 0015 판정 (b), 실행 시작부터 drain 끝(teardown `DB 클릭 합계` 줄 + 30초, 그 줄이 없으면 수집 시각)까지, 정수)",
      "",
      ( { rec: scalar($P.recorded), drp: scalar($P.dropped), app: scalar($P.applied), unk: scalar($P.unknown),
          dups: scalar($P.dups), aerr: scalar($P.aerr), resets: (scalar($P.resets) // 0), gone: (scalar($P.gone) // 0) } as $c
        | if $c.rec == null then "클릭 카운터가 없다(Prometheus가 앱을 긁지 못했다 — 대상 up을 본다)."
          else ($c.rec - $c.drp) as $left | ($c.app + $c.unk) as $right
            | (if $dbtotal == "" then null else ($dbtotal | tonumber) end) as $db
            | "- recorded \($c.rec) − dropped \($c.drp) = **\($left)**",
              "- applied \($c.app) + unknown \($c.unk) = **\($right)** (중복 배치 \($c.dups), 반영 실패 \($c.aerr))",
              "- DB 합계(teardown) = **\($db | show)**",
              "- 카운터 리셋 합 \($c.resets), 실행 중 사라진 파드 \($c.gone) → " + (if $c.resets == 0 and $c.gone == 0 then "**유효**" else "**무효**(둘 다 0이어야 한다 — 마지막 긁기 뒤 증가분이 빠진다)" end),
              "- 판정: " + (if $c.resets != 0 or $c.gone != 0 then "무효"
                            elif $left == $right and ($db == null or $db == $right) then "**일치**"
                            else "**불일치**" end)
          end ),
      "",
      "### 로그 유실 — 파드·분당 줄 수 (plan 0015 판정 (c). 그 분 전체에 살아 있던 파드만, 기대 6줄·5~7 허용·4 이하 유실)",
      "",
      "| 단계 rps | 분(UTC) | 살아 있던 파드 | click queue 최소/최대 | db pool 최소/최대 | 4줄 이하(파드 cq/dp) |",
      "| --- | --- | --- | --- | --- | --- |",
      ($rows[] | .lines as $l
       | "| \(.stage) | \(.t | todate | .[11:16]) | \($l | length) | \($l | map(.cq) | minn | show)/\($l | map(.cq) | maxn | show) | \($l | map(.dp) | minn | show)/\($l | map(.dp) | maxn | show) | \([$l[] | select(.cq <= 4 or .dp <= 4) | "\(.pod) \(.cq)/\(.dp)"] | if length == 0 then "-" else join(", ") end) |"),
      "",
      ( [$rows[].lines[]] as $all | [$all[] | select(.cq <= 4 or .dp <= 4)] as $bad
        | "유실 의심 칸(파드×분) **\($bad | length)** / 전체 \($all | length) — 0이어야 (c) 통과다. LOG_LEVEL=warn 실행(실험 E3)은 두 줄이 Info라 전부 의심으로 나오는 것이 정상이다." )
  '
fi

# ---- 로그 양·Fluent Bit·ArgoCD (plan 0016 판정 (b)·(d)) — 자료가 하나라도 있을 때만 덧붙인다 ----
# 표본화 판정: 줄 수 ≈ 늘 남는 요청 + (요청 − 늘 남는 요청) ÷ N. 요청·늘 남는 요청은 지표(increase라 정수가 아니다)로 센다.
if [[ -r $PROM_RAW ]]; then cp "$PROM_RAW" "$TMP/prom-in.json"; else echo '{}' > "$TMP/prom-in.json"; fi
jq -nr --argjson start "$START" --argjson stages "$STAGES_JSON" \
  --slurpfile cw "$TMP/cw.json" --slurpfile req "$TMP/req.json" --slurpfile prom "$TMP/prom-in.json" '
  def num: if . == null then null elif type == "number" then . elif . == "NaN" or . == "+Inf" or . == "-Inf" then null else tonumber end;
  def byminute: map({key: ((.[0] | floor) - 60 | tostring), value: (.[1] | num)}) | from_entries; # 분 끝 평가 → 그 분
  def series($r): ($r.data.result[0].values // []) | byminute;
  def bints: .[0:10] + "T" + .[11:19] + "Z" | fromdateiso8601 | tostring;
  def cwts:
    if type == "number" then floor | tostring
    else capture("^(?<dt>[0-9-]{10}T[0-9:]{8})(\\.[0-9]+)?(?<tz>Z|[+-][0-9]{2}:[0-9]{2})$") as $c
      | ($c.dt + "Z" | fromdateiso8601)
        - (if $c.tz == "Z" then 0
           else ($c.tz[1:3] | tonumber) * 3600 + ($c.tz[4:6] | tonumber) * 60
                | if $c.tz[0:1] == "-" then -. else . end end)
      | tostring
    end;
  def r1: if . == null then null else . * 10 | round / 10 end;
  def mul($k): if . == null then null else . * $k end;
  def div($d): if . == null or $d == null or $d == 0 then null else . / $d end;
  def show: if . == null then "-" else tostring end;
  def maxn: map(select(. != null)) | max;
  def avgn: map(select(. != null)) | if length == 0 then null else add / length end;
  ($cw[0].MetricDataResults | map(select(.Id == "log_bytes")) | first) as $lb
  | (if $lb == null then {} else [$lb.Timestamps, $lb.Values] | transpose | map({key: (.[0] | cwts), value: .[1]}) | from_entries end) as $bytes
  | ($req[0].results // [] | map(map({key: .field, value: .value}) | from_entries)
     | map({key: (.["bin(1m)"] | bints), value: (.n | tonumber)}) | from_entries) as $lines
  | $prom[0] as $P
  | (["fb_cpu", "fb_max", "req_total", "req_kept", "argo_alert"] | map({key: ., value: series($P[.] // {})}) | from_entries) as $s
  | ($P.argo_health.data.result // [] | map({lab: "\(.metric.name // "?"):\(.metric.health_status // "?")", v: (.values | byminute)})) as $H
  | if ($bytes | length) == 0 and ($lines | length) == 0 and ($s.fb_cpu | length) == 0 and ($H | length) == 0 then empty else
    [ range(0; $stages | length) as $i | range(1; 3) as $j
      | ($start + ($i * 3 + $j) * 60 | tostring) as $t
      | { stage: $stages[$i], t: ($t | tonumber),
          req: $s.req_total[$t], kept: $s.req_kept[$t], lines: $lines[$t],
          mb: ($bytes[$t] | div(1000000)),
          fb: ($s.fb_cpu[$t] | mul(1000)), fbmax: ($s.fb_max[$t] | mul(1000)),
          health: ([$H[] | select(.v[$t] == 1) | .lab] | join(" ")),
          alert: ($s.argo_alert[$t] // 0) } ] as $rows
    | ([range($start; $start + ($stages | length) * 180; 60) | tostring]) as $all
    | "",
      "### 로그 양·Fluent Bit·ArgoCD (plan 0016 판정 (b)·(d). 요청은 지표, 줄은 Logs Insights \"http request\", Fluent Bit는 노드 합/가장 바쁜 파드 밀리코어)",
      "",
      "| 단계 rps | 분(UTC) | 요청(지표) | 늘 남는 요청(4xx·5xx·100ms 초과) | http request 줄 | 로그 MB | Fluent Bit CPU 합/최대 m | ArgoCD health | ArgoCD 알림 시계열 |",
      "| --- | --- | --- | --- | --- | --- | --- | --- | --- |",
      ($rows[] | "| \(.stage) | \(.t | todate | .[11:16]) | \(.req | round? // null | show) | \(.kept | round? // null | show) | \(.lines | show) | \(.mb | r1 | show) | \(.fb | r1 | show)/\(.fbmax | r1 | show) | \(if .health == "" then "-" else .health end) | \(.alert) |"),
      "",
      "| 단계 rps | 요청/분(지표) | 늘 남는 요청/분 | 줄/분 | 줄 ÷ 요청 % | 로그 MB/분 | Fluent Bit CPU 합 m |",
      "| --- | --- | --- | --- | --- | --- | --- |",
      ($rows | group_by(.stage)[] | . as $g
       | "| \($g[0].stage) | \($g | map(.req) | avgn | round? // null | show) | \($g | map(.kept) | avgn | round? // null | show) | \($g | map(.lines) | avgn | round? // null | show) | \($g | map(.lines) | avgn | div($g | map(.req) | avgn) | mul(100) | r1 | show) | \($g | map(.mb) | avgn | r1 | show) | \($g | map(.fb) | avgn | r1 | show) |"),
      "",
      ( [$H[] | .lab as $l | {lab: $l, n: ([$all[] as $t | select(.v[$t] == 1)] | length)}] as $hm
        | [$all[] | select(($s.argo_alert[.] // 0) > 0)] as $am
        | "실행 전체(\($all | length)분): ArgoCD health 분 수 — \(if ($hm | length) == 0 then "자료 없음" else ($hm | map("\(.lab) \(.n)") | join(", ")) end). LinkpulseArgoCDAppUnhealthy(pending 포함)가 있던 분 **\($am | length)** — 0이어야 (b) 통과다." )
    end
'
