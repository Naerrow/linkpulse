# 0005 배포 실패 알림 — 이벤트 필터 검증 fixture

`infra/prod/eventbridge.tf`의 `event_pattern`이 실제 ECS 배포 실패 이벤트를 잡는지 apply 전에 검증하기 위한 자료다. (plan 0005 실행 1단계 — 하드 게이트.)

## 파일

- `sample-event.json` — `ECS Deployment State Change` / `SERVICE_DEPLOYMENT_FAILED` 실패 이벤트 샘플. ARN·account는 자리표시자(`123456789012`, `linkpulse-prod-cluster/linkpulse-prod-app`).
- `event-pattern.json` — Terraform이 만드는 필터와 동일한 EventBridge 패턴. 위 샘플과 동일 자리표시자 ARN을 쓴다.
- `sample-event-reason-edgecase.json` — `reason`에 따옴표·개행을 넣은 edge-case. input_transformer 변환 후 payload가 유효 JSON으로 남는지 사람이 1회 실측하는 용도(아래 §reason escaping).

## D5 수기 대조 (핵심 근거)

- 서비스 ARN은 이벤트 **top-level `resources[0]`** 에 온다 → 패턴도 `resources`로 매칭한다.
- `detail`에는 `eventType`/`eventName`/`deploymentId`/`updatedAt`/`reason`만 있고 **`clusterArn`이 없다**. `clusterArn`으로 필터하면 이 이벤트 타입에선 영구 미매칭(조용한 실패)이라 쓰지 않는다.
- 서비스 ARN은 신규 long-ARN 포맷 `arn:<partition>:ecs:<region>:<account>:service/<cluster>/<service>`. `eventbridge.tf`는 이 값을 수기 조립하지 않고 **provider가 평가한 `aws_ecs_service.app.arn`을 직접** 쓴다(파티션/포맷 drift 원천 차단). ECS long-ARN은 2021년부터 계정 강제라 실이벤트 `resources[0]`와 동일 포맷이다. 이 fixture의 자리표시자 ARN은 그 포맷을 재현한 것일 뿐이므로, **실값 대조는 아래 preflight 하드 게이트**로 확인한다.

## 사람이 실행: AWS로 패턴 검증

AWS 접촉 명령이라 사람이 실행한다([[ask-before-external-services]]). 자리표시자 ARN이 양쪽 파일에 동일하므로 `true`가 나와야 한다(필드 경로·필터 구조가 맞다는 뜻).

```bash
cd docs/plans/0005-p4-deploy-failure-alerts/fixtures
aws events test-event-pattern \
  --event-pattern "$(cat event-pattern.json)" \
  --event "$(cat sample-event.json)"
# 기대 출력: { "Result": true }
```

## 실값 ARN 대조 (하드 게이트 — apply 전/후)

위 `test-event-pattern`의 `true`는 **두 자리표시자 복사본이 같다**는 것만 증명한다(구조·필드 경로 검증). Terraform이 평가한 pattern이 **실제 ECS 서비스 ARN과 바이트 동일**한지는 별도로 대조해야 한다(어긋나면 규칙이 영구 침묵). `describe-services`에서 독립적으로 실 `serviceArn`을 얻어 평가된 pattern과 맞춘다:

```bash
# (1) 실제 서비스 ARN
aws ecs describe-services --cluster linkpulse-prod-cluster --services linkpulse-prod-app \
  --region ap-northeast-2 --query 'services[0].serviceArn' --output text

# (2) Terraform이 규칙에 넣은 값 (state 접근 가능한 사람)
terraform state show aws_cloudwatch_event_rule.deploy_failed | grep -A3 event_pattern
```

두 값의 `service/<cluster>/<service>` 세그먼트가 정확히 일치하는지 눈으로 확인한다. (apply 후에는 실발화 카드의 서비스 ARN 렌더로 재확인 — 종단 검증.)

## reason escaping edge-case (사람 1회 실측 — 선택)

`input_transformer`가 `<reason>`을 따옴표 문자열에 보간한다. EventBridge는 유효 JSON template에서 따옴표는 escape하지만 **개행 포함 reason은 payload를 깰 수 있다**(카드 조용한 드롭). `reason`은 ECS 통제 단일 줄 문자열이라 리스크는 낮고 ADR 0003에서 "알고서 수용"으로 명시했으나, 더 단단히 하려면 1회 실측한다:

- AWS 콘솔 **EventBridge → Sandbox → Input transformer**에 `sample-event-reason-edgecase.json`(따옴표·개행 reason)을 넣고 `eventbridge.tf`의 `input_paths`/`input_template`을 그대로 붙여, 출력 target input이 **유효 JSON**으로 남는지 확인한다. 깨지면 description을 정적으로 두거나 JSON 인코딩 계층을 검토한다.

## 음성(negative) 확인 — 오탐 없음

`sample-event.json`의 `detail.eventName`을 `SERVICE_DEPLOYMENT_COMPLETED`로 바꾸거나
`resources[0]`를 다른 서비스 ARN으로 바꾸면 `{ "Result": false }`가 나와야 한다(정확 매칭이라 다른 서비스·성공 이벤트는 통과 못 함).
