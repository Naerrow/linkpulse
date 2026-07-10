# ADR 0003 — 배포 실패 알림(EventBridge → SNS → Chatbot → Slack)

- 상태: 채택 (2026-07-10)
- 관련: AGENTS.md 가드레일 #1(인프라 변경은 사람 승인), Phase P4(하드닝), [`docs/plans/0005-p4-deploy-failure-alerts/plan.md`](../plans/0005-p4-deploy-failure-alerts/plan.md), 재사용 경로는 [ADR 0002](0002-alerting-design.md)

## 맥락

ECS 롤링 배포가 실패하면 `deployment_circuit_breaker`가 자동 롤백하지만(ecs.tf), 그 사실이 **사람에게 통지되지 않았다.** 실패를 알려면 GitHub Actions 로그를 사람이 직접 열어봐야 했다. P3에서 이미 만든 통지 경로(SNS `alarms` → AWS Chatbot → Slack)를 재사용해, 배포 실패도 같은 Slack 채널로 자동 통지한다.

ECS는 배포 실패 시 EventBridge 기본 버스로 `ECS Deployment State Change` / `SERVICE_DEPLOYMENT_FAILED` 이벤트를 발행한다. 규칙 하나로 이 이벤트를 잡아 SNS로 흘리면 된다.

## 결정

1. **SNS 토픽 재사용(`alarms`).** 새 토픽·Lambda 없이 기존 경로에 EventBridge 규칙 1개만 얹는다. 운영 Slack 채널이 하나라 메트릭 알람과 배포 이벤트가 한 채널에 섞이는 건 무해하다. 전용 `-deploy` 토픽은 같은 Chatbot config·같은 Slack 채널로 팬아웃해 blast-radius 실이득이 없어(최악=가짜 카드 1건) 도입하지 않는다. 권한 스코핑은 아래 3번(role)으로 해결한다.

2. **EventBridge→SNS publish 권한 = IAM 실행 role(target `role_arn`).** SNS 타깃은 IAM 실행 role을 지원한다(AWS 문서 [Using resource-based policies for Amazon EventBridge](https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-use-resource-based.html): _"For Lambda, Amazon SNS, and Amazon SQS resources, EventBridge can use either an IAM execution role or a resource-based policy"_). `role_arn` 지정 시 그 role에 대상 토픽 한정 `sns:Publish`만 준다(최소권한). **이 방식이 우월한 이유**: 같은 문서가 _"You can't use `Condition` blocks in Amazon SNS topic policies for EventBridge"_ 라 명시 → 토픽 정책으로는 confused-deputy 조건을 걸 수 없다. role의 trust policy에는 걸 수 있어, [Cross-service confused deputy prevention](https://docs.aws.amazon.com/eventbridge/latest/userguide/cross-service-confused-deputy-prevention.html) 권장대로 `aws:SourceArn`=규칙 ARN·`aws:SourceAccount`=계정을 trust에 건다. 기존 `aws_sns_topic_policy.alarms`(cloudwatch용)는 **손대지 않는다.**

3. **이벤트 필터 = `resources` 기반 서비스 ARN 정확 매칭.** 필터: `source=["aws.ecs"]`, `detail-type=["ECS Deployment State Change"]`, `detail.eventName=["SERVICE_DEPLOYMENT_FAILED"]`, `resources=[<서비스 ARN>]`. 서비스 ARN은 이벤트 **top-level `resources[0]`** 에 오고, `detail`에는 `eventType`/`eventName`/`deploymentId`/`updatedAt`/`reason`만 있어 **`clusterArn`이 없다**([ECS EventBridge 이벤트 문서](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/ecs_cwe_events.html)). `clusterArn`으로 필터하면 이 이벤트 타입에선 영구 미매칭(조용한 실패)이라 쓰지 않는다. 서비스 ARN은 수기 조립 대신 provider가 평가한 `aws_ecs_service.app.arn`을 직접 써 파티션/포맷 drift를 원천 차단한다(`eventbridge.tf:11-13`). ECS long-ARN(`arn:aws:ecs:<region>:<account>:service/<cluster>/<service>`)은 2021년부터 계정 강제라 이 값이 실이벤트 `resources[0]`와 바이트 동일하다. 정확 매칭이라 다른 서비스·성공 이벤트 노이즈를 차단한다. (apply 전 `docs/plans/0005-.../fixtures`의 `aws events test-event-pattern`으로 검증.)

4. **Chatbot custom notification 스키마 고정(리터럴 JSON).** 네이티브 렌더에 의존하지 않고 input transformer로 [Chatbot custom notification](https://docs.aws.amazon.com/chatbot/latest/adminguide/custom-notifs.html)을 만든다: `{ "version": "1.0", "source": "custom", "content": { "title": ..., "description": ... } }`. `source`는 반드시 리터럴 `"custom"`(이벤트 원본 `aws.ecs`를 넣으면 렌더 실패), `version="1.0"`, `content.description` 필수. **escaping 위험 최소화**: `description`엔 `reason` 단독만 넣고 서비스 ARN·이벤트명·시각은 `title`로 분리한다. `reason`이 따옴표/개행을 포함하면 여전히 template JSON을 깰 수 있어(조용한 드롭), apply 후 실발화로 실제 `reason` 렌더를 확인한다.

## 결과 / 트레이드오프 (운영자가 알아야 할 함정)

### 커버리지 갭 — 반드시 인지

이 경로는 **ECS단 배포 실패**(태스크가 뜨다 실패 → 서킷브레이커 롤백)만 잡는다. 다음은 **못 잡는다:**

- **GitHub Actions단 실패** — gofmt/vet/test 실패, 빌드 실패는 ECS까지 도달하지 않아 EventBridge가 볼 이벤트가 없다.
- **러너 미획득 / 플랫폼 장애** — 2026-07-09 인시던트처럼 job이 아예 안 떠서 워크플로 `if: failure()` 스텝조차 안 도는 경우(githubstatus.com 확인이 답, `docs/postmortems/2026-07-09-deploy-runner-acquisition.md`).
- **서킷브레이커가 롤백을 안 하는 실패 유형** — `wait-for-service-stability` 타임아웃 등은 `SERVICE_DEPLOYMENT_FAILED`를 안 낼 수 있다.

GitHub측 실패 통지(워크플로 실패 시 SNS publish, deploy OIDC 롤에 `sns:Publish` 추가)는 **의도적으로 연기**한다: 이번 인시던트의 핵심 갭(러너 미획득)을 못 메우면서 IAM·워크플로 표면만 키우기 때문. 필요 시 별도 후속으로 판단.

### 그 외 함정

- **role_arn 실동작은 apply 후 실발화로 최종 확인.** 문서상 SNS 타깃 role_arn 유효는 확정됐으나(위 2번), 실제 assume-role→publish는 apply 후 의도적 나쁜 배포로 카드 수신을 봐야 완결이다. 만에 하나 role_arn이 무시되면 폴백은 순수 principal-only 리소스 정책(SNS 토픽 정책엔 Condition 불가라 조건 없음) — 단일 계정·채널이라 최악은 가짜 카드 1건. (사실상 죽은 가지.)
- **Slack 배선 preflight.** `terraform output slack_alerts_enabled`=`true`가 아니면 SNS까진 동작해도 Slack 카드가 안 온다(완료 조건 미달). Slack 연동 절차는 ADR 0002 참고.
- **AWS description/이름은 ASCII만.** EventBridge 규칙·IAM 이름·description에 한글을 넣으면 apply가 깨진다(validate·plan은 통과하므로 주의).
- **`reason` escaping 잔여 리스크 = 알고서 수용.** input_template이 `<reason>`을 따옴표 문자열 안에 보간한다. EventBridge는 유효 JSON template에서 문자열 값의 따옴표는 escape하지만 **개행/제어문자 포함 reason은 template JSON을 깨 카드가 조용히 드롭될 수 있다.** 이 리스크를 수용하는 근거: (a) `SERVICE_DEPLOYMENT_FAILED`의 `reason`은 ECS가 생성하는 단일 줄 AWS 통제 문자열(사용자 입력 아님), (b) description에 reason만 격리하고 ARN·이벤트명·시각은 title로 분리, (c) 실발화로 콜론 포함 reason 렌더 검증 완료. 개행 reason 안전성을 더 확인하려면 `fixtures/`의 edge-case 샘플로 EventBridge Sandbox target-input 결과가 유효 JSON인지 사람이 1회 실측한다(선택).
- **롤백 = HCL revert 후 정상 apply(영구), `destroy -target`은 임시.** `terraform destroy -target`으로 event_rule/target/role/policy를 지워도 **HCL이 남아 있으면 다음 정상 apply가 다시 만든다.** 영구 롤백은 이 구성(eventbridge.tf 등)을 revert/삭제한 뒤 `terraform plan`→사람 apply로 제거한다. `destroy -target`은 재생성됨을 전제로 한 임시 긴급 조치로만 쓴다. 어느 경우든 기존 alarms 토픽 정책·Chatbot 경로는 불변이라 영향 없다(순수 추가라 되돌리기 쉬움).
