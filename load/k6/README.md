# k6 부하 테스트 — 단일 클라이언트에서 프로덕션 확인

스크립트: [`linkpulse.js`](linkpulse.js). 대상: `https://lpulse.live`(`BASE_URL`로 바꿀 수 있다).
처리 한계 측정은 아래 [한계 측정](#한계-측정--capacityjs-plan-0012)의 [`capacity.js`](capacity.js)를 쓴다.

## 왜 "최대 처리량"을 재지 않나

앱에 **IP·태스크별 인메모리 레이트리밋**이 있다(`app/internal/httpapi/ratelimit.go`).
- 읽기: 분당 300, 버스트 100
- 쓰기: 분당 20, 버스트 10

노트북 한 대(IP 하나)로는 한도에 먼저 막힌다. 그래서 이 테스트는 **"한도 안에서 빠르고 정확한가, 한도 밖에서 안전하게 막히는가"**를 본다.

| 단계 | 부하 | 확인하는 것 | 합격선 |
| --- | --- | --- | --- |
| setup | 링크 5개 생성 | 쓰기 경로 | 전부 201 |
| steady (3분) | 리다이렉트 8 rps. 태스크 2개에 나뉘어 각각 약 240/분으로 한도 안 | 지연·오류율 | p95 < 500ms, 실패율 < 1%, 429 0건 |
| (휴식 30초) | — | 토큰 버킷 회복 | — |
| burst (30초) | 리다이렉트 30 rps. 한도 초과 | 레이트리밋 보호 | 429 > 0건, **5xx 0건** |
| teardown | 링크 5개 통계 조회 | 클릭 집계 정합성 | DB 클릭 합계 = `redirects_302` count |

리다이렉트는 따라가지 않는다(`maxRedirects: 0`). 따라가면 대상 사이트(example.com)를 부하 테스트하게 된다.

## 실행 [사람]

```bash
brew install k6
read -rs "ADMIN_TOKEN?관리자 토큰: "; echo
ADMIN_TOKEN="$ADMIN_TOKEN" k6 run load/k6/linkpulse.js; unset ADMIN_TOKEN
```

setup의 링크 생성은 관리자만 할 수 있다(plan 0011). 토큰은 env로만 넘기고 파일에 적지 않는다.
`read -rs`로 받는 이유: 명령줄에 원문을 쓰면 셸 히스토리에 남는다.

총 4분 남짓 걸린다. 끝나면 요약 맨 아래 thresholds가 전부 ✓인지 본다. 그리고 `DB 클릭 합계 = N` 로그가 `redirects_302` count와 같은지 본다.

⚠️ 실행하는 동안 같은 IP의 다른 요청(브라우저 등)도 같은 한도를 나눠 쓴다.
burst 단계에서는 그 IP에서 링크를 열면 429가 날 수 있다. 1분 남짓 지나면 풀린다.

## 결과 — 2026-10-07 (서울 → ap-northeast-2, 태스크 2개)

**thresholds 5개 전부 통과.**

| 항목 | 결과 |
| --- | --- |
| steady 리다이렉트 1,441건 | **p95 10.24ms**, 평균 8.72ms, 최대 98ms, 실패 0%, 429 0건 |
| burst 리다이렉트 901건 | 302 **498건** / 429 **403건** / **5xx 0건** |
| 클릭 집계 정합성 | DB 클릭 합계 **1,939** = `redirects_302` **1,939** — 동시 증가에서 유실 0 |

**burst 통과량이 레이트리밋 설계와 정확히 맞는다.** 30초 동안 태스크 하나가 허용하는 양은
버스트 100 + 30초 × 초당 5(분당 300) = 250건이고, 태스크가 2개이니 이론값은 **500건**이다. 실측은 **498건**이었다.
→ 한도를 넘는 트래픽은 앱이 5xx 없이 429로 거절하고, 한도 안의 트래픽은 10ms대로 처리된다.

⚠️ **이 숫자의 한계**: 한 IP에서 잰 값이라 서비스의 최대 처리량이 아니다. 지연도 대부분 네트워크 왕복이다.
분산 부하(여러 IP)와 Redis 기반 분산 레이트리밋은 P5 과제다.

## 한계 측정 — `capacity.js` (plan 0012)

위 테스트와 달리 **처리 한계와 첫 병목**을 잰다. 측정 클라이언트 IP 하나만 레이트리밋 예외(`RATE_LIMIT_EXEMPT_IPS`)로 두고, 다른 IP는 그대로 막는다.
판정 규칙(한계·무릎·첫 병목)은 `docs/plans/0012-capacity-baseline/plan.md`에 있다.

| `SCENARIO` | 요청 | 보려는 것 |
| --- | --- | --- |
| `spread` | 링크 100개에 고르게 `GET /{code}` | 실제에 가까운 상한 |
| `hot` | 링크 1개에 전부 `GET /{code}` | 인기 링크의 행 잠금 경합 |
| `read` | 링크 100개에 `GET /api/links/{code}`(쓰기 없음) | 대조군. 쓰기를 뺀 상한 |

- 단계: **50 → 100 → 200 → 400 → 800 → 1600 rps**, 단계마다 30초 램프 + 2분 30초 유지. 한 번에 최대 18분이고, 시작 전에 정각까지 최대 1분 기다린다.
  `MAX_RPS`로 단계를 자른다(로컬 검증은 100).
- 안전 정지: 오류율 5%, p95 1초, 429 1건. 임계값은 누적값이라 **단계 판정은 `collect-metrics.sh`의 표로 한다.**
- `collect-metrics.sh`는 단계마다 램프가 섞인 첫 1분을 빼고 유지 구간의 온전한 2분만 쓴다. 읽기 전용 AWS 호출만 한다.
- 원자료(k6 CSV·실행 로그·수집 표)는 `load/k6/results/raw/`에 둔다(gitignore). 결과 문서는 `load/k6/results/0012-baseline.md`.
- 측정 IP는 리미터(전역 뮤텍스·버킷 조회)를 통째로 건너뛴다. 그래서 측정값에는 리미터 비용이 빠진다.
  요청 비용(DB 왕복·JSON 로그)에 비하면 무시할 수준이고 0013·0014도 같은 방식으로 재므로 비교는 유효하다. 결과 문서의 구성 사양에 이 점을 적는다.

### 로컬 검증 [사람]

로컬에는 ALB가 없어 k6가 `LOCAL_XFF`로 예외 IP를 `X-Forwarded-For`에 직접 넣는다. 문서용 IP `198.51.100.7`을 쓴다.
관리자 토큰은 운영과 같이 `read -rs`로 받는다. 저장소 루트 `.env`에 `ADMIN_TOKEN_SHA256`이 있으면 그 해시의 토큰이고,
없을 때만 compose 기본값 `dev-token`이다(`.env`가 기본 해시를 덮어써 `dev-token`이 401이 된다 — ops-gotchas G-10).

```bash
RATE_LIMIT_EXEMPT_IPS=198.51.100.7 docker compose up --build -d
sleep 20; docker compose logs app | grep -E 'rate_limit_exempt_count|"db pool"' | tail -3
mkdir -p load/k6/results/raw
read -rs "ADMIN_TOKEN?관리자 토큰: "; echo
for s in spread hot read; do
  ADMIN_TOKEN="$ADMIN_TOKEN" k6 run -e BASE_URL=http://localhost:8080 -e SCENARIO=$s -e MAX_RPS=100 \
    -e LOCAL_XFF=198.51.100.7 load/k6/capacity.js 2>&1 | tee load/k6/results/raw/local-$s.log
done
```

기대: 첫 grep에 `"rate_limit_exempt_count":1`과 `"msg":"db pool"` 줄. 세 실행 모두 thresholds가 ✓이고 `status_429`가 없다.
`spread`·`hot`은 `DB 클릭 합계`가 `redirects_302` count와 같고, `read`는 `DB 클릭 합계 = 0`이다. 실행당 약 7분이다.

예외가 안 맞을 때 바로 멈추는지도 본다. `LOCAL_XFF` 없이 `hot`으로 돌린다.
(`spread`·`read`는 setup의 링크 100개 생성이 쓰기 한도(버스트 10)에 먼저 걸려 setup 오류로 멈춘다.)

```bash
ADMIN_TOKEN="$ADMIN_TOKEN" k6 run -e BASE_URL=http://localhost:8080 -e SCENARIO=hot -e MAX_RPS=50 load/k6/capacity.js
unset ADMIN_TOKEN; docker compose down
```

기대: 정각 대기 뒤 20초 안쪽에 `status_429` 임계값으로 멈춘다(읽기 버스트 100을 다 쓰는 시점).

### 운영 측정 [사람]

측정 창은 2시간 안쪽이다(plan 0012 단계 6). 집 IP는 화면·로그·문서 어디에도 찍지 않는다.
명령 블록에는 주석을 넣지 않았다 — zsh 대화형 셸은 `#`을 인자로 넘긴다(ops-gotchas G-3). 설명은 블록 밖에 둔다.
모든 블록은 저장소 루트에서, **main을 받아 둔 상태**로 실행한다(측정 코드와 tf 변수가 main에 있어야 한다).

**1) 인프라 켜기(main 이미지).** 스크립트가 확인 문구를 두 번 묻고, 중간에 배포 성공을 기다린다.

```bash
./scripts/full-apply-prod.sh --trigger-deploy
```

**2) 측정 예외 + SMS 끄기, 그리고 새 태스크 정의를 서비스에 반영.** IP가 비면 apply까지 가지 않는다.
apply는 태스크 정의 revision만 새로 등록한다(서비스는 `ignore_changes`). 배포 워크플로가 그 revision을 기준으로 실제 이미지를 띄운다.

```bash
MYIP=$(curl -fsS https://checkip.amazonaws.com) && [[ -n "$MYIP" ]] && terraform -chdir=infra/prod apply -var=ecr_force_delete=false -var "rate_limit_exempt_ips=$MYIP" -var "alarm_sms_number="
gh auth switch --user Naerrow && gh workflow run deploy.yml --ref main
```

배포 워크플로가 성공할 때까지 기다린다(`gh run watch` 또는 Actions 화면).

**3) 확인.** IP가 아니라 개수만 찍힌다. 1)의 태스크는 0, 2)의 배포 뒤 태스크는 1이어야 한다.
`container_definitions`가 sensitive라 plan에서는 env 차이가 가려진다 — 이 기동 로그가 실제 확인이다.

```bash
aws logs tail /ecs/linkpulse-prod-app --since 30m --region ap-northeast-2 | grep -E 'rate_limit_exempt_count|"db pool"' | tail -6
```

**4)·5) 시나리오 3개를 차례로 실행하고 수집한다.** 한 셸에서 블록 전체를 붙여 넣는다. 약 75분 걸린다.
- 실행이 끝나면 3분 기다렸다가 수집한다(지표·로그가 늦게 들어온다). 그다음 2분 더 쉬어 지표 구간을 나눈다.
- setup이 실패하면(시작 epoch 줄이 없으면) 거기서 멈춘다. 대개 예외 IP가 안 맞아 링크 생성이 429로 막힌 것이다.
- `ulimit -n 10240`: macOS 기본 256개로는 VU 500개의 연결을 못 연다. 그러면 발생기 오류가 서버 한계처럼 보인다.
- 노트북 CPU를 활동 모니터로 함께 보고, 단계(3분)마다 k6 프로세스 CPU를 따로 적는다(판정 0번).
  1600 rps면 k6가 CSV를 초당 약 2만 행 gzip으로 쓴다. 발생기가 포화하면 CSV 출력이 첫 용의자다.

```bash
ulimit -n 10240
read -rs "ADMIN_TOKEN?관리자 토큰: "; echo
for S in spread hot read; do
  ADMIN_TOKEN="$ADMIN_TOKEN" k6 run -e SCENARIO=$S --out csv=load/k6/results/raw/0012-$S.csv.gz load/k6/capacity.js 2>&1 | tee load/k6/results/raw/0012-$S.log
  grep -q 'epoch=' load/k6/results/raw/0012-$S.log || { echo "$S setup 실패 — 멈춘다"; break; }
  sleep 180
  load/k6/collect-metrics.sh "$(grep -o 'epoch=[0-9]*' load/k6/results/raw/0012-$S.log | cut -d= -f2)" load/k6/results/raw/0012-$S.csv.gz | tee load/k6/results/raw/0012-$S-metrics.md
  sleep 120
done
unset ADMIN_TOKEN MYIP
```

**6) 끄기.** 수집이 다 끝난 뒤에 한다(destroy하면 로그 그룹이 지워진다). `Destroy complete!`까지 컴퓨터를 끄지 않는다(ops-gotchas G-9).

```bash
./scripts/full-destroy-prod.sh --drop-dev-db
```

실행마다 남는 것: `load/k6/results/raw/0012-<시나리오>.log`(k6 요약·`DB 클릭 합계`), `.csv.gz`, `-metrics.md`(수집 표).
"302 수 > DB 합계"일 때만 클릭 유실로 본다. abort로 끝난 실행은 진행 중 요청이 끊겨 DB 합계가 더 클 수 있다.
