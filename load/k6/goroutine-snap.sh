#!/usr/bin/env bash
# plan 0016 — 한 단계의 유지 구간 동안 앱 파드 하나의 고루틴 스냅숏을 벽시계 :00·:30마다 +0.2초에 받는다(정지 중·대조).
#
# 사용법: load/k6/goroutine-snap.sh <출력 접두> <k6 시작 epoch> [단계 rps(기본 3200)]
#   <k6 시작 epoch>: capacity.js setup이 찍은 실제 epoch(START_OFFSET_S=30 실행이면 정각 + 30).
#   <접두>-gr-<경계 epoch>.txt: /debug/pprof/goroutine?debug=2 — 고루틴마다 상태와 스택. 실행 대기(runnable)는 debug=2에만 나온다.
#   <접두>-gr.tsv: 경계 epoch, 파드, 받기 시작·끝 epoch(소수), 바이트
# 요약은 load/k6/goroutines.py가 낸다.
#
# k6를 띄운 뒤 백그라운드로 띄운다. 유지 구간 20초 전에 가장 오래된 앱 파드로 port-forward를 열어 둔다(여는 지연이 정지를 놓치지 않게).
# HPA가 늘린 새 파드가 아니라 처음부터 있던 파드를 고른다 — 단계 중간에 사라지지 않는다.
# 정각 시작이면 :00 스냅숏이 정지 중, :30이 대조다. 오프셋 30 실행(r0)이면 :00은 벽시계 기준 정지, :30은 k6 기준 정지 자리다 —
# 정지가 어느 쪽에 있든 정지 중 스냅숏이 남는다.
# 읽기 전용 kubectl(get·port-forward)과 내부 포트(9090) GET만 쓴다.
set -euo pipefail

OUT=${1:?출력 접두가 필요하다(예: load/k6/results/raw/0016-r1)}
START=${2:?k6 시작 epoch가 필요하다}
RATE=${3:-3200}
NS=${NAMESPACE:-linkpulse}
PORT=${PF_PORT:-19092}

# capacity.js의 ALL_RATES와 같아야 한다.
RATES=(50 100 200 400 800 1600 3200 6400)
idx=-1
for k in "${!RATES[@]}"; do
  if [[ ${RATES[k]} == "$RATE" ]]; then idx=$k; fi
done
(( idx >= 0 )) || { echo "단계 rps가 표에 없다: $RATE" >&2; exit 1; }
HOLD0=$(( START + idx * 180 + 30 ))
HOLD1=$(( START + idx * 180 + 180 ))

now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
sleep_until() { perl -MTime::HiRes=time,sleep -e 'my $d = $ARGV[0] - time; sleep($d) if $d > 0' "$1"; }
hms() { perl -MPOSIX=strftime -e 'print strftime("%H:%M:%S", gmtime($ARGV[0]))' "$1"; }

echo "단계 $RATE rps 유지 구간 $(hms "$HOLD0")~$(hms "$HOLD1") UTC — :00·:30 + 0.2초마다 받는다"
sleep_until $(( HOLD0 - 20 ))

POD=$(kubectl -n "$NS" get pods -l app.kubernetes.io/name=linkpulse --field-selector=status.phase=Running \
  --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[0].metadata.name}')
[[ -n $POD ]] || { echo "Running 앱 파드가 없다" >&2; exit 1; }
kubectl -n "$NS" port-forward "pod/$POD" "$PORT:9090" >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "$PF_PID" 2>/dev/null || true' EXIT
for _ in $(seq 15); do
  if curl -fsS --max-time 2 "http://127.0.0.1:$PORT/debug/pprof/" >/dev/null 2>&1; then break; fi
  sleep 1
done
echo "파드 $POD port-forward 준비"

b=$(( (HOLD0 + 29) / 30 * 30 ))
while (( b < HOLD1 )); do
  sleep_until "$b.2"
  t0=$(now)
  if curl -fsS --max-time 5 "http://127.0.0.1:$PORT/debug/pprof/goroutine?debug=2" -o "$OUT-gr-$b.txt"; then
    t1=$(now)
    printf '%d\t%s\t%s\t%s\t%d\n' "$b" "$POD" "$t0" "$t1" "$(wc -c < "$OUT-gr-$b.txt")" >> "$OUT-gr.tsv"
    echo "$(hms "$b") +0.2초 → $OUT-gr-$b.txt (받기 $t0 ~ $t1)"
  else
    echo "$(hms "$b") 스냅숏 실패(그 시각만 빠진다)" >&2
  fi
  b=$(( b + 30 ))
done
