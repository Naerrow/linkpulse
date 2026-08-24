# Runbook — RDS 비밀번호 로테이션 마감 브릿지 (사람 수동)

RDS 마스터 비밀번호가 자동 로테이션되면 **로테이션 직후에 사람이 ECS 태스크를 강제 재기동해** 새 비밀번호를
주입하는 임시 절차다. 앱이 `DB_PASSWORD`를 **컨테이너 기동 시 1회만** 주입받는 구조(`infra/prod/ecs.tf`의 `secrets`)라,
로테이션 후 커넥션 풀이 만료되면 신규 연결이 전부 인증 실패하기 때문이다.

- **왜 임시인가**: 이 절차는 **근본 대응이 아니라 브릿지**다. 근본 대응(앱이 로테이션을 스스로 견디게 하는 것)은
  **plan 0009**가 맡는다. 탐지 경로 보강은 [plan 0008](../plans/0008-p4d2-secrets-rotation-and-db-detection/plan.md),
  탐지 실증 드릴은 plan 0010.
- **왜 필요한가**: 2026-08-16 로테이션에서 이 절차가 없어 **2일 18시간 31분** 동안 서비스가 죽었다
  (`docs/postmortems/evidence/db-password-rotation-2026-08-19/`).
- **담당자**: **사람**(저장소 소유자). 1인 운영이라 에스컬레이션 대상은 없다.
  이 문서의 **모든 `aws` 명령 중 상태를 바꾸는 것은 사람이 실행**한다(AGENTS.md 가드레일 #1 — 에이전트 자율 실행 금지).
  (a)의 조회 명령만 read-only다.
- **적용 대상 로테이션**: 주기는 7일(`AutomaticallyAfterDays: 7`)이다.
  ⚠️ **로테이션은 "시각"이 아니라 "창(window)"이다.** `RotationRules`에 `Duration`이 없으면 회전 창은
  **UTC 자정에 열려 그 UTC 날짜가 끝날 때 닫히고**, Secrets Manager는 그 안의 임의 시점에 회전한다.
  `NextRotationDate`는 예상 실행 시각이 아니라 **창의 마지막 시각(on or before)** 이다
  ([DescribeSecret](https://docs.aws.amazon.com/secretsmanager/latest/apireference/API_DescribeSecret.html) ·
  [Rotation schedules](https://docs.aws.amazon.com/secretsmanager/latest/userguide/rotate-secrets_schedule.html)).
  - **2026-08-21 실측**: `LastRotated=2026-08-16T22:08:46+09:00`, `NextRotationDate=2026-08-24T08:59:59+09:00`
    (= `2026-08-23T23:59:59Z`), `RotationRules={AutomaticallyAfterDays:7}` — `Duration` 없음.
    → **다음 창 = 2026-08-23 09:00 KST ~ 2026-08-24 08:59:59 KST, 약 24시간.**
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
| 창 안, 자는 동안 | 폴링을 유지할 수 없다 → **Slack 알람이 유일한 신호**다((d)의 유보를 함께 읽을 것) |
| `LastRotatedDate` 갱신 감지 | **즉시 (b) 실행** |
| 창이 닫힌 뒤(`NextRotationDate` 경과) | (a) 1회 더 — 창 끝에 회전했을 수 있다 |

**더 나은 길은 창을 좁히는 것이다.** `ScheduleExpression` + `Duration`으로 회전 창을 1시간대로 고정하거나
회전을 사람이 직접 일으키면 이 폴링 자체가 불필요해진다. 다만 둘 다 **인프라·API 변경**이고 공식 회전 명령의
형태는 plan 0009가 확정해야 할 미해결 항목이라, **08-23 창은 이 표로 버틴다.**

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

```bash
aws ecs update-service --cluster linkpulse-prod-cluster --service linkpulse-prod-app \
  --force-new-deployment --region ap-northeast-2
```

새 태스크가 기동하면서 태스크 정의의 `secrets`가 **로테이션된 최신 비밀번호**를 다시 주입한다.

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

## (d) 실행하지 못했다면 — 실제로 무슨 일이 일어나나

**알람은 탐지만 준다. 복구를 보장하지 않는다.**
plan 0008이 넣은 것은 canary가 `/readyz`를 프로빙하게 만든 **탐지 경로**뿐이다.
알람이 와도 **복구는 사람이 (b)를 수동 실행**해야 한다. 브릿지를 놓치고 알람에도 대응하지 않으면 다운은 계속된다.

⚠️ 더구나 **이 탐지 경로가 실제 DB 장애에서 울린다는 실측은 아직 없다**(plan 0010이 담당).
그래서 "6분 안에 알람이 오니까 괜찮다"에 기대지 말고 **브릿지를 반드시 수행한다.**

**야간 구간이 이 브릿지의 실질적 구멍이다.** 창이 24시간이라 자는 동안은 폴링이 끊기고, 그때 기댈 것은
아직 실증되지 않은 알람뿐이다. 이 구멍은 회전 창을 좁히거나(plan 0009) 앱이 회전을 스스로 견디게
만들기 전에는 닫히지 않는다 — **이 런북으로 다운 시간을 0으로 만들 수는 없고, 상한을 줄일 뿐이다.**

## (e) 유효 범위 — 이 브릿지가 덮는 회전 창은 한 번뿐이다

로테이션 주기가 7일이므로 이 절차가 덮는 것은 **회전 창 한 번**이다 —
이번 창은 **2026-08-23 09:00 ~ 08-24 08:59:59 KST**(두 날짜에 걸친다), **다음 창은 그로부터 7일 뒤**다.
plan 0009(근본 대응)가 다음 창 전에 닫히지 않으면 사람이 **매주** 이 절차를 반복해야 한다
→ **plan 0009의 실질 마감은 2026-08-30 창이 열리기 전**이다.
