# ADR 0006 — DB 비밀번호를 런타임에 다시 읽는다 (회전 저항성)

- 상태: 채택 (2026-10-06, plan 0009 Step 2). **라이브 전** — 2-5 배포·2-6 실전 검증이 남아 있다.
- 관련: AGENTS.md 가드레일 #2(비밀값은 Secrets Manager/환경변수로만), Phase P4(d)-2,
  [plan 0009](../plans/0009-p4d2-rotation-resilience/plan.md),
  회고 [2026-08-16](../postmortems/2026-08-16-db-password-rotation-outage.md) ·
  [08-23](../postmortems/2026-08-23-rds-rotation-outage.md) ·
  [08-30](../postmortems/2026-08-30-rds-rotation-outage.md) ·
  [09-06](../postmortems/2026-09-06-rds-rotation-outage.md),
  배포 경계는 [ADR 0001](0001-cicd-terraform-ci-boundary.md).

## 맥락

RDS 관리형 회전이 마스터 비밀번호를 바꾸면 **신규 커넥션이 전부 SQLSTATE 28P01로 죽는다.**
앱이 기동 시 주입된 비밀번호를 DSN 문자열에 박아 프로세스 수명 내내 재사용했기 때문이다.
기존 커넥션은 살아 있어 한동안 멀쩡해 보이고, 풀이 신규 연결을 열기 시작하면 그때부터 무너진다.

**4회 재발했다: 66시간 · 24시간 · 118시간 · 13시간.** 복구 동작 자체는 네 번 다 2~6분이었다 —
다운 시간을 정한 것은 **사람이 시작하기까지**였다. 4차에는 SMS까지 도착했는데도 13시간이 걸렸다.
알림을 늘리는 방향으로는 닫히지 않는다는 것이 네 번으로 확인됐다.

Step 1(회전 완료 → EventBridge → Lambda → `UpdateService`)이 사람 개입을 없애 다운을 **수 분**으로
줄였다. 이 ADR은 재기동 자체를 불필요하게 만드는 쪽이다.

## 결정

### 1. 인증 실패 관찰은 pgx 훅이 아니라 `driver.Connector` 래퍼에서 한다

pgx의 `BeforeConnect` 훅으로는 구현할 수 없다. `pgx/v5/stdlib/sql.go:266-273`을 직접 확인했다:

```go
if err = c.BeforeConnect(ctx, &connConfig); err != nil { return nil, err }
if conn, err = pgx.ConnectConfig(ctx, &connConfig); err != nil { return nil, err }
```

훅은 연결 **전에** `ConnConfig` 사본을 고칠 뿐이고, 인증 실패는 바로 다음 줄이 반환한다.
그래서 `stdlib.GetConnector`로 얻은 connector를 감싸 `Connect`의 **반환 오류**를 본다.

- **`28P01`일 때만** 무효화한다. 네트워크·타임아웃까지 재조회 대상으로 삼으면 회전과 무관한 장애에서
  Secrets Manager를 두드린다.
- 분류는 `errors.As`로 한다 — pgx가 실제로 주는 오류는 `ConnectError` → `errors.Join`(다중 호스트)
  → `"server error: %w"`의 **3중 래핑**이다.

### 2. 재조회는 호출자 context와 **분리된** context에서 돈다 (detached)

`/readyz`의 점검 예산은 1초다([ADR 0004](0004-notraffic-canary.md) §1-a). refresh를 호출자
context에 묶으면 Secrets Manager 호출이 **매번 1초에 잘려** 영영 완료되지 않는다 — 무트래픽
시간대(카나리만 도는 밤)에는 그것이 유일한 트래픽이므로 **영영 회복하지 못한다.**

→ provider가 `context.Background()`에서 만든 자기 context(상한 10초)에서 조회하고, 호출자가
취소돼도 끝까지 간다. **결과는 다음 `Connect`가 승계한다.**

통합 테스트 실측(2026-10-06): 조회가 2초 걸리게 하면 `/readyz`가 `503`(1초 예산에서 끊김) →
다음 호출 `200`. **회복 2.08~2.11초**(2회 실행). 묶여 있었다면 이 경로는 성립하지 않는다.

### 3. "포기하지 않는다"는 호출 수준이 아니라 **provider 수준**의 계약이다

`Connect` 1회는 **유한하게** 실패한다(dial 5초 + refresh 대기 3초 + dial 5초 = 13초 상한).
`database/sql`이 풀 보충 `Connect`를 단일 `connectionOpener`에서 **직렬로** 부르기 때문이다 —
그 안에서 무한히 돌면 opener를 점유해 연결 생성 경로 자체가 막힌다.

대신 backoff 상태가 provider 전역에 남아 **다음 호출이 이어받는다.** 트래픽이 없으면 재시도도
없지만, 운영에서 "트래픽 없음"은 존재하지 않는다 — ADR 0004의 카나리가 `/readyz`를 30초 주기로
상시 호출한다(§2가 detached를 정당화한 근거가 바로 그것이다).

**Step 1과의 관계: 둘은 매 회전에 *동시에* 반응하는 중복 경로다.** Step 1의 트리거는
`Secret Label Updated`뿐이고 Step 2의 상태를 보지 않는다. 그래서 정상 회전마다 Step 2가 수 초 안에
먼저 회복시키고, **수 분 뒤 Step 1의 롤링이 이미 회복한 태스크를 교체한다.** 정상 동작이다 —
회전마다 ECS 배포가 한 번 뜨는 것을 이상 징후로 읽지 않는다. (2-6의 판정이 시각 비교가 아니라
"회전 전 task ID 집합"으로 설계된 이유도 이 중복 때문이다. Step 1의 롤링이 Step 2의 관측 창을 잘라 먹는다.)

**백스톱은 한 방향뿐이다**: Step 1은 **태스크 쪽 조회 실패**(task role IAM 거부, 앱 SDK 타임아웃)를
받아 준다. 그러나 Step 2의 **코드 결함**은 받지 못한다 — 같은 이미지를 다시 띄우기 때문이다.
⚠️ **Secrets Manager 자체 장애는 둘 다 못 받는다.** Step 1도 라벨 확인을 선행 조건으로 두고,
새 태스크는 execution role로 `DB_PASSWORD`를 주입받는다.

### 4. **값이 바뀌지 않은** refresh는 세대를 올리지 않는다

회전 중 창(DB는 새 비밀번호인데 `AWSCURRENT`는 아직 옛 값)에서 세대만 올리면 이어지는 28P01이
전부 "새 세대의 실패"로 집계돼 재조회가 폭주한다. 값이 같으면 backoff 간격만 늘린다
(200ms 시작, full jitter, 5초 상한).

실측(2회): 구값 창 20초 동안 조회 **13~15회**, 창이 끝난 뒤 **619ms~2.36초**에 회복.
편차는 full jitter가 게이트 잔여를 0~5초에서 고르기 때문이고, 이론상 상한은
**게이트 잔여(<5초) + 폴링 + dial ≈ 5.3초**다.

### 5. 세대는 **조건부로만** 무효화한다

늦게 도착한 옛 세대의 28P01이 조건 없이 refresh를 시작하면, Secrets Manager의 비단조적 응답이
**이미 복구한 값을 옛 값으로 되돌릴 수 있다.** 그래서 **실패 세대를 현재 세대와 비교하는 모든 자리**
(connector 진입·nil 분기, `RequestRefresh`, 비행 시작, 적용 시점)에서 낡았으면 조회하지 않고
현재 값으로 재시도한다.

교차 검토 round-2가 수정 전후를 200회씩 돌려 확인했다: 수정 전 **조회 2회가 95%**,
수정 후 **1회가 100%**.

### 6. SDK 리전의 source of truth는 **`DB_SECRET_ARN` 파싱**이다

ECS Fargate는 Lambda와 달리 `AWS_REGION`을 자동 주입하지 않고, AWS SDK for Go v2에는 기본 리전이
없다. ARN은 리전을 항상 포함하고 어차피 시크릿을 지목하는 값이라 **리전이 어긋날 수 없다.**
`AWS_REGION` env는 목적이 다르다 — *"task definition이 의도한 리전으로 만들어졌는지"*를 배포 시점에
거르는 교차검사이고, production에서 비면 기동을 막는다.

## 트레이드오프 — 무엇을 받아들였나

- **실패를 기다렸다가 고친다.** 선제적으로 주기 폴링하지 않는다. **Step 2 경로로 회복할 때는
  첫 28P01을 반드시 한 번 거친다**(풀이 신규 연결을 열 때). 거꾸로 Step 1의 롤링이 먼저 태스크를
  교체하면 그 28P01조차 관측되지 않을 수 있다. 폴링은 평시 비용과 코드를 늘리는 데 비해, 어차피 기존 커넥션이 살아 있는
  동안은 아무 일도 일어나지 않는다. 대가는 **회복 창 동안의 소수 5xx/503**이다.
- **이 설계가 못 받는 구간이 있다.** `T_set`(DB 비밀번호 변경) → `T_label`(`AWSCURRENT` 이동)은
  회전 lambda 내부이고, `T_label` → `T_avail`(전파)에도 상한이 없다. 앱이 줄이는 것은
  `T_avail` 이후뿐이다. **"회전해도 무중단"이 아니라 "수 초 내 자동 회복"이다.**
- **Step 1을 대체하지 않는다.** Step 2의 refresh가 **태스크 쪽 사유**(task role IAM 거부, SDK 타임아웃)로
  실패하면 실행 중 태스크의 env는 이미 옛 비밀번호라 **폴백으로 복구되지 않는다.** 그 구간은 Step 1의
  재배포가 받는다(새 태스크는 execution role로 현재 값을 주입받는다).
  반대로 **Step 2의 코드 결함은 Step 1이 못 받는다** — `force-new-deployment`는 같은 이미지를
  다시 띄우므로 깨진 코드는 깨진 채 뜬다. 그래서 직전 ACTIVE task definition revision을
  롤백 백스톱으로 보존한다(`skip_destroy = true`).
  그리고 **Secrets Manager 자체 장애는 둘 다 못 받는다** — Step 1도 라벨 확인과 `DB_PASSWORD` 주입에
  같은 서비스를 쓴다.
- **IAM은 시크릿 하나로 좁혔다.** task role에 대상 시크릿의 `GetSecretValue` 하나만 준다.

## 대안과 기각 사유

| 대안 | 기각 사유 |
| --- | --- |
| IAM 데이터베이스 인증 | 비밀번호 자체를 없애는 정답에 가깝다. **쓸 수는 있다** — [AWS 문서](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/UsingWithRDS.IAMDBAuth.html#UsingWithRDS.IAMDBAuth.Limitations)는 *"if the IAM role (`rds_iam`) is added to a user (**including the RDS master user**), IAM authentication takes precedence over password authentication"*이라고 적는다. 즉 마스터 유저에 붙이면 **비밀번호 로그인이 막혀 관리 경로를 잃으므로** 앱 전용 role이 따로 필요하고, 토큰 15분 갱신도 따라온다. **결정적인 것은 메모리다** — 같은 문서가 *"You must have between 300 and 1000 MiB extra memory"*, *"If you are using a burstable class instance, avoid running out of memory"*라고 적는데 이 서비스의 RDS는 **`db.t4g.micro`(1 GiB, burstable)**다. P5에서 인스턴스를 키울 때 다시 본다 |
| 주기적 비밀번호 폴링 | 평시에 쓰지 않을 호출을 상시 발생시킨다. 회전은 7일에 한 번이고 그마저 기존 커넥션이 흡수한다 |
| 회전 때마다 재배포만 (Step 1 단독) | 다운을 수 분으로 줄이지만 **재기동이 필요하다**는 전제가 남는다. 배포 파이프라인 장애와 회전이 겹치면 다시 사람 대기다 |
| `BeforeConnect` 훅 | 구현 불가 — 위 §1 |

## 검증

- 단위: 16개 검증 항목 + 회귀 테스트. 새 테스트는 **변이 검사로 확인**했다 — 수정을 되돌리면 실패한다.
- 통합(실제 Postgres, `ALTER ROLE`로 진짜 회전, 2회 실행): `T_set`→성공 **90~102ms**,
  구값 창 20초 후 **619ms~2.36초**, SDK 3연속 실패 후 **432~716ms**,
  `/readyz` 전용 경로 **2.08~2.11초**(503 1회). 2회차는 `-race`로 돌렸고 레이스 없음.
  절차는 [`rotation_integration_test.go`](../../app/internal/db/rotation_integration_test.go) 머리 주석.
- ⛔ **남은 것**: 2-6 실전 검증(온디맨드 회전). 위 숫자는 전부 앱 쪽 회복 시간이고,
  실제 창에는 `T_set`→`T_avail`이 앞에 붙는다. **프로덕션 값은 2-6에서만 나온다.**
