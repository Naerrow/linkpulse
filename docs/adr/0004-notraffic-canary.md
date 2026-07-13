# ADR 0004 — 무트래픽 방어선 canary(Route53 헬스체크 + us-east-1 HealthCheckStatus 알람)

- 상태: 채택 (2026-07-10). A-5 근본원인 절은 **2026-07-13 무트래픽 canary 드릴 실측으로 확정**(아래 §A-5 규명 — [`plan.md`](../plans/0006-p4-notraffic-canary/plan.md) step 7~8 완료).
- 관련: AGENTS.md 가드레일 #1(인프라 변경은 사람 승인), Phase P4(하드닝), 회고 [`2026-07-06-gameday-01-retro.md`](../postmortems/2026-07-06-gameday-01-retro.md) A-5·A-10, 통지 경로 재사용은 [ADR 0002](0002-alerting-design.md), 배포 실패 통지는 [ADR 0003](0003-deploy-failure-alerts.md).

## 맥락

GameDay(P3-2) 실측에서 드러난 **무트래픽 사각지대**(회고 A-5·A-10): 전면 다운(S2)을 가장 빠르고 일관되게 잡은 알람은 `alb-elb-5xx`(트래픽 기반, MTTD 3~5분 2회 재현)였다. 그러나 이 알람은 **요청이 있어야 5xx가 생긴다** — 진짜 무트래픽 다운(새벽 등 실사용 0)이면 5xx 자체가 없어 침묵한다. 그 상황의 설계상 방어선인 `alb-no-healthy-hosts`(`HealthyHostCount<1`, `treat_missing=breaching`)는 S2 2회 모두 MTTD 신호로 실패했다(1차 9분48초·복구 후 발화, 2차 미발화). 그리고 실측 MTTD 3~5분이 나온 것도 C-1 폴링이라는 synthetic 트래픽이 있었기 때문이다.

즉 **무트래픽에서 다운을 결정론적으로 감지하는 방어선이 미검증 상태로 남았다.** 이 ADR은 상시 외부 canary로 (1) 무트래픽 시간대에도 서비스에 요청 흐름을 만들고, (2) 트래픽·5xx 카운트와 무관한 직접 liveness 신호를 확보해 그 사각지대를 메운다.

## 결정

### 1. 채택안 — Route53 헬스체크 + 전용 `HealthCheckStatus` 알람 (선언형, 코드·Lambda 없음)

`aws_route53_health_check`가 AWS 글로벌 헬스체커 다수 지점에서 `lpulse.live/healthz`를 HTTPS로 상시(30초 주기) 프로빙한다. 한 메커니즘이 두 문제를 동시에 푼다:

- **(a) 외부 트래픽 생성** — 헬스체커가 실제로 ALB를 두드리므로, 무트래픽 다운이어도 ALB가 503을 뱉고 기존 `alb-elb-5xx`가 무트래픽에서도 발화한다.
- **(b) 결정론적 liveness 신호** — 헬스체크 자신의 `HealthCheckStatus` 지표(**1=정상 / 0=다운**)를 발행한다. 트래픽·5xx 카운트와 무관해, 전용 알람 `canary_down`이 이를 직접 감시한다.

**코드·시크릿 없는 선언형**을 택한 이유는 ADR 0002가 Chatbot을 택한 것과 같다 — 최소 운영부담, 사람이 이해·운영. 비용도 최저(아래 §비용).

**1차 출처(핵심 사실 — 이 설계가 성립하는 근거):**

- `HealthCheckStatus`는 `AWS/Route53` 네임스페이스, 차원 `HealthCheckId`, **1=healthy·0=unhealthy**이고 유효 통계에 **Minimum**이 포함된다(그래서 알람은 `statistic=Minimum`, `threshold<1`): [Monitoring your resources with Route 53 health checks and CloudWatch](https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/monitoring-cloudwatch.html) — _"HealthCheckStatus ... 1 indicates healthy, and 0 indicates unhealthy. Valid statistics: Minimum, Average, and Maximum."_
- HTTPS 헬스체크(string-match 없음)는 **2xx 또는 3xx 응답을 healthy로 판정**한다(`/healthz`=200이라 정상): [How Route 53 determines whether a health check is healthy](https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/dns-failover-determining-health-of-endpoints.html) — _"the endpoint must respond with an HTTP status code of 2xx or 3xx within two seconds after connecting."_ 프로빙 주기는 10초/30초 중 선택, `failure_threshold`는 연속 실패 횟수.
- 헬스체크 리소스는 **글로벌**이라 리전을 지정하지 않는다(기존 `route53_acm.tf`처럼 기본 provider로 생성): [How health checks work in complex configurations](https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/dns-failover-complex-configs.html) — _"Route 53 is a global service, so you don't specify the region that you want to create health checks in."_

### 2. 크로스리전 배선 (채택안의 유일한 함정)

Route53 헬스체크의 CloudWatch 지표는 **us-east-1에만 발행된다.** 콘솔에서조차 리전을 N. Virginia로 바꿔야 지표가 보인다: [Monitoring health checks using CloudWatch](https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/monitoring-health-checks.html) — _"Change the current region to US East (N. Virginia). Route 53 metrics are not available if you select any other region as the current region."_

CloudWatch 알람은 **자기 지표가 있는 리전에서** 만들어야 한다(알람은 같은 리전의 지표만 평가). 지표가 us-east-1에만 있으니 `canary_down` 알람도 us-east-1(`provider = aws.use1`)이어야 하고, **알람 액션 SNS 토픽 역시 알람과 같은 리전**이어야 한다. 그래서:

- `canary_down` 알람 = `provider = aws.use1`.
- us-east-1 **전용 SNS 토픽 + 토픽 정책**(`monitoring.tf`의 `alarms` 미러 — `cloudwatch.amazonaws.com` publish만 허용 + confused-deputy 조건, 단 `SourceArn` 리전 리터럴은 `us-east-1`)을 `provider = aws.use1`로 만든다.
- 기존 Chatbot config(`aws_chatbot_slack_channel_configuration.alarms`, us-east-2)의 `sns_topic_arns`에 이 us-east-1 토픽을 **추가** 바인딩한다. 이 config는 이미 ap-northeast-2 토픽을 크로스리전으로 바인딩 중이므로(ADR 0002) 검증된 패턴이다. 두 토픽이 같은 Slack 채널로 팬아웃한다.

> **1차 출처 규율**: 위 "us-east-1 발행"·"지표 존재"는 문서 근거에 더해 **2026-07-13 드릴에서 실측 확정** — `canary_down` 알람이 us-east-1에서 실제 발화(13:38:47 KST)한 것 자체가 `HealthCheckStatus`의 us-east-1 발행·평가 증거다. 미실측 단정이 A-5를 만든 재발 방지 원칙([[infra-plan-review-and-first-source]]).

### 3. 다운 중에도 DNS가 계속 resolve된다 (canary 트래픽 생성 전제)

apex `lpulse.live`는 ALB alias(A) 레코드이고 `evaluate_target_health = true`다(`route53_acm.tf`). 전면 다운으로 ALB 뒤 정상 타깃이 0이 돼도, Route53은 **"전 레코드가 unhealthy면 전부 healthy로 간주하고 응답한다"**: [How Route 53 chooses records when health checking is configured](https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/health-checks-how-route-53-chooses-records.html) — _"If no record is healthy, all records are healthy ... Route 53 considers all the records in the group to be healthy and selects one based on the routing policy."_ 즉 다운 중에도 `lpulse.live`가 계속 ALB로 resolve돼, canary가 요청을 만들고 ALB가 503을 반환하는 전제가 유지된다.

### 4. 대안 (문서화만, 채택 안 함)

- **Alt-1 (더 가벼움): 헬스체크를 트래픽 생성기로만 쓰고 별도 알람 없이 기존 `alb-elb-5xx`에 의존.** 리소스 1개(헬스체크)뿐, 크로스리전 불필요. **탈락**: 무트래픽 down의 직접 신호가 없고 감지가 5xx 카운트(≥5/5분) 임계에 결합된다. 크로스리전 배선이 끝내 불가하면 이걸로 축소한다(plan step 7 fallback).
- **Alt-2: CloudWatch Synthetics canary.** 같은 리전이라 크로스리전은 불필요하나 **Lambda+S3 아티팩트 버킷+IAM+canary 스크립트 코드**가 붙고 실행당 과금이 크다(1분 주기 ~$52/월, 5분 ~$10/월). ADR 0002의 "코드·시크릿 없는 선언형" 결정과 어긋나 탈락.

## A-5 규명 — 근본원인 (2026-07-13 무트래픽 canary 드릴 실측으로 확정)

`alb-no-healthy-hosts`가 무트래픽 전면 다운을 MTTD 신호로 잡지 못한 근본원인을, 무트래픽 canary 드릴(desired=0, T0 = 2026-07-13 13:33 KST)의 CloudWatch 실측으로 확정한다. **근본원인 = 이 알람은 잡아야 할 장애가 나면 감시 지표 자체가 사라지는 구조적 결함이다.**

1. **`HealthyHostCount`는 다운 시 "0"이 아니라 통째로 사라진다(missing).** desired=0으로 태스크가 전부 종료·deregister되면 타깃 그룹 등록 타깃이 0이 되고 ALB가 이 지표 발행을 멈춘다. AWS 문서가 보고 기준을 명시한다: [CloudWatch metrics for your Application Load Balancer](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/load-balancer-cloudwatch-metrics.html) — HealthyHostCount _"Reporting criteria: Reported if there are registered targets."_ / 페이지 상단 _"If there are no requests flowing through the load balancer or no data for a metric, the metric is not reported."_ (UnHealthyHostCount도 _"When you deregister a target, this decreases HealthyHostCount but does not increase UnhealthyHostCount"_ — deregister는 unhealthy 신호조차 남기지 않는다.)
   - **실측:** `get-metric-statistics --metric-name HealthyHostCount --period 60` → 13:20~13:32 값 **2**(매분 발행) → **13:33~13:39 데이터포인트 완전 부재(7분 공백)** → 13:40~ 값 **2** 복귀. desired=0 구간 지표가 **0이 아니라 missing**임을 확정(가설의 missing 분기 성립, "0 발행" 분기 기각).
2. **`treat_missing_data=breaching`은 이 부재를 제때 보상하지 못한다.** missing을 breaching으로 취급하도록 설계됐지만(구성 `Average / 60s / eval 3 / <1 / breaching`), 지표가 **완전히 부재**할 때의 발화는 실측상 신뢰할 수 없다 — 같은 desired=0 다운을 **3회**(GameDay S2 2회 + 이번 1회) 겪는 동안 이 알람은 9분48초(복구 후)·미발화·미발화로 **한 번도 MTTD를 담당하지 못했다**(이번 드릴 상태이력 전 구간 공백 = 미발화 실측).
3. **대조 — 장애 중에도 살아있는 지표만 신뢰 가능하다.** 같은 드릴에서 실제로 발화한 두 알람은 발행이 끊기지 않는 지표를 본다: `alb-elb-5xx`(`HTTPCode_ELB_5XX_Count` — canary의 503이 ELB 5xx로 카운트) T0→ALARM **약 3분44초**(13:36:43) — 무트래픽인데도 발화한 것은 canary 요청(외부 클라이언트)이 ALB를 두드려 503을 만든 실증. `canary_down`(`HealthCheckStatus` — Route53 외부 발행) T0→ALARM **약 5분47초**(13:38:47), 복구 시 ALARM→OK 해제도 6분 만에 결정론적(13:44:47 — `alb-elb-5xx`의 OK 복귀가 14분+ 지연된 것과 대조, 회고 A-9 재확인).

**결론:** `alb-no-healthy-hosts`의 지연/미발화는 튜닝으로 고칠 문제가 아니라 **구조적**이다 — 감시 지표가 대상 장애에서 소멸한다. 따라서 주기·임계 튜닝은 무효이고(아래 판정 §1), 무트래픽 방어선은 장애 중에도 살아있는 신호(canary 트래픽발 5xx, Route53 liveness)로 세운다(아래 판정 §2). 이 실측이 A-5를 "미실측 단정"에서 "1차 출처 + 실측 확정"으로 종결한다([[infra-plan-review-and-first-source]]).

## A-5 판정 = 유지(느린 백스톱) + 비신뢰 + canary가 1차 방어선

이 판정은 근본원인 실측과 무관하게 확정한다:

1. **`alb-no-healthy-hosts`는 유지하되 MTTD 신호로 신뢰하지 않는다.** canary·외부 신호까지 죽은 최후의 경우를 위한 **느린 백스톱**으로만 남긴다. (발행 공백이 지연 원인이면 주기 튜닝은 효과가 없으므로) **주기 튜닝으로 해결하지 않는다** → **`monitoring.tf`의 `alb_no_healthy_hosts`는 건드리지 않는다**(surgical).
2. **무트래픽 1차 방어선은 canary가 맡는다.** MTTD 역할 분리(2026-07-13 무트래픽 드릴 실측):
   - `alb-elb-5xx`(canary 트래픽 기반) = **빠른 1차** — 실측 약 3분44초.
   - `canary_down`(`HealthCheckStatus`) = **결정론적 백스톱** — 실측 약 5분47초(설계 추정 ≈flip 90s + 알람 3분과 정합).
   - 둘 다 `alb-no-healthy-hosts`(9분48초/미발화, 3회 드릴)보다 빠르고, 무트래픽에서도 동작함이 실증됐다.

이로써 A-8(runbook "S2 1순위=`alb-elb-5xx`")이 임시 운영 우회가 아니라 **canary로 뒷받침되는 항구 대책**이 된다.

## 결과 / 트레이드오프 (운영자가 알아야 할 함정)

- **최초 apply 오탐 카드 레이스.** 헬스체크와 `canary_down`을 동시에 만들면, `HealthCheckStatus`가 us-east-1에 **최초 발행되기 전 공백**이 `breaching`으로 취급돼 **서비스는 정상인데 down 카드가 1회** 뜰 수 있다(기존 12 breaching 알람은 대상 지표가 이미 존재해 이 레이스가 없었다). 발행(~1–2분)이 알람 창(3분)보다 빠를 공산이 크나 레이스는 실재한다. **완화**: (a) 헬스체크 먼저 apply→`list-metrics`로 발행·Healthy 확인→알람 2단계 apply, 또는 (b) 단일 apply 시 "**첫 canary ALARM은 preflight/`list-metrics`로 진위 확인 전까지 장애로 단정하지 않는다**"(runbook에 명시).
- **canary 자기발 트래픽.** 상시 프로빙으로 `alb-target-5xx`·`alb-latency-p95`에 항상 샘플이 생긴다 → 정상 200/저지연이면 무해. `/healthz`는 레이트리밋 예외(`ratelimit.go`)라 429 오탐 없음.
- **크로스리전 오배선.** us-east-1 알람이 ap-northeast-2 토픽을 가리키면 카드가 안 온다 → 알람은 반드시 `canary_use1`(us-east-1) 토픽에 붙인다. plan에서 알람 리전·토픽 ARN 리전 일치 육안 확인, apply 전 비파괴 preflight(`set-alarm-state --region us-east-1`)로 실배선 검증.
- **AWS 이름/description은 ASCII만.** SNS 토픽·알람 이름·description에 한글을 넣으면 apply가 깨진다(validate·plan은 통과 — [[aws-descriptions-ascii-only]]).
- **비용.** 헬스체크 HTTPS ≈ $0.50(AWS 엔드포인트)+$1.00(HTTPS) ≈ **$1.50/월**, 신규 알람 ~$0.10/월, us-east-1 SNS publish 무시가능. string-matching·fast-interval(10s)은 각 +$1이라 미사용(30초 표준 주기). 총 증분 ≈ **$1.6/월**. 사람이 과금 인지 후 apply.
- **롤백 = HCL revert 후 정상 apply.** `synthetic-canary.tf`·`providers.tf`의 `use1` alias·`chatbot.tf` 1줄·`outputs.tf` 2블록을 되돌리고 `plan`→사람 apply. 전부 additive라 기존 12알람·SNS·Chatbot·ECS/ALB/RDS에 무영향.
