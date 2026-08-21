---
status: approved
revision: 7
created: 2026-08-20
---

# 0008. P4(d)-2 DB 장애 탐지 공백 해소 — canary를 readiness로 전환

## 목표

**이 plan의 범위는 탐지(Step 1)뿐이다.** 로테이션 근본 대응(revision 2의 Step 2·3)은
리뷰 3인의 [높음] 지적을 반영해 **plan 0009로 분리**했다(사유는 "검토 반영 로그" 참고).

1. **[설계 목표 — 실증·완료 판정은 plan 0010]** 앱이 DB 신규 연결에 **전면 실패**하면(= 모든 태스크) **기대값 약 6분 · 상한 8분** 안에 Slack 알람이 온다
   (현재는 66시간 동안 무발화였다). 측정 구간은 **모든 태스크가 지속적으로 503이 된 시점 → Slack 수신**이다.
   정확한 판정 정의(`T_partial`/`T_unavailable`)와 실측은 **plan 0010**이 맡는다 — 이 plan은 경로를 넣는 데까지다.
   - **"하나라도 실패"가 아니라 "전부 실패"가 기준이다.** `service_desired_count = 2`(`terraform.tfvars:8`)이고
     태스크마다 커넥션 풀이 독립적이라 만료 시점이 어긋난다. 한 태스크만 503인 구간에서는 ALB가 두 타깃에
     번갈아 보내 503/200이 섞이고, Route53은 체커별 **3회 연속 실패**(`failure_threshold=3`)로 판정하고, 집계에서도
     **healthy 체커가 18%를 초과하면 전체를 healthy로 유지**하므로 **flip이 일어나지 않는다.**
     (1차 출처 확인 완료 — [How Route 53 determines whether a health check is healthy](https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/dns-failover-determining-health-of-endpoints.html):
     _"If more than 18% of health checkers report that an endpoint is healthy, Route 53 considers it healthy."_) 부분 실패 구간을 기준점으로 잡으면
     정상 동작도 상한 초과로 오판정된다(**plan 0010** 참고).
   - 근거 실측(P4(c), `docs/adr/0004-notraffic-canary.md:57`·`:68`): `canary_down` **5분 47초**.
     `alb-target-5xx`는 canary의 503이 첫 5분 버킷에서 임계(5건)를 넘기므로 **버킷 경계에 따라 최대 약 6분**
     (`period=300`·`evaluation_periods=1`, `monitoring.tf:57-58`). 하한은 버킷 위치에 좌우되므로 보장값이 아니다.
   - **탐지 시계는 "로테이션 시점"이 아니라 "커넥션 풀 소진 시점"부터다.**
     `sql.DB.PingContext`는 풀의 유효한 기존 커넥션을 재사용하고 Postgres는 비밀번호 변경으로 기존 세션을
     끊지 않으므로, 로테이션 후 `ConnMaxLifetime`(5분, `db.go:29`) 동안 `/readyz`가 200을 유지할 수 있다.
     2026-08-16 타임라인의 "22:08:46 로테이션 → 22:13경 500 시작" 간격이 정확히 이것이다.
2. **이 plan의 범위는 탐지 경로를 "넣는 것"까지다.** 목표 1이 실제로 달성됐는지 **실측**하는 드릴은
   **plan 0010**으로 분리했다(사유는 "검토 반영 로그" 참고). 따라서 0008만 끝낸 시점에는
   탐지 경로가 **들어갔으나 실증되지는 않은** 상태다 — 이 사실이 목표 3의 판단에 영향을 준다.
3. 2026-08-23 로테이션은 **마감 브릿지**(**Step 1-6**)로 넘긴다.
   **0008 apply 목표일: 2026-08-22.** 범위를 두 번(0009·0010) 좁힌 유일한 근거가 이 마감이므로 날짜를 명시한다.
   미달하면 08-23은 **브릿지 단독**으로 간다 — 안전 자체는 브릿지가 담보하므로 진행이 막히지는 않는다.
   브릿지가 막는 것은 **그 한 번뿐이고 다음 로테이션은 08-30이다** → **plan 0009의 실질 마감은 2026-08-30**이다.

## 배경/제약

### 이 plan을 만든 장애 (2026-08-16 ~ 08-19, 실측)

| 시각 (KST) | 사건 |
| --- | --- |
| 08-11 10:10 | 태스크 배포(`linkpulse-prod-app:33`). 이때 주입된 비밀번호를 계속 사용 |
| 08-16 22:08:46 | RDS 마스터 비밀번호 **자동 로테이션** (`LastRotated`) |
| 08-16 22:13경 | 커넥션 풀 수명(5분) 만료 → 전 커넥션 교체 → **전면 500 시작** |
| 08-17 00:18:45 | `alb-target-5xx` 최초 ALARM (로테이션 2시간 10분 뒤) |
| 08-19 16:44:19 | `force-new-deployment`로 복구 |

**총 2일 18시간 31분.** 원인은 앱이 `DB_PASSWORD`를 컨테이너 기동 시 1회만 주입받는 구조(`infra/prod/ecs.tf` secrets)다.
로테이션 주기는 7일(`AutomaticallyAfterDays: 7`)이므로 **다음 로테이션은 2026-08-23 22:08경**이다.

### 탐지가 66시간 실패한 이유 (실측, `docs/postmortems/evidence/db-password-rotation-2026-08-19/`)

- ALB 타깃그룹 헬스체크(`alb.tf:30`)와 Route53 canary(`synthetic-canary.tf:56`)가 **둘 다 `/healthz`** 를 본다.
  `/healthz`는 liveness라 DB를 조회하지 않아 장애 내내 200을 반환했다 → `canary_down`·`alb-unhealthy-hosts` 전부 무발화.
- 그 canary 트래픽이 시간당 약 1,976건으로 **전체 요청의 96.4%** 를 차지한다(총 132,382건 중 5xx 4,735건).
  결과적으로 5xx 비율이 **3.58%** 로 희석돼, 비율 기반 알람을 만들었더라도 침묵했을 것이다.
- `alb-target-5xx`는 절대 건수(5건/5분) 기준이라 봇 트래픽 변동에 따라 **ALARM↔OK를 53회 왕복**했다(Slack 알림 106건).
  알람은 정상 작동했으나 심각도를 전달하지 못했고 "봇 소음"으로 읽혔다.
- 500 응답 경로가 원인 에러를 로깅하지 않아 **66시간 동안 앱 에러 로그가 0건**이었다.
  (이 수정은 현재 브랜치 `docs/p4d1-restore-drill-closure`에 **미커밋 상태로 떠 있다** —
  `links.go`·`links_test.go`·`response.go`의 `writeInternalError` 도입. `fix/log-500-cause` 브랜치는 아직 존재하지 않는다.
  이 plan의 범위 밖이며, Step 1 착수 **전에** 자기 브랜치로 분리한다(Step 1-0, AGENTS.md §5 "한 커밋 = 한 가지 일"))

P4(c)에서 확인한 `HealthyHostCount` 소멸과 **같은 계열의 결함**이다 — 감시 지표가 정작 장애 상황에서 의미를 잃는다.

### 확인된 기술 전제

**이 plan(탐지)에 걸리는 전제**

- `/readyz`는 이미 레이트리밋 면제다(`ratelimit.go:151`, `tierExempt`) → 30초 프로빙에 429가 나지 않는다.
- Route53 HTTPS 헬스체크는 **연결 후 2초 안에** 2xx/3xx를 받아야 healthy로 판정한다(ADR 0004가 인용한 AWS 문서).
  그런데 `/readyz`의 DB ping 타임아웃이 정확히 **2초**다(`health.go:10` `readinessTimeout`) → **마진이 0이다.**
- 이 Route53 헬스체크는 어떤 DNS 레코드에도 연결돼 있지 않다(`health_check_id` 참조 0건, `failover` 정책 없음).
  순수 알람 트리거 전용이므로 경로를 바꿔도 트래픽 영향이 없다.

**0009로 승계되는 전제** (확인은 끝났으나 이 plan에서 쓰지 않는다)

- 런타임 이미지가 `gcr.io/distroless/static-debian12:nonroot`(`app/Dockerfile`) → 셸·curl 없음.
  ECS 컨테이너 `healthCheck`는 앱에 헬스체크 서브커맨드를 넣지 않는 한 실행 수단이 없다.
- pgx **v5.10.0**에 `stdlib.OptionBeforeConnect`가 실재한다. 단 이 훅은 **연결 전에만** 실행되므로
  인증 실패(`28P01`)를 관찰하지 못한다 — 리뷰 3인이 전원 지적한 설계 공백이며 0009에서 해소한다.
- `secretsmanager:GetSecretValue`는 `ecs_execution` 롤에만 있다(`iam.tf:29`). `ecs_task` 롤에는 없다.

### 가드레일 (AGENTS.md)

- #1: `terraform apply`·변경형 `aws` 명령은 **사람이 실행한다.** 에이전트는 `terraform plan`까지만 하고 멈춘다.
- 커밋·푸시도 사람이 한다.
- #4: 비자명한 결정은 주석 또는 ADR로 근거를 남긴다.

## 설계 선택 — 0009로 이관

revision 2에는 로테이션 대응 5개 안(A: ECS 컨테이너 헬스체크 / B: EventBridge→Lambda /
C: 연결 시점 시크릿 조회 / C'': 인증 실패 시 자살 / F: 로테이션 주기 연장)의 비교표가 있었다.
**리뷰 3인이 채택안 C의 근간(인증 실패를 관찰할 위치가 없다)과 "무중단" 전제 자체를 [높음]으로 반박했으므로,
그 표는 결론째로 재검토 대상이다.** 이 plan에 남겨두면 "C가 확정됐다"고 오독되므로 통째로 0009로 넘긴다.

**이 plan이 탐지를 먼저 하는 이유**: 어떤 대응안을 고르든, 실패를 6분 안에 알아채는 것이
66시간 뒤에 알아채는 것보다 먼저다. 탐지는 대응안 선택과 독립적이고, 변경이 작으며,
2026-08-23 마감 안에 닫을 수 있다.

## 승인·검토 조건

revision 2에 대해 **리뷰어 3인이 모두 검토를 제출했다**(`codex-cli`·`codex-ide`·`claude-ide`, 전원 `request-changes`).
revision 2에 적었던 "codex 2인 참여 불가 → 단독 검토 승인" 조건은 **철회한다**. 정상 3인 체제로 진행한다.

- **범위가 r6에서 한 번 더 좁혀졌다.** r5까지 포함돼 있던 탐지 드릴(구 Step 1-7)은 **plan 0010**으로 분리했다.
  0008에 남은 것은 Step 1-0~1-6이며, r3 이후 이 범위에서는 블로킹이 나오지 않았다(r4의 Step 1-4 검증 문구가 마지막).
- **이 plan(0008)** 은 탐지 1건으로 범위를 좁혔다. 변경은 인프라 1줄(`resource_path`) + 앱 상수 1개(`readinessTimeout`)
  + 문서 3종이고, 롤백은 원복 한 줄이다.
- **plan 0009**(로테이션 근본 대응)는 리뷰 3인이 [높음]으로 지적한 설계 공백
  (인증 실패 관찰 위치·시크릿 JSON 파싱·런타임 폴백·라이브 전파·공식 로테이션 명령·무중단 입증 기준)을
  처음부터 반영해 새로 작성한다. 이 plan에서는 다루지 않는다.

## 실행 단계

> 인프라 변경은 `terraform plan`까지만 하고 멈춘다. **apply와 변경형 `aws` 명령은 사람이 실행한다**(AGENTS.md #1).

### Step 1-0 — 선행 조건: 미커밋 변경 분리 (✅ 충족됨)

**✅ 충족됨 (2026-08-21).** 500 원인 로깅 수정을 Step 1 PR과 분리하는 조건이었다.
`1b95ab3 fix(app): 500 응답 시 원인 에러를 로그에 남긴다`가 **PR #14(`71d5f4a`)로 main에 머지**됐고,
작업 트리에 `app/` 미커밋 변경이 없다.

→ 실행자는 이 단계에서 할 일이 없다. 다만 착수 시점에 `git status --short`에 `app/` 변경이 남아 있다면
  **그때만** 자기 브랜치로 분리한다(커밋은 가드레일대로 사람이 수행).

### Step 1-1 — `readinessTimeout` 2s → 1s

`app/internal/httpapi/health.go`. 근거를 주석에 남긴다: Route53 헬스체크는 연결 후 **2초 안에**
2xx/3xx를 받아야 healthy로 판정하므로(ADR 0004 인용 AWS 문서), ping 타임아웃이 2초면 마진이 0이다.
**같이 고칠 것**: `app/cmd/server/main.go:33`의 `writeTimeout` 주석 "레디니스 DB 핑 상한(2s, health.go)와 여유를 둔 값"이
낡는다 — 1s로 정정한다.
→ 검증: `cd app && go test ./...` 통과. `health_test.go`가 이 상수에 결합돼 있지 않은지 확인한다.

### Step 1-2 — canary 프로빙 경로 `/healthz` → `/readyz` (plan까지)

`infra/prod/synthetic-canary.tf`의 `resource_path`와 같은 파일의 canary 주석·`canary_down` `alarm_description`.
→ 검증: `terraform plan`이 **두 리소스의 `~ update in-place`인지 확인한다**
  (`aws_route53_health_check.canary`의 `resource_path` + `aws_cloudwatch_metric_alarm.canary_down`의 `alarm_description`).
  변경 건수를 1건으로 못박으면 올바른 plan을 거부하거나 알람 설명 갱신을 누락하게 된다. `-/+`(교체)면 `HealthCheckId`가 바뀌어
  `canary_down`의 dimension이 갈리고, 런북 §13이 경고한 "최초 발행 레이스" 오탐이 1회 뜬다 —
  그 경우 dimension 갱신과 preflight를 함께 계획한다. **apply는 Step 1-4의 순서를 지켜 사람이 실행한다.**

### Step 1-3 — 문서 3종 갱신 (1-2와 **같은 PR**, apply 전)

이 단계가 빠지면 운영자가 지시대로 `/healthz`를 curl해 200을 보고 **실장애를 오탐으로 기각한다.**
2026-08-16에 66시간을 놓친 메커니즘과 동일하다. 리뷰 3인이 r2에서 전원 [높음]으로 지적했다.

- `docs/runbooks/alarm-response.md`
  - `:17` §0-2 초동 확인 — `/healthz`만 보던 curl에 `/readyz`를 추가하고 둘의 의미를 분리한다.
  - `:190`·`:193` §13 — **"`/healthz` 200이면 오탐 의심"을 정정한다.**
    DB 장애에서는 `/healthz` 200 + `/readyz` 503이 **정상적인 실장애 신호**다.
  - `:204` §13 — **대응 분기를 문구가 아니라 구조로 고친다.** 현재는 "503이면 `alb-elb-5xx`도 곧 발화 → §2/§1로 복구"로
    단정하는데, DB 장애에서는 ALB 타깃이 `/healthz`로 healthy를 유지하므로 `alb-elb-5xx`가 아니라 **`alb-target-5xx`**가
    발화하고 진단 경로는 **§3(DB 연결 실패)**다. →
    `healthz 실패 → §2/§1(태스크·ALB)` / `healthz 200 + readyz 실패 → §3(DB 연결)` 두 분기로 명시한다.
  - `:205` "역할 분리(MTTD)" — **구조를 지금 정정한다. 숫자만 미룬다.**
    현재 서술(`alb-elb-5xx`=빠른 1차 / `canary_down`=백스톱)은 canary가 `/readyz`로 바뀐 뒤
    **DB 장애에서는 틀리다**: 타깃이 `/healthz`로 healthy를 유지해 ALB가 503을 만들지 않으므로
    `alb-elb-5xx`가 아니라 **`alb-target-5xx`가 1순위**다. **08-23 로테이션이 정확히 그 케이스이므로 미룰 수 없다**
    (이 항목을 0010까지 미루면 그 창 동안 운영자가 낡은 런북을 따라 헛짚는다 — Step 1-3이 존재하는 이유가 깨진다).
    → 역할 분담을 **"DB 장애 → `alb-target-5xx` 1순위 / 전면 다운 → `alb-elb-5xx`·`canary_down`"** 으로 나눠 서술하고,
    **MTTD 실측값만** "plan 0010 실측 후 기입" 자리표시로 남긴다.
  - `:78` canary 프로빙 경로 서술.
- `infra/README.md:165-171` — canary가 무엇을 프로빙하는지, liveness/readiness 역할.
- `docs/adr/0004-notraffic-canary.md:16`·`:26`·`:76` — 경로와 변경 근거(2026-08-16 장애에서 liveness 감시가 DB 장애를 통과시킴).

→ 검증: `grep -rn "healthz" docs/runbooks/alarm-response.md infra/README.md docs/adr/0004-notraffic-canary.md`
  결과의 잔여 언급을 **하나씩** 확인해 전부 의도된 것인지 판정한다.

### Step 1-4 — 배포·apply 순서 게이트

**앱이 먼저다.** canary를 `/readyz`로 먼저 바꾸면, 운영 태스크가 아직 2초 timeout인 동안
Route53의 2초 제한과 **마진 0** 상태로 프로빙돼 오탐이 난다. "같은 PR"은 이 순서를 보장하지 않는다.

1. PR 머지 → CI 앱 배포 실행.
   `deploy.yml`이 `wait-for-service-stability: true`이므로 **CI 성공 = 안정화 완료**다(`aws ecs wait`는 중복).
2. **라이브 판정** — 이 게이트의 유일한 실질 근거다:
   ```bash
   aws ecs describe-services --cluster linkpulse-prod-cluster --services linkpulse-prod-app \
     --query 'services[0].deployments[?status==`PRIMARY`].[taskDefinition,rolloutState]'
   ```
   PRIMARY 배포의 task definition revision이 **이번 CI가 등록한 revision과 같은지** 대조한다.
   기준값은 Actions 로그의 `amazon-ecs-deploy-task-definition` 단계 출력에서 읽는다.
   확실히 하려면 `list-tasks`/`describe-tasks`로 **실행 중인 모든 태스크**의 taskDefinition·image digest까지 맞춘다.
   (`describe-task-definition`은 등록된 문서를 보여줄 뿐 그게 돌고 있다는 증거가 아니다.)
3. 외부에서 `curl -si https://lpulse.live/readyz` → 200.
   **단 이는 엔드포인트 생존 확인일 뿐이다** — 정상 DB ping은 수 ms라 1초 버전과 2초 버전이 구분되지 않는다.
   라이브 판정은 2번이 한다.
4. `terraform plan` 재실행 → **두 리소스 in-place update**(Step 1-2)만인지 확인
5. **사람이 `terraform apply`** → Step 1-5로

### Step 1-5 — apply 후 정상 상태 실증

드릴이 0010으로 분리됐으므로 **이 단계가 0008의 유일한 라이브 검증**이다.
"탐지 경로를 넣었다"는 완료 조건이 여기서만 성립하므로, **변경 전 관측으로 통과하는 일이 없어야 한다.**

1. **라이브 설정을 먼저 확인한다.** `HealthCheckStatus` 지표만 봐서는 **어떤 경로로 검사했는지 알 수 없다.**
   ```bash
   aws route53 get-health-check --health-check-id <id> \
     --query 'HealthCheck.HealthCheckConfig.ResourcePath'     # "/readyz" 여야 한다
   ```
2. **post-apply 관측만으로 판정한다.** `get-health-check-status`의 각 체커 결과에는 `CheckedTime`이 있으므로,
   **apply 완료 시각보다 오래된 성공 관측은 배제**한다(변경 전 상태가 최신으로 남아 있을 수 있다).
   → 합격: `CheckedTime`이 apply 이후인 관측에서 **특정 지점의 실패가 지속되지 않음**.
   "전 지점 healthy"를 조건으로 걸지 않는다 — Route53은 **healthy 체커가 18%를 초과**하면 전체를 healthy로 보므로
   (1차 출처: _"If more than 18% of health checkers report that an endpoint is healthy, Route 53 considers it healthy."_),
   원거리 체커 1곳의 일시 실패로 불필요한 롤백 판단을 하게 된다. 1회 실패는 재관측 후 판정한다.
3. **`canary_down`이 완전한 post-apply 평가 구간 동안 `OK`인지 확인한다.**
   `period=60 × evaluation_periods=3`이므로 apply 후 **최소 3분**이 지나야 첫 완전 구간이 나온다.
4. **관찰 창: apply 후 최소 15분.** 오탐은 `failure_threshold` 3회(30초×3=90초) + 알람 평가 3분 = **설계상 4분 30초**,
   P4(c) 실측으로는 **5분 47초**가 지나야 드러나므로
   그 이전 조회는 아무것도 보증하지 않는다. **이 창 동안 배포·인프라 변경을 하지 않는다.**
   이 15분은 `resource_path` 롤백 판단의 시한이기도 하다.

→ **보조 관측**: `measure_latency`가 꺼져 있어 `ConnectionTime`/`TimeToFirstByte` 지표가 없고, 이 속성은
  생성 후 변경이 불가하다(켜려면 헬스체크 교체 = dimension 문제). 대신 외부에서
  `curl -w '%{time_connect} %{time_starttransfer} %{time_total}'`로 `/readyz`를 여러 번 측정하고
  **`time_starttransfer - time_connect`** 를 계산해 2초 대비 마진을 기록한다.
  (1차 출처가 이 구간을 정확히 규정한다 — _"Route 53 must be able to establish a TCP connection with the endpoint
  within four seconds. In addition, the endpoint must respond with an HTTP status code of 2xx or 3xx
  within two seconds after connecting."_) 단 운영자 위치의 지연이므로 **합격 근거가 아니라 참고값**이다.

### Step 1-6 — 마감 브릿지 (2026-08-23 로테이션) · `docs/runbooks/` 항목으로 등록

(a) **확인** —
```bash
SECRET_ARN=$(aws rds describe-db-instances --db-instance-identifier linkpulse-prod-pg   --query 'DBInstances[0].MasterUserSecret.SecretArn' --output text)
aws secretsmanager describe-secret --secret-id "$SECRET_ARN"   --query '{LastRotated:LastRotatedDate,Next:NextRotationDate}'
aws rds describe-db-instances --db-instance-identifier linkpulse-prod-pg   --query 'DBInstances[0].MasterUserSecret.SecretStatus'   # active 여야 실행한다
```
(b) **실행** — `LastRotatedDate`가 갱신됐고 `SecretStatus=active`면 즉시(사람):
    `aws ecs update-service --cluster linkpulse-prod-cluster --service linkpulse-prod-app --force-new-deployment`
(c) **확인** — `aws ecs wait services-stable` 후 `/readyz` 200 + 링크 생성→리다이렉트 왕복 성공
(d) **미실행 시 실제 결과** — Step 1은 **탐지만** 준다. 알람이 와도 **복구는 사람이 수동으로** 해야 한다.
    브릿지를 놓치고 알람에도 대응하지 않으면 다운은 계속된다.
    "6분 안에 알람이 오므로 안전하다"는 서술은 **탐지에 한정**되며 복구를 보장하지 않는다.
(e) **유효 범위** — 로테이션 주기가 7일이므로 이 브릿지가 막는 것은 **2026-08-23 한 번뿐이고 다음은 08-30이다.**
    0009가 08-30 전에 닫히지 않으면 사람이 **매주** 반복해야 한다 → 0009의 실질 마감은 **2026-08-30**이다.

→ 검증: 런북에 (a)~(e)가 명령·판정 기준·확인 주기·담당자와 함께 들어가 있다.

### Step 1-7 — (분리) 탐지 드릴 → plan 0010

**→ plan 0010으로 분리했다.**

0008 r3~r4 두 라운드 연속으로 블로킹 지적이 이 단계 하나에 집중됐고(T0 정의 오류 → T1 정의 오류,
게이트 논리 모순), 그동안 **2026-08-23 로테이션을 막을 수 있는 Step 1-0~1-6이 대기 상태로 묶였다.**
프로덕션에 의도적 장애를 일으키는 설계는 본질적으로 검토가 오래 걸리므로 별도 plan에서 다룬다.
r2~r4에서 다듬은 내용(시계 4분할 · preflight 7 · 실행 시퀀스 5단계)은 **유실 없이 0010으로 승계**했다.

- 0010은 **이 plan의 apply 완료를 선행 조건**으로 한다.
- 0010은 **08-23 브릿지와 같은 날 수행하지 않는다.**
- ⚠️ **0010 전까지는 `canary_down`이 실제 DB 장애에서 울린다는 실증이 없다.**
  Step 1-5는 *정상 상태*만 확인한다. 따라서 **08-23에는 브릿지(Step 1-6)를 반드시 수행한다.**

## 리스크/롤백

| 리스크 | 완화 |
| --- | --- |
| `/readyz` 프로빙이 Route53의 2초 제한에 걸려 오탐 | Step 1-1로 ping 타임아웃 1초. Step 1-4가 **앱 배포를 canary apply보다 먼저** 강제한다(순서가 뒤집히면 마진 0 상태로 프로빙된다). Step 1-5에서 `time_starttransfer - time_connect` 실측 |
| 오탐이 계속되면 `failure_threshold`를 3→5로 | **MTTD도 함께 늘어난다**(30초×2 추가). 실제 적용 시 목표값·런북 §13·드릴 성공 기준을 **함께 갱신하고 재실측**한다 |
| canary가 liveness 백스톱을 잃는다 | **잃지 않는다.** 프로세스가 죽으면 `/readyz`도 응답하지 못하므로 `/readyz`는 `/healthz`의 상위집합이다 |
| DB 순간 장애에 canary 알람 오탐이 늘어난다 | 이 헬스체크는 어떤 Route53 레코드에도 연결돼 있지 않다(`health_check_id` 참조 0건) → DNS·트래픽 영향 0. 알람만 뜨며, 그것이 이 plan의 목적이다 |
| **canary의 503이 `alb-target-5xx`를 즉시·지속 발화시킨다** | **양면이다.** (a) *신호*: canary가 분당 약 33건의 503을 만들어 5분 임계(5건)를 첫 버킷에서 넘긴다. (b) *소음*: 배경에서 비판한 플래핑과 같은 축이다. 다만 canary는 30초마다 확실히 오므로 DB 장애 중에는 **ALARM에 고정**되고 플래핑하지 않는다(플래핑은 봇 트래픽의 간헐성 때문이었다). `datapoints_to_alarm`·임계 재검토는 후속 과제 |
| **탐지 경로가 apply됐지만 실측되지 않았다** | 이 plan은 `canary_down`이 *정상 상태에서 1을 유지하는 것*만 확인한다(Step 1-5). 실제 DB 장애에서 울린다는 실증은 **plan 0010**에서 나온다. 그때까지는 08-23 브릿지(Step 1-6)를 안전망으로 반드시 수행하고, 0010을 조속히 진행한다 |
| `resource_path` 변경이 교체(`-/+`)로 잡혀 `HealthCheckId`가 바뀐다 | Step 1-2 검증에서 `~ update in-place` 여부를 먼저 확인. 교체면 알람 dimension 갱신 + preflight를 함께 계획 |

**롤백**: `resource_path`를 `/healthz`로 되돌린다. **이때 Step 1-2에서 함께 바꾼 canary 주석과
`canary_down`의 `alarm_description`도 반드시 같이 원복한다** — 빠뜨리면 canary는 `/healthz`를 보면서
알람 설명은 `/readyz` 장애라고 안내한다. 정상 적용이 두 리소스 update였으므로 **롤백도 두 리소스여야 대칭**이다.
→ 롤백 plan이 그 **두 리소스의 in-place update만** 포함하는지 확인하고, apply 후
  `aws route53 get-health-check`로 라이브 경로가 `/healthz`인지 확인한다. 문서 3종도 함께 원복한다.
`readinessTimeout` 1s는 되돌리지 않아도 무해하다(정상 DB ping은 수 ms).

## 검토 반영 로그

<!-- 형식: [rN] 리뷰어#번호 지적요약 → 반영|기각 — 사유 -->

**[r2] 범위 결정** — 리뷰 3인이 revision 2의 Step 2·3에 [높음] 결함을 총 7건 지적했다
(인증 실패 관찰 위치 부재 · 시크릿 JSON 파싱 누락 · 런타임 폴백 미정의 · 롤백 부성립 · 공식 로테이션 명령 오류 ·
라이브 전파 단계 누락 · 무중단 입증 기준 부재). 이는 문구 수정이 아니라 **설계 재작업**이며 2026-08-23 마감 안에
닫을 수 없다. 반면 Step 1은 지적 2건만 고치면 실행 가능하고 마감 대응에 필요하다.
→ **0008의 범위를 Step 1(탐지)로 좁히고, Step 2·3은 plan 0009로 분리한다.** 아래 "→ 0009 승계" 항목이 그 대상이다.

[r2] claude-ide#1 / codex-cli#1 / codex-ide#1 — 목표의 "4분"이 실측(5분47초)·설계추정(4분30초)과 불일치, Step 1은 타이밍 파라미터를 바꾸지 않음 → **반영** — 목표를 실측 기반 "약 6분"으로 정정하고 근거 실측치를 명시. `alb-target-5xx`(버킷 경계에 따라 최대 약 6분)도 함께 적었다. **단, "4분 유지를 위해 `evaluation_periods` 3→2 또는 `request_interval` 30→10으로 변경"안은 기각** — Step 1의 목적은 "DB 장애를 보게 하는 것"이지 탐지 단축이 아니고(66시간→6분이면 목적 달성), 오탐 트레이드오프와 추가 과금을 새로 도입하면 이 라운드에서 닫히지 않는다. 타이밍 튜닝은 후속 과제로 분리한다.
[r2] claude-ide#1 개선 — 탐지 시계가 "로테이션 시점"이 아니라 "풀 소진 시점"부터 → **반영** — 목표 1에 근거(`ConnMaxLifetime` 5분, 세션 미절단)와 함께 명시. Step 1-7 판정이 흔들리지 않게 한다.
[r2] claude-ide#2 / codex-cli#6 / codex-ide#4 — 런북·infra README 갱신 누락으로 실장애를 오탐 기각하게 됨 → **반영** — Step 1-3을 신설해 `alarm-response.md`(`:17`·`:78`·`:190`·`:193`·`:205`)·`infra/README.md:165-171`·ADR 0004를 **1-2와 같은 PR·apply 전**에 갱신하도록 못박았다. 특히 §13의 "healthz 200이면 오탐 의심"을 명시적 정정 대상으로 지목했다.
[r2] claude-ide#8 — 마감 브릿지가 번호 없는 산문이고 "복구는 수동"을 흐림 → **반영** — Step 1-6으로 번호화하고 (a)확인 (b)실행 (c)확인 (d)미실행 시 결과 4항으로 구조화. (d)에 "알람은 탐지만 주며 복구를 보장하지 않는다"를 명시.
[r2] codex-cli#1 후반 — 탐지 경로를 검증하는 드릴이 없다(로테이션 드릴은 성공 시 장애가 안 나 탐지 미검증) → **반영** — Step 1-7(의도적 DB 장애 드릴)을 신설하고 목표 2로 승격. 측정 구간은 r4에서 T_unavailable→Slack으로 확정됐다.
[r2] claude-ide#9 — `fix/log-500-cause` 브랜치가 실존하지 않고 미커밋 상태 → **반영** — 배경 서술을 사실대로 정정하고 Step 1-0(브랜치 분리 선행)을 신설.
[r2] claude-ide 개선 / codex-ide 개선 — canary 503이 `alb-target-5xx` 소음을 증폭 → **반영** — 리스크표에 별도 항목으로 추가하되 **양면**(빠른 탐지 신호 / 소음)으로 기술. DB 장애 중에는 ALARM 고정이라 플래핑하지 않는다는 점을 구분했다.
[r2] claude-ide 개선 — 1-2 검증에 `~ update in-place` 확인 필요 → **반영** — Step 1-2 검증과 리스크표에 추가.
[r2] claude-ide 개선 — 1초 마진 실측 수단 부재(`measure_latency` 없음, 생성 후 변경 불가) → **반영** — Step 1-5에 `curl -w` 외부 측정으로 대체.
[r2] codex-ide 개선 — "codex-ide 참여 불가" 승인 조건이 현 상태와 불일치 → **반영** — 3인 검토가 실제로 제출됐으므로 단독 검토 조건을 철회하고 정상 3인 체제로 정정.
[r2] claude-ide 개선 — 앱이 마스터 계정으로 DB 접속(`ecs.tf:47`) → **기각(범위 밖)** — 타당하나 이 plan의 탐지 범위와 무관하다. plan 0009(IAM 최소권한)의 후속 과제로 등록한다.

**[r6] 병합 자기점검**:
- Step 1-5의 "오탐은 ≈5~6분"이 설계값(4분30초)과 실측값(5분47초)을 뭉뚱그림 → 둘을 분리해 명시.
- `get-health-check-status`의 `CheckedTime` 필드는 codex-ide가 AWS CLI 문서로 제시한 것이며 **메인 코더가 실행으로 확인하지 않았다.** Step 1-5 실행 시 즉시 드러나고, 없더라도 관찰 창 15분(4번)이 대체 판정을 담보하므로 진행을 막지 않는다.
- "healthy 체커 18%"는 r5까지 유보였으나 이번 라운드에 **1차 출처로 확인 완료**(원문 인용을 목표 1과 Step 1-5에 삽입). ADR 0004는 같은 문서를 인용하지만 이 수치는 담고 있지 않아 직접 확인이 필요했다.
- apply 목표일 2026-08-22는 내일이다. 승인→구현→PR→CI 배포→apply를 하루에 넣는 것이라 빠듯하다. 미달 시 "브릿지 단독"이 명시돼 있어 안전은 담보된다.

**[r5] 병합 자기점검**:
- Step 1-0 헤더가 "선행: 미커밋 변경 분리"인데 본문은 "충족됨"이라 불일치 → 헤더에 (✅ 충족됨) 표시.
- Step 1-7 헤더가 "탐지 드릴 · 목표 1의 실측"인데 본문은 분리 안내로 바뀌어 불일치 → "(분리) 탐지 드릴 → plan 0010"으로 정정.
- 목표 1이 `T_unavailable`을 참조하는데 **그 정의가 0010으로 옮겨가 0008 안에서 깨졌다** → 자족적 서술로 바꾸고 정의·실측 책임을 0010으로 명시.
- 리스크표의 드릴 관련 2건(state drift · 태스크 재기동)은 0010으로 이관하고, 대신 **"탐지 경로가 apply됐지만 실측되지 않았다"** 를 새 리스크로 넣었다. 분리의 대가를 리스크표에 드러내지 않으면 분리가 은폐가 된다.

[r5] codex-ide#1 [높음] — Step 1-5가 어떤 `ResourcePath`로 검사했는지 확인하지 않고, apply 직후엔 **변경 전 관측이 최신으로 남아 있을 수 있어** `/readyz` 프로빙을 실증하지 못한다. 드릴 분리 후 **이것이 0008의 유일한 라이브 검증**이므로 완료 조건 자체가 성립하지 않는다 → **반영** — ① `get-health-check`로 라이브 `ResourcePath == /readyz` 확인 ② `CheckedTime`이 apply 이후인 관측만으로 판정 ③ `canary_down`이 완전한 post-apply 평가 구간(최소 3분) 동안 OK, 3단 게이트로 재작성했다. 분리의 대가를 리스크표에 적어놓고도 정작 남은 검증을 강화하지 않은 것은 내 누락이다.
[r5] codex-ide#2 [중간] — 정상 적용은 두 리소스 update인데 롤백은 `resource_path`와 문서만 되돌려, canary는 `/healthz`를 보면서 알람 설명은 `/readyz` 장애라고 안내하게 된다 → **반영** — 롤백에 `alarm_description`·canary 주석 원복을 넣고, 롤백 plan이 두 리소스 in-place만인지 + apply 후 라이브 경로 확인까지 대칭으로 맞췄다.
[r5] claude-ide#1 [중간] / codex-cli 개선 / codex-ide 개선 — Step 1-3의 `:205` 항목이 0010에 묶여 **08-23 창 동안 런북이 틀린 채로 남는다**(DB 장애에서는 `alb-elb-5xx`가 아니라 `alb-target-5xx`가 1순위) → **반영** — **구조는 지금 정정**(역할 분담을 DB 장애/전면 다운으로 분기), **숫자만** 0010 실측 자리표시로 남겼다. claude-ide가 "1은 08-23 창에 직접 걸리므로 넘기지 않기를 권한다"고 했고, 동의한다 — Step 1-3의 존재 이유가 "운영자가 낡은 런북을 따라 헛짚는 것"을 막는 것인데 이 항목만 미완이면 목적이 부분적으로 깨진다.
[r5] claude-ide#3 [낮음] / codex-cli 개선 — Step 1-5에 관찰 시간 창이 없어 apply 직후 한 번 조회로 통과 가능 → **반영** — "apply 후 최소 15분, 그동안 배포·인프라 변경 금지"를 넣고 롤백 판단 시한으로도 규정했다.
[r5] claude-ide#2 [낮음] — `plan.md:23`의 `Step 1-7 참고`가 빈 곳을 가리킨다 → **반영** — `plan 0010 참고`로 정정.
[r5] codex-cli 개선 / codex-ide 개선 — Step 1-0의 전제가 현재 `git status`와 불일치(이미 머지됨) → **반영** — "✅ 충족됨(2026-08-21, PR #14 `71d5f4a`)"으로 갱신하고, 잔여 변경이 있을 때만 분리하는 조건부로 바꿨다.
[r5] codex-cli 개선 — 목표 1이 0008 완료 = SLA 검증 완료로 오독될 여지 → **반영** — "**[설계 목표 — 실증·완료 판정은 plan 0010]**" 표지를 붙였다.
[r5] claude-ide 개선 — "healthy 체커 18%"를 1차 출처로 확인 → **반영** — 확인 완료, 원문 인용으로 유보를 닫았다.
[r5] claude-ide 개선 — 0008 자신의 apply 목표일이 없다(범위를 두 번 좁힌 근거가 마감인데) → **반영** — 목표 3에 "apply 목표일 2026-08-22, 미달 시 08-23은 브릿지 단독"을 명시.
[r5] claude-ide 개선 — Step 1-4의 기준 revision을 어디서 읽는지 → **반영** — Actions 로그의 `amazon-ecs-deploy-task-definition` 출력.
[r5] claude-ide 개선 — §3이 DB 원인 진단 경로를 실제로 갖추고 있다는 확인 → **확인 수용**(지적 아님). `:204` 분기를 §3으로 보내는 결정에 근거가 있음이 확인됐다.

**[r5] 범위 결정 — 탐지 드릴을 plan 0010으로 분리** (리뷰 지적이 아니라 메인 코더 제안 + 사용자 승인):
r3·r4 두 라운드 연속으로 블로킹이 구 Step 1-7 하나에 집중됐다(r3: T0가 풀 소진 시점과 모순 / r4: T1이 태스크별
풀 편차와 모순 + 게이트 논리 불가능). 매 라운드 이 단계를 전면 재작성하는 동안 **Step 1-0~1-6은 r3 이후 안정**됐고,
그 사이 **2026-08-23 로테이션을 막을 수 있는 변경이 승인 대기로 묶였다.**
드릴은 어차피 08-23 이후에만 수행 가능하므로(preflight 7 — 브릿지와 같은 날 금지) 함께 묶어 둘 이유가 없다.
→ 구 Step 1-7을 **plan 0010**으로 이관(내용 유실 없음, r2~r4 검토 이력도 승계). 0008은 Step 1-0~1-6만 남긴다.
→ **대가**: 0008만으로는 탐지가 *실증되지 않은* 상태다. 이 사실을 목표 2·리스크표·Step 1-7 자리에 명시하고,
  08-23 브릿지를 안전망으로 유지한다. 숨기지 않는 것이 이 분리의 조건이다.

**[r4] 병합 자기점검**:
- Step 1-4에 "`deploy.yml`이 `wait-for-service-stability: true`"라고 단정 → **실물 확인 완료**(`deploy.yml:138`, `:2`). 리뷰어 주장이 정확했다.
- "healthy 체커 18%"를 단정적으로 썼으나 **1차 출처를 확인하지 않았다**(리뷰어 2인 인용을 옮긴 것) → 출처와 미확인 사실을 명시하고, 결론이 `failure_threshold=3`만으로 성립함을 밝혔다. 확인하지 않은 AWS 수치를 단정하지 않는다.
- 중단 상한 `T0 + 20분`의 산출 근거가 없었다 → 5+5+8=18분 + 마진 2분으로 명시.
- `swap` 헬퍼가 헤더 레벨을 구분하지 못해 Step 1-5~1-7을 삼킬 뻔했다(assertion으로 중단, 파일 무손상). 도구 오류지만 기록해 둔다.

**[r4] 충돌 처방 우선순위** — 시계 재정의를 두고 두 처방의 층위가 달랐다:
claude-ide#1의 **"지속 503"(연속 6회/60초)** 이라는 구체 판정 기준과, codex-ide#2의 **T_partial/T_unavailable 명명 +
알람별 기준점 분리**를 합성했다. 전자가 "어떻게 재는가", 후자가 "무엇을 어느 알람에 거는가"를 담당해 충돌하지 않는다.

[r4] claude-ide#1 / codex-ide#2 — `desired_count=2`라 태스크별 커넥션 풀 만료가 독립적. "첫 503"은 한 태스크만 실패한 시점일 수 있고, 부분 실패 구간에서는 ALB round-robin으로 503/200이 섞여 Route53이 flip하지 않는다(체커별 3회 연속 실패 + healthy 18% 초과 유지) → T1→T2가 최대 5분 늘어 8분 상한을 정상 동작에서도 초과 → **반영** — 시계를 T0/T_partial/T_unavailable/T2 넷으로 분리하고 `canary_down` 판정을 **T_unavailable**에 걸었다. 목표 1도 "전면 실패(모든 태스크)" 기준으로 못박았다. **r3에서 T0→T1을 고친 것과 같은 구조의 오류가 태스크 단위에 한 번 더 있었다.**
[r4] claude-ide#2 / codex-cli#1 / codex-ide#1 — 시작 전 게이트 2("복구 plan을 미리 실행해 1 add 확인")가 논리적으로 불가능. 규칙이 살아 있는 상태의 정상 plan은 `No changes`이고 `1 add`는 revoke 후에만 나온다 → **반영** — preflight는 `No changes`(기준선 확인)로, `1 add` 검증은 revoke 직후 별도 단계로 분리했다. 내 명백한 오류다.
[r4] codex-ide 개선 / codex-cli#1 후반 — 복구 plan을 파일로 저장해 동일 plan을 apply / 1 add가 아닐 때의 비상 복구 → **반영** — `-out=drill-restore.tfplan` 저장 후 그 plan을 apply하도록 하고, 비상 복구(CLI 재생성 → 서비스 우선 → state 정합화 별도)를 명시했다.
[r4] codex-cli#2 — `canary_down` ALARM **전이**를 필수 조건으로 걸면서 드릴 직전 상태가 OK인지 확인하는 게이트가 없다. 이미 ALARM이면 새 전이·통지가 없어 결과 해석 불가 → **반영** — preflight 3에 `StateValue=OK`·`HealthCheckStatus=1`·ECS stable을 넣었다.
[r4] claude-ide#3 / codex-cli#3 — Step 1-4의 `describe-task-definition`은 라이브 증거가 아니고, `/readyz` 200은 1s/2s 버전을 구분하지 못한다 → **반영** — 라이브 판정을 PRIMARY 배포 revision 대조(+ `describe-tasks` image digest)로 옮기고, curl은 "엔드포인트 생존 확인"으로 격을 낮췄다. `deploy.yml`이 `wait-for-service-stability: true`라 `aws ecs wait`가 중복이라는 지적도 반영했다.
[r4] codex-ide#3 — Step 1-2는 `resource_path`와 `alarm_description` 둘 다 바꾸므로 정상 plan은 **2건** in-place update인데 "1건"을 요구했다 → **반영** — Step 1-2·1-4 검증을 두 리소스 지정으로 정정. 내 오류다.
[r4] claude-ide#4 — 검토 반영 로그의 Step 번호가 r4 재번호와 어긋난다 → **반영** — r2 로그 4곳을 현재 번호로 갱신했다(로그는 당시 기록이지만, 읽는 사람이 현재 plan에서 그 Step을 찾을 수 없으면 기록의 목적을 잃는다).
[r4] claude-ide 개선 — Step 1-5의 "모든 체커 healthy"가 과엄격(Route53 자체 집계는 18% 초과면 healthy) → **반영** — "`HealthCheckStatus`=1 유지 + 특정 지점 실패가 지속되지 않음"으로 완화하고 1회 실패는 재관측 규칙을 뒀다.
[r4] claude-ide 개선 — `alb-target-5xx`를 "부분 실패 탐지" 증거로 기록 → **반영** — 보조 기록을 T_partial 기준으로 남기고, 두 알람의 역할 분담(부분 실패=target-5xx / 전면 실패=canary_down)을 런북 `:205` 갱신 입력으로 연결했다.
[r4] claude-ide 개선 — 드릴과 08-23 브릿지 일정 분리 → **반영** — preflight 7로 추가. 두 장애가 섞이면 원인 분리도 실측도 무의미하다.
[r4] codex-ide 개선 — 최대 차단 시간 기본 상한을 plan이 제시 → **반영** — `T_unavailable + 8분` 또는 `T0 + 20분` 중 먼저 도달하는 시점.
[r4] codex-cli 개선 / codex-ide 개선 — 폴링 시계열 보존 + 간헐적 200 판정 규칙 → **반영** — 원장에 전체 상태코드 시계열 파일 보존을 명시하고, T_unavailable 정의 자체가 "연속 6회"라 간헐 200 구간을 배제한다.
[r4] codex-cli 개선 — revoke 명령·T0 기록 시점·복구 plan 파일·apply 명령을 런북 형태로 → **반영** — 실행 시퀀스 5단계로 구조화(T0 = API 성공 응답 시각).
[r4] codex-cli 개선 — Step 1-0 커밋 주체 명시 → **반영** — "커밋은 사람이 수행".

**[r3] 병합 자기점검** (메인 코더가 이 라운드에 새로 쓴 문장을 되짚어 잡은 것):
- Step 1-4(배포 순서 게이트) 신설로 번호가 밀렸는데 **목표 2·3의 Step 참조를 고치지 않았다**(1-6/1-5 → 1-7/1-6) → 정정.
- T1("첫 `/readyz` 503")의 **관측 방법이 없어 측정이 불가능**했다 → 드릴 전부터 10초 간격 폴링을 거는 조건을 명시.
- 0009 실질 마감(2026-08-30)이 실행 단계 깊숙이만 있어 눈에 띄지 않았다 → 목표 3으로 끌어올렸다.
- 리스크표의 "드릴 중 배포 금지가 **유일한** 조건"이 과장 → 게이트 3(최대 차단 시간·중단 조건)이 별도로 있으므로 "핵심 조건"으로 정정.

**[r3] 충돌 처방 우선순위** — 성공 기준을 두고 세 리뷰어의 처방이 층위가 달랐다. 다음 순서로 합성했다(충돌이 아니라 보완 관계):
① *시계 정의*는 claude-ide#1의 T0/T1/T2 3분할을 채택(가장 구체적이고 근거가 명확).
② *상한값*은 codex-cli#3의 "상한·기대값 분리"를 채택 → 기대값 약 6분(실측 5분47초) / 상한 8분(마진 2분13초).
   상한을 6분으로 두면 실측 대비 마진이 13초뿐이라 정상 동작도 실패로 판정된다.
③ *검증 대상*은 codex-ide#2의 "canary_down을 별도 필수 조건으로"를 채택 → `alb-target-5xx`는 보조 기록으로 격하.

[r3] claude-ide#1 / codex-cli#1 / codex-ide#1 — Step 1-6의 T0가 목표 1의 "풀 소진 시점" 서술과 정면 모순. SG connection tracking 때문에 tracked flow가 끊기지 않아 `/readyz`가 최대 5분간 200 유지 → T0 기준 10분이면 정상 동작(≈11분)도 실패 판정 → **반영** — T0/T1/T2로 분리하고 판정을 **T1→T2**에 걸었다. T0→T1은 "풀 블라인드 구간"으로 별도 기록해 0009 입력값으로 남긴다. 내가 revision 3에서 만든 모순이다.
[r3] claude-ide#1 부수 — 드릴과 실장애의 등가성 근거가 없어 결과 해석이 흔들린다 → **반영** — "두 경우 모두 기존 커넥션은 정상 · 신규 연결만 실패(로테이션=인증 계층, SG=네트워크 계층)"를 Step 1-7에 명시.
[r3] claude-ide#2 / codex-cli#2 / codex-ide#3 — Terraform 관리 SG 규칙을 수동 변경하면 rule id가 바뀌어 state drift·`InvalidPermission.Duplicate` → **반영** — 복구를 `terraform apply`로 규정하고, 시작 전 게이트(복구 plan 미리 실행·1 add 확인)와 종료 후 `terraform plan` No changes 검증을 넣었다.
[r3] codex-ide#2 — "두 알람 중 하나 10분"이면 Route53→us-east-1 SNS 경로가 고장 나 있어도 통과 → **반영** — `canary_down` ALARM 전이 + Slack 통지를 필수 조건으로 분리하고 `alb-target-5xx`를 보조로 격하.
[r3] codex-cli#3 — 목표 "약 6분"과 드릴 "10분"이 달라 완료 판정 불가 → **반영** — 목표를 기대값/상한으로 정의하고 드릴 기준을 상한 8분으로 통일.
[r3] codex-cli#4 — 앱 1초 timeout 배포와 canary apply의 라이브 순서가 없어 마진 0 상태로 프로빙될 수 있다 → **반영** — Step 1-4(배포·apply 순서 게이트)를 신설했다. "같은 PR"이 배포 순서를 보장하지 않는다는 지적이 정확하다.
[r3] codex-ide#4 — §13이 "503이면 alb-elb-5xx도 발화 → §2/§1"로 단정하는데 DB 장애에서는 `alb-target-5xx`·§3(DB)가 옳다 → **반영** — Step 1-3에 `:204` 분기 구조 수정을 추가(문구가 아니라 분기 자체).
[r3] claude-ide#3 — Step 1-0의 "`git status` 깨끗" 검증이 untracked plan 디렉터리 때문에 통과 불가 → **반영** — "`app/` 미커밋 변경 없음"으로 좁혔다.
[r3] claude-ide 개선 — Step 1-0의 "main 머지 완료" 게이트가 마감 앞에서 직렬 왕복을 추가 → **반영** — "분리해 커밋, 머지는 별개 트랙"으로 완화.
[r3] claude-ide 개선 — 드릴 중 태스크 재기동 시나리오 → **반영** — 리스크표에 추가. 기존 태스크가 `/healthz`로 healthy를 유지해 교체되지 않는다는 안전 근거와 "드릴 중 배포 금지" 조건을 함께 적었다.
[r3] claude-ide 개선 — Step 1-6(a)의 `<arn>`을 실행 가능한 형태로 → **반영** — ARN 조회 명령을 그대로 넣었다.
[r3] claude-ide 개선 — 0009의 실질 마감 명시(브릿지가 막는 건 8/23 한 번, 다음은 8/30) → **반영** — Step 1-6(e)로 추가. **0009 실질 마감 = 2026-08-30.**
[r3] claude-ide 개선 — 드릴 실측값을 런북 §13 `:205` MTTD 역할 줄에도 반영 → **반영** — Step 1-3 대상에 `:205` 추가.
[r3] codex-cli 개선 — `main.go:33`의 "레디니스 DB 핑 상한(2s)" 주석이 낡는다 → **반영** — Step 1-1 대상에 포함.
[r3] codex-cli 개선 — Step 1-5 측정에 `time_starttransfer - time_connect` 계산 → **반영**
[r3] codex-cli 개선 — Step 1-6에 `SecretStatus=active` 확인·주기·담당자 → **반영**
[r3] codex-ide 개선 — 로컬 `curl -w`는 글로벌 체커 마진이 아니다 → **반영** — `get-health-check-status`를 합격 근거로, curl은 참고값으로 격하.
[r3] codex-ide 개선 — `failure_threshold` 3→5는 MTTD도 늘린다 → **반영** — 리스크표에 "목표·런북·드릴 기준 함께 갱신 후 재실측" 조건을 붙였다.

**[r2] 병합 자기점검** (리뷰 지적이 아니라 메인 코더가 이 라운드에 새로 쓴 문장을 재확인해 잡은 것):
- 제목이 좁힌 범위와 불일치 → 「DB 장애 탐지 공백 해소 — canary를 readiness로 전환」으로 정정.
- "설계 선택"의 C 채택 표를 남기면 "C 확정"으로 오독됨 → 0009 이관 안내로 대체.
- 기술 전제에 0009 소관(distroless·pgx·IAM)이 섞여 있음 → "이 plan에 걸리는 전제"와 "0009 승계 전제"로 분리.
- `alb-target-5xx` "3~6분"의 하한 3분은 근거 없음 → "버킷 경계에 따라 최대 약 6분, 하한은 보장값 아님"으로 정정.
- 드릴 성공 기준 "6분 이내"가 실측 5분47초 대비 마진 13초 → **10분**으로 완화(정상 동작을 실패로 판정하지 않도록).
- 드릴 방법이 "확인해 확정한다"로 미정 → 데이터 SG의 5432 인바운드 규칙 일시 제거로 확정.

**→ 0009 승계** (이 plan에서 다루지 않고 0009 작성 시 처음부터 반영):
[r2] claude-ide#3 / codex-cli#3 / codex-ide#3 — 로테이션 명령이 RDS 관리 시크릿에 부적합. `aws rds modify-db-instance --rotate-master-user-password --apply-immediately`가 옳다(1차 출처 확인됨) → **승계**
[r2] claude-ide#7 / codex-cli#2 / codex-ide#2 — `OptionBeforeConnect`는 연결 *전*에만 돌아 `28P01`을 관찰할 수 없다. connector 래퍼 등 실제 관찰 위치가 필요 → **승계** (설계 근간이 걸린 지적)
[r2] codex-ide#2 후반 — AWS 관리형 로테이션은 로테이션 중 최신 조회에도 이전 자격증명을 반환할 수 있어 "무중단"이 보장되지 않는다 → **승계** — 0009의 목표를 "무중단" 대신 "N초 내 자동 회복"으로 낮출지 함께 판단한다.
[r2] claude-ide#4 / codex-cli#5 / codex-ide#7 — 시크릿 `SecretString`이 JSON인데 파싱 단계 없음 → **승계**
[r2] claude-ide#5 / codex-cli#4 / codex-ide#5 — either/or 구조라 런타임 폴백이 없고, `force-new-deployment`는 같은 taskdef라 롤백이 아니다 → **승계**
[r2] claude-ide#6 / codex-ide#6 — `ignore_changes = [task_definition]` 때문에 CI 배포 1회가 있어야 라이브 전파됨 → **승계**
[r2] codex-cli 개선 / codex-ide#8 — Step 3 성공 기준("알람 미발화")이 무중단을 입증하지 못함 → **승계**
[r2] codex-ide 개선 / codex-cli — DI 경계(`config.Config` 명시 필드) · singleflight/mutex 동시성 → **승계**

