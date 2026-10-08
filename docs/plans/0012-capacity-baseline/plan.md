---
status: approved # in-review | approved
revision: 2
created: 2026-10-08
---

# 0012. 한계 측정 — 지금 ECS 구성의 처리 한계와 첫 병목

## 목표

P5(AWS 완성)의 첫 단계다. **고치지 않고 잰다.**

1. 지금 구성의 **처리 한계**를 숫자로 남긴다. 기준을 만족하는 최대 RPS와 그때의 p95·p99·오류율이다.
2. **첫 병목이 어디인지 근거로 가린다.** 후보는 앱 CPU, DB 연결 풀, RDS CPU, 행 잠금 경합, 부하 발생기다.
3. 이 숫자가 0013(클릭 집계 비동기화)과 0014(EKS) 뒤 재측정의 **기준선**이 된다. 0013의 범위는 이 결과로 확정한다.

## 배경/제약

- **한계를 잰 적이 없다.** 지난 k6(PR #22)는 초당 8건에서 p95 10.24ms였다. 순간 부하는 레이트리밋이 429로 막았다(의도한 검증).
- **레이트리밋은 IP·태스크별 인메모리다**(`ratelimit.go`, 읽기 300/분·버스트 100). 한 대의 클라이언트로 한계를 재려면 예외가 필요하다.
  한도는 코드 상수뿐이고 env로 바꿀 수 없다(`main.go:101` — `RateLimit` 미지정 = 운영 기본값).
- **리다이렉트 1회 = SELECT 1번 + 같은 행 UPDATE 1번**이다(`service.go:59-72` → `postgres.go:99-100`). 인기 링크 하나에 몰리면 행 잠금 경합이 의심된다.
  클릭 집계 실패는 로그만 남기고 리다이렉트는 성공시킨다(`service.go:68-70`). 그래서 고부하에서 **클릭이 조용히 유실될 수 있다.**
- 사양: 태스크 0.25 vCPU·512MB × 2(`variables.tf` `task_cpu`·`service_desired_count`), 연결 풀 태스크당 25(`db.go:63`), RDS `db.t4g.micro`.
  - db.t4g는 **Unlimited 모드**다. 기준선을 넘는 CPU는 24시간 창에서 추가 과금된다(AWS RDS 문서 "DB instance class types").
    새로 만든 RDS라 크레딧 잔고가 적으니, 고부하 구간은 소액 과금을 감안한다.
- 관측 수단: Container Insights가 켜져 있어 태스크 CPU·메모리가 보인다(`enable_container_insights` 기본 true).
  RDS 잠금 대기는 CloudWatch 기본 지표로 직접 보이지 않는다. 그래서 **실험 설계로 가린다**(같은 RPS에서 집중과 분산을 비교).
  앱 쪽 연결 풀 대기는 지금 어디에도 안 보인다 → 이 plan에서 로그로 연다.
- 인프라는 꺼져 있다(2026-10-08 destroy). 측정 때만 켜고 끝나면 내린다.
- **AGENTS.md 가드레일**: `terraform apply`·`destroy`, 인프라 생성·삭제는 사람이 한다. 에이전트는 `terraform plan`까지만 한다.
- 부하 발생기는 노트북 한 대다. 응답이 작아(302) 회선은 충분할 것으로 보지만, **발생기가 병목이 아닌지 매 단계 확인한다**.
  지연의 1차 지표는 서버 쪽 ALB `TargetResponseTime`이다. 가정 회선의 잡음을 빼기 위해서다.
- **공개 저장소다.** 측정 클라이언트의 집 IP를 커밋·PR·로그·대화에 남기지 않는다.

## 설계

### 측정 클라이언트만 레이트리밋 예외

- env `RATE_LIMIT_EXEMPT_IPS`(쉼표로 구분한 IP, 정확히 일치)를 둔다. 그 IP는 모든 티어 한도를 건너뛴다. **다른 IP는 그대로 막힌다.**
  - 클라이언트 IP는 기존 `clientIP`를 그대로 쓴다. ALB가 붙인 XFF 맨 오른쪽 값이라 위조할 수 없다. 그래서 XFF 왼쪽을 꾸며도 예외를 얻을 수 없다.
  - 잘못된 IP가 들어오면 기동 오류로 멈춘다. 기동 로그에는 개수만 찍는다(`rate_limit_exempt_count=N`). IP는 찍지 않는다.
  - 전체를 끄지 않는 이유: 측정하는 1~2시간 동안에도 다른 IP에 대한 보호를 유지한다.
- Terraform 변수 `rate_limit_exempt_ips`(기본 `""`, `sensitive`)를 두고, 값이 있을 때만 env를 넣는다(관리자 해시와 같은 조건부 `concat`).
  **tfvars에는 쓰지 않는다.** 측정할 때 `-var`로만 준다. 측정이 끝나면 destroy하므로 라이브에는 남지 않는다.
  변수에 IPv4 목록 형식 `validation`을 둔다. 잘못된 값이 기동 실패·서킷브레이커 롤백까지 가지 않고 plan에서 걸리게 하기 위해서다.
- **로컬 검증 경로**: 로컬에는 ALB가 없어, XFF 맨 오른쪽 값을 클라이언트가 정한다(`clientIP`는 XFF가 없을 때만 `RemoteAddr`로 폴백, `ratelimit.go:214-229`).
  그래서 `capacity.js`는 `__ENV.LOCAL_XFF`가 있을 때만 `X-Forwarded-For`를 붙인다. `docker-compose.yml`은 `RATE_LIMIT_EXEMPT_IPS: ${RATE_LIMIT_EXEMPT_IPS:-}`로 넘긴다.
  값은 문서용 IP(`198.51.100.7`)를 쓴다. 운영에서는 ALB가 진짜 IP를 맨 뒤에 붙이므로, 이 헤더가 실수로 붙어도 무시된다(단계 1의 "XFF 왼쪽 위조" 테스트가 보장).

### DB 연결 풀 통계 로그

- 10초마다 `sql.DBStats`를 한 줄 로그로 남긴다: `in_use`, `idle`, `max_open`, 그리고 직전 로그 이후의 `wait_count`·`wait_ms` 증가분.
  풀이 찼다는 **사실**을 보여 준다. **이유**(쿼리가 느려져서인지, 풀이 작아서인지)는 보여 주지 않으므로 판정표에서 다른 신호와 함께 읽는다.
  CloudWatch Logs Insights로 집계한다.
- 상시 켠다. 태스크당 분당 6줄이라 비용은 무시할 만하다. 0015(Prometheus) 전까지 쓰는 임시 관측 수단이기도 하다.

### 부하 시나리오 — `load/k6/capacity.js` (신규)

- 실행기는 `ramping-arrival-rate`(열린 모델)다. 서버가 느려져도 요청률이 유지돼야 한계가 드러난다.
  `preAllocatedVUs` 100, `maxVUs` 500. 1600 rps에서 평균 지연 약 300ms까지 버틴다. 그보다 느리면 그 단계는 이미 p95 기준에서 불합격이다.
  `maxRedirects: 0` — 빠지면 부하가 대상 URL(example.com)로 간다(`linkpulse.js:20-21`과 같은 이유).
- 단계: **50 → 100 → 200 → 400 → 800 → 1600 rps**, 단계마다 **30초 올리고 2분 30초 유지(= 3분)**.
  `setup`이 끝나면 다음 정각(:00초)까지 기다렸다가 시나리오를 시작한다(`setupTimeout` 3분). 그러면 단계마다 유지 구간이 CloudWatch의 온전한 1분 두 개를 정확히 덮는다.
  **단계 값 = 각 단계 유지 구간의 온전한 2분.** 램프가 섞인 분은 뺀다.
- 안전 정지(임계값): 오류율 5% 초과, p95 1초 초과, `status_429` 1건 이상에 `abortOnFail`을 건다.
  k6 임계값은 실행 시작부터의 **누적값**이고 `delayAbortEval`은 첫 평가만 늦춘다. 그래서 이것은 안전 정지일 뿐, 단계 판정은 아래 분 단위 자료로 한다.
  `status_429`에 거는 이유: 예외 IP가 안 맞으면 바로 멈춰 원인이 드러나게 한다.
- k6는 `--out csv=<파일>`로 시계열을 남긴다. 단계별 `dropped_iterations`와 클라이언트 쪽 집계는 이 CSV에서 뽑는다(끝 요약은 실행 전체 합계라 단계별로 못 쓴다).
- 시나리오 3개를 따로 실행한다(`SCENARIO` env):

  | 시나리오 | 요청 | 보려는 것 |
  | --- | --- | --- |
  | `spread` | 링크 100개에 고르게 `GET /{code}` | 실제에 가까운 상한 |
  | `hot` | 링크 1개에 전부 `GET /{code}` | 인기 링크의 행 잠금 경합 |
  | `read` | 링크 100개에 `GET /api/links/{code}`(SELECT만, 쓰기 없음) | 대조군. 쓰기를 뺀 상한 |

- `setup`은 관리자 토큰으로 링크 100개를 만든다(예외 IP라 쓰기 한도와 무관). 토큰은 기존처럼 `read -rs`로 받는다.
- `teardown`은 DB의 클릭 합계만 찍는다. k6 스크립트는 메트릭 값을 읽을 수 없어서, 사람이 요약의 `redirects_302`와 비교한다(기존 `linkpulse.js:87-94` 방식).
  **"302 수 > DB 합계"일 때만 유실로 본다.** abort로 끝난 실행은 진행 중 요청이 끊겨 DB 합계가 302 수보다 클 수 있기 때문이다.
  서버 쪽 직접 신호는 Logs Insights의 `"클릭 집계 실패"` 건수다(`service.go:69`). 이게 "조용한 유실"의 1차 근거다.

### 한계의 정의

- **한계 = 아래를 모두 만족한 마지막 단계의 RPS.** 1600 rps에서도 만족하면 "≥1600"으로 적는다.
  서버 쪽 p95(ALB `TargetResponseTime`) < 100ms, 5xx 비율 < 1%, 발생기 정상(아래 판정 0번에 걸리지 않음).
- `dropped_iterations` > 0은 **서버 지연이 오른 상태라면 서버 포화**다. 필요한 VU(요청률 × 지연)가 `maxVUs`를 넘었다는 뜻이고, 그 단계는 불합격이다.
- **무릎**도 기록한다. p95가 직전 단계의 2배를 처음 넘고 **20ms 이상**인 단계다(수 ms대의 잡음을 무릎으로 잡지 않으려고).

### 첫 병목 판정 규칙

신호는 서로 원인과 증상이 섞인다. **위에서부터 처음 해당하는 행으로 판정한다.**

| # | 조건 | 판정 |
| --- | --- | --- |
| 0 | 노트북 CPU 포화, 또는 ALB `RequestCount`가 목표 RPS의 90%에 못 미치는데 `TargetResponseTime`은 낮음 | 발생기 한계 → 그 단계는 무효 |
| 1 | ELB 5xx(타깃 5xx 아님) | ALB 확장 지연 → 램프를 늦춰 재측정 |
| 2 | 태스크 CPU ≥ 90% | 앱 CPU(0.25 vCPU) |
| 3 | RDS CPU ≥ 90% | DB CPU |
| 4 | `hot` 한계가 `spread`보다 뚜렷이 낮음(같은 RPS에서 `hot` p95가 높음), RDS CPU는 여유 | 행 잠금 경합. **`hot`에서만 나타나는 풀 대기는 이 증상으로 본다** |
| 5 | 풀 대기가 `spread`·`read`에서도 나타남, `hot`≈`spread`, 연결 보유 시간(평균 `in_use` ÷ 태스크당 처리량)이 저부하 단계와 비슷함 | 연결 풀 크기(태스크당 25) |
| 6 | `read` 한계가 `spread`보다 훨씬 높음 | 쓰기(클릭 UPDATE) 비용이 지배 — 4·5와 함께 성립할 수 있다 |

풀 대기를 원인으로 보는 조건(5)을 좁힌 이유: 쿼리가 느려지면(행 잠금·DB CPU) 풀 대기는 따라 오르는 증상이다.
요청당 DB 보유가 수 ms면 태스크당 수백 rps에서도 동시 사용 연결은 몇 개뿐이라 25에 닿지 않는다(Little's law). 풀이 차는 건 거의 쿼리 시간이 몇 배로 늘었을 때다.

### 지표 수집 — `load/k6/collect-metrics.sh` (신규)

- 시나리오 시작 시각(정각)과 단계표를 받아, 분마다 단계 라벨을 붙이고 램프가 섞인 분은 빼서 표를 찍는다.
  콘솔을 손으로 뒤지지 않고, 0013·0014 재측정 때 같은 방식으로 비교하기 위해서다.
  - ALB: `RequestCount`, `TargetResponseTime` p95·p99, `HTTPCode_Target_5XX_Count`, `HTTPCode_ELB_5XX_Count`
  - ECS: 서비스 `CPUUtilization`·`MemoryUtilization`
  - RDS: `CPUUtilization`, `DatabaseConnections`, `CPUSurplusCreditsCharged`(Unlimited 과금 확인), `WriteLatency`, `ReadLatency`
  - Logs Insights: `db pool` 로그의 `wait_count`·`wait_ms` 합계와 `in_use` 최대, `"클릭 집계 실패"` 건수
  - k6 CSV: 단계별 `dropped_iterations`와 클라이언트 쪽 요청 수
- 읽기 전용 AWS 호출만 쓴다. 실행은 사람이 한다.

### 결과 기록 — `load/k6/results/0012-baseline.md`

구성 사양, 시나리오별 단계 표(RPS·p95/p99·오류·태스크 CPU·RDS CPU·연결·풀 대기·클릭 유실), 한계·무릎, 판정, 0013에 넘길 결론. **집 IP는 넣지 않는다.**

## 실행 단계

1. **레이트리밋 예외** — `config.go`에 `RATE_LIMIT_EXEMPT_IPS` 파싱, `RateLimitConfig.ExemptIPs`, 미들웨어에서 분류 전에 예외 확인.
   → 검증: 단위 테스트(예외 IP는 한도를 넘어도 통과, 다른 IP는 429, XFF 왼쪽 위조로 예외 못 얻음, 빈 값이면 동작 불변, 잘못된 IP면 `Load` 오류).
   **변이 검사**: 예외 분기를 지우거나, 예외 판정에 `clientIP` 대신 XFF 첫 값을 쓰면 테스트가 실패해야 한다.
2. **DB 풀 통계 로그** — 10초 주기, 종료 시 멈춤.
   → 검증: 증가분 계산 단위 테스트, `-race`. 로컬 `docker compose`에서 로그 한 줄 확인 [사람].
3. **`capacity.js`·`collect-metrics.sh`·`docker-compose.yml`·README 실행법.**
   → 검증: 로컬 compose에서 `RATE_LIMIT_EXEMPT_IPS=198.51.100.7`, `LOCAL_XFF=198.51.100.7`로 낮은 단계(50 → 100 rps)를 세 시나리오 모두 돌린다 [사람].
   429가 0건이고, 302 수와 DB 합계가 같아야 한다. `LOCAL_XFF`를 빼면 `status_429` 임계로 곧바로 멈추는지도 본다. `bash -n collect-metrics.sh`.
4. **인프라 변수** — `variables.tf`(`sensitive`, 기본 `""`, IPv4 목록 `validation`), `ecs.tf` 조건부 env.
   → 검증: `terraform fmt`·`validate`. `terraform plan`을 `-var` 없이 한 번, 문서용 IP(`-var rate_limit_exempt_ips=203.0.113.10`)로 한 번 돌린다.
   `container_definitions`가 sensitive라 값이 가려지므로, `terraform show -json`에서 **env 이름만** 뽑아 비교한다(값을 찍지 않는 `jq`).
   기대: `-var`가 있을 때만 `RATE_LIMIT_EXEMPT_IPS`가 생긴다. `-var rate_limit_exempt_ips=not-an-ip`는 plan에서 실패한다. **apply는 하지 않는다.**
5. **코드 검토 1라운드(claude-ide) → 커밋·PR·머지 [사람].** 인프라가 꺼져 있어 머지 후 배포는 실패한다 — 정상이다.
   PR에 지금 작업 트리의 `README.md`(다음 단계 문단)와 `docs/runbooks/ops-gotchas.md`(G-9)를 별도 `docs:` 커밋으로 함께 싣는다.
6. **측정 창 [사람]** — 2시간 안쪽. 시나리오당 최대 18분 × 3, 휴식 10분, 인프라 생성·배포·확장 약 25분, 측정 apply·배포 약 8분, destroy 약 15분이다.
   1. `./scripts/full-apply-prod.sh --trigger-deploy` — main 이미지로 인프라를 만든다.
   2. 측정 예외와 SMS 끄기를 함께 apply한다. IP는 화면에 찍지 않는다.
      `MYIP=$(curl -fsS https://checkip.amazonaws.com)`, 이어서 `[[ -n "$MYIP" ]]`로 값이 들어왔는지 확인한다(curl이 조용히 실패하면 빈 값으로 env가 빠진 채 apply가 성공한다).
      그다음 `terraform apply -var "rate_limit_exempt_ips=$MYIP" -var "alarm_sms_number="`.
      측정 중에는 알람이 울리는 게 정상이라 SMS만 끈다(Slack은 남긴다). 그다음 CI 배포를 한 번 돌려 새 태스크 정의를 반영한다.
   3. 기동 로그 `rate_limit_exempt_count=1`과 `db pool` 로그를 확인한다.
   4. k6를 `spread` → `hot` → `read` 순으로 `--out csv`와 함께 실행한다. 사이에 5분씩 쉬어 지표 구간을 나눈다.
      시나리오 시작 시각은 `setup`이 정각 맞춤 후 찍어 준다.
   5. `collect-metrics.sh`를 실행마다 돌린다(시작 시각·k6 CSV를 넘긴다).
   6. `./scripts/full-destroy-prod.sh --drop-dev-db` — `Destroy complete!`까지 컴퓨터를 끄지 않는다(ops-gotchas G-9).
   → 검증: 세 실행 모두 k6 요약·CSV·수집 출력이 있고, 판정에 쓴 단계가 판정 0번(발생기 한계)에 걸리지 않았다.
7. **결과 기록** — `0012-baseline.md`를 쓰고, 판정 규칙으로 첫 병목을 정한다. 0013의 범위를 확정한다(비동기화가 맞는지, 다른 병목이 먼저인지).
   → 검증: 표의 숫자가 원자료(k6 요약·수집 출력)와 일치한다. docs PR [사람].

검토: 사용자 확인 → plan 검토 1라운드(claude-ide) → 구현 → 코드 검토 1라운드 → 측정. 블로커가 없으면 라운드를 더 열지 않는다.

## 리스크/롤백

| 리스크 | 완화 | 롤백 |
| --- | --- | --- |
| 예외 IP가 운영에 남음 | tfvars에 쓰지 않고 `-var`로만 준다. 측정 뒤 destroy. 단 태스크 정의는 `skip_destroy = true`(`ecs.tf:18`)라 IP가 든 revision이 계정 안에는 남는다. 라이브에는 쓰이지 않고(다음 full-apply가 새 revision을 등록) 계정 밖으로 나가지 않는다 | 일반 apply로 env가 빠진다 |
| 노트북이 병목 | 단계마다 `dropped_iterations`·노트북 CPU 확인. 지연은 서버 쪽 지표로 판단 | 그 단계 무효. 필요하면 같은 리전 EC2 발생기를 별도 plan으로 |
| 새 ALB가 급증을 못 따라감(ELB 5xx) | 30초 램프 + 2분 30초 유지로 단계적으로 올린다. ELB 5xx를 타깃 5xx와 따로 기록 | 램프를 늦춰 재측정 |
| RDS 고부하 추가 과금(Unlimited) | 측정 창을 2시간 안쪽으로 제한. `CPUSurplusCreditsCharged`로 확인 | — |
| 측정 중 알람 폭주 | SMS를 측정 apply에서 끈다. Slack 알람은 예상된 것으로 본다 | destroy로 함께 사라진다 |
| destroy 도중 중단 | 끝까지 확인한다 | ops-gotchas G-9 절차(`errored.tfstate` → `state push`) |
| 집 IP 노출 | `sensitive` 변수, IP를 찍지 않는 명령, 로그에는 개수만, 결과 문서에 IP 없음 | — |

앱 변경은 env가 없으면 동작이 그대로라, 측정 뒤에 되돌릴 코드가 없다. 풀 통계 로그는 상시 유지한다.

## 검토 반영 로그

<!-- /plan-merge가 라운드별로 기록. 형식: [rN] 리뷰어#번호 지적요약 → 반영|기각 — 사유 -->

리뷰어는 claude-ide 1인이다(codex-cli·codex-ide는 2026-10-06부터 사용 불가). 개선 제안은 결함 다음 번호(#5~#13)로 매겼다.

- [r1] claude-ide#1 로컬 검증이 예외 IP를 걸 방법이 없어 돌지 않는다 → 반영 — 로컬은 XFF 맨 오른쪽을 클라이언트가 정한다는 점을 쓴다. `LOCAL_XFF`·compose 전달·문서용 IP, 단계 3 경로에 `docker-compose.yml` 추가
- [r1] claude-ide#2 판정표가 증상(풀 대기·dropped)을 원인으로 읽는다 → 반영 — 위에서부터 처음 해당하는 행으로 판정하는 우선순위 표로 바꿨다. 풀 원인 조건을 좁혔다. dropped는 서버 지연이 오른 상태면 서버 포화로 봤다. 발생기 한계는 노트북 CPU 또는 "RequestCount 미달 + 지연 낮음"으로 한정했다. `maxVUs` 500 명시
- [r1] claude-ide#3 단계별 판정 자료를 만드는 방법이 없다 → 반영 — 단계 3분(30초 램프 + 2분 30초 유지), `setup` 뒤 정각 맞춤, 단계 값 = 유지 구간의 온전한 2분, `--out csv`로 단계별 dropped
- [r1] claude-ide#4 `teardown`은 302 수를 읽을 수 없다 → 반영 — DB 합계만 찍고 사람이 비교, "302 > DB"만 유실, 서버 쪽 `"클릭 집계 실패"` 건수를 수집에 추가
- [r1] claude-ide#5 `maxRedirects: 0` 명시 → 반영
- [r1] claude-ide#6 `status_429`에 `abortOnFail` → 반영 — 예외 IP 불일치가 바로 드러나게
- [r1] claude-ide#7 임계값은 누적값이고 `delayAbortEval`은 첫 평가만 늦춘다 → 반영 — 문장을 "안전 정지일 뿐, 단계 판정은 분 단위 자료로"로 고쳤다
- [r1] claude-ide#8 단계 4의 `-var` 검증 값은 문서용 IP로 → 반영 — `203.0.113.10`
- [r1] claude-ide#9 `MYIP` 빈 값 확인 + 변수 `validation` → 반영 — `curl -fsS` + `[[ -n "$MYIP" ]]`, IPv4 목록 validation, 잘못된 값이 plan에서 실패하는지 단계 4에서 확인
- [r1] claude-ide#10 `skip_destroy`라 IP가 든 revision이 남는다 → 문장만 반영, deregister 단계 추가는 기각 — 계정 밖으로 나가지 않고 라이브에 쓰이지 않는다. 사람 손이 한 번 더 드는 변경형 명령을 넣을 만큼의 위험이 아니다
- [r1] claude-ide#11 시간 예산이 1.5시간을 넘는다 → 반영 — 2시간으로 고치고 내역을 적었다. 50 rps 단계는 남긴다(판정 5번의 "저부하 단계 연결 보유 시간" 기준점이라서)
- [r1] claude-ide#12 1600에서도 만족하면 "≥1600" / 무릎 절대 하한 → 반영 — "≥1600" 정의, 무릎은 20ms 이상일 때만
- [r1] claude-ide#13 과금 확인은 `CPUSurplusCreditsCharged`가 직접적 → 반영 — `CPUCreditBalance`를 대체
