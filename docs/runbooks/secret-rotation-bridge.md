# Runbook — RDS 비밀번호 로테이션 마감 브릿지 (사람 수동)

RDS 마스터 비밀번호가 자동 로테이션되면 **로테이션 직후에 사람이 ECS 태스크를 강제 재기동해** 새 비밀번호를
주입하는 임시 절차다. 앱이 `DB_PASSWORD`를 **컨테이너 기동 시 1회만** 주입받는 구조(`infra/prod/ecs.tf`의 `secrets`)라,
로테이션 후 커넥션 풀이 만료되면 신규 연결이 전부 인증 실패하기 때문이다.

- **왜 임시인가**: 이 절차는 **근본 대응이 아니라 브릿지**다. 근본 대응(앱이 로테이션을 스스로 견디게 하는 것)은
  **plan 0009**가 맡는다. 탐지 경로 보강은 [plan 0008](../plans/0008-p4d2-secrets-rotation-and-db-detection/plan.md),
  탐지 실증 드릴은 plan 0010.
- **왜 필요한가**: 2026-08-16 로테이션에서 이 절차가 없어 **2일 18시간 31분** 동안 서비스가 죽었다
  (`docs/postmortems/evidence/db-password-rotation-2026-08-19/`).
- ⛔ **그 뒤로 두 번 더 죽었고, 세 번째가 가장 길었다. 이 런북의 문제는 "모르는 것"이 아니라 "실행되지 않는 것"이다.**

  | # | 회전 시각(KST) | 다운 | 이 런북의 상태 |
  | --- | --- | --- | --- |
  | 1차 | 2026-08-16 22:08 | **2일 18시간 31분** | 런북 없음 |
  | 2차 | 2026-08-23 (창 안) | **24시간 23분** | 런북 있음 · 미실행 |
  | **3차** | **2026-08-30 13:08:48**(일) | **4일 22시간 22분 54초** (09-04 11:31:42 복구) | 런북 있음 · 탐지 경로(plan 0008) apply 완료 · **미실행** |
  | **4차** | **2026-09-06 19:09:00**(일) | **13시간 10분** (09-07 08:19 복구) | 위 전부 + **SMS 경로(f-2b) apply 완료** · **문자 2통 도착** · **13시간 미대응** |

  ⚠️ **3차는 조건이 가장 좋았는데 1·2차를 합친 것보다 길었다.** 회전이 **일요일 주간 13:08**에 났고
  (야간 구멍이 아니다), `/readyz` canary도 이미 apply돼 있었다.

  **그리고 탐지는 완벽했다**(`describe-alarm-history` 실측):

  | 알람 | ALARM 진입 | 회전 이후 | 전이 횟수 |
  | --- | --- | --- | --- |
  | `alb-target-5xx` | 2026-08-30 13:15:37 | **+6분 49초** | ALARM 1 · OK 1 |
  | `canary_down` | 2026-08-30 13:17:04 | **+8분 16초** | ALARM 1 · OK 1 |

  - **plan 0008의 설계값**(*"전면 실패 시 기대값 약 6분 · 상한 8분"*)**이 실측으로 확인됐다.**
  - **플래핑 0회.** 2차 때는 19회 ALARM↔OK 왕복에 OK 8건이 무트래픽 오탐이었는데,
    `/readyz` + canary 전환이 그것을 없앴다 — **118시간 내내 ALARM을 끊김 없이 유지**했다.

  ⛔ **즉 탐지는 세 번 다 성공했고, 세 번째는 설계대로 완벽했다. 실패한 것은 응답뿐이다.**
  → 아래 **(f)**를 (a)보다 먼저 읽는다. 이 런북의 실효를 정하는 것은 (a)~(c)의 내용이 아니라 (f)다.
- **담당자**: **사람**(저장소 소유자). 1인 운영이라 에스컬레이션 대상은 없다.
  이 문서의 **모든 `aws` 명령 중 상태를 바꾸는 것은 사람이 실행**한다(AGENTS.md 가드레일 #1 — 에이전트 자율 실행 금지).
  (a)의 조회 명령만 read-only다.
- **적용 대상 로테이션**: 주기는 7일(`AutomaticallyAfterDays: 7`)이다.
  ⚠️ **로테이션은 "시각"이 아니라 "창(window)"이다.** `RotationRules`에 `Duration`이 없으면 회전 창은
  **UTC 자정에 열려 그 UTC 날짜가 끝날 때 닫히고**, Secrets Manager는 그 안의 임의 시점에 회전한다.
  `NextRotationDate`는 예상 실행 시각이 아니라 **창의 마지막 시각(on or before)** 이다
  ([DescribeSecret](https://docs.aws.amazon.com/secretsmanager/latest/apireference/API_DescribeSecret.html) ·
  [Rotation schedules](https://docs.aws.amazon.com/secretsmanager/latest/userguide/rotate-secrets_schedule.html)).
  - **2026-09-04 실측**: `LastRotated=2026-08-30T13:08:48+09:00`, `NextRotationDate=2026-09-07T08:59:59+09:00`
    (= `2026-09-06T23:59:59Z`), `RotationRules={AutomaticallyAfterDays:7}` — `Duration` 없음.
    → **다음 창 = 2026-09-06 09:00 KST ~ 09-07 08:59:59 KST, 약 24시간.**
  - ℹ️ **이 해석이 실측으로 검증됐다**: 08-21에 계산한 창(08-23 09:00~08-24 08:59:59)과 그 다음 창
    (08-30 09:00~08-31 08:59:59) 안에서 실제 회전이 각각 일어났고, 3차는 **08-30 13:08**이었다.
  - 직전 회전이 22:08 KST였다고 해서 이번에도 그 무렵이라는 보장은 **없다.**
  - 창은 매번 (a)의 `NextRotationDate`로 다시 계산한다 — 끝 = `NextRotationDate`, 시작 = 그 UTC 날짜의 `00:00Z`.
- 관련: 알람이 먼저 왔다면 [alarm-response.md §13](alarm-response.md)의 대응 분기 2번이 여기로 보낸다.

---

## 확인 주기 — 24시간 창을 사람이 폴링으로 다 덮을 수는 없다

**한계를 먼저 밝힌다.** 회전 창이 약 24시간이라 5분 간격 감시는 불가능하다.
다운 시간은 **"회전 → 커넥션 만료 → 내가 알아채는 시각"** 으로 결정되고, **폴링 간격이 그 상한을 정한다.**

| 시점 | 할 일 |
| --- | --- |
| 창이 열리기 전날 | (a)로 `NextRotationDate`를 읽어 **창의 시작·끝**을 계산한다 |
| 창이 열린 직후(그 UTC 날짜 `00:00Z` = KST 09:00) | (a) 1회 — 창 시작과 동시에 회전했는지 확인 |
| 창 안, 깨어 있는 동안 | (a)를 **30분~1시간 간격**으로 반복. **이 간격이 곧 최대 다운 시간이다** |
| 창 안, 자는 동안 | 폴링을 유지할 수 없다 → **Slack 알람이 유일한 신호**다((d)의 유보를 함께 읽을 것). ⚠️ **3차는 일요일 주간 13:08에 났고 알람이 +7분에 울렸는데도 4일 22시간 갔다 — "깨어 있는 구간"도 안전하지 않다**((f-1)) |
| `LastRotatedDate` 갱신 감지 | **즉시 (b) 실행** |
| 창이 닫힌 뒤(`NextRotationDate` 경과) | (a) 1회 더 — 창 끝에 회전했을 수 있다 |

**더 나은 길은 창을 좁히는 것이다.** `ScheduleExpression` + `Duration`으로 회전 창을 1시간대로 고정하거나
회전을 사람이 직접 일으키면 이 폴링 자체가 불필요해진다. 다만 둘 다 **인프라·API 변경**이고 공식 회전 명령의
형태는 plan 0009가 확정해야 할 미해결 항목이다.
⛔ **그런데 이 표만으로 버틴 창이 세 번 다 실패했다**(위 표) — **09-06 창은 이 표가 아니라 (f)로 버틴다.**
이 표는 (f)를 끝낸 뒤의 보조 수단이다.

**시간 예산 — "5분 유예"는 없다. 상한이 5분이고 하한은 0이다.**
`SetConnMaxLifetime(5m)`(`app/internal/db/db.go:29`)은 **각 커넥션이 만들어진 시점부터** 재사용을 허용하는
최대 수명이지, 회전 시점부터 새로 시작하는 타이머가 아니다. 만료된 커넥션은 재사용 직전에 지연 폐기된다
([Go `database/sql` 문서](https://pkg.go.dev/database/sql#DB.SetConnMaxLifetime)).

- 회전 순간 이미 4분 59초 된 커넥션은 **다음 요청에서 곧바로** 폐기되고, 앱은 구 비밀번호로 재연결해 실패한다.
- 유휴 커넥션이 모자라 풀이 커지는 순간에도 신규 연결이 **즉시** 실패한다.
- 태스크가 2개(`service_desired_count = 2`)라 풀 만료가 서로 어긋나므로, 처음에는 **부분 실패**(503/200 혼재)로 나타난다.

→ 회전 후 **5분이 지나면** 전 커넥션이 교체돼 전면 503이 되지만, **그 전 어느 시점에든 부분 실패가 시작될 수 있다.**
(b)를 빨리 실행할수록 좋으나 **무중단은 보장되지 않는다.**

---

## (a) 확인 — 로테이션이 실제로 일어났는가 (read-only)

```bash
SECRET_ARN=$(aws rds describe-db-instances --db-instance-identifier linkpulse-prod-pg \
  --query 'DBInstances[0].MasterUserSecret.SecretArn' --output text --region ap-northeast-2)

aws secretsmanager describe-secret --secret-id "$SECRET_ARN" \
  --query '{LastRotated:LastRotatedDate,Next:NextRotationDate}' --region ap-northeast-2

aws rds describe-db-instances --db-instance-identifier linkpulse-prod-pg \
  --query 'DBInstances[0].MasterUserSecret.SecretStatus' --output text --region ap-northeast-2
```

**판정 기준 — 둘 다 만족해야 (b)로 간다:**

- `LastRotatedDate`가 **직전에 기록해 둔 값보다 최신**이다(= 이번 로테이션이 끝났다).
- `SecretStatus` == **`active`**.

`SecretStatus`가 `rotating`이면 **아직 진행 중이다 — 기다린다.** 이 상태에서 재기동하면 교체 중인 값을 잡을 수 있다.

타임스탬프는 **CLI 설정에 따라 로컬 오프셋으로 출력된다**(이 환경에서는 `+09:00`). UTC로 단정하지 말고
반환된 오프셋 그대로 비교한다. 창 계산에 쓰는 `00:00Z`는 `NextRotationDate`를 UTC로 환산한 뒤 그 날짜의 자정이다.

## (b-0) 먼저 관측을 켠다 — **(b)보다 앞이고, 별도 터미널이다**

회전 감지 시점에 이미 부분 실패가 시작됐을 수 있다. (b)의 재배포가 도는 동안 **실패 시작·회복 시각을 기록**하려면
이 루프가 **(b)보다 먼저** 돌고 있어야 한다. (c)의 `aws ecs wait`는 터미널을 점유하므로 같은 창에서는 불가능하다.

```bash
# 별도 터미널. (b) 실행 전에 시작해 (c) 판정이 끝날 때까지 그대로 둔다.
while true; do
  printf '%s ' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  curl -sk -o /dev/null -w 'readyz=%{http_code}\n' --max-time 5 https://lpulse.live/readyz
  sleep 10
done
```

관측 대상은 **`/readyz`뿐이다.** 링크 왕복은 쓰기가 섞여 반복 실행에 적합하지 않으므로 (c)의 최종 검증에서 1회만 한다.

## (b) 실행 — 태스크 강제 재기동 [사람] ⚠️

⚠️ **2026-09-09부터 이 일은 자동화됐다. (b)를 바로 실행하지 말고 (b-1)을 먼저 본다.**
plan 0009 Step 1이 라이브다: `Secret Label Updated`(AWSCURRENT) 이벤트 → EventBridge rule →
Lambda `linkpulse-prod-rotation-redeploy` → `UpdateService(forceNewDeployment)`.
**정상이라면 회전 수 분 안에 재배포가 자동으로 시작된다.**
**(b)는 폐지된 것이 아니라 그 자동화가 실패했을 때의 수동 절차로 남는다.**

### (b-1) 자동 재배포가 돌았는지 먼저 확인한다 [read-only · 30초]

```bash
R=ap-northeast-2
# ① Lambda가 무엇을 했나 — outcome 하나로 판정이 끝난다
aws logs filter-log-events --log-group-name /aws/lambda/linkpulse-prod-rotation-redeploy \
  --region $R --start-time $(( ($(date +%s) - 3600) * 1000 )) --query 'events[].message' --output text

# ② 새 배포가 실제로 생겼나
aws ecs describe-services --cluster linkpulse-prod-cluster --services linkpulse-prod-app --region $R \
  --query 'services[0].deployments[].{Status:status,Rollout:rolloutState,Created:createdAt}' --output table
```

| ① `outcome` | 뜻 | 할 일 |
| --- | --- | --- |
| `redeploy_submitted` | 재배포를 **제출했다** | ②의 `rolloutState`를 본다. `COMPLETED`면 **(c)로 가서 확인만** 한다. `FAILED`거나 정체면 **(b) 수동 실행** |
| `redeploy_submitted_raced_marker` | 위와 같다(동시 중복에서 marker 경합에 졌을 뿐) | 위와 같다 |
| `skipped_duplicate` | 같은 `versionId`를 이미 처리했다 — **재배포를 부르지 않았다** | 앞선 호출이 만든 배포가 ②에 있어야 한다. **없으면 (b) 수동 실행** |
| `failed_*` | 실패 지점이 이름에 있다(`failed_label_wait`/`failed_update_service`/`failed_putitem`/`failed_deadline`/`failed_getitem`/`failed_validation`) | **(b) 수동 실행** 후 (b-2)로 원인을 남긴다 |
| **로그가 아예 없다** | 이벤트가 Lambda에 도달하지 못했다 | **(b) 수동 실행** 후 (b-2) 계층 1을 본다 |

⚠️ **`redeploy_submitted`는 "제출됨"이지 "복구됨"이 아니다.** 롤아웃이 실패해 서킷브레이커가 롤백하면
marker는 이미 남아 있어 **같은 회전에 대한 자동 재시도는 없다.** 그때 복구 주체는 사람이고 (b)가 그 절차다.

### (b-2) 자동화가 실패했다면 — 어느 계층에서 끊겼는지 남긴다 [read-only]

**복구((b))를 먼저 하고 이걸 한다.** 순서를 바꾸지 않는다.

| 증상 | 계층 | 확인 |
| --- | --- | --- |
| Lambda 로그 없음 | **계층 1** — EventBridge가 Lambda에 전달하지 못했다 | `linkpulse-prod-rotation-events-dlq` 적재 · `rotation-rule-failed` 알람 |
| Lambda 로그에 `failed_*` | **계층 2** — handler가 실행됐으나 실패 | `linkpulse-prod-rotation-redeploy-failures` 적재 · `rotation-redeploy-errors` 알람 |
| 양쪽 큐 다 비었는데 재배포도 없음 | **전달 자체가 실패** — depth로는 안 보인다 | `rotation-rule-dlq-send-failed` · `rotation-redeploy-destination-failed` 알람 |
| rule이 이벤트를 아예 못 잡음 | **패턴 미매칭**(무증상) | 광역 관찰 로그 `/aws/events/linkpulse-prod-secret-events`에 **원문**이 있는지 |

```bash
R=ap-northeast-2
for q in rotation-events-dlq rotation-redeploy-failures; do
  U=$(aws sqs get-queue-url --queue-name linkpulse-prod-$q --region $R --query QueueUrl --output text)
  echo "$q = $(aws sqs get-queue-attributes --queue-url "$U" --attribute-names ApproximateNumberOfMessages \
    --region $R --query 'Attributes.ApproximateNumberOfMessages' --output text)"
done
# 광역 관찰 rule의 실이벤트 원문 (rule이 패턴을 못 맞췄는지 여기서 갈린다)
aws logs filter-log-events --log-group-name /aws/events/linkpulse-prod-secret-events \
  --region $R --start-time $(( ($(date +%s) - 3600) * 1000 )) --query 'events[].message' --output text
```

⚠️ **DLQ depth 알람은 최대 15분 늦게 뜬다**(SQS는 비활성 큐가 활성화될 때 지표 전송이 지연된다).
빠른 신호는 `rotation-rule-failed`·`rotation-redeploy-errors`·`*-destination-failed`(1분 지표)다.
***"DLQ가 조용하니 괜찮다"로 판단하지 않는다.***

---

**아래가 (b) 수동 절차다 — (b-1)에서 필요하다고 판정됐을 때만 실행한다.**

```bash
aws ecs update-service --cluster linkpulse-prod-cluster --service linkpulse-prod-app \
  --force-new-deployment --region ap-northeast-2
```

새 태스크가 기동하면서 태스크 정의의 `secrets`가 **로테이션된 최신 비밀번호**를 다시 주입한다.

**실측(2026-09-04, 3차 복구)**: `update-service` **11:26:01** → `canary_down` OK **11:31:42**
(**5분 41초**) → `alb-target-5xx` OK **11:45:37**. **이 절차 자체는 빠르다 — 6분이면 끝난다.**
다운 시간을 정하는 것은 (b)의 소요가 아니라 **(b)를 시작하기까지 걸린 시간**이다 —
3차는 그것이 **4일 22시간 17분**이었다(알람은 회전 +7분에 이미 울리고 있었다).

## (c) 확인 — 복구됐는가

```bash
aws ecs wait services-stable --cluster linkpulse-prod-cluster --services linkpulse-prod-app --region ap-northeast-2

# readiness(= DB 핑 포함)가 200이어야 한다. healthz 200만으로는 판정하지 않는다.
curl -si https://lpulse.live/readyz | head -1

# 실제 왕복: 링크 생성 → 리다이렉트 (쓰기·읽기가 모두 DB를 탄다)
CODE=$(curl -s -X POST https://lpulse.live/api/links -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com"}' | python3 -c 'import sys,json; print(json.load(sys.stdin)["code"])')
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' "https://lpulse.live/${CODE}"
```

(b-0)의 관측 루프는 **여기까지 계속 돌려 둔다** — 실패 시작 시각과 회복 시각이 회고·MTTD의 근거가 된다.
판정이 끝나면 멈추고 출력을 원장에 남긴다.

**판정 기준 — 셋 다 만족해야 종료한다:**

- `services-stable` 반환(= 새 배포 안정화 완료).
- `/readyz` **200**. (`/healthz` 200은 DB를 보지 않으므로 근거가 되지 않는다 — 2026-08-16에 66시간을 놓친 신호가 그것이다.)
- 링크 생성 → 리다이렉트가 **`302 https://example.com/`**(`http.StatusFound`, `links.go`).

실패하면 [alarm-response.md §13](alarm-response.md) 대응 분기 2번(DB 연결 장애)으로 간다.

### ℹ️ Step 2 배포 후에는 이 절차 자체가 거의 필요 없어진다 — 2-4 실측

plan 0009 Step 2(앱이 연결마다 현재 비밀번호를 읽는다)가 배포되면, 위 (b)의 **사람 개입도
Step 1의 재배포도 없이** 앱이 스스로 회복한다. 아래는 **로컬 통합 테스트 실측**이다
(2026-10-06, 실제 Postgres에 `ALTER ROLE`로 비밀번호 변경, `go test -tags=integration ./internal/db/`).

| 시나리오 | 기준 시각 | 실측 | 합격선 |
| --- | --- | --- | --- |
| 새 값이 즉시 보임 | `T_set`(비밀번호 변경) | **102ms** | ≤ 15초 |
| 구값 창 20초(회전 중 창 재현) | `T_avail`(새 값 조회 가능) | **2.36초** | ≤ 30초 |
| 조회 3회 연속 실패 후 성공 | 마지막 실패 | **432ms** | ≤ 15초 |
| 구값 창 20초 동안 재조회 횟수 | — | **13회** | 폭주 아님(backoff 정상) |

**무트래픽(`/readyz`만 도는) 경로**: 조회에 2초 지연을 주면 `503`(1초 예산에서 끊김) → 다음 호출
`200`, **회복 2.08초 · 503 1회**. 조회가 즉답하면 **단일 호출 643ms 안에서** 회복이 끝난다.

⚠️ **이 숫자를 프로덕션 값으로 읽지 않는다.** 전부 **앱 쪽 회복 시간**이고, 실제 창에는
**`T_set`→`T_label`(회전 lambda 내부)과 `T_label`→`T_avail`(전파)** 가 앞에 붙는다. 이 plan은 그 두
구간에 상한을 줄 수 없다. 503 건수도 주입한 조회 지연에 비례하므로 프로덕션 값이 아니다 —
**실제 값은 2-6 온디맨드 회전에서만 나온다.**

→ **그때까지는 이 런북이 그대로 유효하다.** Step 2가 라이브가 된 뒤 2-6 실측으로 이 절을 갱신한다.

## (d) 실행하지 못했다면 — 실제로 무슨 일이 일어나나

**알람은 탐지만 준다. 복구를 보장하지 않는다.**
plan 0008이 넣은 것은 canary가 `/readyz`를 프로빙하게 만든 **탐지 경로**뿐이다.
알람이 와도 **복구는 사람이 (b)를 수동 실행**해야 한다. 브릿지를 놓치고 알람에도 대응하지 않으면 다운은 계속된다.

⚠️ 더구나 **이 탐지 경로가 실제 DB 장애에서 울린다는 실측은 아직 없다**(plan 0010이 담당).
그래서 "6분 안에 알람이 오니까 괜찮다"에 기대지 말고 **브릿지를 반드시 수행한다.**

⛔ **2026-08-30에 이 문단이 그대로 실현됐다.** *"브릿지를 놓치고 알람에도 대응하지 않으면 다운은 계속된다"* —
**4일 22시간 계속됐다.** 이 런북은 무슨 일이 일어날지 정확히 알고 있었고, 그것을 막지 못했다.

⚠️ **"야간 구간이 실질적 구멍"이라는 아래 진단은 3차로 반증됐다.** 3차 회전은 **13:08 주간**이었다.
구멍은 야간이 아니라 **알람과 사람 사이**에 있다 — (f)가 그것을 다룬다.

**야간 구간도 여전히 구멍이다.** 창이 24시간이라 자는 동안은 폴링이 끊긴다. 이 구멍은 회전 창을 좁히거나
(plan 0009) 앱이 회전을 스스로 견디게 만들기 전에는 닫히지 않는다 —
**이 런북으로 다운 시간을 0으로 만들 수는 없고, 상한을 줄일 뿐이다.**

## (e) 유효 범위 — 이 브릿지가 덮는 회전 창은 한 번뿐이다

로테이션 주기가 7일이므로 이 절차가 덮는 것은 **회전 창 한 번**이다.

**2026-09-09 실측**(1-6 드릴 직후): `LastRotated=2026-09-09T15:39:56+09:00`,
`NextRotationDate=2026-09-17T08:59:59+09:00` → **다음 창 = 2026-09-16 09:00 ~ 09-17 08:59:59 KST.**
(드릴이 회전을 일으켜 09-14 창은 사라졌다 — `AutomaticallyAfterDays: 7`은 "마지막 회전 + 7일"이다.)

✅ **2026-09-09에 plan 0009 Step 1이 라이브가 됐고 온디맨드 드릴로 종단 검증됐다.**
**자동 재배포가 1차이고, 이 런북은 그것이 실패했을 때의 백스톱**이다(판정은 (b-1)).

**드릴 실측 (2026-09-09, 사람 개입 0):**

| 구간 | 시각(UTC) | 회전 이후 |
| --- | --- | --- |
| 회전 명령 | 06:38:46 | — |
| 첫 `readyz` 503 | 06:39:07 | +21초 |
| `Secret Label Updated` 이벤트 | 06:39:56 | +1분 10초 |
| Lambda `redeploy_submitted` | 06:40:01 | +1분 15초 |
| 롤아웃 `COMPLETED` · `readyz` 200 고정 | 06:42:56 | **+4분 10초** |

**컷오프 10분 대비 절반 이하다.** 같은 장애가 사람 손으로는 13~118시간이었다.
`canary_down`은 발화하지 않았다 — 다운이 3분 연속 조건에 못 미쳤다(설계대로다).
`alb-target-5xx`는 발화했고, **Step 2가 라이브가 아닌 동안 이것은 정상 신호다**
(회전 직후 5xx는 재배포가 끝날 때까지 지속된다 — `alarm-response.md` §13 분기 2의 시점 가드).

---

## (f) ⛔ 창을 맞기 전에 — 이 런북이 실제로 실행되게 만든다

**세 번의 실패는 절차를 몰라서가 아니다.** (a)~(c)는 세 번 다 문서에 있었고 세 번 다 실행되지 않았다.
아래 셋을 **창이 열리기 전에** 끝낸다.

### f-1. 알람이 사람에게 실제로 닿았는지 확인한다 [사람]

✅ **3차에 대해서는 답이 완전히 나왔다(2026-09-04 실측). 원인은 (B)다 — 조치는 f-2·f-2b.**

| 단계 | 결과 |
| --- | --- |
| 지표·임계·평가 | ✅ `alb-target-5xx` **13:15:37** · `canary_down` **13:17:04** ALARM 진입(회전 +7~8분) |
| ALARM 유지 | ✅ 전이가 각 알람 정확히 2회 — **118시간 내내 ALARM** |
| SNS → Chatbot → Slack | ✅ **ALARM 카드가 채널에 실제로 도착**(08-30 **13:15**·**13:17**, 카드 원문 보존) |
| **Slack → 사람** | ⛔ **여기서 끊겼다. 4일 22시간 동안 아무도 보지 않았다** |

**앞의 세 단계가 전부 정상이었다.** 실패 지점은 **한 구간**이고, 그 구간에는 지금까지 어떤 장치도 없었다.
⚠️ **발생 시각이 일요일 오후 1시 15분**이라 자리에 없었을 가능성이 크다 — 그렇다면
**모바일 푸시가 유일한 경로였고 그것이 작동하지 않았다.**

**다음 회전에도 같은 판정을 반복한다** — 아래 명령이 그 도구다. 둘 중 하나로 갈린다:

- **(A) ALARM 카드가 Slack에 오지 않았다** → **기술적 결함이고 지금 고칠 수 있다.**
- **(B) 왔는데 사람이 못 봤거나 넘겼다** → **알림 도달 경로의 문제다.**

```bash
# 두 알람의 상태 전이 이력 — ALARM 진입 시각이 찍힌다
aws cloudwatch describe-alarm-history --alarm-name linkpulse-prod-canary-down \
  --history-item-type StateUpdate --start-date 2026-08-30T00:00:00Z --region us-east-1 \
  --query 'AlarmHistoryItems[].{T:Timestamp,S:HistorySummary}' --output table

aws cloudwatch describe-alarm-history --alarm-name linkpulse-prod-alb-target-5xx \
  --history-item-type StateUpdate --start-date 2026-08-30T00:00:00Z --region ap-northeast-2 \
  --query 'AlarmHistoryItems[].{T:Timestamp,S:HistorySummary}' --output table
```

그리고 **Slack 채널을 회전 시각 ±20분 구간으로 스크롤해** ALARM 카드가 있는지 눈으로 확인한다.
✅ **3차는 이 확인까지 끝났다** — 카드 원문을 `evidence/rotation-outage-2026-08-30/alarm-history.txt`에 보존했다.

| 결과 | 원인 | 조치 |
| --- | --- | --- |
| ALARM 전이 **있음** + Slack 카드 **있음** | (B) 알림이 폰까지 안 갔다 | **f-2** |
| ALARM 전이 **있음** + Slack 카드 **없음** | (A) SNS→Chatbot 전달 실패 | 구독·Chatbot 설정 점검. **OK 카드는 오늘 왔으므로 경로 자체는 산다** → 조건·필터·전달 실패를 본다 |
| ALARM 전이 **없음** | (A) 알람이 아예 안 울렸다 | `ActionsEnabled`·임계·`treat_missing_data` 점검. **plan 0008의 탐지 설계가 반증된다** |

### f-2. Slack 알림이 폰까지 가는지 실증한다 [사람]

**"Slack에 카드가 있다"와 "내가 안다"는 다르다.** 3차에서 갈린 것이 여기일 가능성이 높다.

- 알람 채널의 알림 설정을 **"모든 새 메시지"**로 (기본값은 멘션만인 경우가 많다).
- 모바일 Slack 앱에서 그 채널을 **알림 예외**로 지정 — 방해 금지 시간에도 울리게.
- **실증한다**: 채널에 테스트 메시지를 넣고 **폰이 실제로 울리는지 눈으로 본다.**
  울리지 않으면 위 설정 중 하나가 안 걸린 것이다.

⚠️ **이 단계를 "설정했다"로 끝내지 않는다.** 세 번 연속 실패한 것이 정확히 *"됐을 것이다"*라는 가정이다.

### f-2b. Slack에 의존하지 않는 두 번째 경로를 만든다 [사람] ⚠️ 변경형(Terraform)

**f-2는 Slack 앱 설정에 의존한다** — 앱 업데이트·재설치·OS 알림 권한 변경으로 **조용히 되돌아갈 수 있고,
되돌아간 것을 알 방법이 없다.** 3차의 실패 지점이 정확히 그 한 구간이므로 **경로를 하나 더 둔다.**

✅ **apply 완료(2026-09-04)** — `aws_sns_topic_subscription.alarms_sms`(monitoring.tf) ·
`.canary_use1_sms`(synthetic-canary.tf). `Plan: 2 to add, 0 to change, 0 to destroy`로 적용됐고
`endpoint`는 plan 출력에서 `(sensitive value)`로 마스킹됐다.
`var.alarm_sms_number`가 비면 `count = 0`이라 **리소스를 만들지 않는다** — 번호를 넣어야 켜진다.
⚠️ **번호를 `terraform.tfvars`에 `>>`로 덧붙이지 않는다** — 파일 끝에 개행이 없으면 앞 줄에
그대로 이어붙는다(2026-09-04에 실제로 겪었다). 에디터로 직접 추가한다.

**[사람]이 할 일 — 순서를 지킨다:**

⛔ **1. SMS 샌드박스가 먼저다.** 이 계정은 **두 리전 모두 샌드박스 상태로 확인됐다**(2026-09-04 실측,
`IsInSandbox: true`). 샌드박스에서는 **검증된 번호에만** 발송되므로, 이 단계를 건너뛰면
**구독은 만들어지고 문자는 조용히 안 온다** — 이 회고가 다룬 것과 정확히 같은 실패 형태다.
**샌드박스 상태·검증 목록은 리전별이라 같은 번호도 두 번 등록·검증한다.**

```bash
PHONE="+8210XXXXXXXX"   # E.164, 하이픈 없음

aws sns create-sms-sandbox-phone-number --phone-number "$PHONE" --region ap-northeast-2
aws sns verify-sms-sandbox-phone-number --phone-number "$PHONE" --one-time-password <OTP> --region ap-northeast-2

aws sns create-sms-sandbox-phone-number --phone-number "$PHONE" --region us-east-1
aws sns verify-sms-sandbox-phone-number --phone-number "$PHONE" --one-time-password <OTP> --region us-east-1

aws sns list-sms-sandbox-phone-numbers --region ap-northeast-2   # Verified 확인
aws sns list-sms-sandbox-phone-numbers --region us-east-1
```

✅ **한국(+82) 발송 확인됨(2026-09-04 실측)** — `create-sms-sandbox-phone-number`(ap-northeast-2)의
OTP가 *"국외발신"* 표기로 실제 도착했다. **SMS 경로가 이 계정·이 번호에서 작동한다.**
(도착하지 않았다면 SMS 경로를 접고 f-4(창 이동)로 갔어야 한다.)
⚠️ **OTP는 리전별로 따로 발급된다** — `us-east-1` 등록 시 **새 OTP가 다시 온다.**
`us-east-1`을 빠뜨리면 **`canary_down` 문자가 오지 않는다** — 그쪽이 *"서비스가 죽었다"*의 최상위 신호다.
ℹ️ **샌드박스를 탈출할 필요는 없다.** 탈출은 지원 케이스가 필요하고, 1인 운영에 번호 1개면
검증된 번호로 충분하다. 샌드박스에서도 **검증된 번호는 토픽 구독으로 정상 수신**한다.

**2.** `infra/prod/terraform.tfvars`에 `alarm_sms_number = "+8210XXXXXXXX"`.
**이 파일은 `.gitignore`에 있어 번호가 커밋되지 않는다.**

**3.** `terraform plan` → **신설 2건(SMS 구독)만** 뜨는지 확인 → **[사람] apply.**
⚠️ 번호가 비어 있으면 `count = 0`이라 **plan에 "No changes"가 뜬다** — 그것은 정상이고,
**2번을 안 했다는 뜻**이다.

**4.** ⛔ **실증한다 — 다음 알람을 기다리지 않는다.** 두 토픽에 직접 publish해 **두 통 다 오는지 눈으로 본다.**
하나만 오면 그 리전 구독이 안 붙은 것이다. f-2와 같은 규율이다: *"설정했다"*로 끝내지 않는다.

```bash
aws sns publish --region ap-northeast-2 \
  --topic-arn <sns_alarms_topic_arn> \
  --message "linkpulse SMS path test - ap-northeast-2 alarms topic"

aws sns publish --region us-east-1 \
  --topic-arn <sns_canary_topic_arn> \
  --message "linkpulse SMS path test - us-east-1 canary topic"
```

- 토픽 ARN은 `terraform output sns_alarms_topic_arn` · `sns_canary_topic_arn`.
- **메시지는 ASCII로 쓴다** — 한글은 SMS가 UCS-2로 인코딩돼 70자에서 잘리고 요금 단위도 달라진다.
- `alarms` 토픽 쪽은 **Slack에도 함께 간다**(Chatbot이 CloudWatch 알람 형식을 기대하므로
  카드로 안 뜨거나 무시될 수 있다 — 무해하다).

⚠️ **감수 사항**: 번호가 `tfstate`에 평문으로 남는다(S3 백엔드). 변수는 `sensitive = true`라
CLI 출력에는 안 뜨지만 state는 별개다. 개인 번호이므로 판단하고 쓴다.

**문자 폭주 위험은 실측으로 낮다** — 3차는 전이가 알람당 2회뿐이었고, 2차의 19회 왕복은
`/readyz` canary 전환으로 이미 사라졌다. 시끄러우면 `alarms_sms`만 지우고 canary 쪽만 남긴다.

ℹ️ **이것은 알림을 늘리는 것이 아니라 단일 장애점을 없애는 것이다.** Slack 카드는 세 번 다 도착했다 —
문제는 그 하나가 전부였다는 것이다.

### ⚠️ f-2c. 알림 경로를 테스트하기 전에 **현재 알람 상태부터 본다**

**CloudWatch는 상태 "전이"에만 알림을 보낸다.** 이미 `ALARM`인 알람을 `set-alarm-state`로 다시
`ALARM`으로 밀면 **아무 일도 일어나지 않는다** — 알림 경로가 멀쩡해도 조용하다.

```bash
aws cloudwatch describe-alarms --alarm-names linkpulse-prod-canary-down --region us-east-1 \
  --query 'MetricAlarms[].{State:StateValue,Updated:StateUpdatedTimestamp,Actions:ActionsEnabled}'
aws cloudwatch describe-alarms --alarm-names linkpulse-prod-alb-target-5xx --region ap-northeast-2 \
  --query 'MetricAlarms[].{State:StateValue,Updated:StateUpdatedTimestamp,Actions:ActionsEnabled}'
```

ℹ️ **2026-09-07에 이 침묵이 4차 장애를 발견하게 했다** — 테스트가 조용해서 상태를 봤더니
두 알람 다 전날 저녁부터 `ALARM`이었다. **테스트가 조용하면 경로를 의심하기 전에 상태를 먼저 본다.**

### f-3. 창 당일 능동 확인을 건다 [사람]

알람이 유일한 신호가 되지 않게 한다. 09-06 창(09:00~다음날 09:00)에 대해:

- **09-06 09:00 · 12:00 · 18:00 · 21:00**에 폰 알림을 미리 걸고, 그때마다 아래 한 줄을 실행한다.

```bash
curl -s -o /dev/null -w 'readyz=%{http_code}\n' --max-time 5 https://lpulse.live/readyz
```

- **`200`이 아니면 즉시 (b)**로 간다. `(a)`의 `LastRotatedDate` 확인은 그 다음이다 —
  **회복이 진단보다 먼저다**(3차의 교훈: 진단을 기다리다 5일이 갔다).
- 자는 동안은 여전히 f-1·f-2에 의존한다.
⛔ **4차(09-06)에서 이 단계를 실행하지 않았고, 그것이 13시간의 직접 원인이다.**
문자는 19:14에 도착했다 — **21:00 알림이 있었다면 2시간 안에 끝났다.**
⚠️ **이 항목은 매 창마다 사람이 수동으로 걸어야 해서 잊으면 그냥 사라진다.**
f-2b(SMS)는 인프라라 한 번 만들면 남지만 이건 아니다 → **캘린더 반복 일정으로 등록한다.**

### f-4. [선택] 창 자체를 사람이 지켜보는 시각으로 옮긴다 [사람] ⚠️ 변경형

**회전을 지금 직접 일으키면 09-06 창이 사라지고 다음 창이 약 7일 뒤로 밀린다.**

```bash
aws rds modify-db-instance --db-instance-identifier linkpulse-prod-pg \
  --rotate-master-user-password --apply-immediately --region ap-northeast-2
```

✅ **2026-09-07 10:06에 처음 수행했다. 실측:**

| | 값 |
| --- | --- |
| 회전 | 10:06:07 (Step 1 rule은 **켠 채**) |
| `canary_down` ALARM → OK | 10:14:41 → 10:18:41 |
| **통제된 다운** | **약 12분 34초** |
| 알림 | ✅ Slack·SMS 모두 ALARM·OK 양방향 도착 |
| 창 이동 | 09-13 09:00 → **09-14 09:00** |

⛔ **정정 두 가지 — 이 항목을 처음 쓸 때 틀렸던 값이다:**
1. **"다음 창이 +7일 밀린다"는 틀렸다.** 스케줄은 **"마지막 회전 + 7일"**이라
   **직전 회전으로부터 지난 기간만큼만** 번다. 09-07 이동은 직전 회전(09-06 19:09)의 **다음 날**이라
   **+1일**만 벌었다. → **장애 직후에 쓰면 효과가 가장 작다. 창 직전에 쓸수록 크다.**
2. **"약 5분"이 아니라 약 12분 34초다.** 5분은 *"재배포 → 알람 OK"*만 센 값이고,
   **회전 명령 → `SecretStatus=active`까지 기다리는 시간**이 빠져 있었다.

| | 대가 | 얻는 것 |
| --- | --- | --- |
| **수행** | **통제된 다운 약 12분**(사람이 지켜보는 중, (b-0) 관측 루프를 켜고) | **가장 가까운 무방비 창이 사라진다.** 다음 창은 **이 회전 + 7일** |
| **미수행** | 그 창을 f-1~f-3만으로 맞는다 | Step 1 기한이 유지된다 |

⚠️ **plan 0009의 1-6 드릴은 *"Step 1이 라이브여야 한다"*를 선행 조건으로 둔다**
(*"Step 1 없이 돌리면 장애를 하나 더 만드는 것"*). **f-4는 그 드릴이 아니다** — 목적이 Step 1 검증이 아니라
**무방비 창을 사람이 지켜보는 시각으로 옮기는 것**이고, 대가가 **약 6분 vs 3차의 118시간**이다.
다만 **수행하면 Step 1 기한이 09-13 → 약 09-10으로 당겨진다.** 그 대가를 받아들일 때만 한다.
