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
- **적용 대상 로테이션**: 로테이션 주기는 7일(`AutomaticallyAfterDays: 7`)이다.
  **다음 예정: 2026-08-23 22:08경 KST**(직전 `LastRotated` = 2026-08-16 22:08:46 KST 기준 추정).
  실제 시각은 (a)의 `NextRotationDate`로 확인한다.
- 관련: 알람이 먼저 왔다면 [alarm-response.md §13](alarm-response.md)의 대응 분기 2번이 여기로 보낸다.

---

## 확인 주기

| 시점 | 할 일 |
| --- | --- |
| 로테이션 예정일 하루 전 | (a)로 `NextRotationDate`를 읽어 **실제 예정 시각**을 확정한다(아래 22:08은 추정치다) |
| 예정 시각 **15분 전**부터 | (a)를 **5분 간격**으로 반복 — `LastRotatedDate` 갱신을 기다린다 |
| `LastRotatedDate` 갱신 감지 | **즉시 (b) 실행.** 지체할수록 다운 시간이 늘어난다(아래 "시간 예산") |
| 예정 시각 **+1시간**까지 미갱신 | 로테이션이 밀린 것이다. `NextRotationDate`를 다시 읽고 재예약 |

**시간 예산 — 5분 안에 (b)를 실행하면 다운이 0이다.**
`sql.DB.PingContext`는 풀의 기존 커넥션을 재사용하고 Postgres는 비밀번호 변경으로 기존 세션을 끊지 않으므로,
로테이션 후 `ConnMaxLifetime`(5분, `app/internal/db/db.go`) 동안은 앱이 정상 동작한다.
그 5분이 지나면 커넥션이 교체되며 **전면 503**이 시작된다. 늦게 실행해도 복구는 되지만 그 사이는 다운이다.

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
타임스탬프는 **UTC**로 나온다(22:08 KST = 13:08 UTC).

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

## (e) 유효 범위 — 이 브릿지가 막는 것은 한 번뿐이다

로테이션 주기가 7일이므로 이 절차는 **2026-08-23 한 번**을 막는다. **다음 로테이션은 2026-08-30**이다.
plan 0009(근본 대응)가 08-30 전에 닫히지 않으면 사람이 **매주** 이 절차를 반복해야 한다
→ **plan 0009의 실질 마감은 2026-08-30**이다.
