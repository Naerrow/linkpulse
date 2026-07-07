# ADR 0002 — 알람·알림 설계(CloudWatch → SNS → Chatbot → Slack)

- 상태: 채택 (2026-07-02)
- 관련: AGENTS.md 가드레일 #1(인프라 변경은 사람 승인), Phase P3(관측성), [`docs/plans/0002-p3-observability.md`](../plans/0002-p3-observability.md)

## 맥락

P3에서 "감지·통지"를 도입한다. 로깅·자동 롤백(circuit breaker)은 이미 있으나, 장애를 사람에게 **자동으로 알리는** 채널이 없다. Slack으로 통지하기로 했고, 통지 경로 구현에 두 선택지가 있었다.

1. **AWS Chatbot(Amazon Q Developer in chat applications)** — CloudWatch Alarm → SNS → Chatbot → Slack. Terraform 선언만으로 끝나고, Slack webhook 시크릿·Lambda 코드가 없다. 대신 Slack workspace OAuth 승인을 콘솔에서 사람이 1회 해야 한다.
2. **Lambda + Slack Incoming Webhook** — SNS → Lambda → webhook. 유연하지만 webhook 시크릿(Secrets Manager)·Lambda 코드·런타임 유지보수가 생긴다.

이 서비스는 사람이 직접 운영하고(이해 못 하면 운영 못 함), "완성 우선" 원칙을 따른다. 시크릿·코드가 없는 **선언형 Chatbot**이 운영 부담이 가장 작다.

## 결정

1. **AWS Chatbot 채택.** 단, Chatbot **API는 us-east-2·us-west-2·ap-southeast-1·eu-west-1 4개 리전에만 존재**한다(ap-northeast-2 엔드포인트 없음 — CLI 실측: 연결 실패 / us-east-2·ap-southeast-1 정상 응답). 설정 데이터는 글로벌 저장소이므로 config 리소스에 `region = "us-east-2"`만 명시한다(AWS provider v6 per-resource region, alias 불요). SNS 토픽·알람은 ap-northeast-2 유지 — Chatbot은 타 리전 SNS 토픽 결합을 지원한다. Slack 미준비 구간을 위해 `slack_team_id`/`slack_channel_id`가 **둘 다 있을 때만** Chatbot role/attachment/config를 만든다(`count = local.slack_enabled`). 그전엔 알람+SNS만 존재.
2. **SNS는 KMS 미사용.** 알람 메시지에 민감정보가 없다. SNS SSE는 KMS 기반인데, 암호화하면 **CloudWatch가 그 KMS 키를 쓰도록 키정책을 따로 열어야** 알람 publish가 되는 흔한 함정이 생긴다 — 이번엔 회피. topic policy로 `cloudwatch.amazonaws.com`의 publish만 허용하고, confused-deputy 방지로 `aws:SourceArn`(계정 내 alarm ARN)·`aws:SourceAccount`를 강제한다.
3. **IAM 최소화.** Chatbot 채널 role 신뢰 주체는 `chatbot.amazonaws.com`(서비스링크드롤의 `management.chatbot.amazonaws.com`이 아니다). 권한은 `CloudWatchReadOnlyAccess`(알람 카드 렌더 + Slack "Show logs"). guardrail을 **지정하지 않으면 기본이 AdministratorAccess**(CFN 문서 원문)라, `guardrail_policy_arns`를 `CloudWatchReadOnlyAccess`로 명시해 상한을 낮춘다.
4. **임계값은 소스오브트루스에서 파생.** RunningTaskCount 임계값 = `var.service_desired_count`, FreeStorageSpace 임계값 = `var.db_allocated_storage`의 10%. 리터럴을 박지 않아 용량/desired를 바꿔도 알람이 따라온다.
5. **treat_missing_data 방향(설계 의도):**
   - 트래픽 지표(5xx·p95·unhealthy): `notBreaching` — 무트래픽=정상.
   - `HealthyHostCount<1`: `breaching` — 데이터가 없으면(타깃 전부 deregister) 다운으로 간주. **무트래픽에서 5xx/unhealthy가 침묵하는 사각지대를 이 알람이 메운다.**
   - `RunningTaskCount`: `missing` — Container Insights 활성 직후 초기 공백을 오탐으로 만들지 않기 위함(INSUFFICIENT_DATA는 정상).
   - RDS `FreeStorageSpace`: `breaching`(소진은 고위험). CPU/Connections/Memory: `missing`(재부팅 공백 오탐 회피).
6. **p95 저트래픽 보정.** `TargetResponseTime` p95는 샘플이 적으면 튄다 → `evaluate_low_sample_count_percentiles = "ignore"`.

## 결과 / 트레이드오프 (운영자가 알아야 할 함정)

- **Chatbot 권한은 "채널 단위"로 공유된다.** `user_authorization_required=false`라 채널 멤버 전원이 개별 AWS 인증 없이 채널 role(CloudWatchReadOnlyAccess = 계정 전역 CloudWatch·Logs 읽기)을 사용할 수 있다. **운영 전용 private 채널 유지가 전제조건.** 진짜 최소권한(로그 그룹 스코프 축소·user authorization)은 P4(IAM 최소권한)에서 재검토한다.
- **Slack은 콘솔 OAuth + 채널 초대가 선행돼야 통지가 온다.** workspace OAuth 승인만으론 부족하고, **Slack 채널에서 `/invite @Amazon Q`**(private 채널 필수)로 앱을 초대해야 한다. "Terraform은 성공했는데 Slack에 안 옴"이면 ① 앱 초대 ② SNS 구독 존재 ③ **SNS 구독의 raw message delivery가 켜져 있지 않은지**(꺼야 함)를 본다.
- **Chatbot 리전 함정(교차검토 5차에서 오판 발견·수정).** 초기 설계는 "ap-northeast-2 지원"을 전제로 기본 리전에 생성하려 했으나 **틀렸다** — 콘솔이 글로벌이라 생긴 혼동으로, ap-northeast-2에는 Chatbot API 엔드포인트가 없어 Slack 변수를 넣는 순간 apply가 연결 오류로 실패한다. `region = "us-east-2"` 명시로 해소. 워크스페이스 OAuth·설정 데이터는 글로벌이라 콘솔에서 승인한 workspace는 어느 지원 리전에서든 보인다(리전 불일치 걱정 없음). SNS·알람은 영향 없다.
- **Container Insights ON은 클러스터 setting in-place 변경**이다(태스크 정의와 무관, `ignore_changes=[task_definition]` 영향 없음). apply 즉시 반영되고 비용이 소폭 는다. tfvars에서 `false`로 끌 수 있다.
- **RunningTaskCount 알람은 desired와 연동**돼 있어, 나중에 오토스케일링을 붙여 `ignore_changes=[desired_count]`로 desired를 1까지 낮추면 `<2` 조건이 오탐일 수 있다 — 그때 이 알람을 재검토한다.
- **임계값은 전부 초기값**이다. 실트래픽을 며칠 보고 FreeableMemory(만성 발화 시 하향/SwapUsage 병행)·연결 수(파라미터그룹 실 `max_connections`)·p95 등을 조정한다.
- **AWS description/이름은 ASCII만.** SNS/알람 name·description, IAM 이름에 한글을 넣으면 apply가 깨진다(validate·plan은 통과하므로 주의).
- **CFN과 Terraform 인자명이 다르다.** Terraform은 `user_authorization_required`/`guardrail_policy_arns`. CFN 문서의 `UserRoleRequired`/`GuardrailPolicies`를 그대로 베끼면 `validate`가 깨진다.
