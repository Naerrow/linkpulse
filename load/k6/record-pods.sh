#!/usr/bin/env bash
# plan 0014 — 측정 동안 파드 CPU와 HPA 상태를 30초마다 기록한다(EKS).
#
# 사용법: load/k6/record-pods.sh <출력 접두>   (예: load/k6/results/raw/0014-spread)
#   <접두>-pods.tsv: epoch, 파드, CPU(밀리코어), 메모리(MiB)   — kubectl top pods
#   <접두>-hpa.tsv:  epoch, 현재 파드 수, 목표 파드 수, HPA CPU %  — kubectl get hpa
# k6를 돌리기 전에 백그라운드로 띄우고, 수집 전에 끈다(kill). collect-metrics.sh가 같은 접두의 파일을 읽는다.
#
# metrics-server 값은 CloudWatch에 남지 않는다. Container Insights 대신 이 기록을 쓰고, 0015에서 Prometheus로 정리한다.
# 읽기 전용 kubectl 호출만 쓴다. 한 번 실패해도 멈추지 않는다(그 시각만 빠진다).
set -uo pipefail

OUT=${1:?출력 접두가 필요하다(예: load/k6/results/raw/0014-spread)}
NS=${NAMESPACE:-linkpulse}
INTERVAL=${INTERVAL:-30}

while true; do
  now=$(date +%s)
  # "linkpulse-abc-xyz   123m   45Mi" — CPU가 코어 단위("1")로 나오면 밀리코어로 바꾼다.
  kubectl -n "$NS" top pods --no-headers 2>/dev/null | awk -v t="$now" 'NF >= 3 {
      cpu = $2; mem = $3
      if (cpu ~ /m$/) { sub(/m$/, "", cpu) } else { cpu = cpu * 1000 }
      sub(/Mi$/, "", mem)
      printf "%d\t%s\t%d\t%d\n", t, $1, cpu, mem }' >> "$OUT-pods.tsv"
  kubectl -n "$NS" get hpa linkpulse \
    -o jsonpath='{.status.currentReplicas}{"\t"}{.status.desiredReplicas}{"\t"}{.status.currentMetrics[0].resource.current.averageUtilization}{"\n"}' 2>/dev/null \
    | awk -v t="$now" -F'\t' 'NF >= 2 { printf "%d\t%d\t%d\t%d\n", t, $1, $2, $3 + 0 }' >> "$OUT-hpa.tsv"
  sleep "$INTERVAL"
done
