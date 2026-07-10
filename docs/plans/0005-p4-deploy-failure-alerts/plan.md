---
status: approved # in-review | approved
revision: 5
created: 2026-07-09
---

# 0005. P4 배포 실패 알림 (EventBridge → SNS → Slack)

## 목표

ECS 롤링 배포가 **실패해 서킷브레이커가 롤백**하면, 사람이 GitHub Actions 로그를 열어 보기 전에 **Slack으로 자동 통지**된다. P3에서 만든 SNS(`alarms`) → AWS Chatbot → Slack 경로를 재사용해, EventBridge 규칙 하나로 ECS 배포 실패 이벤트를 잡아 같은 채널로 흘린다.

**완료 조건:** 의도적 나쁜 이미지 배포 → 서킷브레이커 롤백 → **Slack에 배포 실패 카드 수신** → 정상 이미지 재배포로 복구. (P3-2의 fault-injection 절차 재사용.)

## 배경/제약

- **이미 있는 것 (재사용):**
  - `infra/prod/monitoring.tf`: `aws_sns_topic.alarms` + `aws_sns_topic_policy.alarms`. 현재 정책은 `cloudwatch.amazonaws.com`의 `sns:Publish`만 허용(confused-deputy 조건 포함).
  - `infra/prod/chatbot.tf`: `aws_chatbot_slack_channel_configuration.alarms`가 `sns_topic_arns = [alarms.arn]`를 Slack으로 렌더. `local.slack_enabled`(team/channel ID 양쪽 존재) count-gate.
  - `infra/prod/ecs.tf:93` `deployment_circuit_breaker { enable=true, rollback=true }` → 배포 실패 시 ECS가 **`ECS Deployment State Change` / `SERVICE_DEPLOYMENT_FAILED`** 이벤트를 EventBridge 기본 버스로 발행.
  - `outputs.tf`: `sns_alarms_topic_arn`.
- **아직 없는 것:** EventBridge 규칙 0개(`grep` 확인). 배포 실패는 현재 어떤 채널로도 통지되지 않는다.
- **커버리지 갭 (반드시 인지):** 이 경로는 **ECS단 배포 실패**(태스크가 뜨다 실패 → 서킷브레이커 롤백)만 잡는다. **GitHub Actions단 실패**(gofmt/vet/test 실패, 빌드 실패, 그리고 이번 P4를 촉발한 *러너 미획득/플랫폼 장애* [[deploy-arm-runner-flake]])는 ECS까지 도달하지 않아 **EventBridge가 못 잡는다.** → §설계결정 D4에서 범위를 명시하고 GitHub측 보완은 별도로 판단한다.
- **가드레일(AGENTS.md §1·§2):**
  - AWS를 건드리는 명령(`terraform plan`/`apply`, `aws ...` CLI, 의도적 배포 테스트)은 **전부 사람이 실행**한다([[ask-before-external-services]]). 에이전트는 tf/코드 작성 + AWS 미접촉 로컬 검증(`terraform fmt`/`validate`, `go build/test`)까지만.
  - AWS-bound description(SNS/EventBridge 등)은 ASCII만([[aws-descriptions-ascii-only]]).
  - 단정적 IAM/이벤트 스키마 주장은 1차 출처(Service Authorization / ECS EventBridge 문서)·CLI 실측으로 확인([[infra-plan-review-and-first-source]]).
  - Simplicity-first: 새 토픽·Lambda 없이 기존 경로에 규칙 1개만 얹는다.

## 설계 결정 (리뷰어 논점)

- **D1. SNS 토픽 재사용.** → **재사용**(`alarms`). Chatbot이 이미 렌더하고 Slack 채널도 하나다. 트레이드오프: 메트릭 알람과 배포 이벤트가 한 토픽/채널에 섞인다(운영 채널 하나라 무해). r2에서 제안했던 "조건 미지원 시 전용 `-deploy` 토픽 fallback"은 **철회**한다 — r2 claude-ide#2 지적대로 전용 토픽도 같은 Chatbot config·같은 Slack 채널로 팬아웃하므로 blast radius 실이득이 사실상 없어(최악=가짜 카드 1건) Simplicity-first에 반한다. 권한 스코핑은 D2(role)로 해결하므로 토픽 분리 불필요.
- **D2. EventBridge→SNS publish 권한 — IAM 실행 role(target `role_arn`) 기본. 1차 출처로 확정됨(r3).** **r1에서 "SNS 타깃은 role_arn 부적용"이라 기각한 것은 나의 단정 오류였다**(r2 codex 2인 반박, r3 claude-ide가 1차 출처로 확증). AWS `eb-use-resource-based.html`: **"For Lambda, Amazon SNS, and Amazon SQS resources, EventBridge can use either an IAM execution role or a resource-based policy"**, `RoleArn` 지정 시 그 role에 `sns:Publish` 필요(비교적 최근 추가된 기능이라 과거 지식과 어긋났다). → **IAM 실행 role 기본 채택**:
  - `aws_iam_role`(trust: `events.amazonaws.com`, confused-deputy 조건 = `aws:SourceArn`=규칙 ARN·`aws:SourceAccount`=계정을 **trust policy**에 — AWS `cross-service-confused-deputy-prevention.html`이 EventBridge rule target role trust의 정확한 권장 패턴으로 제시: "value of `aws:SourceArn` must be the rule ARN") + identity policy `sns:Publish`를 **대상 토픽 ARN으로만** 한정. target에 `role_arn` 지정.
  - 이 방식은 단지 대안이 아니라 **우월**하다: 같은 문서가 **"You can't use `Condition` blocks in Amazon SNS topic policies for EventBridge"**라 명시 → 토픽 정책으론 confused-deputy 조건을 아예 못 건다. role trust에선 걸 수 있어 role 방식이 confused-deputy를 제대로 처리한다. 기존 `aws_sns_topic_policy.alarms`는 **손대지 않는다**.
  - **잔여 실측(6단계에서만)**: 문서상 role_arn 유효가 확정됐으므로 apply 후 실발화(카드 수신)로 최종 확인만 한다. 만에 하나 role_arn이 무시되는 것으로 실측되면 → 리소스 기반 정책 폴백 = **순수 principal-only(조건 없음 — 위 문서상 SNS 토픽 정책엔 `Condition` 불가라 `aws:SourceArn`/`aws:SourceAccount`를 못 건다) + ADR 수용 기록**. 단일 계정·단일 채널이라 최악은 가짜 카드 1건(D1). 이 폴백은 사실상 죽은 가지다.
- **D3. Chatbot custom notification 스키마 고정(리터럴 JSON).** 네이티브 렌더에 도박하지 않고 input transformer로 Chatbot **custom notification**을 만든다. 최종 payload 골격을 리터럴로 못박는다(r2 codex 3인 개선안 — colon 빠진 표기 오해 방지):

  ```json
  { "version": "1.0", "source": "custom",
    "content": { "title": "<제목>", "description": "<reason 단독>" } }
  ```

  - `source`는 반드시 리터럴 `"custom"`(이벤트 원본 `aws.ecs` 넣으면 렌더 실패 — r1 3인 공통), `version="1.0"`, `content.description` **필수**.
  - **escaping 위험 최소화(r2 codex-ide#2)**: `content.description`엔 **`reason` 단독**만 넣고, 서비스 ARN·이벤트명·시각은 `content.title` 또는 별도 필드로 분리해 긴 문자열 보간으로 template JSON이 깨질 확률을 낮춘다. input_paths: `svc=$.resources[0]`, `ev=$.detail.eventName`, `reason=$.detail.reason`, `t=$.time`.
  - `reason`이 따옴표/개행을 포함하면 여전히 template JSON을 깰 수 있으니(조용한 드롭) 6단계에서 실제 `reason` 렌더를 확인한다.
- **D4. 범위 = ECS 배포 실패로 한정.** GitHub Actions단 실패 통지는 이 plan 밖. 근거: (a) 메모리가 지정한 경로가 EventBridge→SNS→Slack, (b) 이번 인시던트인 *러너 미획득*은 job이 아예 안 떠서 워크플로 `if: failure()` 스텝조차 안 돌아 GitHub측 보완으로도 못 잡는다(githubstatus.com 확인이 답 [[deploy-arm-runner-flake]]). → GitHub측 보완(워크플로 실패 시 SNS publish, deploy OIDC 롤에 `sns:Publish` 추가)은 **별도 후속**으로 남기고, 리뷰에서 "지금 포함 vs 연기"를 판단. 기본 권장 = **연기**(핵심 갭을 못 메우면서 IAM·워크플로 표면만 키움). (r1 claude-ide 동의.)
- **D5. 이벤트 필터 — `resources` 기반(clusterArn 아님).** `source=["aws.ecs"]`, `detail-type=["ECS Deployment State Change"]`, `detail.eventName=["SERVICE_DEPLOYMENT_FAILED"]`, `resources=[aws_ecs_service.app ARN]`. **`detail.clusterArn`은 이 이벤트 타입에 존재하지 않는다**(리뷰 r1 3인 공통: `detail`엔 `eventType`/`eventName`/`deploymentId`/`updatedAt`/`reason`만, 서비스 ARN은 top-level `resources`. `clusterArn`은 `ECS Service Action`/task-state 계열 필드) → clusterArn 필터는 영구 미매칭(조용한 실패). 서비스 ARN **정확 매칭**으로 좁혀(prefix보다 안전 — r1 claude-ide) 다른 서비스 노이즈 차단.

## 실행 단계

1. **스키마·권한모델 실측 확정 (하드 게이트).** 코딩 전에 다음을 1차 출처로 확정하고 근거를 PR/ADR에 URL로 남긴다([[infra-plan-review-and-first-source]]):
   - (a) `ECS Deployment State Change` 실패 이벤트의 실제 JSON — `detail`에 `clusterArn`이 **없고** 서비스 ARN이 `resources[0]`임을 ECS EventBridge 문서 예시로 확인(D5).
   - (b) [문서 확정됨·r3] EventBridge가 SNS 타깃에서 `role_arn`으로 publish + trust policy의 `aws:SourceArn`(=규칙 ARN)/`aws:SourceAccount` 적용 — `eb-use-resource-based.html`·`cross-service-confused-deputy-prevention.html`로 확증. 구현 시 이 URL을 ADR에 인용만 하고, 실동작(assume-role→publish)은 6단계 실발화로 확인.
   - (c) Chatbot custom notification 필수 필드(`version="1.0"`, `source="custom"`, `content.description`)를 Chatbot 문서로 확인(D3).
   - (d) `aws_ecs_service.app`의 Terraform ARN 속성이 실이벤트 `resources[0]` long-ARN(`.../service/<cluster>/<service>`)과 **바이트 동일**한지 세그먼트(파티션·리전·계정·클러스터·서비스) 대조(r2 claude-ide#3). exact match가 불안하면 서비스명까지 prefix 매칭으로 완화.
   → 검증: 위 출처 URL을 ADR에 기재. 에이전트는 event_pattern + 실제 ARN을 넣은 **샘플 실패 이벤트 JSON(fixture)** 를 저장소에 작성하고 매치를 수기로 대조한다. AWS로 확인이 필요하면 `aws events test-event-pattern` **복붙 명령을 사람에게 넘겨 실행**(AWS 접촉 명령은 에이전트가 실행하지 않는다 — [[ask-before-external-services]]).
2. **EventBridge 규칙 + SNS 타깃 작성** (`infra/prod/eventbridge.tf` 신규).
   - `aws_cloudwatch_event_rule.deploy_failed`: D5 event_pattern(`resources=[aws_ecs_service.app ARN]`). description은 ASCII.
   - `aws_cloudwatch_event_target`: target = `aws_sns_topic.alarms.arn`, `role_arn` = 아래 role(D2), **input_transformer**로 D3 custom-notification 생성 — `input_paths`(svc/ev/reason/t), `input_template`은 D3의 리터럴 JSON heredoc(`version`/`source`/`content.title`/`content.description`).
   → 검증: `terraform fmt` + `terraform validate` Success. event_pattern·input_template 모두 유효 JSON(샘플 값 치환 후 눈검사 + 작은 로컬 JSON fixture로 payload 유효성 확인 — r2 codex 개선안).
3. **SNS publish 권한 부여** (D2 기본 = IAM 실행 role).
   - `aws_iam_role.deploy_events`(trust: `events.amazonaws.com` + confused-deputy 조건 `aws:SourceArn`=규칙 ARN·`aws:SourceAccount`=계정) + identity policy `sns:Publish` on `aws_sns_topic.alarms.arn`만. 기존 `aws_sns_topic_policy.alarms`·Chatbot 경로는 **불변**.
   - (폴백, 사실상 죽은 가지) 6단계 실측에서 role_arn이 무효로 드러난 경우에만 리소스 기반 정책 = **순수 principal-only(조건 없음 — SNS 토픽 정책엔 EventBridge용 Condition 불가)** + ADR 수용 기록.
   → 검증: `terraform validate` Success. 기존 alarms 토픽 정책·Chatbot config diff 없음(재사용 경로 보존).
4. **`terraform plan` — 사람이 실행**(AWS 연결·자격증명이 필요한 인프라 명령이라 에이전트가 실행하지 않는다 [[ask-before-external-services]]). 에이전트는 아래 기대치를 제시하고, 사람이 붙여준 plan 출력을 함께 대조·해석한다.
   → 검증: **destroy 0**, ecs/alb/rds/chatbot 무변경 필수. 기대(role 기본) = add: event_rule 1 + event_target 1 + `aws_iam_role.deploy_events` + role policy(inline 또는 attachment) 1. `aws_sns_topic_policy.alarms` **변경 없음**. 어긋나면 원인 규명 후 멈춤.
5. **문서.** `docs/adr/`에 짧은 ADR(경로 선택·D1~D5 트레이드오프·커버리지 갭). 근거 URL을 **그대로 인용**(r3 claude-ide): `eb-use-resource-based.html`(SNS 타깃 role_arn 유효 + 토픽 정책 Condition 불가), `cross-service-confused-deputy-prevention.html`(rule target role trust 조건 패턴), ECS EventBridge 이벤트 문서(`resources`/`detail` 스키마), Chatbot custom notification 문서. 메모리 링크가 아니라 실제 URL로(r1 codex-ide). `infra/README.md` 모니터링 절에 "배포 실패 → Slack" 경로·검증법 추가.
   → 검증: ADR·README에 갭(§배경)·검증 절차·출처 URL이 적혀 있음.
6. **(사람) apply preflight + apply + 종단 검증.**
   - preflight: `terraform output slack_alerts_enabled`=true(=`local.slack_enabled`) 확인 — false면 SNS까진 동작해도 Slack 카드 수신 불가라 완료 조건 미달(r1 codex 개선안).
   - 사람이 `terraform apply` 후, P3-2 방식으로 **의도적 나쁜 이미지 배포**(부팅 즉시 죽는 이미지) → 서킷브레이커 롤백 유발 → **Slack 배포 실패 카드 수신 확인** → 정상 SHA로 재배포 복구.
   → 검증: Slack 카드 1건 수신(서비스 ARN·이벤트명·`reason` 정상 렌더 확인 — 특수문자 깨짐 없음), 서비스 desired=running 원복.

## 리스크/롤백

- **R1. Chatbot이 custom-notification을 못 렌더.** input transformer JSON이 스키마와 어긋나거나(`source≠"custom"`, `content.description` 누락) `reason`의 특수문자가 template JSON을 깨면 Slack에 아무것도 안 뜬다(조용한 드롭). → D3 리터럴 고정 + 6단계 실측 렌더 확인이 관문. 실패 시 template 교정, 최후엔 전용 채널 + 원문(raw) 확인.
- **R2. 이벤트 필터 미매칭(조용한 실패).** `detail.clusterArn`은 이 이벤트 타입에 없어 그대로 두면 영구 미매칭 → D5를 `resources[0]` 서비스 ARN으로 교정. 1단계 `test-event-pattern`으로 apply 전 확인, 6단계 실발화로 재검증.
- **R3. (완화됨) EventBridge가 SNS 타깃에 role_arn을 무시할 가능성.** 문서상 role_arn 유효 확정(D2)으로 리스크 대폭 축소. 6단계 실발화로 최종 확인. 만에 하나 무효면 순수 principal-only 리소스 정책 폴백(조건 불가) + ADR 수용. 단일 계정·채널이라 최악은 가짜 카드 1건.
- **R4. 서킷브레이커가 롤백을 안 하는 실패 유형.** `wait-for-service-stability` 타임아웃 등은 `SERVICE_DEPLOYMENT_FAILED`를 안 낼 수 있음. → 이 경로의 한계로 ADR에 명시(§배경 갭과 함께).
- **롤백:** 순수 추가(event_rule/target + IAM role/policy)라 되돌리기 쉬움. `terraform destroy -target`으로 event_rule/target/role/policy 제거. 기존 alarms 토픽 정책·Chatbot 경로는 손대지 않아 영향 없음.

## 검토 반영 로그

<!-- /plan-merge가 라운드별로 기록. 형식: [rN] 리뷰어#번호 지적요약 → 반영|기각 — 사유 -->

- [r1] claude-ide#1 / codex-cli#1 / codex-ide#2 (높음, 만장일치) `detail.clusterArn` 미존재로 영구 미매칭 → **반영** — D5를 `resources=[서비스 ARN]` 정확 매칭으로, input_transformer도 `$.resources[0]`에서 파싱하도록 교정. R2 갱신.
- [r1] codex-cli#2 / codex-ide#1 (높음) EventBridge→SNS 토픽 정책 Condition 미지원 → **부분 반영** — 근본 지적 수용해 D2를 "1차 출처로 조건 지원 확정" 하드 게이트로 전환, 미지원 시 전용 토픽 fallback(R3·D1 조건부 반전). 단 리뷰어 제시 대안 `event_target.role_arn`은 **기각** — SNS 타깃은 role_arn을 쓰지 않고 리소스 기반 정책을 쓰므로 부적용(사실 정정, ADR에 근거).
- [r1] codex-cli#3 / codex-ide#3 / claude-ide(제안) (중간) Chatbot custom notification 필수값 미고정 → **반영** — D3에 `version="1.0"`·`source="custom"`(리터럴)·`content.description` 필수 + input_paths/template 못박음. reason escaping 검증을 6단계에 추가.
- [r1] codex-cli/codex-ide(제안) `test-event-pattern`으로 필터 사전 검증 → **반영** — 1단계 검증에 추가.
- [r1] codex-cli/codex-ide(제안) apply 전 `slack_enabled` preflight → **반영** — 6단계 preflight로 추가.
- [r1] codex-ide(제안) 메모리 링크 대신 실제 문서 URL/경로 → **반영** — 5단계 ADR에 출처 URL 명시 지시 추가.
- [r1] claude-ide(제안) `resources` 서비스 ARN 정확 매칭이 prefix보다 안전 → **반영** — D5에 정확 매칭 명시.
- [r1] claude-ide (동의) D4 GitHub측 연기·D1 재사용·롤백 용이·가드레일 → **유지** — 근거 타당, 변경 없음(단 D1은 D2와 연동해 조건부).

- [r2] codex-cli#1 / codex-ide#1 (높음) D2 role_arn 기각이 사실 오류(SNS 타깃도 IAM 실행 role 지원) → **반영(기각 철회)** — r1의 role_arn 기각은 나의 단정 오류. 1차 출처(EventBridge IAM role 문서)+provider 스키마 근거 수용. D2를 **IAM 실행 role 기본**으로 재작성(trust에 confused-deputy 조건, identity policy `sns:Publish` 토픽 한정, 토픽 정책 불변). role_arn의 SNS 실사용 여부는 1단계 실측 게이트로 남김(R3).
- [r2] codex-cli#2 (중간) plan 기대치·롤백이 role 방식과 불일치 → **반영** — step3/4/롤백을 IAM role 추가(토픽 정책 무변경) 기준으로 갱신.
- [r2] claude-ide#2 (낮음~중간) 전용 `-deploy` 토픽 fallback은 blast-radius 실이득 없음(과설계) → **반영** — 전용 토픽 fallback 철회, D1 재사용 확정. 폴백은 principal-only+SourceAccount+ADR로.
- [r2] claude-ide#1 (중간) D2 게이트를 fail-safe 기본값으로 → **반영** — "문서가 적극 확인 못 하면 안전한 쪽, 최종 확증은 6단계 실발화"를 D2에 명시.
- [r2] claude-ide#3 (낮음) exact 서비스 ARN 매칭 포맷 취약성 → **반영** — 1단계(d)에 ARN 바이트 대조 + 실ARN test-event-pattern, 불안 시 prefix 완화.
- [r2] codex-cli/codex-ide (개선) input_template 실제 JSON heredoc 예시 + payload fixture 검증 → **반영** — D3에 리터럴 JSON 골격, step2 검증에 fixture 추가.
- [r2] codex-ide (개선) reason escaping — description 단독 + ARN/시각 분리 → **반영** — D3에 필드 분리 명시.

- [r3] codex-cli / codex-ide / claude-ide **전원 approve** → **수렴.** D2 role 전환·`resources` 필터·Chatbot 필수값 모두 확인. claude-ide가 1차 출처 3건으로 role_arn 유효(O)·SNS 토픽 정책 Condition 불가(X)를 확증.
- [r3] claude-ide#2 / codex-cli·codex-ide(공통) 리소스정책 폴백의 `aws:SourceAccount` 조건은 불가능(SNS 토픽 정책 Condition 미지원) → **반영** — 폴백을 순수 principal-only(조건 없음)로 정정 + "사실상 죽은 가지"로 축소. D2·step3·R3 수정.
- [r3] claude-ide#1 (정보) 확증 URL을 ADR에 인용, step1(b)는 문서상 선충족 → **반영** — step1(b)를 "문서 확정, 6단계 실측만"으로, step5에 실제 URL 명시.
- [r3] claude-ide#3 (낮음) step1(b)에 trust policy 조건 적용까지 포함 → **반영** — step1(b) 문구 확장(assume-role→publish 실동작 확인 포함).
- [r3] codex-ide (개선) input transformer escaping — description은 reason 단독 JSON 값 우선 → **반영(기반영 재확인)** — D3에 명시, step2 fixture 검증 유지.

- [rev5] (가드레일 정렬 — 리뷰 무관, 사용자 지적) `terraform plan`·`aws events test-event-pattern`을 **에이전트 실행 → 사람 실행**으로 정정 + 배경 가드레일 문구 교정. 근거: [[ask-before-external-services]](AWS 연결·자격증명이 필요한 모든 명령은 사람, 에이전트는 코드 작성 + AWS 미접촉 로컬 검증까지). 이전 revision들이 AGENTS.md §2.1 "terraform plan까지만(에이전트)" 문구를 답습한 오류였다. **설계 무변경이라 재검토 불요, status approved 유지.**
