# 0006 무트래픽 canary — 설정·지표 좌표 fixture

`infra/prod/synthetic-canary.tf`가 만드는 것을 apply 전/후에 대조하기 위한 스냅샷이다. AWS 접촉 명령은 전부 사람이 실행한다([[ask-before-external-services]]). **canary 지표·알람은 us-east-1에 있으므로 조회는 전부 `--region us-east-1`.**

## 파일

- `healthcheck.json` — Route53 헬스체크 프로빙 대상(FQDN·path·port·프로토콜·주기·임계). apply 후 실제 헬스체크 설정과 대조.
- `alarm-canary-down.json` — `canary_down` 알람 스키마(네임스페이스·지표·차원 키·통계·임계·missing 처리).
- `metric-coords.json` — `HealthCheckStatus` 지표 좌표(네임스페이스·차원·유효 통계·발행 리전). `HealthCheckId`는 apply 후에야 확정되는 자리표시자.

## 핵심 근거 (ADR 0004 §1차 출처)

- `HealthCheckStatus`: `AWS/Route53`, 차원 `HealthCheckId`, **1=healthy·0=unhealthy**, 유효 통계에 **Minimum** 포함 → 알람 `statistic=Minimum`·`threshold<1`.
- HTTPS(string-match 없음)는 **2xx/3xx를 healthy**로 판정 → `/healthz`=200 정상, 별도 `search_string` 불요.
- 지표는 **us-east-1에만 발행**된다(콘솔에서도 N. Virginia로 바꿔야 보임) → 알람·SNS 토픽 us-east-1.
- 헬스체크 리소스는 **글로벌**(리전 미지정) → 기본 provider로 생성.

## 사람이 실행: apply 후 실측·대조 (하드 게이트)

```bash
# (1) us-east-1에 HealthCheckStatus 지표가 실제로 발행되는지 — 크로스리전 전제의 사후 확정
aws cloudwatch list-metrics --region us-east-1 --namespace AWS/Route53 \
  --metric-name HealthCheckStatus --output table

# (2) 헬스체크 ID·상태 (글로벌 API)
terraform output -raw canary_health_check_id   # 또는 알람 dimension / list-health-checks:
aws route53 list-health-checks \
  --query "HealthChecks[?HealthCheckConfig.FullyQualifiedDomainName=='lpulse.live'].Id" --output text
aws route53 get-health-check-status --health-check-id <위 ID>   # Healthy 인지

# (3) canary 알람이 us-east-1 토픽을 alarm_actions·ok_actions 둘 다 참조하는지
aws cloudwatch describe-alarms --alarm-names "$(terraform output -raw canary_alarm_name)" \
  --region us-east-1 --query 'MetricAlarms[0].{ns:Namespace,metric:MetricName,stat:Statistic,
  cmp:ComparisonOperator,thr:Threshold,miss:TreatMissingData,alarm:AlarmActions,ok:OKActions}' --output json
```

`(1)`이 비어 있으면(지표 미발행) 알람이 영구 `INSUFFICIENT_DATA`/오탐이 될 수 있으니 헬스체크 생성·전파를 먼저 확인한다. `(3)`의 `alarm`/`ok` ARN 리전이 `us-east-1`인지 육안 확인(ap-northeast-2를 가리키면 오배선).

## 최초 발행 오탐 레이스 (apply 직후 1회)

헬스체크와 알람을 동시에 만들면 지표 최초 발행 전 공백이 `breaching`으로 잡혀 **정상인데 down 카드가 1회** 올 수 있다. 첫 canary ALARM은 `(1)`/`(2)`로 진위 확인 전까지 실장애로 단정하지 않는다(runbook §13). 2단계 apply(헬스체크 먼저 → 발행 확인 → 알람)로 회피 가능.
