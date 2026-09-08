# 0009 Step 1 — 회전 자동 재배포 fixture

`infra/prod/rotation-redeploy.tf`의 event pattern과 Lambda handler를 검증하는 자료다.

> ⚠️ **두 디렉터리는 용도가 정반대다. 바꿔 쓰면 반드시 실패한다.**
>
> | | 용도 | `detail.versionId` |
> | --- | --- | --- |
> | `pattern/` | `aws events test-event-pattern` — 패턴 매칭 구조 검증 | **의미 없다**(손으로 쓴 값) |
> | `invoke/` | `aws lambda invoke` — handler 종단 검증(1-4) | **실제 값이어야 한다** |
>
> handler는 라벨 확인을 **하드 선행 조건**으로 두므로(`UpdateService` 앞), 임의의 `versionId`로
> invoke하면 60초 bounded wait를 소진하고 오류로 끝난다. `pattern/`을 1-4에 재사용하면
> 실행자가 *"자동화가 깨졌다"*로 오진하기 쉽다.

## 이벤트 계약 (AWS 1차 출처)

- Secrets Manager는 스테이징 라벨이 새 버전으로 옮겨갈 때 `Secret Label Updated`를 발행하고,
  **`AWSPENDING`·`AWSPREVIOUS`에는 발행하지 않는다.**
- `detail.versionId`는 **라벨이 붙은 새 버전의 ID**다. `resources[0]`이 시크릿 ARN이다.
- **이벤트 본문의 `labelUpdated`는 문자열이고, event pattern의 비교 값은 배열이다.**
  두 파일이 그 차이를 그대로 보여 준다(`sample-event.json` vs `event-pattern.json`).
- AWS가 회전 감지에 이 이벤트를 권장한다(CloudTrail `RotationSucceeded`는 `Resources=[]`라
  top-level `resources` 정확 매칭이 성립하지 않고 전달도 best-effort다).

출처: [Secret Label Updated event](https://docs.aws.amazon.com/secretsmanager/latest/userguide/event-detail-secret-label-updated-secretsmanager.html) ·
[Match events when a secret value rotates](https://docs.aws.amazon.com/secretsmanager/latest/userguide/monitoring-eventbridge.html#monitoring-eventbridge_examples-rotations)

## pattern/ — 1-2 검증 (사람이 실행)

AWS 접촉 명령이라 사람이 실행한다. 자리표시자 ARN이 패턴·이벤트 양쪽에 동일하므로 `true`가
나와야 한다(필드 경로·필터 구조가 맞다는 뜻).

```bash
cd docs/plans/0009-p4d2-rotation-resilience/fixtures/pattern

# 기대: true  (대상 ARN + AWSCURRENT)
aws events test-event-pattern --event-pattern "$(cat event-pattern.json)" --event "$(cat sample-event.json)"

# 기대: 셋 다 false
aws events test-event-pattern --event-pattern "$(cat event-pattern.json)" --event "$(cat negative-other-secret.json)"
aws events test-event-pattern --event-pattern "$(cat event-pattern.json)" --event "$(cat negative-custom-label.json)"
aws events test-event-pattern --event-pattern "$(cat event-pattern.json)" --event "$(cat negative-cloudtrail-detail-type.json)"
```

반례가 각각 잡는 것: (a) 다른 시크릿 ARN, (b) 커스텀 스테이징 라벨, (c) CloudTrail 유래 `detail-type`.

### 실값 ARN 대조 (하드 게이트)

위 `true`는 **두 자리표시자 복사본이 같다**는 것만 증명한다. Terraform이 평가한 패턴이 실제
시크릿 ARN과 바이트 동일한지는 따로 봐야 한다(어긋나면 rule이 영구 침묵). RDS 관리 시크릿의
이름에는 `rds!db-...` 형태의 `!`가 들어가므로 자리표시자도 그 포맷을 재현해 뒀다.

```bash
aws rds describe-db-instances --db-instance-identifier linkpulse-prod-pg \
  --region ap-northeast-2 --query 'DBInstances[0].MasterUserSecret.SecretArn' --output text

terraform -chdir=infra/prod state show aws_cloudwatch_event_rule.rotation_label_updated | grep -A3 event_pattern
```

## invoke/ — 1-4 검증 (사람이 실행, 프로덕션 재배포가 실제로 일어난다)

⚠️ **①은 프로덕션 롤링 재배포를 실제로 일으킨다.** 저트래픽 시간대에 수행하고 서킷브레이커가
켜져 있는지 먼저 확인한다.

자리표시자를 실값으로 바꾼 뒤 쓴다. 손으로 고치지 말고 아래로 생성한다:

```bash
cd docs/plans/0009-p4d2-rotation-resilience/fixtures/invoke

SECRET_ARN=$(aws rds describe-db-instances --db-instance-identifier linkpulse-prod-pg \
  --region ap-northeast-2 --query 'DBInstances[0].MasterUserSecret.SecretArn' --output text)
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
SECRET_NAME=$(aws secretsmanager describe-secret --secret-id "$SECRET_ARN" \
  --region ap-northeast-2 --query Name --output text)
# 현재 AWSCURRENT를 들고 있는 버전 id
VERSION_ID=$(aws secretsmanager describe-secret --secret-id "$SECRET_ARN" --region ap-northeast-2 \
  --query VersionIdsToStages --output json | jq -r 'to_entries[] | select(.value|index("AWSCURRENT")) | .key')

for f in rotation-label-updated unknown-version; do
  sed -e "s|REPLACE_SECRET_ARN|$SECRET_ARN|" \
      -e "s|REPLACE_ACCOUNT_ID|$ACCOUNT_ID|" \
      -e "s|REPLACE_SECRET_NAME|$SECRET_NAME|" \
      -e "s|REPLACE_CURRENT_VERSION_ID|$VERSION_ID|" "$f.json" > "/tmp/$f.live.json"
done
```

- **① `rotation-label-updated.json`** — 정상 경로. `outcome=redeploy_submitted`,
  `deployments[].createdAt` 갱신 + `rolloutState=COMPLETED`, `/readyz` 200, 왕복 302.
- **①-2 같은 fixture를 한 번 더 invoke** — `outcome=skipped_duplicate`로 끝나고
  `deployments[].createdAt`이 **갱신되지 않아야** 한다. `GetItem` 경로와 그 IAM은 이렇게
  하지 않으면 첫 중복 이벤트가 올 때까지 드러나지 않는다.
- **② `unknown-version.json`** — 존재하지 않는 `versionId`라 라벨 확인이 상한(60초)까지
  소진되고 `outcome=failed_label_wait`로 오류를 반환한다.

  **`--invocation-type Event`(비동기)로 invoke해야** on-failure destination이 시험된다
  (기본 동기 호출로는 전혀 시험하지 못한다). 비동기 재시도 2회가 끝날 때까지 **최대 10분**.

  ⚠️ **치환을 빠뜨리면 다른 경로가 시험된다.** `REPLACE_SECRET_ARN`이 그대로면 handler의 이벤트
  대조가 먼저 걸러 **즉시** `outcome=failed_validation`으로 끝난다 — 60초를 태우지 않는다.
  이때도 예외는 그대로 전파되므로 **on-failure destination 자체는 정상적으로 시험된다.**
  시험하지 못하는 것은 **60초 bounded wait와 `failed_label_wait` 경로**다.
  로그의 `outcome`으로 둘을 구분한다 — `failed_validation`이 나왔으면 치환을 다시 하고 재실행한다.

```bash
aws lambda invoke --function-name linkpulse-prod-rotation-redeploy \
  --region ap-northeast-2 --payload fileb:///tmp/rotation-label-updated.live.json out.json

aws lambda invoke --function-name linkpulse-prod-rotation-redeploy --invocation-type Event \
  --region ap-northeast-2 --payload fileb:///tmp/unknown-version.live.json out.json
```

ℹ️ **①이 남기는 marker는 현재 `AWSCURRENT` 버전의 것이다.** 실해는 없다 — 그 버전의 라벨 이동
이벤트는 이미 지나갔고 이후 회전은 새 `versionId`를 만든다. 다만 멱등 테이블 잔여 위험(같은
버전에 `AWSCURRENT` 재부여)과 겹치는 유일한 실사례이므로, 1-6·1-7에서 로그를 읽을 때
*"이 marker는 1-4가 남긴 것"*임을 기억한다.

⚠️ **확인 후 [사람]이 on-failure 큐의 테스트 메시지를 삭제**해 알람을 정상화한다(안 하면 DLQ
depth 알람이 ALARM에 남는다). 의도적으로 발생한 Lambda `Errors` 알람도 함께 정리한다.

**rule→Lambda 배선은 여기서 증명되지 않는다** — 1-6 드릴이 그 역할이다.
(`put-events`로는 종단 검증이 불가능하다: 고객 이벤트의 `source`는 `aws.`로 시작할 수 없다.)
