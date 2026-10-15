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
  `MAX_RPS`로 단계를 자른다(로컬 검증은 100). `START_OFFSET_S`(0~59, 기본 0)를 주면 시작을 "정각 + 오프셋"으로 비킨다(plan 0016 r0).
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
plan 0013부터 클릭은 비동기로 반영된다(로컬은 메모리 큐). teardown이 합계가 멈출 때까지 기다렸다가 찍고,
앱 로그에 10초마다 `"msg":"click queue"` 줄이 나온다(`duplicate_batches`는 0이어야 한다).

예외가 안 맞을 때 바로 멈추는지도 본다. `LOCAL_XFF` 없이 `hot`으로 돌린다.
(`spread`·`read`는 setup의 링크 100개 생성이 쓰기 한도(버스트 10)에 먼저 걸려 setup 오류로 멈춘다.)

```bash
ADMIN_TOKEN="$ADMIN_TOKEN" k6 run -e BASE_URL=http://localhost:8080 -e SCENARIO=hot -e MAX_RPS=50 load/k6/capacity.js
unset ADMIN_TOKEN; docker compose down
```

기대: 정각 대기 뒤 20초 안쪽에 `status_429` 임계값으로 멈춘다(읽기 버스트 100을 다 쓰는 시점).

### 운영 측정 [사람]

측정 창은 약 2시간 30분이다(plan 0016 단계 7 — 0016 기본값·ArgoCD 알림·매 분 :00초 정지·요청 로그 표본화. 0015의 창(실험 E1~E4·세 시나리오 재측정·알림 경로 시험)과 0014·0012·0013의 절차는 git 이력에 있다).
집 IP는 화면·로그·문서 어디에도 찍지 않는다. **ALB 접근 로그에는 클라이언트(집) IP가 들어 있다** — 원자료(`raw/`, gitignore)에만 두고 붙여 넣지 않는다.
명령 블록에는 주석을 넣지 않았다 — zsh 대화형 셸은 `#`을 인자로 넘긴다(ops-gotchas G-3). 설명은 블록 밖에 둔다.
모든 블록은 저장소 루트에서, **main을 받아 둔 상태**로 실행한다(측정 코드·차트가 main에 있어야 하고, 차트 기본값은 머지로 배포된다).
단계마다 확인 지점이 있다. 걸리면 거기서 멈춘다.

**1) 인프라 켜기(GHCR `main` 이미지, 처음부터).** 스크립트가 확인 문구를 두 번 묻고, ArgoCD 동기화·롤아웃·헬스체크까지 기다린다.

```bash
./scripts/full-apply-prod.sh
```

**1-1) 노드 확인.** 노드 **3대**, 요청 합이 할당 가능량 안인지, t4g 크레딧이 `unlimited`인지 본다(`standard`면 측정하지 않는다). 모든 파드가 Ready인지 본다.

```bash
kubectl get nodes; kubectl describe nodes | grep -A8 'Allocated resources'
aws ec2 describe-instance-credit-specifications --region ap-northeast-2 --instance-ids $(aws ec2 describe-instances --region ap-northeast-2 --filters Name=tag:eks:cluster-name,Values=linkpulse-prod-eks Name=instance-state-name,Values=running --query 'Reservations[].Instances[].InstanceId' --output text) --query 'InstanceCreditSpecifications[].CpuCredits'
kubectl get pods -A | grep -vE 'Running|Completed'
```

**1-2) 모니터링·기본값 확인(plan 0016 7-1).** Prometheus를 다른 탭에서 띄워 둔다(`kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090`) — 4)의 확인 줄도 이 포트를 쓴다. 그다음 이 블록을 돈다.
- 규칙: linkpulse 그룹 4개(`linkpulse-slo`·`-clicks`·`-admin`·`-argocd`)가 `ok`여야 한다.
- 차트 기본값 ConfigMap `linkpulse-defaults`에 `{"ACCESS_LOG_SAMPLE":"100","GOMAXPROCS":"1"}`이 있어야 한다.
- 앱 파드마다 `go_sched_gomaxprocs_threads`가 **1**이고, 기동 로그의 `access_log_sample`이 **100**이어야 한다. 아니면 기본값이 파드에 닿지 않은 것이다 — 멈춘다.

```bash
curl -s localhost:9090/api/v1/rules | jq -r '.data.groups[] | select(.name | startswith("linkpulse")) | "\(.name) \([.rules[].health] | unique | join(","))"'
kubectl -n linkpulse get configmap linkpulse-defaults -o jsonpath='{.data}{"\n"}'
curl -s localhost:9090/api/v1/query --data-urlencode 'query=go_sched_gomaxprocs_threads{namespace="linkpulse"}' | jq -r '.data.result[] | "\(.metric.pod) GOMAXPROCS=\(.value[1])"'
kubectl -n linkpulse logs -l app.kubernetes.io/name=linkpulse --prefix --tail=-1 | grep -o '"access_log_sample":[0-9]*'
```

**2) 측정 예외 + SMS 끄기 + ALB 접근 로그 켜기.** IP가 비면 apply까지 가지 않는다. 예외 IP는 `infra/k8s`의 사이트 Secret으로 들어간다.
envFrom은 Secret 변경을 다시 읽지 않으므로 파드를 다시 띄운다. `infra/prod` apply가 접근 로그 버킷(7일 만료)을 만들고 ALB에 켠다 — plan에 `aws_s3_bucket.alb_logs[0]` 등 5개 생성, `aws_lb.main` 변경, SMS 구독 2개(`alarms_sms[0]`·`canary_use1_sms[0]`) 삭제가 보여야 한다(`5 to add, 1 to change, 2 to destroy`).
마지막 줄은 ALB가 쓴 시험 파일(`ELBAccessLogTestFile`)이 보이는지다 — 안 보이면 버킷 정책 문제다.

```bash
MYIP=$(curl -fsS https://checkip.amazonaws.com) && [[ -n "$MYIP" ]] && terraform -chdir=infra/k8s apply -var "rate_limit_exempt_ips=$MYIP" && terraform -chdir=infra/prod apply -var "alarm_sms_number=" -var enable_alb_access_logs=true && kubectl -n linkpulse rollout restart deploy/linkpulse && kubectl -n linkpulse rollout status deploy/linkpulse
B=$(aws s3api list-buckets --query "Buckets[?starts_with(Name, 'linkpulse-prod-alb-logs-')].Name" --output text); aws s3 ls "s3://$B" --recursive | grep -c ELBAccessLogTestFile
```

**3) 확인.** IP가 아니라 개수만 찍힌다. 다시 띄운 파드는 `rate_limit_exempt_count`가 1이어야 한다.

```bash
kubectl -n linkpulse logs -l app.kubernetes.io/name=linkpulse --prefix --tail=200 | grep -E 'rate_limit_exempt_count|"db pool"|지표 서버' | tail -6
```

**3-1) 클릭 파이프라인 스모크 + 로그 필드 확인.** 링크 하나에 리다이렉트 3번 → 15초 뒤 통계의 클릭이 3이어야 한다.
Logs Insights 결과에 `pods`(파드 수)가 숫자로 나와야 한다. `302 302 302`·`3`·`pods` 숫자가 아니면 측정하지 않고 멈춘다(`alarm-response.md` §14).
마지막 줄은 Fluent Bit의 재시도·일시정지·유실·오류 줄 수다. 0이 아니면 `grep -iE` 로 그 줄을 직접 본다(G-15).

```bash
read -rs "ADMIN_TOKEN?관리자 토큰: "; echo
C=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{"url":"https://example.com/smoke"}' https://lpulse.live/api/links | jq -r .code); echo "code=$C"
for i in 1 2 3; do curl -s -o /dev/null -w '%{http_code} ' https://lpulse.live/$C; done; echo
sleep 15; curl -s https://lpulse.live/api/links/$C | jq .clicks
Q=$(aws logs start-query --region ap-northeast-2 --log-group-name /eks/linkpulse-prod/app --start-time $(( $(date +%s) - 600 )) --end-time $(date +%s) --query-string 'filter msg = "db pool" | stats max(in_use) as in_use_max, count_distinct(task_id) as pods' --query queryId --output text); sleep 8; aws logs get-query-results --region ap-northeast-2 --query-id "$Q" --query results
kubectl -n logging logs -l app.kubernetes.io/name=fluent-bit --tail=-1 | grep -ciE 'retry|paused|drop|error'
```

**발생기 CPU 기록(판정 0번).** 4) 전에 **다른 터미널 탭**에서 켜 두고, 4)가 끝나면 Ctrl+C로 멈춘다. 10초 남짓마다 시각·맥 전체 CPU %·k6 CPU %(코어 1개 = 100)를 한 줄씩 적는다.

```bash
L=/Users/parkjaehee/mini-project/linkpulse/load/k6/results/raw/0016-cpu.log; sysctl -n hw.ncpu > $L; while true; do printf '%s %s %s\n' "$(date -u +%s)" "$(top -l 2 -n 0 -s 1 | awk '/CPU usage/ {u=$3; s=$5} END {print u+s}')" "$(ps -A -o %cpu=,comm= | awk '$2 ~ /k6$/ {s+=$1} END {print s+0}')" >> $L; sleep 8; done
```

**4) 실행 r0 → r1 → r2(plan 0016 설계 4).** `SCENARIO=spread MAX_RPS=3200`(50 → 3200 7단계, 21분)을 세 번 한다. 한 셸에서 블록 전체를 붙여 넣는다(약 1시간 30분).

| 실행 | k6 시작 | 사이트 ConfigMap 덮어쓰기 | 보는 것 |
| --- | --- | --- | --- |
| `r0` | **정각 + 30초**(`START_OFFSET_S=30`) | `GOMAXPROCS=2`, `ACCESS_LOG_SAMPLE=1` | 0015와 같은 설정. 정지가 :30(k6 기준 :00)으로 옮겨 가면 발생기 쪽, :00에 남으면 서버·AWS 쪽 |
| `r1` | 정각 | `ACCESS_LOG_SAMPLE=1` | `GOMAXPROCS`만 1(기본값). r0과 비교해 CPU·지연 |
| `r2` | 정각 | 없음 | 0016 기본값(요청 로그 1/100). r1과 비교해 로그 줄 수·Fluent Bit CPU |

- **실험은 사이트 ConfigMap(`linkpulse-site`)만 고친다.** 그것은 `infra/k8s`가 소유해 ArgoCD selfHeal이 되돌리지 않는다. 차트 ConfigMap `linkpulse-defaults`를 고치면 ArgoCD가 곧 되돌린다.
  같은 키는 사이트 ConfigMap이 차트 기본값을 이긴다. merge patch의 `null`은 그 키를 지운다(차트 기본값으로 돌아간다). 순서가 덮어쓰기를 하나씩 지우므로 마지막(r2)이 기본값이다 — 되돌리는 단계가 따로 없다.
- **확인 한 줄**: 파드를 다시 띄운 뒤 모든 앱 파드의 `go_sched_gomaxprocs_threads`(r0 2, r1·r2 1)와 사이트 ConfigMap의 두 키를 찍는다. 기대값과 다르면 그 실행을 하지 않고 멈춘다.
  `rollout status`는 옛 파드가 종료를 시작할 때 끝나지만, 옛 파드는 preStop 10초 + 종료 동안 계속 긁힌다. 그래서 10초마다 최대 90초까지 다시 묻는다(그동안은 옛 값이 섞인다).
- **r0의 오프셋과 수집 시작**: CloudWatch는 벽시계 분 단위로만 준다. 30초 비킨 단계는 램프가 정각에 끝나 유지 구간 2분 30초가 벽시계 두 분을 그대로 덮는다.
  그래서 `collect-metrics.sh`에는 직전 정각(`epoch − 30`)을 주고(블록이 `S − S % 60`으로 계산한다), `alb-latency.py`에는 실제 epoch를 준다.
- 실행마다 함께 도는 것: `record-pods.sh`(파드 CPU·HPA·**ArgoCD health와 Deployment·HPA 리소스 health**, 30초), `goroutine-snap.sh`(3200 단계 유지 구간의 :00·:30 + 0.2초 고루틴 스냅숏, 가장 오래된 파드 하나).
- 수집 뒤 **정지 초의 앱 쪽 시간**을 Logs Insights로 받는다(3200 유지 구간, 앱 로그 `time`의 초별 `duration_ms` p99·최대·건수 → `-appsec.json`). destroy하면 로그 그룹이 사라지므로 여기서 받는다.
  r0·r1은 모든 요청이 로그에 남는다. r2는 표본이라 성공·빠른 요청이 1/100만 남는다(정지 초 판단에는 r0·r1을 쓴다).
  `time`은 앱이 줄을 쓴 시각(응답 직전)이다. 결과가 0줄이면(쿼리 실패) 창이 끝나기 전에 `substr(time, 17, 2)`를 `datefloor(@timestamp, 1s)`로 바꿔 다시 묻는다 — Fluent Bit가 읽은 시각이라 버퍼만큼 늦을 수 있다.
- 실행마다 파드 2개에서 시작한다(HPA 축소 안정화 5분을 기다린다).

```bash
ulimit -n 10240
P=0016; M=3200; R=/Users/parkjaehee/mini-project/linkpulse/load/k6; LG=/eks/linkpulse-prod/app
[[ -n "$ADMIN_TOKEN" ]] || { read -rs "ADMIN_TOKEN?관리자 토큰: "; echo; }
CODE=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ADMIN_TOKEN" https://lpulse.live/api/admin/links); echo "토큰 확인: $CODE"
site() { kubectl -n linkpulse patch configmap linkpulse-site --type merge -p "{\"data\":$1}"; }
two() { until [[ "$(kubectl -n linkpulse get hpa linkpulse -o jsonpath='{.status.currentReplicas}')" == 2 ]]; do echo "파드 2개로 줄기를 기다린다"; sleep 30; done; }
restart() { kubectl -n linkpulse rollout restart deploy/linkpulse && kubectl -n linkpulse rollout status deploy/linkpulse; }
verify() {
  local got
  for _ in $(seq 9); do
    sleep 10
    got=$(curl -s localhost:9090/api/v1/query --data-urlencode 'query=go_sched_gomaxprocs_threads{namespace="linkpulse"}' | jq -r '[.data.result[].value[1]] | unique | join(",")')
    [[ $got == "$1" ]] && break
  done
  echo "확인: GOMAXPROCS=$got(기대 $1) · 사이트 ConfigMap $(kubectl -n linkpulse get configmap linkpulse-site -o jsonpath='GOMAXPROCS={.data.GOMAXPROCS} ACCESS_LOG_SAMPLE={.data.ACCESS_LOG_SAMPLE}')"
  [[ $got == "$1" ]] || { echo "기대값과 다르다 — 멈춘다"; return 1; }
}
run() {
  local N=$1 L=$R/results/raw/$P-$1 REC SNAP S Q
  echo "$N 시작 ($(date -u +%H:%M:%S))"
  $R/record-pods.sh $L & REC=$!
  ( until grep -q 'epoch=' $L.log 2>/dev/null; do sleep 5; done; $R/goroutine-snap.sh $L "$(grep -o 'epoch=[0-9]*' $L.log | cut -d= -f2)" 3200 ) & SNAP=$!
  ADMIN_TOKEN="$ADMIN_TOKEN" k6 run -e SCENARIO=spread -e MAX_RPS=$M -e START_OFFSET_S=$2 --out csv=$L.csv.gz $R/capacity.js 2>&1 | tee $L.log
  grep -q 'epoch=' $L.log || { kill $REC $SNAP; echo "$N setup 실패 — 멈춘다"; return 1; }
  wait $SNAP; sleep 180; kill $REC
  S=$(grep -o 'epoch=[0-9]*' $L.log | cut -d= -f2)
  $R/collect-metrics.sh $(( S - S % 60 )) $L.csv.gz $M $LG > $L-metrics.md && echo "$N 수집 완료"
  Q=$(aws logs start-query --region ap-northeast-2 --log-group-name $LG --start-time $(( S + 1110 )) --end-time $(( S + 1260 )) --query-string 'filter msg = "http request" | fields substr(time, 17, 2) as ss | stats pct(duration_ms, 99) as p99, max(duration_ms) as mx, count(*) as n by ss | sort ss' --query queryId --output text)
  for _ in $(seq 60); do st=$(aws logs get-query-results --region ap-northeast-2 --query-id "$Q" --query status --output text); [[ $st == Complete || $st == Failed ]] && break; sleep 3; done
  aws logs get-query-results --region ap-northeast-2 --query-id "$Q" > $L-appsec.json && echo "$N 앱 쪽 초별 시간 $(jq '.results | length' $L-appsec.json)줄"
}
[[ $CODE == 200 ]] && two && site '{"GOMAXPROCS":"2","ACCESS_LOG_SAMPLE":"1"}' && restart && verify 2 && run r0 30 \
  && two && site '{"GOMAXPROCS":null,"ACCESS_LOG_SAMPLE":"1"}' && restart && verify 1 && run r1 0 \
  && two && site '{"ACCESS_LOG_SAMPLE":null}' && restart && verify 1 && run r2 0
kubectl -n linkpulse get configmap linkpulse-site -o jsonpath='GOMAXPROCS={.data.GOMAXPROCS} ACCESS_LOG_SAMPLE={.data.ACCESS_LOG_SAMPLE}{"\n"}'
unset ADMIN_TOKEN MYIP
```

마지막 줄은 `GOMAXPROCS= ACCESS_LOG_SAMPLE=`(두 키 없음 = 차트 기본값)이어야 한다.

**5) ALB 접근 로그 내려받기·초 단위 분석·고루틴 요약.** ALB는 로그를 5분 단위로 전달한다 — **마지막 요청 뒤 5분 넘게 기다린 다음** 받는다. 읽기만 한다.
- `alb-latency.py`: 실행마다 초별 p99·정지/정지 밖 p99·정지 앞뒤 100ms 도착(판정 (a) 꼬리·(c)). r0은 k6 기준 표도 나온다. 클라이언트 IP를 찍지 않는다.
- `goroutines.py`: 스냅숏마다 처리 중 요청이 어디서 기다리는지(실행 대기·DB 연결·DB 응답·로그 쓰기·응답 쓰기·기타, 판정 (c)).
- 분류(`alb-errors.py`)는 실패 요청이 있을 때 본다.

```bash
R=/Users/parkjaehee/mini-project/linkpulse/load/k6; D=$R/results/raw
B=$(aws s3api list-buckets --query "Buckets[?starts_with(Name, 'linkpulse-prod-alb-logs-')].Name" --output text) && aws s3 sync "s3://$B" $D/0016-alb-logs --only-show-errors && du -sh $D/0016-alb-logs
for N in r0 r1 r2; do python3 -I $R/alb-latency.py $D/0016-alb-logs "$(grep -o 'epoch=[0-9]*' $D/0016-$N.log | cut -d= -f2)" $D/0016-$N-prom.json 3200 > $D/0016-$N-alb-latency.md && echo "== $N" && awk '/### 요약 — 벽시계/{f=1;next} f&&/^\|/{print;t=1;next} t{exit}' $D/0016-$N-alb-latency.md; done
python3 -I $R/goroutines.py $D/0016-r*-gr-*.txt
python3 -I $R/alb-errors.py $D/0016-alb-logs > $D/0016-alb-errors.md; head -2 $D/0016-alb-errors.md
```

**6) 끄기.** 수집·내려받기가 다 끝난 뒤에 한다(destroy하면 로그 그룹·Prometheus·접근 로그 버킷이 지워진다). `Destroy complete!`까지 컴퓨터를 끄지 않는다(ops-gotchas G-9).
스크립트가 두 스택의 state가 비었는지 확인한다.

```bash
./scripts/full-destroy-prod.sh --drop-dev-db
```

실행마다 남는 것: `load/k6/results/raw/<P>-<이름>.log`(k6 요약·`DB 클릭 합계`), `.csv.gz`, `-metrics.md`(수집 표),
`-cw.json`·`-pool.json`·`-fail.json`·`-cq.json`·`-cons.json`(AWS 원본 응답 — destroy 뒤에도 표를 다시 만들 수 있다),
`-prom.json`·`-lines.json`(plan 0015, Prometheus 응답·파드별 로그 줄 수 — 클러스터가 없어도 수집 표의 Prometheus 절을 다시 만든다),
`-pods.tsv`·`-hpa.tsv`(EKS, 파드 CPU·HPA 30초 기록), 0015 실험은 `-cpu.pb.gz`(CPU 프로파일).
plan 0016부터 `-req.json`(분당 요청 로그 줄 수), `-argo.tsv`(ArgoCD health·리소스 health 30초 기록), `-gr-<epoch>.txt`·`-gr.tsv`(고루틴 스냅숏),
`-appsec.json`(3200 유지 구간 앱 쪽 초별 시간), `-alb-latency.md`(초 단위 분석). 창 전체로 `<P>-alb-logs/`·`<P>-alb-errors.md`·`<P>-cpu.log`.

클릭 정합성:
- **0012(동기 집계):** "302 수 > DB 합계"일 때만 유실로 본다. abort로 끝난 실행은 진행 중 요청이 끊겨 DB 합계가 더 클 수 있다.
- **0013·0014(비동기, 로그 합계):** 수집 표의 "정합성(click queue 통계 합)" 줄. `recorded − dropped` = `applied + unknown` = DB 합계여야 한다. 고부하에서 로그가 빠지면 거짓으로 어긋난다(G-15).
- **0015부터(Prometheus 카운터):** 수집 표의 "정합성 — Prometheus 카운터" 줄로 판정한다(판정 (b)). 정수로 정확히 같아야 하고, 카운터 리셋 합과 실행 중 사라진 파드가 둘 다 0일 때만 유효하다.
  로그 유실은 따로 "로그 유실 — 파드·분당 줄 수" 표로 본다(판정 (c), 유실 의심 칸 0).
  클릭은 응답 전에 기록되므로 클라이언트가 302를 못 받은 요청도 DB에 들어간다. k6 쪽은 `redirects_302 ≤ DB 합계`만 본다.
