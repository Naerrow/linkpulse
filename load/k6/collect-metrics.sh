#!/usr/bin/env bash
# plan 0012 — capacity.js 한 번의 실행을 분 단위·단계 단위 표(Markdown)로 모은다.
#
# 사용법: load/k6/collect-metrics.sh <시작_epoch> <k6_csv(.gz 가능)> [MAX_RPS]
#   시작_epoch: capacity.js setup이 찍은 "시나리오 시작 … epoch=" 값(정각)
#   MAX_RPS:    k6 실행 때 준 값(기본 1600). 단계표를 capacity.js와 같은 규칙으로 자른다.
#
# 실행이 끝나고 3분쯤 뒤, destroy 전에 돌린다 — 지표·로그가 늦게 들어오고, destroy하면 로그 그룹이 사라진다.
# 읽기 전용 AWS 호출만 쓴다(describe-load-balancers, get-metric-data, logs start-query/get-query-results).
#
# 단계 i(0부터)는 [시작 + 3i분, 시작 + 3i+3분)이다. 첫 1분은 30초 램프가 섞여 빼고,
# 유지 구간의 온전한 2분(3i+1, 3i+2)만 단계 값으로 쓴다.
set -euo pipefail

START=${1:?시작 epoch가 필요하다}
CSV=${2:?k6 CSV 경로가 필요하다}
MAX_RPS=${3:-1600}
REGION=${AWS_REGION:-ap-northeast-2}
PREFIX=${NAME_PREFIX:-linkpulse-prod}

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
for r in 50 100 200 400 800 1600; do
  if (( r <= MAX_RPS )); then STAGES+=("$r"); fi
done
if (( ${#STAGES[@]} == 0 )); then
  echo "MAX_RPS=$MAX_RPS 이하 단계가 없다" >&2
  exit 1
fi
END=$(( START + ${#STAGES[@]} * 180 ))
STAGES_JSON=$(printf '%s\n' "${STAGES[@]}" | jq -sc .)

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# ---- CloudWatch 지표(1분 단위) ----
# ALB 지표 차원은 ARN 끝부분(app/<이름>/<id>)이다. 계정 ID가 든 ARN 전체는 찍지 않는다.
ALB_ARN=$(aws elbv2 describe-load-balancers --region "$REGION" --names "$PREFIX-alb" \
  --query 'LoadBalancers[0].LoadBalancerArn' --output text)
ALB_DIM=${ALB_ARN#*:loadbalancer/}

jq -n --arg alb "$ALB_DIM" --arg p "$PREFIX" '
  def m(id; ns; name; stat; dims):
    {Id: id, MetricStat: {Metric: {Namespace: ns, MetricName: name, Dimensions: dims}, Period: 60, Stat: stat}};
  [{Name: "LoadBalancer", Value: $alb}] as $alb
  | [{Name: "ClusterName", Value: "\($p)-cluster"}, {Name: "ServiceName", Value: "\($p)-app"}] as $ecs
  | [{Name: "DBInstanceIdentifier", Value: "\($p)-pg"}] as $rds
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
      m("rds_rlat"; "AWS/RDS"; "ReadLatency"; "Average"; $rds)
    ]' > "$TMP/queries.json"

iso() { jq -rn --argjson t "$1" '$t | todate'; }
aws cloudwatch get-metric-data --region "$REGION" \
  --metric-data-queries "file://$TMP/queries.json" \
  --start-time "$(iso "$START")" --end-time "$(iso "$END")" \
  --scan-by TimestampAscending --output json > "$TMP/cw.json"

# ---- Logs Insights(앱 로그) ----
insights() {
  local qid status
  qid=$(aws logs start-query --region "$REGION" --log-group-name "/ecs/$PREFIX-app" \
    --start-time "$START" --end-time "$END" --query-string "$1" --query queryId --output text)
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
insights 'filter msg = "db pool"
  | stats sum(wait_count) as wait_count, sum(wait_ms) as wait_ms, max(in_use) as in_use_max,
          avg(in_use) as in_use_avg, count_distinct(task_id) as tasks by bin(1m)' "$TMP/pool.json"
insights 'filter msg = "클릭 집계 실패" | stats count(*) as n by bin(1m)' "$TMP/fail.json"

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
jq -nr --argjson start "$START" --argjson stages "$STAGES_JSON" \
  --slurpfile cw "$TMP/cw.json" --slurpfile pool "$TMP/pool.json" \
  --slurpfile fail "$TMP/fail.json" --slurpfile k6 "$TMP/k6.json" '
  def cwts: .[0:19] + "Z" | fromdateiso8601 | tostring;                  # "2026-10-08T12:34:00+00:00"
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
          wait_count: $p[$t].wait_count, wait_ms: $p[$t].wait_ms,
          in_use_max: $p[$t].in_use_max, in_use_avg: $p[$t].in_use_avg,
          # 연결 보유 시간(ms) = 태스크당 평균 in_use ÷ 태스크당 처리량 (Little의 법칙)
          hold_ms: ($p[$t].in_use_avg | div(($req / 60) | div($p[$t].tasks)) | mul(1000)),
          click_fail: (if $p[$t] then ($f[$t].n // 0) else null end) };

  [ range(0; $stages | length) as $i | range(1; 3) as $j
    | minute($stages[$i]; ($start + ($i * 3 + $j) * 60 | tostring)) ] as $rows

  | "### 분 단위 (램프가 섞인 분 제외)",
    "",
    "| 단계 rps | 분(UTC) | k6 rps | dropped | ALB rps | p95 ms | p99 ms | 타깃 5xx | ELB 5xx | ECS CPU 평균/최대 % | ECS 메모리 % | RDS CPU % | DB 연결 | 초과 크레딧 과금 | RDS 쓰기/읽기 지연 ms | 풀 대기 건 | 풀 대기 ms | in_use 최대/평균 | 연결 보유 ms | 클릭 집계 실패 |",
    "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |",
    ($rows[] | "| \(.stage) | \(.t | todate | .[11:16]) | \(.k6_rps | r1) | \(.dropped) | \(.alb_rps | r1) | \(.p95 | r1 | show) | \(.p99 | r1 | show) | \(.tgt5xx) | \(.elb5xx) | \(.ecs_cpu | r1 | show)/\(.ecs_cpu_max | r1 | show) | \(.ecs_mem | r1 | show) | \(.rds_cpu | r1 | show) | \(.rds_conn | show) | \(.rds_surplus | r1 | show) | \(.wlat | r1 | show)/\(.rlat | r1 | show) | \(.wait_count | show) | \(.wait_ms | show) | \(.in_use_max | show)/\(.in_use_avg | r1 | show) | \(.hold_ms | r1 | show) | \(.click_fail | show) |"),
    "",
    "### 단계 단위 (유지 구간 2분 — 처리량·CPU는 평균, p95·p99·연결·in_use는 두 분 중 큰 값, 건수는 합)",
    "",
    "| 단계 rps | ALB rps (목표 대비 %) | k6 rps | dropped | p95 ms | p99 ms | 5xx % (타깃+ELB) | ELB 5xx | ECS CPU 평균/최대 % | RDS CPU % | DB 연결 | 초과 크레딧 과금 | 풀 대기 건/ms | in_use 최대 | 연결 보유 ms | 클릭 집계 실패 |",
    "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |",
    ($rows | group_by(.stage)[] | . as $g
     | ($g | map(.alb_req) | add) as $req
     | ($g | map(.tgt5xx + .elb5xx) | add) as $e5
     | "| \($g[0].stage) | \($g | map(.alb_rps) | avgn | r1) (\($g | map(.alb_rps) | avgn | div($g[0].stage) | mul(100) | r1 | show)) | \($g | map(.k6_rps) | avgn | r1) | \($g | map(.dropped) | add) | \($g | map(.p95) | maxn | r1 | show) | \($g | map(.p99) | maxn | r1 | show) | \($e5 | div($req) | mul(100) | r1 | show) | \($g | map(.elb5xx) | add) | \($g | map(.ecs_cpu) | avgn | r1 | show)/\($g | map(.ecs_cpu_max) | maxn | r1 | show) | \($g | map(.rds_cpu) | avgn | r1 | show) | \($g | map(.rds_conn) | maxn | show) | \($g | map(.rds_surplus) | sumn | r1 | show) | \($g | map(.wait_count) | sumn | show)/\($g | map(.wait_ms) | sumn | show) | \($g | map(.in_use_max) | maxn | show) | \($g | map(.hold_ms) | avgn | r1 | show) | \($g | map(.click_fail) | sumn | show) |")
'
