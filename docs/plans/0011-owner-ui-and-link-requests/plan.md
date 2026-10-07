---
status: approved # in-review | approved
revision: 3
created: 2026-10-07
---

# 0011. 화면 한 장 + 링크 생성은 관리자만 + 방문자 링크 요청

## 목표

1. `https://lpulse.live/`에 들어가면 **화면이 뜬다**(지금은 404). Go 앱에 내장한 정적 파일이라 **인프라·비용 추가 0**.
2. **단축 링크 생성은 관리자(토큰 보유자)만** 한다. 클릭(리다이렉트)과 클릭 수 조회는 지금처럼 누구나 할 수 있다.
3. 방문자는 **링크를 요청**할 수 있다. 관리자가 화면에서 보고 승인 또는 거절하고, 방문자는 접수 때 받은
   **요청 확인 링크**로 결과(완성된 단축 링크)를 본다. **개인정보(이메일 등)는 받지 않는다.**

사람이 URL을 직접 보고 승인하므로, 공개 생성의 핵심 위험인 피싱 링크가 내 도메인으로 발급되는 일이 구조적으로 막힌다.

## 배경/제약

- 사용자 결정(2026-10-07): 큰 공개 서비스로 키우지 않는다. 화면은 한 장으로, 비용은 추가하지 않는다. 생성은 나만 하고,
  남이 원하면 나에게 요청하게 한다.
- 현재 `POST /api/links`는 무인증 공개다(`app/internal/httpapi/router.go:35`). URL 검증은 http/https와 host 존재 여부뿐이다
  (`links/service.go:83-90`).
- 레이트리밋 분류는 경로 문자열로 한다(`ratelimit.go:148` `classify`). **새 경로를 추가하지 않으면 전부 읽기 한도(분당 300)에 묶인다.**
- 요청 로그가 원래 경로를 그대로 찍는다(`middleware.go:19` `requestLogger`). 요청 ID가 곧 열쇠이므로 로그에서 가려야 한다.
- 레이어 구조(Controller → Service → Repository)와 DTO, 트랜잭션 규칙을 따른다(전역 CLAUDE.md). 저장소는 인메모리·Postgres 두 구현이다.
- AGENTS.md 가드레일: 인프라 변경은 `terraform plan`까지만 하고 **apply는 사람**이 한다. 비밀값(토큰 원문)은 코드·커밋에 넣지 않는다.
- 공개 저장소다. 토큰 원문은 어디에도 남기지 않고, 해시만 gitignore된 `terraform.tfvars`에 둔다.

## 설계

### 인증 — 토큰 원문이 아니라 해시만 서버에 둔다

- 관리자 토큰은 운영자가 외우는 8자 이상의 비밀번호다(아래 revision 3 참고). 서버에는 **SHA-256 해시만** env `ADMIN_TOKEN_SHA256`으로 준다.
  → Secrets Manager가 필요 없어 비용이 0이다. 해시는 운영자의 AWS 계정과 로컬 tfvars에만 있다.
- 요청은 `Authorization: Bearer <토큰>`. 서버는 `sha256(토큰)`을 `subtle.ConstantTimeCompare`로 비교한다.
- **해시가 비어 있으면 관리자 기능 전체가 꺼진다(fail-closed)** — 모든 관리자 엔드포인트가 401이고, 기동 로그에 `admin_enabled=false`를 찍는다.
  기동을 막지는 않는다. 그래서 앱 배포와 인프라 apply의 순서가 바뀌어도 서비스가 죽지 않는다.

### API

| 메서드·경로 | 인증 | 레이트리밋 | 내용 |
| --- | --- | --- | --- |
| `GET /` | 공개 | 읽기 | 화면(`index.html`) |
| `GET /static/{file}` | 공개 | 읽기 | `app.js`·`style.css` |
| `POST /api/links` | **관리자**(변경) | 쓰기 | 직접 생성. 토큰 없으면 401 |
| `GET /api/links/{code}`, `GET /{code}` | 공개 | 통계·읽기 | 변경 없음 |
| `POST /api/requests` | 공개 | **쓰기**(신규 분류) | `{url, note?}` → 201 `{id, status: "pending"}` |
| `GET /api/requests/{id}` | 공개(id가 열쇠) | **통계**(신규 분류) | `{status, url, short_url?}` |
| `GET /api/admin/requests` | 관리자 | 통계 | 대기 중 요청 목록(최근순 50건) |
| `POST /api/admin/requests/{id}/approve` | 관리자 | 쓰기 | 링크 생성 + 요청을 approved로. **한 트랜잭션** |
| `POST /api/admin/requests/{id}/reject` | 관리자 | 쓰기 | 요청을 rejected로 |
| `GET /api/admin/links` | 관리자 | 통계 | 최근 링크 50건(클릭 수·중단 여부) |
| `POST /api/admin/links/{code}/disable` · `/enable` | 관리자 | 쓰기 | 링크 **중단·재개**. 중단된 링크는 410, 클릭을 세지 않는다 |

- 요청 확인 링크는 `https://lpulse.live/#r=<id>`다. 프래그먼트(`#` 뒤)는 브라우저가 서버로 보내지 않으므로
  페이지 로드 로그에 남지 않는다. 화면의 JS가 `GET /api/requests/{id}`를 부르고, 이 경로는 로그에서 `/api/requests/{id}`로 가린다.
- 입력 제한: `url` ≤ 2048자이고 기존 URL 검증을 재사용한다. `note` ≤ 200자. **대기 중 요청이 100건이면 새 요청은 429**로 받지 않는다(스팸이 쌓이는 상한).
- 이미 결정된 요청을 다시 승인·거절하면 409다.

### 데이터

```sql
CREATE TABLE IF NOT EXISTS link_requests (
    id         TEXT PRIMARY KEY,                 -- 16바이트 crypto/rand, base64url(22자). 추측 불가
    url        TEXT NOT NULL,
    note       TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected')),
    code       TEXT REFERENCES links(code),      -- 승인 시 발급된 단축 코드
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS link_requests_status_created ON link_requests (status, created_at DESC);
```

- `schema.sql`은 기동할 때마다 적용된다(`db.go:74`). `IF NOT EXISTS`이므로 기존 `links`에 영향이 없다.
- 승인은 `BEGIN → SELECT … FROM link_requests WHERE id=$1 FOR UPDATE → INSERT links → UPDATE link_requests → COMMIT`이다.
  `pending`이 아니면 409로 끝낸다. `FOR UPDATE`가 동시 승인을 줄 세우므로 뒤에 온 승인은 결정된 상태를 보고 409가 된다.
  코드가 충돌하면 트랜잭션 전체를 되돌리고, 서비스가 새 코드로 다시 시도한다.

### 화면 (파일 3개를 바이너리에 내장)

`app/internal/web/`에 `index.html`·`app.js`·`style.css`를 두고 `go:embed`로 내장한다. 빌드 도구·외부 CDN·프레임워크는 쓰지 않는다.

- **링크 요청**: URL과 메모를 넣고 요청하면, 요청 확인 링크를 보여 주고 복사 버튼을 준다.
- **요청 확인**: `#r=<id>`로 열리면 상태를 보여 준다. 승인되면 단축 링크와 복사 버튼을 보여 준다.
- **운영자 화면은 `/#admin`**이다. 방문자 화면에는 링크를 두지 않는다(사용자 확인에서 "관리자 칸이 뭔지 모르겠다"는 혼란이 나와 숨겼다).
  토큰을 한 번 넣으면 브라우저 `localStorage`에 둔다. 직접 생성 폼과 대기 목록(승인·거절 버튼)이 열리고, 승인하면 단축 링크 복사 버튼이 뜬다.
- 요청하면 **곧바로 그 요청의 확인 페이지(`/#r=<id>`)로 넘어가** "이 페이지 주소를 저장하라"고 안내한다.
- **XSS 방지가 토큰 보호의 전부다**: 요청 URL과 메모는 남이 쓴 문자열이다. **`textContent`로만 넣고 `innerHTML`은 쓰지 않는다.**
  화면 응답에 CSP `default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'`를 걸고,
  `X-Content-Type-Options: nosniff`도 단다.
- 좁은 화면에서도 쓸 수 있게 한 열 레이아웃으로 둔다.

### 인프라 (비용 0)

- `ecs.tf` environment에 `ADMIN_TOKEN_SHA256 = var.admin_token_sha256`(변수 기본값 `""`)을 추가한다. 새 리소스는 없다.
  - **값이 비면 env 자체를 넣지 않는다**(조건부 `concat`). 빈 문자열 env는 ECS 응답에서 빠져 매 plan마다 교체가 뜰 수 있다.
  - **변수를 `sensitive`로 둔다**(코드 검토 r1에서 바꿈). 토큰이 외우는 비밀번호라 해시가 새면 대입으로 풀릴 수 있고, plan 출력은 공개 PR에 붙는다.
    대가로 `container_definitions` diff 전체가 plan에서 가려진다. env 변경은 `ecs.tf` 코드 diff로 검토한다.
    태스크 정의에는 값이 남는다. 하지만 읽을 수 있는 주체(운영자·CI 배포 role)는 이미 임의 이미지를 배포할 수 있어 새 권한이 생기지 않는다.
  - 변수 `validation`으로 "빈 값 또는 16진수 64자"만 받는다. 토큰 원문을 넣는 실수를 apply 전에 막는다.
- 값은 gitignore된 `infra/prod/terraform.tfvars`에만 둔다.

## 실행 단계

1. **도메인·저장소** — `links` 패키지 안에 요청 기능(모델·서비스·인메모리/Postgres 저장소), `schema.sql`에 테이블 추가, 승인 트랜잭션.
   → 검증: 단위 테스트(접수·조회·승인·거절·409·대기 상한 429·입력 길이 400·코드 충돌 재시도), 인메모리 저장소 `-race`.
2. **인증·라우팅** — 관리자 미들웨어(해시 비교, 빈 해시는 fail-closed), `POST /api/links` 보호, 새 엔드포인트,
   `classify`에 새 경로 분류, `requestLogger`의 요청 ID 마스킹.
   → 검증: 핸들러 테스트(토큰 없음·틀림 401, 맞음 201, 빈 해시면 맞는 토큰도 401), `classify` 테이블 테스트, 로그에 id가 안 찍히는지.
   **변이 검사**: 비교를 `==`로 바꾸거나 fail-closed 분기를 지우면 테스트가 실패해야 한다.
3. **화면** — `web/` 3파일 + 내장 + `GET /`·`/static/` 라우트 + 보안 헤더.
   → 검증: `GET /` 200과 CSP 헤더 테스트. `docker compose up`으로 띄워 **[사람]이 브라우저에서 요청 → 승인 → 확인 링크**를 끝까지 따라가 본다.
   이때 메모에 `<img src=x onerror=alert(1)>`를 넣어 그대로 글자로 보이는지 확인한다.
4. **Postgres 통합 확인** — 일회용 컨테이너에서 승인 트랜잭션을 검증한다. 동시에 승인 두 번을 보내 한 건만 성공하고, 고아 링크가 남지 않아야 한다(G-7 절차).
   → 검증: `go test -tags=integration` [사람 실행].
5. **로컬·문서** — `docker-compose.yml`에 개발용 해시를 넣는다(개발 토큰 `dev-token`, 원문도 README에 적는다 — 로컬 전용).
   README의 API·사용법을 갱신하고, k6 setup이 `ADMIN_TOKEN` env로 토큰을 보내게 고친다.
   → 검증: `docker compose up` 후 README 절차가 그대로 동작한다.
6. **인프라** — `ecs.tf` env와 변수를 추가한다. `terraform fmt`·`validate`, **`terraform plan`까지만** 한다.
   → 기대: `aws_ecs_task_definition.app` 교체 1건(`1 to add, 0 to change, 1 to destroy`), 다른 변경 없음.
7. **[사람] 배포** — 토큰을 정하고, 해시(`printf %s "$TOKEN" | shasum -a 256 | cut -d" " -f1`)를 tfvars에 넣는다 →
   **apply → PR 머지(CI 배포)**. 인프라를 먼저 하는 이유: 새 env는 옛 이미지에 무해하고, 배포 직후 바로 관리자 기능이 켜진다.
   → 검증: 기동 로그 `admin_enabled=true`, `curl -X POST /api/links`(토큰 없음) 401, 화면에서 요청 → 승인 → 확인 링크 → 리다이렉트까지 한 번.

검토: 사용자 확인 1회 → 구현 → **코드 검토 1라운드(claude-ide)** → 머지. 블로커가 없으면 라운드를 더 열지 않는다.

## 리스크/롤백

| 리스크 | 완화 | 롤백 |
| --- | --- | --- |
| 화면 XSS로 `localStorage`의 토큰 탈취 | `textContent`만 사용, CSP `script-src 'self'`, 3단계 수동 확인 | 토큰 교체(새 해시 → apply → CI 배포) |
| 토큰 분실·유출 | 원문은 사람만 보관한다. 해시는 tfvars에 두고 `sensitive`로 plan 출력에서 가린다 | 위와 같다 |
| 요청 스팸 | 쓰기 한도(IP당 분당 20), 대기 상한 100건, 사람 승인 | 거절 처리. 심하면 `POST /api/requests`만 끄는 배포 |
| 요청자가 확인 링크를 잃음 | 개인정보를 받지 않는 대가다. 다시 요청하면 된다 | — |
| 승인 트랜잭션 버그로 고아 링크 | 4단계 동시 승인 통합 테스트 | PR revert → CI 재배포 |
| 배포 순서 실수(앱 먼저) | fail-closed라 관리자 기능만 꺼지고 서비스는 정상 | 7단계 apply 후 CI 배포 1회 |
| 기동 때 스키마 잠금 대기 | `schema.sql`의 `ALTER TABLE`·`CREATE INDEX`는 이미 있어도 매 기동 `links` 잠금을 잡는다(평소 마이크로초). 운영 DB에서 트랜잭션을 열어 둔 채 배포하지 않는다 | 열어 둔 세션을 끝낸다 |

롤백은 전체적으로 **PR revert → CI 재배포**다. 테이블은 남아도 무해하다(`IF NOT EXISTS`). env도 옛 이미지에 무해하다.
다만 0011 이전 이미지로 돌아가면 **운영자가 중단한 링크가 다시 리다이렉트되고 `POST /api/links`가 다시 공개된다.** 옛 코드는 `disabled_at`과 관리자 인증을 모른다.

## 구현 중 바꾼 것 (revision 2)

- **요청 기능을 별도 `requests` 패키지가 아니라 `links` 패키지 안에 뒀다.** 승인이 링크 생성과 같은 락·같은 트랜잭션을 써야 해서,
  같은 저장소 구현체(`MemoryRepository`·`PostgresRepository`)가 두 인터페이스를 함께 만족하게 했다(`links.Store`).
  코드 발급(`randomCode`)과 URL 검증(`normalizeURL`)도 내보내지 않고 그대로 재사용한다.
- 인프라 env를 조건부로, 변수를 비-sensitive로 했다(위 "인프라" 절). 비-sensitive는 코드 검토 r1에서 sensitive로 바꿨다.
- 승인 SQL을 "UPDATE … WHERE status='pending'이 0행이면 409"에서 "`SELECT … FOR UPDATE`로 먼저 판정"으로 바꿨다. 동작은 같고 순서가 코드에서 바로 보인다.
- fail-closed는 두 겹이다. `enabled()` 검사가 있고, 해시가 비면 `ConstantTimeCompare`가 길이 불일치로 0을 돌려준다.
  그래서 `enabled()` 하나만 지우는 변이는 동작을 바꾸지 않는다. 테스트는 동작(빈 해시면 맞는 토큰도 401)을 고정한다.

## 사용자 확인에서 추가한 것 (revision 3)

- **링크 중단·재개.** 사용자 질문: *"한 번 승인하면 계속 사용 가능한 거야? 중간에 중단 못 하나?"* — 못 했다.
  승인할 때 멀쩡했던 주소도 나중에 도메인 주인이 바뀌어 위험해질 수 있으므로 운영자가 끌 수 있어야 한다.
  `links.disabled_at`(NULL이면 사용 중)을 `ADD COLUMN IF NOT EXISTS`로 붙이고, 중단된 링크는 **410 Gone**으로 답하며 클릭을 세지 않는다.
  **지우지 않는다** — 클릭 기록을 남기고 언제든 다시 켤 수 있게 한다. 운영자 화면에 최근 50개 목록과 중단·다시 사용 버튼을 둔다.
  롤링 배포 중 옛 태스크는 열 이름을 지정해 읽으므로 새 열의 영향을 받지 않고, 롤백해도 열 자체는 무해하다(중단이 풀리는 효과는 리스크/롤백 절).
- **토큰을 난수에서 외울 수 있는 비밀번호로 바꿨다.** 사용자 질문: *"다른 컴퓨터에서 사이트만 치고 들어갈 때도 입력해야 하는데,
  이 컴퓨터만 써야 한다면 무슨 의미지?"* — 44자 난수는 외울 수 없어 사실상 보관한 기기에 묶인다. 서버는 어떤 문자열이든 해시로 비교하므로 코드 변경은 없다.
  트레이드오프: 해시가 새면 난수보다 대입에 약하다. 최소 길이는 사용자 결정으로 8자다(20자는 외우기 부담). 다른 곳에 안 쓴 비밀번호로 하고, 해시는 AWS 계정과 로컬 tfvars 밖으로 내보내지 않는다.
  영문·숫자·기호만 쓴다 — 브라우저 `fetch`는 헤더에 한글을 실으면 오류를 낸다. 남의 컴퓨터에서는 쓰고 나서 로그아웃한다(토큰이 `localStorage`에 남는다).

## 검토 반영 로그

<!-- /plan-merge가 라운드별로 기록. 형식: [rN] 리뷰어#번호 지적요약 → 반영|기각 — 사유 -->
