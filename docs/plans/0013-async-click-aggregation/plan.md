---
status: approved # in-review | approved
revision: 2
created: 2026-10-12
---

# 0013. 클릭 집계 비동기화 — 리다이렉트에서 행 UPDATE를 떼어 낸다

## 목표

1. 리다이렉트가 더는 DB에 쓰지 않는다. 클릭은 **태스크 안에서 모아 큐로 보내고, 워커가 묶음으로 DB에 반영한다.**
   큐는 인터페이스 뒤에 두고 SQS로 구현한다(AGENTS.md §5 "앱이 AWS SDK를 직접 부르지 않는다").
2. 0012의 첫 병목이 사라졌는지 같은 방법으로 다시 잰다. **`hot`의 한계가 `spread`만큼 올라와야 한다.**
3. 클릭 수는 **유실도 중복도 없이** 맞아야 한다. 판정은 클라이언트 결과가 아니라 **서버 쪽 불변식**으로 한다.
   큐가 빈 뒤 `sum(recorded_clicks) − sum(dropped_clicks)` = DB 합계여야 한다.
   이제는 응답 전에 기록하므로 클라이언트가 302를 못 받은 요청(타임아웃·abort)도 DB에 들어간다. 그래서 `redirects_302` = DB는 포화 단계에서 성립하지 않는다.

## 배경/제약

- **0012 결과**(`load/k6/results/0012-baseline.md`):
  - `hot` 한계는 400 rps이고 800 rps가 무릎이었다(p95 4.8 → 118.6ms). `spread`·`read`는 1600 rps 이상이었다.
  - 800 rps에서 태스크 CPU 31%, RDS CPU 27%로 여유가 있었다. 그래서 첫 병목은 **한 행에 대한 클릭 UPDATE 행 잠금 경합**이다.
  - 연결 풀 포화(25개)는 그 증상이다.
- **지금의 클릭 경로**: `linkHandler.redirect` → `Service.Resolve(r.Context())` → `repo.IncrementClicks(ctx)` = `UPDATE links SET clicks = clicks + 1 WHERE code = $1`(`postgres.go:99-100`).
  - 요청 context를 쓰므로 **클라이언트가 끊기면 UPDATE도 취소된다.** 0012 `hot`의 `"클릭 집계 실패"` 260건이 모두 abort한 그 분에 몰린 것과 맞는다.
- **다음 병목 후보는 앱 CPU다.** `spread` 1600에서 57%였고, 선형으로 늘리면 약 2,600 rps에서 90%다. 그래서 재측정은 **3200 rps 단계까지** 늘린다.
- **Redis는 넣지 않는다.** 0012에서 `read`(조회만)가 1600 rps에서 p95 2.4ms였다. 조회 병목은 보이지 않았다(AGENTS.md §5).
- 인프라는 꺼져 있다. 측정할 때만 켠다.
  - **AGENTS.md 가드레일**: `terraform apply`·`destroy`와 리소스 생성은 사람이 한다. 에이전트는 `fmt`·`validate`까지만 하고, `terraform plan`도 사람이 실행한다(0011·0012 선례).
- AWS로 가는 설명 문자열(알람 `alarm_description`, IAM `sid`)은 ASCII만 쓴다. 한글이 들어가면 apply에서 실패한다(메모리: aws-descriptions-ascii-only).
- **공개 저장소다.** 측정 클라이언트의 집 IP는 0012와 같은 방식(`-var`, sensitive)으로만 다룬다.

### 더 단순한 대안과 고르지 않은 이유

**큐 없이 태스크 안에서 모아 DB에 묶어 쓰기만 해도** 행 잠금 경합은 같은 정도로 풀린다. 코드도 절반쯤이다.
그래도 큐를 넣는 이유는 2026-10-08 로드맵 결정 때문이다.

- **DB 장애 버퍼:** DB가 잠깐 죽어도 클릭이 큐에 남는다. 자동으로 버티는 시간은 **약 2.5분**(가시성 30초 × 5회)이다.
  그 뒤에는 DLQ로 옮겨지고 사람이 재처리한다(보존 14일, 런북에 명령을 적는다).
- **워커 분리:** 워커를 다른 곳에서 돌릴 수 있다. P6의 "워커를 집으로 옮겨 VPN 끊김 실험"이 이 분리에 기댄다.
- **인터페이스 연습:** 큐를 인터페이스 뒤에 두는 원칙을 처음으로 실제 코드로 지킨다.

## 설계

### 흐름

```
리다이렉트 ─▶ Batcher.Record(code)       메모리 맵에 +1 (DB·네트워크 없음)
                 │ 1초마다(또는 종료 시). 빈 버퍼는 보내지 않는다
                 ▼
            Queue.Publish(Batch{id, counts})   ── SQS 메시지 1개(코드 최대 1,000개)
                 │ 실패하면 그 배치를 ID 그대로 재시도 목록에 둔다
                 ▼
                 ▼
            Worker: Receive(최대 10개, 롱폴링 20초)
                 → repo.ApplyClickBatch(id, counts)   한 트랜잭션: 중복 확인 + 코드별 UPDATE
                 → 성공하면 Ack(삭제), 실패하면 그대로 둔다(가시성 타임아웃 뒤 재전달)
```

- **태스크 쪽에서 먼저 모은다.**
  - 클릭마다 메시지를 보내면 1600 rps에서 SQS 요청이 시간당 576만 건이다(약 2.3달러/시간). 리다이렉트 경로에 네트워크 왕복도 생긴다.
  - 1초씩 모으면 태스크당 초당 메시지 1개다. `hot`에서는 1,600번의 UPDATE가 **초당 UPDATE 1번(`clicks + 1600`)**이 된다.
- **워커는 웹 태스크 안의 고루틴으로 돌린다.** 새 ECS 서비스를 만들지 않는다.
  - 역할 분리(같은 이미지, 다른 실행 인자)는 0014(Helm)에서 한다. 그쪽이 훨씬 싸다.
- **정확히 한 번 반영한다.** SQS 표준 큐는 최소 한 번 전달이라 같은 메시지가 두 번 올 수 있다.
  - `ApplyClickBatch`는 같은 트랜잭션에서 `click_batches`에 배치 ID를 넣는다(`ON CONFLICT DO NOTHING`).
  - 이미 있으면(0행) UPDATE 없이 성공으로 끝낸다. 그래서 반영 후 삭제 전에 죽어 재전달돼도 두 번 세지 않는다.
  - **발행 재시도도 같은 ID로 한다.** SDK가 재시도를 다 쓰고 오류를 돌려줬어도 그중 한 번은 실제로 들어갔을 수 있다(응답 타임아웃).
    실패한 배치를 새 클릭과 합쳐 새 ID로 보내면 같은 클릭이 두 ID로 두 번 반영된다. 그래서 실패한 배치는 ID와 개수를 그대로 재시도 목록에 두고, 다음 주기에 같은 ID로 다시 보낸다.
    새 클릭은 새 배치로 나간다.
- **교착을 막는다.** 두 워커가 겹치는 코드를 다른 순서로 잠그면 교착이 난다. 코드를 **정렬한 순서로** UPDATE한다.
- **없는 코드는 건너뛴다.** UPDATE가 0행이면 반영할 곳이 없는 것이다. 배치는 성공으로 처리하고 통계에 센다.
  - 중단된 링크는 지금처럼 `Resolve`가 기록 전에 410을 돌려주므로 큐에 들어가지 않는다.

### 코드 구조

| 위치 | 내용 |
| --- | --- |
| `links` | `ClickRecorder` 인터페이스(`Record(code string)`)를 `Service`가 주입받는다. `Repository.IncrementClicks`를 **`ApplyClickBatch(ctx, batchID, counts) (applied bool, err)`로 바꾼다**(Postgres·메모리 둘 다). 안 쓰게 되는 `IncrementClicks`는 지운다 |
| `clicks`(신규) | `Batch`, `Queue` 인터페이스(`Publish`·`Receive`·`Ack`), `MemoryQueue`(로컬·테스트), `Batcher`, `Worker`, 10초 통계 로그. `links`를 import하지 않는다(워커는 자기 쪽 `Applier` 인터페이스로 저장소를 받는다) |
| `clicks/sqsqueue`(신규) | SQS 어댑터. **AWS SDK는 이 패키지에만 들어간다.** SDK 호출은 작은 인터페이스(`SendMessage`·`ReceiveMessage`·`DeleteMessage`) 뒤에 둬 가짜로 테스트한다. 리전은 `AWS_REGION` |
| `db/schema.sql` | `click_batches (id TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())` + `applied_at` 인덱스. PK가 있어 논리 복제 조건(P6)을 지킨다 |
| `config` | `CLICK_QUEUE_URL`. 비면 메모리 큐(로컬). **`APP_ENV=production`인데 비어 있으면 기동 오류**다 — 운영에서 조용히 메모리 큐로 떠 태스크마다 따로 노는 사고를 막는다(DB와 같은 fail-fast) |
| `main` | 큐 선택, Batcher·Worker 기동. 종료 순서: HTTP 종료(진행 중 요청 마무리) → Batcher 마지막 flush → Worker 중지 → 풀 닫기 |

고정값(상수, env로 열지 않는다):

| 항목 | 값 |
| --- | --- |
| flush 주기 | 1초 |
| 메시지당 코드 | 최대 1,000개(256KB 한도 안쪽) |
| 롱폴링 | 20초 |
| 한 번에 받는 메시지 | 최대 10개 |
| 대기 중 코드 상한 | 10만 개(모으는 버퍼 + 재시도 목록 합). 넘으면 버리고 센다 — SQS가 오래 죽어도 메모리가 무한히 늘지 않게 |
| 마지막 flush 상한 | 5초(SDK 재시도 포함) |
| `click_batches` 보존 | **21일**. 워커가 1시간마다 지운다. 근거는 아래 |

`click_batches` 보존이 21일인 이유: 반영은 됐는데 삭제 응답이 유실되고, 이어진 수신이 DB 장애로 모두 실패하면 이미 반영된 메시지가 DLQ로 간다.
표준 큐는 DLQ로 옮겨도 **처음 넣은 시각을 기준으로 만료**되고, **DLQ 재처리는 새 메시지(새 enqueue 시각)로 다시 넣는다**(AWS SQS 개발자 안내서 "Understanding message retention periods for dead-letter queues", "configure a dead-letter queue redrive").
그래서 이런 메시지는 처음 보낸 뒤 DLQ 보존 14일 + 재처리 뒤 본 큐 4일까지 다시 들어올 수 있다. 21일은 여기에 여유를 둔 값이다.
중복 판단은 SQS 메시지 ID가 아니라 본문의 배치 ID로 하므로 재처리로 메시지 ID가 바뀌어도 걸러진다.

### 실패와 유실 창

| 상황 | 동작 | 유실 |
| --- | --- | --- |
| Publish 실패(SQS 장애) | 그 배치를 **ID 그대로** 재시도 목록에 두고 다음 주기에 같은 ID로 재발행. `"클릭 발행 실패"` 로그 | 없음(상한 10만 코드 안에서) |
| 반영 실패(DB 장애) | Ack하지 않는다 → 30초 뒤 재전달 → 5번 실패하면(약 2.5분) DLQ. `"클릭 반영 실패"` 로그 | 없음(DLQ에서 수동 재처리, 런북) |
| 메시지 해석 실패(깨진 JSON) | Ack하지 않는다 → DLQ → 알람 | 그 배치 |
| 태스크 정상 종료(배포·축소) | SIGTERM → HTTP 종료 → 마지막 flush(상한 5초) → 워커 중지 | 없음 |
| 태스크 강제 종료(OOM·SIGKILL) | — | **최대 1초분 + 발행 못 한 버퍼** |
| 클라이언트가 끊김 | 기록은 응답 전에 메모리에서 끝난다. 요청 context와 무관 | 없음(0012의 260건 경로가 사라진다) |

- **통계 조회는 1~2초 늦게 반영된다.** 링크 통계 API와 운영자 화면의 클릭 수가 그만큼 늦다. 운영자 화면에는 문제가 없다.
- **종료 시간 상한.** `ecs.tf`에 `stopTimeout`이 없어 SIGTERM 뒤 30초면 SIGKILL이다. HTTP 종료(최대 10초) + 마지막 flush(최대 5초) + 워커 중지가 그 안에 든다.
  워커의 롱폴링 `Receive`는 ctx 취소로 바로 빠져나온다. 지금 `main.go`는 `srv.Shutdown` 오류 때 `os.Exit(1)`로 나가 버리는데, 오류가 나도 **마지막 flush는 하고** 나가게 바꾼다.

### 관측

- **통계 로그:** 10초마다 `click queue` 한 줄. 필드는 아래와 같고, 앞의 `db pool` 로그와 같은 방식이다. 0015(Prometheus) 전까지 쓴다.
  - 기록·발행: `recorded_clicks`(Record 호출 수), `published_batches`, `published_clicks`, `publish_errors`, `pending_codes`(버퍼 + 재시도 목록), `dropped_clicks`
  - 반영: `applied_batches`, `applied_clicks`, `duplicate_batches`, `unknown_codes`, `apply_errors`
- **SQS 지표:** `ApproximateAgeOfOldestMessage`, `NumberOfMessagesSent`, `ApproximateNumberOfMessagesVisible`(본 큐·DLQ).
- **알람(Slack·SMS 기존 토픽):**
  - DLQ에 메시지가 1개라도 있음
  - 본 큐의 가장 오래된 메시지가 5분을 넘음 — 워커가 멈췄다는 뜻. 메시지가 없을 때는 지표가 비므로 `treat_missing_data = "notBreaching"`
  - 표준 큐는 3번 넘게 받고도 지워지지 않은 메시지를 큐 뒤로 보내고, 이 지표는 그 메시지를 빼고 계산한다(같은 안내서). 그래서 깨진 메시지는 이 알람이 아니라 DLQ 알람으로 잡힌다

### 인프라

- `infra/prod/sqs.tf`(신규):
  - 본 큐: 표준, SSE(SQS 관리 키), 가시성 30초, 보존 4일, 롱폴링 20초, 재전달 정책 5회 → DLQ
  - DLQ: 보존 14일
  - 알람 2개(위 관측 참고)
- `iam.tf`: 태스크 역할에 `sqs:SendMessage`·`ReceiveMessage`·`DeleteMessage`를 **이 큐 ARN 하나로만** 준다. 앱은 큐 속성을 읽지 않는다(지표는 CloudWatch에서 본다).
  DLQ 재처리는 사람이 CLI로 한다(`aws sqs start-message-move-task --source-arn <DLQ ARN>`). 이 명령을 `docs/runbooks/alarm-response.md`의 DLQ 알람 항목에 적는다.
- `ecs.tf`: env `CLICK_QUEUE_URL` 추가. 비밀이 아니다.
- **네트워크는 그대로 쓴다.** 태스크는 기존 NAT·443 egress(`app_https`)로 SQS 공개 엔드포인트에 닿는다.
  VPC 엔드포인트는 시간당 요금이 붙고, 지금 트래픽(초당 메시지 몇 개)에서는 이득이 없다.

### 재측정

- **`capacity.js`:**
  - 단계표에 **3200 rps**를 더한다. 기본 `MAX_RPS`는 1600으로 두고(0012 재현용), 0013은 `MAX_RPS=3200`으로 돌린다.
  - `teardown`은 DB 합계가 **6초 동안 변하지 않을 때까지(최대 2분)** 기다렸다가 찍는다. 이제 반영이 비동기라서다.
    k6 기본 `teardownTimeout`(60초)에 먼저 걸리지 않게 `teardownTimeout: '3m'`을 함께 둔다.
  - `preAllocatedVUs`를 100 → 200으로 올린다. 0012 `spread` 1600에서 VU 확보 지연으로 `dropped` 144건이 났고 3200에서는 더 는다.
    서버 부하는 바뀌지 않는다(열린 모델의 요청률은 같다). 0012와 다른 점으로 결과 문서에 적는다.
- **`collect-metrics.sh`:**
  - SQS 열을 더한다: 가장 오래된 메시지 나이, 보낸 메시지 수.
  - 실패 집계를 `"클릭 발행 실패"`·`"클릭 반영 실패"`로 바꾸고, `click queue` 통계의 `pending_codes` 최대·`dropped_clicks`를 더한다.
  - **정합성 줄을 더한다:** 실행 전체(시작 ~ 수집 시각)의 `sum(recorded_clicks)`·`sum(dropped_clicks)`·`sum(applied_clicks)`와 `teardown`이 찍은 DB 합계.
    Logs Insights 별칭은 필드 이름과 다르게 짓는다(ops-gotchas G-12).
  - 같은 단계표 규칙을 3200까지 넓힌다.
- **판정은 0012와 같은 판정표로 한다.** 거기에 아래를 더 본다.
  - **(a) `hot`:** 800 rps에서 p95 < 100ms이고 무릎이 사라졌다. 한계가 `spread`와 같은 단계까지 올라왔다.
  - **(b) 정합성(모든 실행, abort 포함):** 큐가 빈 뒤 `sum(recorded_clicks) − sum(dropped_clicks)` = DB 합계(유실 0·중복 0). 서버 쪽 값이라 abort·타임아웃과 무관하게 성립해야 한다.
    보조로 k6 쪽 `redirects_302 ≤ DB 합계 ≤ redirects_302 + (302가 아닌 응답 + 타임아웃)`도 본다.
  - **(c) 다음 병목:** 3200까지 가는 동안 판정표의 어느 행이 먼저 걸리는지 본다. 예상은 앱 CPU(2번)다.
- 발생기: 3200 rps면 k6 CSV가 초당 약 4만 5천 행이다. 판정 0번(노트북 CPU, `RequestCount` 미달 + 지연 낮음)에 걸리면 그 단계는 무효로 하고, "마지막 유효 단계 이상"으로 적는다.

## 실행 단계

1. **저장소: 배치 반영** — `schema.sql`에 `click_batches`를 추가한다. `ApplyClickBatch`를 Postgres·메모리 두 곳에 구현하고 `IncrementClicks`를 지운다.
   → 검증: 메모리 구현 단위 테스트(같은 배치 두 번 → 한 번만 반영, 없는 코드 건너뜀).
   통합 테스트(`-tags=integration`, compose DB) [사람]:
   - 같은 배치 ID 중복 반영
   - 겹치는 코드 50개를 담은 배치 두 개를 200번 동시에 반영 → 교착 오류 0, 합계 정확.
     `counts`는 map이라 순서를 정할 수 없다(순회 순서가 무작위). 겹치는 코드를 많이 넣어, 정렬을 지우는 변이에서 교착이 거의 확실히 나게 한다
   **변이 검사:** 중복 확인을 지우면 중복 테스트가, 코드 정렬을 지우면 동시성 테스트가 실패해야 한다.
2. **`clicks` 패키지** — `Batch`·`Queue`·`MemoryQueue`·`Batcher`·`Worker`·통계 로그.
   → 검증: `-race`를 붙인 단위 테스트. 시간은 주입한 tick·채널로 진행해 결정적으로 만든다.
   - 모으기·1,000개 분할
   - 종료 시 마지막 flush
   - Publish 실패 → **같은 ID로** 재발행, 새 클릭은 새 배치
   - 빈 버퍼는 발행하지 않음
   - 반영 성공 뒤에만 Ack, 실패·해석 실패면 Ack 없음
   - 10만 상한(버퍼 + 재시도 목록) 초과는 버리고 셈. `recorded_clicks − dropped_clicks = published_clicks + pending`
   - 워커의 `Receive`가 ctx 취소로 바로 돌아옴, 마지막 flush가 5초 상한을 지킴
   **변이 검사:** 종료 flush, 재시도 때 같은 ID(새 ID를 만들게 바꾸면 실패), Ack 순서, 빈 버퍼 미발행을 하나씩 되돌리면 각 테스트가 실패해야 한다.
3. **연결(메모리 큐만)** — `Service`가 `ClickRecorder`를 쓰게 하고, `main`에서 메모리 큐로 Batcher·Worker를 띄우고 종료 순서를 맞춘다.
   `srv.Shutdown`이 오류여도 마지막 flush는 하고 나간다. SQS 선택은 단계 4에서 넣는다.
   → 검증:
   - 서비스 테스트: `Resolve`는 기록하고, 중단·없는 링크는 기록하지 않는다
   - **동기 반영을 전제한 기존 테스트를 고친다.** `links/service_test.go`의 `TestResolveCountsClicks`, `httpapi/links_test.go`의 `TestStatsCountsClicks`는 리다이렉트 직후 클릭 수를 본다.
     가짜 recorder로 "기록 호출"을 확인하게 바꾼다. 나머지 핸들러 테스트는 그대로 통과해야 한다
   - `go vet`·`go vet -tags=integration`
4. **SQS 어댑터와 큐 선택** — `clicks/sqsqueue`, `config`의 `CLICK_QUEUE_URL`(운영 필수), `main`에서 값이 있으면 SQS·없으면 메모리 큐.
   → 검증: 가짜 SQS 클라이언트로 단위 테스트(인코딩 왕복, 수신 → 해석 → Ack 호출 인자, 해석 실패 시 Ack 없음, 롱폴링·최대 10개 인자, ctx 취소 시 수신 중단).
   config 테스트: 운영인데 비면 오류.
   진짜 SQS는 단계 9의 스모크에서 처음 닿는다. ElasticMQ 같은 로컬 SQS는 넣지 않는다(범위).
5. **인프라와 런북** — `sqs.tf`, `iam.tf` 문장 추가, `ecs.tf` env. `alarm-response.md`에 두 알람의 대응과 DLQ 재처리 명령을 적는다(재처리는 21일 보존 안에서).
   → 검증: `terraform fmt`·`validate`.
   `terraform plan` [사람]: 인프라가 꺼져 있어 전체 생성으로 나온다. 큐 2개, 재전달 정책, 알람 2개, IAM 문장이 들어 있는지 리소스 이름으로 확인한다. **apply는 하지 않는다.**
6. **부하·수집 도구** — `capacity.js`(3200 단계, drain 대기), `collect-metrics.sh`(SQS 열·새 실패 로그), README.
   → 검증: `k6 inspect`, `bash -n`, shellcheck, 가짜 `aws`로 `collect-metrics.sh`를 끝까지 실행.
7. **로컬 E2E** [사람] — compose(메모리 큐)에서 세 시나리오를 `MAX_RPS=100`으로 돌린다.
   → 검증: 429 0건. `spread`·`hot`은 `redirects_302` = DB 합계(drain 뒤, 오류 0인 실행이라 이것도 성립한다)이고 `sum(recorded_clicks) − sum(dropped_clicks)` = DB 합계. `read`는 DB 0.
   로그에 `click queue` 줄과 `duplicate_batches=0`.
8. **코드 검토 1라운드(claude-ide) → 커밋·PR·머지 [사람].** 인프라가 꺼져 있어 머지 후 배포는 실패한다 — 정상이다.
9. **측정 창 [사람]** — 0012 절차와 같다(`load/k6/README.md` 운영 측정). 약 2시간 20분이다.
   1. `full-apply-prod.sh --trigger-deploy`. SQS·IAM·env도 이때 함께 만들어진다.
   2. 측정 예외 + SMS 끄기 apply → 배포 → 기동 로그 `rate_limit_exempt_count=1`.
   3. **스모크:** 링크 하나를 만들어 리다이렉트를 3번 한 뒤 5초 안에 통계 API의 클릭이 3인지 본다. `click queue` 로그에 `applied_batches`가 늘었는지 본다.
      실패하면 측정하지 않고 멈춘다.
   4. `spread` → `hot` → `read`를 `MAX_RPS=3200`으로 실행하고 실행마다 수집한다(시나리오당 최대 21분).
   5. 결과 파일 확인 → `full-destroy-prod.sh --drop-dev-db`.
   → 검증: 세 실행 모두 k6 요약·CSV·수집 표·원본 응답이 있고, 판정에 쓴 단계가 판정 0번에 걸리지 않았다.
10. **결과 기록** — `load/k6/results/0013-async-clicks.md`. 0012와 단계별로 나란히 비교하고, 판정 (a)·(b)·(c)와 다음 병목을 적는다.
    정합성 표에는 실행마다 서버 불변식 줄과 k6 보조 줄을 함께 둔다. 0014로 넘길 결론을 적는다.
   → 검증: 표의 숫자가 원자료와 일치한다. docs PR [사람].

검토: 사용자 확인 → plan 검토 1라운드(claude-ide) → 구현 → 코드 검토 1라운드 → 측정. 블로커가 없으면 라운드를 더 열지 않는다.

## 리스크/롤백

| 리스크 | 완화 | 롤백 |
| --- | --- | --- |
| 강제 종료 시 최대 1초분 클릭 유실 | 정상 종료 경로에서 마지막 flush. 유실 창을 문서에 명시 | — (설계상 감수) |
| SQS 장기 장애로 버퍼가 커짐 | 대기 코드 10만 상한, 넘으면 버리고 `dropped_clicks`로 센다. 리다이렉트는 영향 없음 | — |
| 중복 반영 | `click_batches` 배치 ID로 걸러 냄(같은 트랜잭션). 발행 재시도도 같은 ID. 보존 21일 > DLQ 14일 + 재처리 뒤 본 큐 4일 | — |
| 워커 교착 | 코드 정렬 순서로 UPDATE. 동시성 통합 테스트로 확인 | — |
| 통계가 1~2초 늦음 | 운영자 화면 용도에는 문제없음. 결과 문서에 명시 | — |
| 진짜 SQS는 측정 창에서 처음 닿음 | 가짜 클라이언트 단위 테스트 + 단계 9-3 스모크에서 멈춤 | 이전 이미지 재배포(이전 앱은 `CLICK_QUEUE_URL`을 무시하고 동기 UPDATE로 돈다). 큐에 남은 메시지는 보존 4일 안에 새 버전이 다시 처리한다 |
| 3200 rps에서 발생기 포화 | 판정 0번으로 무효 처리, "마지막 유효 단계 이상"으로 기록 | 필요하면 같은 리전 EC2 발생기를 별도 plan으로 |
| 비용 | SQS는 태스크당 초당 메시지 1개 수준이라 무시할 만하다(월 100만 요청 무료 범위 근처). 측정 창은 0012와 같다 | — |
| 운영에서 큐 설정 누락 | `APP_ENV=production` + 빈 `CLICK_QUEUE_URL` → 기동 실패 → 서킷브레이커가 이전 리비전으로 되돌린다 | 같은 경로 |

## 검토 반영 로그

<!-- /plan-merge가 라운드별로 기록. 형식: [rN] 리뷰어#번호 지적요약 → 반영|기각 — 사유 -->

리뷰어는 claude-ide 1인이다(codex-cli·codex-ide는 2026-10-06부터 사용 불가). 개선 제안은 결함 다음 번호(#5~#13)로 매겼다.

- [r1] claude-ide#1 발행 실패 때 버퍼로 되돌리면 배치 ID가 사라져 중복을 못 막는다 → 반영 — 실패한 배치를 ID 그대로 재시도 목록에 두고 같은 ID로 재발행. 상한에 재시도 목록 포함, 단계 2 테스트·변이 검사에 추가
- [r1] claude-ide#2 응답 전에 기록하므로 `redirects_302` = DB는 포화 단계에서 성립하지 않는다 → 반영 — 판정 (b)·목표 3을 서버 불변식 `sum(recorded) − sum(dropped)` = DB로 바꾸고 k6 쪽은 범위 보조 판정으로. `recorded_clicks` 통계, 수집 정합성 줄, 결과 문서 표에 추가
- [r1] claude-ide#3 teardown 2분 대기가 기본 `teardownTimeout` 60초에 먼저 걸린다 → 반영 — `teardownTimeout: '3m'`
- [r1] claude-ide#4 DLQ 메시지를 7일 뒤 재처리하면 중복 반영 → 반영(보존 연장 쪽) — AWS 문서로 확인했다. 표준 큐는 DLQ로 옮겨도 처음 넣은 시각 기준으로 만료되고, 재처리는 새 enqueue 시각으로 다시 넣는다. 리뷰어가 제안한 15일로는 재처리 뒤 본 큐 4일을 못 덮어 21일로 정했다. 런북에 재처리 명령
- [r1] claude-ide#5 종료 시간 상한(SIGKILL 30초)과 `srv.Shutdown` 오류 때 flush 건너뜀 → 반영 — 마지막 flush 상한 5초, `Receive` ctx 취소, 오류여도 flush 후 종료
- [r1] claude-ide#6 IAM `sqs:GetQueueAttributes`는 부르는 곳이 없다 → 반영 — 뺐다
- [r1] claude-ide#7 "DB 장애 버퍼"가 실제로는 약 2.5분 → 반영 — 문구를 고치고 DLQ 재처리를 런북(`alarm-response.md`)에
- [r1] claude-ide#8 빈 버퍼 미발행 명시, 오래된 메시지 알람 `notBreaching` → 반영 — 흐름·관측·단계 2 테스트에 넣었다. 3회 넘게 받은 메시지가 이 지표에서 빠진다는 AWS 문서 내용도 적었다
- [r1] claude-ide#9 단계 3이 단계 4의 SQS 어댑터에 기댄다 → 반영 — 단계 3은 메모리 큐만, SQS 선택·config는 단계 4로
- [r1] claude-ide#10 동기 반영을 전제한 기존 테스트 2개 → 반영 — 단계 3에서 가짜 recorder로 바꾼다고 적었다
- [r1] claude-ide#11 map이라 "반대 순서" 배치를 만들 수 없다 → 반영 — 겹치는 코드 50개로 교착이 거의 확실히 나게
- [r1] claude-ide#12 `preAllocatedVUs` 200 → 반영 — 서버 부하는 같다는 점과 0012와의 차이를 결과 문서에 적는다
