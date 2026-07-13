---
status: approved # in-review | approved
revision: 3
created: 2026-07-10
---

# 0006. P4(c) 무트래픽 방어선 canary(A-10) + A-5 규명

## 목표

이 plan이 끝나면:

1. **상시 synthetic canary**가 `https://lpulse.live/healthz`를 외부에서 주기적으로 두드려, 실사용 트래픽이 0인 시간대(새벽 등)에도 서비스에 요청 흐름이 존재한다.
2. 그 canary가 **다운을 수분 내에 Slack으로 통지**한다 — 무트래픽 다운의 유일 방어선이던 `alb-no-healthy-hosts`가 GameDay 2회에서 지연/미발화(9분48초/복구후·무발화)한 사각지대를, 결정론적인 외부 liveness 신호로 메운다.
3. **A-5 종결**: `alb-no-healthy-hosts` 지연·미발화의 원인을 문서로 규명하고, "튜닝/교체/유지" 판정을 내려 백로그에서 뺀다. (판정 요지 = 이 알람은 **느린 백스톱으로 유지**하되 MTTD 신호로 신뢰하지 않고, canary가 무트래픽 1차 방어선을 맡는다.)

성공 판정 = 무트래픽(수동 curl 없이 canary만 돌아가는 상태) 상태에서 desired=0 다운을 유발했을 때, **canary 기반 알람이 수분 내 Slack에 다운 카드**를 띄운다(P3-2 chaos S2 재사용으로 실증).

## 배경/제약

### 왜 이 설계인가 (메커니즘 결정 — 리뷰어 핵심 검토 대상)

무트래픽 사각지대의 근본 문제(회고 `docs/postmortems/2026-07-06-gameday-01-retro.md` A-5·A-10): 실측상 다운을 가장 빠르고 일관되게 잡은 건 `alb-elb-5xx`(트래픽 기반, MTTD 3~5분 2회 재현)인데, 이건 **요청이 있어야 5xx가 생긴다**. 진짜 무트래픽 다운이면 5xx 자체가 없어 침묵한다. 그 상황의 설계상 방어선인 `alb-no-healthy-hosts`(HealthyHostCount<1, breaching)는 2회 모두 MTTD 신호로 실패했다.

**채택안(권장): Route53 헬스체크 + 전용 HealthCheckStatus 알람.**

- `aws_route53_health_check`가 AWS 글로벌 헬스체커 다수 지점에서 `lpulse.live/healthz`를 HTTPS로 상시 프로빙한다 → (a) **외부에서 실제 트래픽을 생성**하므로 무트래픽 다운도 ALB가 503을 뱉고 기존 `alb-elb-5xx`가 발화하며, (b) 헬스체크 자신의 `HealthCheckStatus` 지표(1=정상/0=다운)를 결정론적으로 발행해 **트래픽·5xx 카운트와 무관한 직접 liveness 신호**를 준다. 한 메커니즘이 A-10(canary)과 A-5(신뢰 가능한 무트래픽 down 신호)를 동시에 해결한다.
- **코드·Lambda 없음(선언형)** — ADR 0002가 Chatbot을 택한 것과 같은 이유(최소 운영부담, 사람이 이해·운영). 비용도 최저(아래).

**대안(문서화만, 채택 안 함):**

- **Alt-1 (더 가벼움): Route53 헬스체크를 트래픽 생성기로만 쓰고 별도 알람 없이 기존 `alb-elb-5xx`에 의존.** 리소스 1개(헬스체크)뿐, 크로스리전 불필요. 회고 A-10 문구와 정확히 일치. **탈락 사유**: 무트래픽 down의 직접 신호가 없고 감지가 5xx 카운트(≥5/5분) 임계에 결합된다. 다만 크로스리전 비용이 과하다고 판단되면 이걸로 축소 가능(리뷰어 판단 요청).
- **Alt-2: CloudWatch Synthetics canary.** 같은 리전(ap-northeast-2)이라 크로스리전 불필요하나 **Lambda+S3 아티팩트 버킷+IAM+canary 스크립트 코드**가 붙고 실행당 과금이 커진다(1분 주기 ~$52/월, 5분 주기 ~$10/월). ADR 0002의 "코드·시크릿 없는 선언형" 결정과 어긋나 탈락.

### 크로스리전 제약 (채택안의 유일한 함정 — 반드시 반영)

Route53 헬스체크의 CloudWatch 지표(`HealthCheckStatus`)는 **us-east-1에만 발행된다**(글로벌 서비스 규약). CloudWatch 알람은 **자기 리전의 SNS 토픽에만** publish할 수 있으므로:

- 알람을 `provider = aws.use1`(us-east-1 alias)로 만든다.
- us-east-1에 **전용 SNS 토픽 + 토픽 정책**(기존 `aws_sns_topic_policy.alarms`와 동일하게 `cloudwatch.amazonaws.com` publish만 허용 + confused-deputy 조건, 단 SourceArn 리전 리터럴은 `us-east-1`)을 만든다.
- 기존 Chatbot config(`aws_chatbot_slack_channel_configuration.alarms`, us-east-2)의 `sns_topic_arns`에 이 us-east-1 토픽을 **추가**한다. 이 config는 이미 ap-northeast-2 토픽을 크로스리전으로 바인딩 중이므로(ADR 0002) 검증된 패턴이다.

`request_interval`/`region`/지표 존재는 단정 금지 — 사람이 `aws cloudwatch list-metrics --region us-east-1 --namespace AWS/Route53`로 **1차 출처 실측**한다([[infra-plan-review-and-first-source]]).

### 확정 사실 (레포·AWS 실측)

- 도메인 `lpulse.live`(apex → ALB alias, `route53_acm.tf`). ALB HTTPS 리스너 443, 타깃그룹 `/healthz` 헬스체크(`alb.tf`).
- ALB SG는 `443`을 `0.0.0.0/0`에 개방(`security_groups.tf:34-39`) → Route53 글로벌 헬스체커가 도달 가능. **SG 변경 불요.**
- `/healthz`는 liveness(200), 레이트리밋 예외(`ratelimit.go:151`) → canary 프로빙이 429로 막히지 않는다.
- SNS `aws_sns_topic.alarms`(ap-northeast-2) + Chatbot(us-east-2, `sns_topic_arns=[alarms.arn]`) → Slack, 기존 경로 재사용.
- providers.tf에 us-east-1 alias **없음** → 신규 필요. versions.tf AWS provider `~> 6.0`.
- P3-2 chaos 자산 존재(`load/chaos` 이미지 `chaos-healthz-v1`, S2=desired 0 다운 절차, C-1 폴링) → 종단검증 재사용.

### 가드레일 (AGENTS.md #1 — 위반 금지)

- 에이전트는 **`.tf` 작성 + 로컬 `terraform fmt`/`validate` + 문서(ADR·README·fixtures)까지만.** `terraform plan`·`terraform apply`·모든 `aws` CLI·GameDay 드릴은 **전부 사람**([[ask-before-external-services]]).
- 인프라 변경은 plan 출력을 사람에게 보이고 멈춘다. 커밋·PR은 사람([[never-auto-commit]]).
- **비용 고지**: Route53 헬스체크 HTTPS ≈ $0.50(AWS 엔드포인트)+$1.00(HTTPS) ≈ **$1.50/월**, 신규 알람 ~$0.10/월, us-east-1 SNS publish 무시가능. string-matching·fast-interval(10s)은 각 +$1이라 **미사용**(30초 표준 주기; string match 없이 Route53는 **2xx/3xx 응답을 healthy로 판정** — `/healthz`는 200이라 정상. 근거 URL은 ADR에 인용). 총 증분 ≈ **$1.6/월**. 사람이 과금 인지 후 apply.

## 실행 단계

> 1~6 = 에이전트(코드·문서 초안 + 로컬 검증). 7 = 사람(plan/apply/드릴). 8 = 사람 실측 제공 후 문서 확정(A-5/A-10 종결). 같은 파일 동시 편집 없음.

1. **us-east-1 provider alias 추가** — `providers.tf`에 `provider "aws" { alias = "use1"; region = "us-east-1"; default_tags {...} }`(기존 default_tags 동일 복제).
   → 검증: `terraform validate` Success. `terraform providers` 목록에 `aws.use1` 노출.

2. **us-east-1 SNS 토픽 + 토픽 정책** — 신규 `infra/prod/synthetic-canary.tf`에 `aws_sns_topic.canary_use1`(**provider=aws.use1**) + `aws_sns_topic_policy.canary_use1`(**동일하게 provider=aws.use1** — 기본 provider로 만들면 ap-northeast-2 API로 us-east-1 토픽 ARN에 정책을 붙이려다 apply 실패하고, `validate`는 이 provider·리전·ARN 불일치를 못 잡는다). 정책은 기존 `alarms` 미러, `aws:SourceArn = arn:aws:cloudwatch:us-east-1:<acct>:alarm:*`. name/description는 ASCII만([[aws-descriptions-ascii-only]]).
   → 검증: `validate` Success. 정책 JSON에 confused-deputy 조건(SourceArn·SourceAccount) 존재, **두 리소스 모두 `provider = aws.use1`**, 리전 리터럴이 `us-east-1`인지 육안 확인.

3. **Route53 헬스체크** — `synthetic-canary.tf`에 `aws_route53_health_check.canary`: `type=HTTPS`, `fqdn=var.domain_name`, `port=443`, `resource_path="/healthz"`, `enable_sni=true`, `request_interval=30`, `failure_threshold=3`. `measure_latency`/string-match 미사용(비용) — string match 없이 Route53는 **2xx/3xx를 healthy로 본다**(`/healthz`=200 정상). 결정 이유 1~3줄 주석(AGENTS.md #4).
   → 검증: `validate` Success. fixtures에 프로빙 대상(FQDN·path·port) 스냅샷 기록.

4. **HealthCheckStatus 알람 + Chatbot 배선 + outputs** — `synthetic-canary.tf`에 `aws_cloudwatch_metric_alarm.canary_down`(provider=aws.use1): `namespace=AWS/Route53`, `metric_name=HealthCheckStatus`, `dimensions={HealthCheckId=aws_route53_health_check.canary.id}`, `statistic=Minimum`, `period=60`, `evaluation_periods=3`, `threshold=1`, `comparison_operator=LessThanThreshold`, `treat_missing_data=breaching`, **`alarm_actions`·`ok_actions` 둘 다 `[aws_sns_topic.canary_use1.arn]`**(기존 12알람 관례 = 상태 전이 통지, step 7의 복구 카드 관찰에 필요 — 3인 공통 지적). 그리고 `chatbot.tf`의 `sns_topic_arns`에 `aws_sns_topic.canary_use1.arn` **추가**(count 가드는 기존 `slack_enabled` 유지). `outputs.tf`에 **canary 토픽 ARN·알람 이름 출력 추가**(preflight/`set-alarm-state` 검증 명령이 안정 참조 — codex-ide #1).
   → 검증: `validate` Success. 알람이 aws.use1 provider·us-east-1 토픽을 `alarm_actions`+`ok_actions` **둘 다** 참조하는지, Chatbot이 두 토픽(ap-northeast-2+us-east-1)을 다 바인딩하는지 육안 확인.

5. **A-5 규명 문서 *초안* + 판정 (에이전트는 초안까지 — 근본원인 확정·종결 플립은 step 8, 실측 반영 후)** — `docs/adr/0004-notraffic-canary.md` 신규(맥락·결정·트레이드오프 + **1차 출처 URL 인용**: Route53 `HealthCheckStatus`의 us-east-1 발행 / CloudWatch 알람의 동일리전 SNS 제약 / Chatbot 크로스리전 바인딩 / **Route53 alias `evaluate_target_health=true`인데 "그룹 내 전 레코드 unhealthy면 Route53이 그대로 응답을 반환"** — 다운 중에도 `lpulse.live`가 계속 resolve돼 canary 트래픽 생성 전제가 유지됨을 근거로 인용, codex-ide 개선. ADR 0003처럼 URL을 본문에 건다).
   - (i) **A-5 근본원인 — 초안은 자리표시자, 단정 금지**: 초안 ADR의 근본원인 절은 **플레이스홀더 + 증거수집 체크리스트**(desired=0 구간 `HealthyHostCount`가 missing이었는지 0이었는지 `get-metric-statistics`로 확인)로 둔다. **확정 서술은 step 8**에서 step 7 드릴 실측(또는 gameday-01 evidence)으로 채운다 — 실측이 불충분하면 "문서화된 AWS 동작과 정합하며 실측(9분48초/무발화)이 이를 시사한다"로 근거 인용해 완화. canary 지표의 "단정 금지·1차 출처"([[infra-plan-review-and-first-source]]) 규율을 A-5 메커니즘에도 동일 적용(3인 공통 — A-5를 만든 미실측 단정 재발 방지).
   - (ii) **판정 = 유지(느린 백스톱)** (실측과 무관 → 초안에서 확정 가능): canary/외부 신호까지 죽은 최후의 경우를 위해 남기되 MTTD 신호로 신뢰하지 않는다. 주기 튜닝은 (발행공백이 원인일 경우) 무효 → **`monitoring.tf`의 `alb_no_healthy_hosts`는 건드리지 않는다**(surgical).
   - (iii) canary = 무트래픽 방어선(역할 분리는 step 7 성공판정 참조).
   - **문서 갱신 (크로스리전 운용까지 — codex-ide #3, 초안에서 확정 가능)**: `docs/runbooks/alarm-response.md`에 canary 알람 절 추가 + **공통 알람 전수 확인·알람 이력 명령·심각도 표에 us-east-1(`--region us-east-1`) canary 알람 포함**(현재 절차는 ap-northeast-2만 조회 → 온콜이 canary 상태를 놓침) + **"첫 canary ALARM은 preflight 확인 전까지 장애로 단정하지 않는다"**(최초 발행 레이스) 운영 문구. `infra/README.md` Slack 통지 문제해결에 us-east-1 canary 토픽·구독 확인 추가. **단 `docs/postmortems/2026-07-06-gameday-01-retro.md`의 A-5·A-10 "백로그 → P4(c) 종결" 플립은 step 8**(실측 근거 확정 후 — codex-cli #1·codex-ide #1).
   → 검증: ADR 초안·runbook·README 상호 링크 정합(깨진 링크 0), A-5 근본원인 절이 **플레이스홀더+증거 체크리스트** 형태, 판정이 "유지+비신뢰+canary 대체"로 일관, runbook/README 공통 절차가 us-east-1 canary 포함.

6. **README/fixtures + 로컬 종합 검증** — `infra/README.md` 모니터링 절에 canary 경로(무트래픽 방어선) 추가. `docs/plans/0006-.../fixtures/`에 헬스체크 설정·알람 스키마·(가능하면) `HealthCheckStatus` 지표 좌표 스냅샷.
   → 검증: `terraform fmt -check`·`terraform validate` 전부 Success(에이전트 실행 가능한 로컬 정적검사 한도).

7. **[사람] terraform plan → apply → (비파괴 preflight) → 무트래픽 다운 드릴** (에이전트 실행 금지):
   - `terraform plan` 첨부(기대: 헬스체크+us-east-1 토픽/정책/알람 add, `sns_topic_arns` 1건 change, ecs/alb/rds/기존 알람 무변경). 사람 검토 후 `apply`.
   - `aws cloudwatch list-metrics --region us-east-1 --namespace AWS/Route53`로 **지표 발행 실측**(us-east-1 발행 전제의 사후 확정), 헬스체크 Healthy 확인.
   - **[비파괴 preflight — 파괴 드릴 전 반드시] (codex-cli #3·codex-ide #1)**: ① `terraform output slack_alerts_enabled` = true ② Chatbot config가 ap-northeast-2+us-east-1 **두 토픽 ARN을 모두 바인딩**하고 us-east-1 canary 토픽 구독이 존재하는지 확인 ③ `aws cloudwatch set-alarm-state --region us-east-1 --alarm-name <canary_down> --state-value ALARM --state-reason <사유>`(그리고 OK; `--state-reason`은 CLI 필수 — codex 2인)로 **Slack에 canary 다운·복구 카드가 실제로 오는지 먼저 확인**(P3-2에서 검증된 비파괴 통지 테스트). **주의**: 이 테스트 상태는 다음 지표 평가에서 자동 복귀하며, 실제 상태가 OK면 `ok_actions`가 한 번 더 발화해 **여분의 OK 카드**가 올 수 있다(정상 — claude-ide). 실패하면 크로스리전 배선을 고치고, 끝내 불가하면 **Alt-1(알람 없이 트래픽 생성기만)로 축소**한다(배경/제약 참조).
   - **종단검증(P3-2 S2 재사용)**: 수동 curl 없이(=무트래픽) chaos 이미지로 desired=0 다운 유발 → **`canary_down`이 수분 내 ALARM → Slack 다운 카드** 수신(MTTD 기록), `alb-elb-5xx`(=canary 트래픽으로 무트래픽에서도 발화)의 MTTD도 함께 기록 → 정상 복구 시 `canary_down` **OK 카드** 확인. **동시에 A-5 증거 수집**: 같은 down 구간의 `HealthyHostCount` datapoint를 `get-metric-statistics`로 캡처(missing vs 0 판별) + `alb-no-healthy-hosts` 발화 시각 기록 → ADR 0004 근본원인 근거로 사용(codex-cli #1·claude-ide #1).
   → 검증 (역할 분리 반영 — claude-ide MTTD): **Slack 다운·복구 카드 수신** + 두 무트래픽 채널이 실측 기록됨 — **`alb-elb-5xx`(canary 트래픽 기반, 회고상 ~3–5분)=빠른 1차**, **`canary_down`(≈flip 90s + 알람 3분 ≈ 4.5분+)=결정론적 백스톱**, 둘 다 `alb-no-healthy-hosts`(9분48초/무발화)보다 빠름.

8. **[사람→에이전트] 종결 문서 확정** (step 7 직후, 실측 반영 — codex-cli #1·codex-ide #1·claude-ide): step 7 드릴의 `HealthyHostCount` 실측(missing vs 0)·`canary_down`/`alb-elb-5xx` MTTD 결과를 사람이 제공 → **ADR 0004의 A-5 근본원인 절을 플레이스홀더에서 확정 서술로 갱신**하고, `docs/postmortems/2026-07-06-gameday-01-retro.md`의 **A-5·A-10 줄을 "P4(c) 종결"로 플립**. 실측이 missing/0을 명확히 못 가리면 근거-인용 완화형으로 확정.
   → 검증: ADR A-5 근본원인 절에 **실측 datapoint(또는 근거-인용)**가 실려 있고, 회고 A-5·A-10이 "종결"로 바뀜 = goal #3("원인을 문서로 규명·판정")이 **실측 근거 위에서** 마감 = A-5/A-10 실증 종결.

## 리스크/롤백

- **크로스리전 오배선**: us-east-1 알람이 ap-northeast-2 토픽을 못 가리키는 실수 → 알람은 반드시 `canary_use1`(us-east-1) 토픽에 붙인다. plan에서 알람 region·토픽 ARN 리전 일치 육안 확인. 롤백: `synthetic-canary.tf`·providers alias·chatbot 1줄 제거 후 apply(기존 12알람·SNS·Chatbot 무영향 — 전부 additive).
- **헬스체커 SG 차단**: 443이 이미 0.0.0.0/0 개방이라 문제없음. 만약 향후 SG를 조이면 Route53 체커 IP 대역 예외 필요(현재 불요, 주석에 명시).
- **canary 오탐(자기발 트래픽)**: 상시 프로빙으로 `alb-target-5xx`·`alb-latency-p95`에 항상 샘플이 생김 → 정상 200/저지연이면 무해. `/healthz`가 레이트리밋 예외라 429 오탐 없음.
- **HealthCheckStatus missing 처리**: 헬스체크 존재 시 지표는 상시 발행되나, 안전하게 `breaching`(down으로 간주). 헬스체크 삭제/일시 공백을 down 오탐으로 만들 여지 → evaluation_periods=3(약 3분)로 순간 공백 흡수.
- **최초 apply 오탐 카드 레이스 (신규 지표 — codex-ide·claude-ide 지적)**: 헬스체크·`canary_down` 알람을 동시 생성하면 HealthCheckStatus가 us-east-1에 **최초 발행되기 전 공백**이 `breaching`으로 취급돼 **서비스는 정상인데 down 카드가 1회** 뜰 수 있다(기존 12 breaching 알람은 대상 지표가 이미 존재해 이 레이스가 없었음). 발행(~1–2분)이 알람창(3분)보다 빠를 공산이 크나 레이스는 실재. 완화: (a) 헬스체크 먼저 apply→`list-metrics`로 발행·Healthy 확인→알람 2단계 적용, 또는 (b) "apply 직후 1회 오탐 가능"을 preflight에 명시하고 첫 카드는 `list-metrics`/헬스체크 상태로 진위 확인.
- **비용 초과 인식**: 월 ~$1.6, string-match/fast-interval 미사용으로 최소화. apply 전 사람이 승인.

## 검토 반영 로그

<!-- /plan-merge가 라운드별로 기록. 형식: [rN] 리뷰어#번호 지적요약 → 반영|기각 — 사유 -->

**revision 1 리뷰(3인 전원 request-changes, reviewed-revision=1 전부 유효) → revision 2 (전부 반영, 기각 0)**

- [r1] codex-cli#1 [높음] A-5를 미검증 가설로 종결 → **반영** — step 7 드릴에서 `HealthyHostCount` datapoint를 `get-metric-statistics`로 실측(missing vs 0) + step 5 ADR 문구를 실측결과/근거-인용 완화형으로 규정.
- [r1] codex-cli#2 topic policy provider 미명시 → **반영** — step 2에 `aws_sns_topic_policy.canary_use1`도 `provider = aws.use1` 명시(육안 검증 추가).
- [r1] codex-cli#3 Slack preflight 부재(파괴 드릴 후에야 미배선 발견) → **반영** — step 7에 비파괴 preflight(slack_alerts_enabled·두 토픽 바인딩·구독 확인) 추가.
- [r1] codex-cli#4 크로스리전 전제의 1차 출처가 post-apply로 밀림 + fallback → **반영** — step 5 ADR에 1차 출처 URL 인용(하드 게이트), step 7에 Alt-1 축소 fallback 명시. (지표 존재의 최종 확정은 헬스체크 생성이 선행돼야 하므로 본질상 post-apply — 대신 set-alarm-state preflight를 파괴 드릴 전 게이트로 세움.)
- [r1] codex-cli 개선 ok_actions / string-match 2xx-3xx 문구 → **반영** — step 4 ok_actions, step 3·비용줄 문구 교정.
- [r1] codex-ide#1 새 us-east-1→Chatbot 경로를 파괴 드릴 전 비파괴 검증(set-alarm-state --region us-east-1) + outputs 추가 → **반영** — step 7 preflight ③, step 4 outputs(토픽 ARN·알람 이름).
- [r1] codex-ide#2 canary_down ok_actions 누락 → **반영** — step 4(3인 공통).
- [r1] codex-ide#3 문서 갱신이 크로스리전 운용 미반영(runbook 공통·이력·심각도표, README가 ap-northeast-2만) → **반영** — step 5 문서 갱신에 us-east-1(`--region us-east-1`) canary 포함.
- [r1] codex-ide 개선 ADR 1차 출처 URL / breaching 초기 소음 → **반영** — step 5 URL 인용, 리스크에 최초 apply 오탐 레이스 추가.
- [r1] claude-ide#1 [must-fix] A-5 근본원인을 1차 출처 검증 없이 단정 → **반영** — step 5를 "단정 금지·실측 또는 근거-인용 완화"로 재작성(codex-cli#1과 동일 해소).
- [r1] claude-ide 개선 ok_actions / topic policy provider=aws.use1 / 최초생성 오탐 레이스 / MTTD 역할 명시 → **반영** — step 4·step 2·리스크·step 7 성공판정(alb-elb-5xx=빠른 1차, canary_down=결정론적 백스톱)에 각각 반영.

**revision 2 리뷰(claude-ide approve / codex-cli·codex-ide request-changes, reviewed-revision=2 전부 유효) → revision 3 (전부 반영, 기각 0)**

- [r2] codex-cli#1 · codex-ide#1 [중간, 동일 지적] A-5 "종결" 문서화(step 5 에이전트)가 근거 실측(step 7 사람 드릴)보다 앞 → 순서 역전 → **반영** — step 5를 "초안(근본원인=플레이스홀더+증거 체크리스트, 판정·크로스리전 문서만 확정)"으로 낮추고, **step 8 신규**(사람 실측 제공 후 ADR A-5 절 확정 + 회고 A-5/A-10 "종결" 플립) 추가. 두 리뷰어가 명시한 "이 순서만 정리되면 진행 가능"을 정확히 이행.
- [r2] claude-ide 개선#1 [낮음] step 7 실측→ADR 반영 단계가 암묵적 → **반영** — 위 step 8로 명시적 액션화(codex 2인 #1과 동일 해소).
- [r2] codex-cli 개선 / codex-ide 개선 `set-alarm-state`에 `--state-reason` 필수 → **반영** — step 7 preflight ③ 명령에 `--state-reason` 추가.
- [r2] codex-cli 개선 단일 apply 시 "첫 canary ALARM은 preflight 전까지 장애로 간주하지 않음" 운영 문구 → **반영** — step 5 runbook/README 갱신 항목에 추가(리스크 절 최초 apply 레이스와 연결).
- [r2] codex-ide 개선 Route53 alias `evaluate_target_health=true`인데 전 레코드 unhealthy면 응답 반환 → 다운 중 DNS resolve 유지(canary 트래픽 전제) → **반영** — step 5 ADR 1차 출처 URL 인용 목록에 추가(제공된 공식 문서 2건).
- [r2] claude-ide 개선#2 [낮음·선택] `set-alarm-state` 자동 복귀 → 여분 OK 카드 정상 주석 → **반영** — step 7 preflight ③에 자동 복귀·여분 OK 카드 주의 추가.
