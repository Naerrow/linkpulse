---
status: approved # in-review | approved
revision: 3
created: 2026-07-08
---

# 0004. P4 앱 보안 하드닝 트리오 (레이트리밋 · 서버 타임아웃 · 운영 DB 필수화)

## 목표

이 plan이 끝나면, 공개 런칭을 막던 **앱-사이드** 보안/안정성 구멍 3개가 닫혀 있고 테스트로 검증돼 있다.

1. **레이트리밋**: 무인증 공개 엔드포인트(`POST /api/links` 등)가 IP 단위로 제한돼 열거·스팸 생성 남용을 막는다.
2. **HTTP 서버 타임아웃**: `Read`/`Write`/`Idle` 타임아웃을 추가해 느린 연결·유휴 연결의 리소스 고갈을 막는다(현재 `ReadHeaderTimeout`만 있음).
3. **운영 DB 필수화**: `APP_ENV=production`이면 DB 미설정 시 **조용히 인메모리로 폴백하지 않고 기동을 중단**(fail-fast)한다 — 운영 데이터 유실 사고 방지.

**범위 밖(명시)**: 인증/인가는 제품 결정(API 키 모델·사용자 정의)이 필요하므로 **별도 plan**. Safe Browsing/URL 평판도 별도. Redis 기반 분산 레이트리밋은 P4-f(측정 후 판단).

## 배경/제약

### 실측된 현재 상태 (2026-07-08, read-only)

- 미들웨어 체인은 `withMiddleware = requestLogger(recoverer(next))`뿐 — 레이트리밋 없음 (`app/internal/httpapi/middleware.go:12`).
- 서버는 `ReadHeaderTimeout: 5*time.Second`만 설정 (`app/cmd/server/main.go:64`). `Read/Write/Idle` 없음.
- DB 폴백: `DB_HOST/USER/PASSWORD/NAME` 중 **하나라도** 설정되면 4개 전부 필수(fail-fast) — 이미 구현됨 (`config.go:100-131`). **잔여 구멍**: 4개가 **전부 비고** `DATABASE_URL`도 비면 조용히 인메모리 폴백 (`config.go:110`, `main.go:50-53`). prod/dev를 구분하는 신호가 없음.
- 운영 taskdef는 `DB_HOST/PORT/NAME/USER`(평문 env) + `DB_PASSWORD`(Secrets Manager)를 이미 주입 (`infra/prod/ecs.tf:37-55`). 따라서 **현재** prod는 all-or-nothing 규칙으로 보호되고 있고, 위 구멍은 "미래에 taskdef에서 DB_* 블록이 빠지는" 회귀에서만 발현. `APP_ENV=production` 가드가 그 회귀를 항구적으로 봉쇄.
- 앱 응답 형식: `writeError(w, status, code, message)` → `{"error":{"code","message"}}` (`response.go:45`). 레이트리밋 429도 이 형식을 따른다.
- 헬스 경로: `GET /healthz`(라이브니스), `GET /readyz`(레디니스, ALB 타깃 헬스체크 대상) (`router.go:29-30`). **ALB 헬스체크는 XFF 없이 ALB 노드에서 오므로 레이트리밋에서 반드시 예외 처리**(안 하면 헬스체크가 스로틀돼 circuit breaker 오작동).
- 앱 실행: `make test`(=`go test ./...`), `make vet`, `gofmt` — CI(`.github/workflows/ci.yml`)가 이 3개 + `docker build`를 검사.

### 가드레일 (AGENTS.md)

- **인프라 변경은 사람이 apply.** 이 plan의 유일한 인프라 변경은 `ecs.tf`에 `APP_ENV=production` env 1줄 추가. **`terraform plan`까지만 하고 사람 승인·apply.** taskdef 변경은 ADR 0001대로 **다음 CI 배포 1회가 돌아야 라이브 반영**됨(즉시 반영 아님).
- **커밋은 사람.** 모든 코드 변경은 working tree까지만, 커밋/PR은 사용자.
- **베이스**: P3를 main에 머지한 뒤 P4 브랜치를 main에서 분기(사용자 결정). 이 plan 파일은 untracked로 두어 P3 머지에 섞이지 않게 함.

## 실행 단계

> 크기·리스크 오름차순으로 배치. 각 단계는 독립 검증 가능하며 순차로 커밋(사용자).

### 단계 1 — HTTP 서버 타임아웃 (가장 작음, 앱만)

- `main.go`의 서버 생성을 테스트 가능한 `newServer(cfg, handler) *http.Server`로 추출하고 타임아웃을 세팅:
  - `ReadHeaderTimeout: 5s` (유지) · `ReadTimeout: 10s` · `WriteTimeout: 15s` · `IdleTimeout: 60s`.
  - 근거: 모든 핸들러가 1초 미만(리다이렉트·stats·create). `WriteTimeout`은 레디니스 DB 핑 상한(2s, `health.go:11`)과 여유를 두고 15s. 값은 **상수 확정**(codex-ide — 자주 튜닝할 값 아님, env 노출 시 설정 검증 범위만 커짐).
- **검증**: `newServer`가 반환한 `*http.Server`의 4개 타임아웃이 기대값인지 단위 테스트로 단언. `make test`·`make vet`·`gofmt` 통과. 로컬 `make run` 후 `curl /healthz` 200.

### 단계 2 — 운영 DB 필수화 (작음, 앱 + 인프라 1줄)

- `config.go`: `Config`에 `AppEnv string`(env `APP_ENV`) 추가. 허용값 = `""`|`development`|`production`. `Load()`에서 `resolveDatabaseURL()` 뒤에 가드:
  - `if AppEnv == "production" && dsn == "" { return Config{}, error("운영 모드에서 DATABASE_URL/DB_* 미설정 — 인메모리 폴백 금지") }`.
  - **미인식 비-빈 값(예: `prod`)은 에러로 fail-fast** — "prod" 오타로 가드가 조용히 비활성화되는 footgun 방지(codex-ide#2). `""`=dev 기본이라 하위호환 유지(순수 가법적, claude-ide 확인).
  - dev/기본에서는 기존 인메모리 폴백 유지(로컬 편의). `main.go` 폴백 로직 자체는 변경 없음(config가 먼저 에러).
- `infra/prod/ecs.tf`: `environment`에 `{ name = "APP_ENV", value = "production" }` 1줄 추가(= **이 plan 유일의 인프라 변경**). **`terraform plan`만 실행해 사용자에게 제시**(예상: taskdef 새 리비전 in-place). 사용자 apply + 다음 CI 배포로 라이브 반영(ADR 0001).
- `app/.env.example`에 `APP_ENV` 주석 추가(로컬은 미설정=dev).
- **검증**: `config_test.go` 케이스 — (a) `APP_ENV=production` + DB 미설정 → 에러, (b) `APP_ENV=production` + **완전한 `DB_*`(운영 실경로)** → 정상(codex-cli#2), (c) `APP_ENV=production` + `DATABASE_URL` → 정상, (d) `APP_ENV=""` + DB 미설정 → 정상(인메모리), (e) `APP_ENV=prod`(미인식) → 에러. `make test` 통과. 인프라는 `terraform plan` 출력 첨부(사용자 승인 전 apply 금지).

### 단계 3 — 레이트리밋 (가장 큼, 새 미들웨어 + config + 테스트)

- 새 파일 `app/internal/httpapi/ratelimit.go`. 미들웨어 체인을 **`requestLogger(recoverer(rateLimit(next)))`**로 변경 — `recoverer`가 `rateLimit`을 감싸 리미터 자체 버그(nil 맵 등)의 panic도 500으로 복구하고(3인 합의: codex-cli#1·codex-ide#1·claude-ide — 기존 순서는 리미터 panic 미복구), 429는 여전히 최외곽 `requestLogger`가 기록하며 "실작업 전 거부"도 유지된다.
- **의존성**: `golang.org/x/time/rate`(per-key `*rate.Limiter`) 채택 — 3인 합의(검증된 라이브러리, 이미 `pgx` 등 외부 의존성 존재, hand-roll보다 이해·테스트 쉬움). `go.mod`에 추가 + `go mod tidy`로 `go.sum` 갱신(codex-ide).
- **클라이언트 IP 키잉**: `X-Forwarded-For` **최우측** = ALB가 append한 신뢰 IP(claude-ide가 현 토폴로지 클라이언트→ALB→ECS·CDN 없음에서 정확함 확인). 최좌측 위조는 무시. XFF 부재 시 `RemoteAddr` host 폴백. 신뢰 프록시 홉 = 1(ALB 단일)을 상수/주석 명시(CloudFront 등 홉 추가 시 재검토).
- **3티어**(경로/메서드 분기, per-IP 초기 기본값 — **상수**, 조정 가능):
  - **예외(무제한)**: `/healthz`·`/readyz` — ALB 헬스체크 보호(필수).
  - **쓰기(엄격)**: `POST /api/links` — 20 req/min/IP, burst 10.
  - **통계(중간)**: `GET /api/links/{code}` — 60 req/min/IP, burst 20. 데이터 노출 엔드포인트라 리다이렉트와 분리(claude-ide 제안).
  - **읽기/리다이렉트(완만)**: `GET /{code}` 등 — 300 req/min/IP, burst 100. 넉넉히 잡아 NAT 뒤 정상 사용자 오탐 최소화(공유 IP는 합산되는 한계 명시).
- **설정 주입·배선**: 한도는 **상수 확정**(env 아님 — ECS env는 어차피 taskdef 변경+CI 배포 1회 필요라 상수 대비 이점 없음, codex-cli#3·codex-ide#3). 값은 `RateLimitConfig`로 묶어 `RouterDeps.RateLimit`으로 주입하고, **리미터 상태 맵은 `NewRouter` 시점에 1회 생성돼 공유**(요청당 재생성 금지, codex-cli#4·claude-ide). 테스트는 낮은 한도·짧은 TTL·주입 clock(`now func() time.Time`)으로 override. **zero-value `RateLimitConfig` = 운영 기본값 적용**(안전 기본 — 미설정 시에도 prod 보호; codex-cli·codex-ide). 기존 핸들러 테스트 헬퍼(`newTestRouter`)는 레이트리밋을 비활성/높은 한도로 **명시** 설정해 결합 방지.
- **메모리 안전(고루틴 없음)**: per-key 맵의 각 항목에 `lastSeen` 기록, **새 키 삽입 시 opportunistic 스윕**(마지막 스윕 후 sweepInterval=1분 경과 시 TTL=10분 초과 항목 삭제). 백그라운드 janitor 고루틴을 두지 않아 테스트마다 `NewRouter` 호출 시 고루틴 누수가 없다(codex-cli·claude-ide 제안). TTL은 주입 clock으로 결정론적 테스트. 맵 조회·삽입·스윕은 **동일 뮤텍스** 하에서(`*rate.Limiter` 자체는 동시성 안전하나 맵 변형은 락 필요, claude-ide).
- **응답**: `429` — `w.Header().Set("Retry-After", ...)`를 `writeError`(내부에서 `WriteHeader` 호출)보다 **먼저** 설정(claude-ide 주의). 본문은 `writeError(w, 429, "rate_limited", "...")`로 형식 일관. `Retry-After` 값은 리미터 상태를 소비하지 않게 산출(예: `Reserve` 사용 시 즉시 `CancelAt`, 또는 윈도우 기반 상수) — 거절된 요청이 다음 허용 시점을 밀지 않도록(codex-cli).
- **caveat**: 인스턴스별 인메모리 → 태스크 2개면 실질 한도 2배·비공유. 첫 패스 수용, 분산은 P4-f(Redis).
- **검증**: 테이블 테스트 — (a) 티어별 한도 초과 시 429 + `Retry-After`, (b) `/healthz`·`/readyz`는 초과해도 무제한, (c) XFF 파싱: 공백·복수 IP·포트 suffix·IPv6·위조 최좌측 무시(codex-ide 제안), (d) 주입 clock으로 TTL 스윕이 idle 항목을 지우는지, (e) 서로 다른 IP는 독립 버킷. `make test`·`vet`·`gofmt`·`docker build` 통과. 로컬 반복 `curl`로 429 육안 확인. 주의: 주입 clock은 **TTL 스윕(lastSeen)** 만 결정론화하며 리미터 토큰 리필은 `Allow()`가 내부 `time.Now()`를 쓴다 — '윈도 경과 후 한도 회복'을 결정론 테스트하려면 `AllowN(now, 1)` 사용(claude-ide).

### 단계 4 — govulncheck는 이 plan에서 분리 (범위 밖)

- 3인 중 2인(codex-cli·codex-ide) 지적 반영 — govulncheck는 트리오와 실패 모드가 다르고 CI 네트워크 의존성을 더하므로 **별도 소형 PR/plan**으로 분리한다. 이 plan에서는 다루지 않음(prelaunch 백로그에 유지).

## 리스크/롤백

| 리스크 | 완화 | 롤백 |
| --- | --- | --- |
| 레이트리밋이 ALB 헬스체크를 스로틀 → circuit breaker 오작동 | `/healthz`·`/readyz` 경로 예외(테스트로 강제) | 앱 변경 git revert |
| per-IP 맵 무한 성장(메모리 DoS) | opportunistic TTL 스윕(백그라운드 고루틴 없음, 주입 clock 테스트) | 상동 |
| XFF 최좌측 신뢰 시 IP 위조로 우회 | 최우측(ALB 부착) 사용, 홉 수=1 명시 | 상동 |
| `WriteTimeout`이 짧아 정상 응답 절단 | 15s(핸들러 최대 <2s 대비 여유) | 상수 상향 |
| `APP_ENV` taskdef 변경이 CI 배포 전엔 라이브 미반영 → 가드 비활성 | plan에 ADR 0001 함정 명시, 배포 후 로그로 확인 | `ecs.tf`에서 env 제거 + terraform apply + 재배포(사용자) |
| 레이트리밋 오탐으로 정상 사용자 429(특히 NAT 공유 IP) | 읽기 티어를 넉넉히(300/min) | 상수 상향 + 재배포(코드 변경 = ECS env 변경과 동일 비용) 또는 미들웨어 revert |

- 전 항목 공통 롤백: 앱 코드는 `git revert`. 인프라(`APP_ENV`)는 terraform으로 원복 후 사용자 재배포.

## 확정된 설계 결정 (revision 2 — 리뷰 합의로 열린 질문 종결)

1. 레이트리밋 알고리즘 → **`golang.org/x/time/rate` 채택**(3인 합의).
2. 리다이렉트/티어 → **3티어 확정**: 쓰기 20 · 통계 60 · 읽기 300 req/min/IP. 리다이렉트도 완만 상한 포함(3인 합의), 통계는 별도 중간 티어(claude-ide).
3. env vs 상수 → **상수 확정**(한도·타임아웃 모두). ECS env는 어차피 taskdef+CI 배포가 필요해 상수 대비 이점 없음(2인 지적) → 이 plan 유일 인프라 변경 = `APP_ENV` 1줄로 유지.
4. govulncheck → **이 plan에서 분리**(2인 지적, 단계 4).
5. prod 신호 → **`APP_ENV` 유지**(명시 env가 추론보다 명확, claude-ide가 가법적·하위호환 확인) + 미인식 값 fail-fast 추가.

## 검토 반영 로그

<!-- /plan-merge가 라운드별로 기록. 형식: [rN] 리뷰어#번호 지적요약 → 반영|기각 — 사유 -->

**revision 1 → 2** (리뷰 3건: codex-cli `request-changes`, codex-ide `request-changes`, claude-ide `approve`)

- [r2] codex-cli#1 / codex-ide#1 / claude-ide(제안) 미들웨어 순서가 rateLimit panic 미복구 → **반영** — 체인을 `requestLogger(recoverer(rateLimit(next)))`로 변경(단계 3).
- [r2] codex-cli#2 DB 필수화 테스트가 운영 실경로(DB_*) 누락 → **반영** — 단계 2 검증에 `APP_ENV=production`+완전한 DB_* 성공 케이스 추가.
- [r2] codex-cli#3 / codex-ide#3 인프라 범위 모순 + "재배포 불요" 오류 → **반영** — 레이트리밋 한도를 상수화(ECS env 제외)해 유일 인프라 변경=APP_ENV로 정합, 리스크표 "재배포 불요" 문구 정정.
- [r2] codex-cli#4 / claude-ide(제안) 리미터 설정 주입·공유 불명확 → **반영** — `RateLimitConfig`를 `RouterDeps`로 주입, 상태 맵은 `NewRouter`서 1회 생성·공유, 테스트 override 명시(단계 3).
- [r2] codex-ide#2 운영 계약(기본값·burst·TTL·cleanup·미인식 env fail-fast) 미확정 → **반영** — 3티어 구체값·TTL 10분·sweepInterval 1분·미인식 APP_ENV fail-fast 확정(단계 2·3).
- [r2] codex-cli(제안) / claude-ide(제안) janitor 고루틴 생명주기 → **반영** — 백그라운드 고루틴 제거, opportunistic 스윕 + 주입 clock으로 결정론 테스트.
- [r2] codex-ide(제안) XFF 파싱 엣지케이스 → **반영** — 공백·복수·포트·IPv6 테스트 추가(단계 3 검증).
- [r2] claude-ide(사소) Retry-After는 WriteHeader 전에 Set → **반영** — 단계 3 구현 주의로 명시.
- [r2] codex-ide(제안) 서버 타임아웃은 상수로 충분 → **반영** — 단계 1 상수 확정.
- [r2] codex-cli / codex-ide (제안) `golang.org/x/time/rate` 채택 → **반영** — 채택(단계 3).
- [r2] codex-cli / codex-ide (제안) govulncheck 분리 → **반영** — 단계 4에서 별도 PR로 분리.
- [r2] codex-cli / codex-ide (제안) 리다이렉트 완만 상한 / claude-ide(제안) stats 별도 티어 → **반영** — 3티어 설계에 통합.

기각: 없음(전 지적 반영).

**revision 2 → 3** (리뷰 3건 전원 `approve` — 3인 합의 성립. 아래는 비차단 폴리시/구현주석만 반영, 실질 설계 불변)

- [r3] codex-cli / codex-ide / claude-ide 리스크표 "TTL 만료 janitor" 문구가 확정 설계(opportunistic 스윕)와 불일치 → **반영** — 리스크표를 "opportunistic TTL 스윕(고루틴 없음)"으로 정정.
- [r3] codex-cli / codex-ide `RateLimitConfig` zero-value 의미 모호 → **반영** — zero-value=운영 기본값(안전 기본), 테스트 헬퍼는 비활성/높은 한도 명시.
- [r3] codex-ide `go.mod` 추가 시 `go.sum` 갱신 누락 → **반영** — 의존성 bullet에 `go mod tidy` 명시.
- [r3] codex-cli `Retry-After`가 리미터 상태 소비 우려 → **반영** — `Reserve` 즉시 `CancelAt`/윈도우 상수 방식 명시.
- [r3] claude-ide 주입 clock 적용 범위(리필 vs TTL) → **반영** — clock은 TTL 스윕만 결정론화, 리필 테스트는 `AllowN` 명시.
- [r3] claude-ide per-key 맵 동기화 → **반영** — 맵 조회·삽입·스윕은 동일 뮤텍스 명시.

기각: 없음.
