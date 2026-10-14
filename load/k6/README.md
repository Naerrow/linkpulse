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

측정 창은 약 4시간이다(plan 0015 단계 7 — P5 종결을 겸한다. 0014의 창(회전 드릴 포함)과 0012·0013의 ECS 절차는 git 이력에 있다).
집 IP는 화면·로그·문서 어디에도 찍지 않는다. **ALB 접근 로그에는 클라이언트(집) IP가 들어 있다** — 원자료(`raw/`, gitignore)에만 두고 붙여 넣지 않는다.
명령 블록에는 주석을 넣지 않았다 — zsh 대화형 셸은 `#`을 인자로 넘긴다(ops-gotchas G-3). 설명은 블록 밖에 둔다.
모든 블록은 저장소 루트에서, **main을 받아 둔 상태**로 실행한다(측정 코드·차트·tf 변수가 main에 있어야 한다).
단계마다 확인 지점이 있다. 걸리면 거기서 멈춘다(알림 경로만 예외 — 아래 1-2).

**1) 인프라 켜기(GHCR `main` 이미지, 처음부터).** 스크립트가 확인 문구를 두 번 묻고, ArgoCD 동기화·롤아웃·헬스체크까지 기다린다. 이 한 번이 P5 종결의 "처음부터 재현" 확인이다.

```bash
./scripts/full-apply-prod.sh
```

**1-1) 노드 확인.** 노드 **3대**, 요청 합이 할당 가능량 안인지, t4g 크레딧이 `unlimited`인지 본다(`standard`면 측정하지 않는다). 모든 파드가 Ready인지 본다.

```bash
kubectl get nodes; kubectl describe nodes | grep -A8 'Allocated resources'
aws ec2 describe-instance-credit-specifications --region ap-northeast-2 --instance-ids $(aws ec2 describe-instances --region ap-northeast-2 --filters Name=tag:eks:cluster-name,Values=linkpulse-prod-eks Name=instance-state-name,Values=running --query 'Reservations[].Instances[].InstanceId' --output text) --query 'InstanceCreditSpecifications[].CpuCredits'
kubectl get pods -A | grep -vE 'Running|Completed'
```

**1-2) 모니터링 확인(plan 0015 7-2).** Prometheus를 다른 탭에서 띄워 둔다(`kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090`). 그다음 이 블록을 돈다.
- 대상: 모두 `up`이어야 한다 — 앱 파드 2·ArgoCD·kube-state-metrics·node-exporter 3·kubelet(cAdvisor·resource)·apiserver·coredns·operator·Prometheus·Alertmanager·Grafana.
- 규칙: linkpulse 그룹 4개(`linkpulse-slo`·`-clicks`·`-admin`·`-argocd`)가 `ok`여야 한다. 없으면 선택자 계약이 빠진 것이다 — 멈춘다.
- 공개 `/metrics`는 **404**여야 한다(판정 (a)). Fluent Bit 설정에 세 줄(`storage.type filesystem`·`DB`·`Rotate_Wait 30`)이 있어야 한다.

```bash
curl -s localhost:9090/api/v1/targets | jq -r '.data.activeTargets[] | "\(.health) \(.labels.job)"' | sort | uniq -c
curl -s localhost:9090/api/v1/rules | jq -r '.data.groups[] | select(.name | startswith("linkpulse")) | "\(.name) \([.rules[].health] | unique | join(","))"'
curl -s -o /dev/null -w 'public /metrics %{http_code}\n' https://lpulse.live/metrics
kubectl -n logging get cm fluent-bit -o jsonpath='{.data.fluent-bit\.conf}' | grep -E 'storage.type|Rotate_Wait|DB '
```

**알림 경로 시험(판정 (d)).** 늘 울리는 시험 규칙을 넣는다. 2분 안에 Slack에 `FIRING LinkpulseTestAlert · warning (1)` 카드가 오면 지운다(지운 뒤 `RESOLVED` 카드가 한 번 더 온다).
안 오면 `docs/runbooks/alarm-response.md` P-0 순서로 본다. **Slack 도착이 끝내 안 되면 측정은 하고, 알림 경로는 결과에 실패로 남긴다**(측정과 독립이다).

```bash
kubectl apply -f infra/k8s/manifests/test-alert-rule.yaml
kubectl delete -f infra/k8s/manifests/test-alert-rule.yaml
```

**기본 규칙 노이즈(30분).** 1-2를 마친 뒤 30분 동안 Slack을 보고, 그 끝에 지금 울리는 알림을 적는다(`Watchdog`은 늘 울리고 Slack에는 오지 않는다 — 정상).
EKS에 맞지 않아 시끄러운 규칙은 이름을 결과에 남긴다(끄는 것은 다음 PR — runbook "시끄러운 기본 규칙"). Grafana는 `kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80` → http://localhost:3000/d/linkpulse-overview 에서 그려지는지 본다.

```bash
curl -s localhost:9090/api/v1/alerts | jq -r '.data.alerts[] | select(.state=="firing") | .labels.alertname' | sort | uniq -c
```

**2) 측정 예외 + SMS 끄기 + ALB 접근 로그 켜기.** IP가 비면 apply까지 가지 않는다. 예외 IP는 `infra/k8s`의 사이트 Secret으로 들어간다.
envFrom은 Secret 변경을 다시 읽지 않으므로 파드를 다시 띄운다. `infra/prod` apply가 접근 로그 버킷(7일 만료)을 만들고 ALB에 켠다 — plan에 `aws_s3_bucket.alb_logs[0]` 등 5개 생성, `aws_lb.main` 변경, SMS 구독 2개(`alarms_sms[0]`·`canary_use1_sms[0]`) 삭제가 보여야 한다(`5 to add, 1 to change, 2 to destroy`).
마지막 줄은 ALB가 쓴 시험 파일(`ELBAccessLogTestFile`)이 보이는지다 — 안 보이면 버킷 정책 문제다.

```bash
MYIP=$(curl -fsS https://checkip.amazonaws.com) && [[ -n "$MYIP" ]] && terraform -chdir=infra/k8s apply -var "rate_limit_exempt_ips=$MYIP" && terraform -chdir=infra/prod apply -var "alarm_sms_number=" -var enable_alb_access_logs=true && kubectl -n linkpulse rollout restart deploy/linkpulse && kubectl -n linkpulse rollout status deploy/linkpulse
B=$(aws s3api list-buckets --query "Buckets[?starts_with(Name, 'linkpulse-prod-alb-logs-')].Name" --output text); aws s3 ls "s3://$B" --recursive | grep -c ELBAccessLogTestFile
```

**3) 확인.** IP가 아니라 개수만 찍힌다. 다시 띄운 파드는 `rate_limit_exempt_count`가 1이어야 한다.
`set_sensitive`라 plan에서는 값이 가려진다 — 이 기동 로그가 실제 확인이다.

```bash
kubectl -n linkpulse logs -l app.kubernetes.io/name=linkpulse --prefix --tail=200 | grep -E 'rate_limit_exempt_count|"db pool"|지표 서버' | tail -6
```

**3-1) 클릭 파이프라인 스모크 + 로그 필드 확인.** 링크 하나에 리다이렉트 3번 → 15초 뒤 통계의 클릭이 3이어야 한다(배치가 다른 파드의 워커에서 몇 초 늦게 반영될 수 있다).
그리고 Fluent Bit가 앱 JSON을 최상위 필드로 풀었는지 Logs Insights로 본다 — 결과에 `pods`(파드 수)가 숫자로 나와야 한다.
`302 302 302`·`3`·`pods` 숫자가 아니면 측정하지 않고 멈춘다(큐·IAM·워커·로그 중 하나가 막혔다 — `alarm-response.md` §14).
마지막 줄은 Fluent Bit의 재시도·일시정지·유실·오류 줄 수다. 0이 아니면 `grep -iE` 로 그 줄을 직접 본다(G-15 — 0015부터 파일 시스템 버퍼라 `paused`가 나오면 안 된다).

```bash
read -rs "ADMIN_TOKEN?관리자 토큰: "; echo
C=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{"url":"https://example.com/smoke"}' https://lpulse.live/api/links | jq -r .code); echo "code=$C"
for i in 1 2 3; do curl -s -o /dev/null -w '%{http_code} ' https://lpulse.live/$C; done; echo
sleep 15; curl -s https://lpulse.live/api/links/$C | jq .clicks
Q=$(aws logs start-query --region ap-northeast-2 --log-group-name /eks/linkpulse-prod/app --start-time $(( $(date +%s) - 600 )) --end-time $(date +%s) --query-string 'filter msg = "db pool" | stats max(in_use) as in_use_max, count_distinct(task_id) as pods' --query queryId --output text); sleep 8; aws logs get-query-results --region ap-northeast-2 --query-id "$Q" --query results
kubectl -n logging logs -l app.kubernetes.io/name=fluent-bit --tail=-1 | grep -ciE 'retry|paused|drop|error'
```

**4) 요청당 CPU 실험 E1~E4(plan 0015).** `SCENARIO=spread MAX_RPS=800`(15분)을 네 번 한다. 한 셸에서 블록 전체를 붙여 넣는다(약 1시간 30분).
- `e1` 기준 실행 — E1(CPU 세 출처 비교, 수집 표의 Prometheus 절)과 E2(800 단계 유지 구간에 파드 하나의 30초 CPU 프로파일)를 함께 본다. GOMAXPROCS 실제 값도 수집 표에 나온다.
- `e3` `LOG_LEVEL=warn` — 요청 로그 경로의 비용. `click queue`·`db pool` 줄도 Info라 사라진다. **그 실행의 로그 기반 표(정합성·풀·로그 줄 수)는 비어 나오는 것이 정상이다** — 정합성은 Prometheus 절로 본다.
- `e4` `GOMAXPROCS=1`(로그는 info로 되돌린다) — CPU 한도 250m 아래의 스케줄러·스로틀 비용. 비교 대상은 `e1`에서 본 실제 GOMAXPROCS 값이다.
- `e1b` 원래 설정으로 되돌린 기준 재실행 — 되돌림 확인과 같은 창 안의 실행 간 차이.
- 사이트 ConfigMap은 `infra/k8s`가 소유해 ArgoCD selfHeal이 되돌리지 않는다. 실험 사이에 고치고 파드를 다시 띄운다. 마지막 실험 뒤에는 원래대로다(destroy로도 사라진다).
- 실행마다 파드 2개에서 시작한다(HPA 축소 안정화 5분을 기다린다). 프로파일은 `raw/0015-<이름>-cpu.pb.gz`에 남는다.

```bash
ulimit -n 10240
P=0015; R=/Users/parkjaehee/mini-project/linkpulse/load/k6; LG=/eks/linkpulse-prod/app
read -rs "ADMIN_TOKEN?관리자 토큰: "; echo
CODE=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ADMIN_TOKEN" https://lpulse.live/api/admin/links); echo "토큰 확인: $CODE"
restart() { kubectl -n linkpulse rollout restart deploy/linkpulse && kubectl -n linkpulse rollout status deploy/linkpulse; }
run800() {
  local N=$1 L=$R/results/raw/$P-$1 REC PROF
  until [[ "$(kubectl -n linkpulse get hpa linkpulse -o jsonpath='{.status.currentReplicas}')" == 2 ]]; do echo "파드 2개로 줄기를 기다린다"; sleep 30; done
  echo "$N 시작 ($(date -u +%H:%M:%S))"
  $R/record-pods.sh $L & REC=$!
  ( until grep -q 'epoch=' $L.log 2>/dev/null; do sleep 5; done
    S=$(grep -o 'epoch=[0-9]*' $L.log | cut -d= -f2); sleep $(( S + 760 - $(date +%s) ))
    POD=$(kubectl -n linkpulse get pods -l app.kubernetes.io/name=linkpulse -o jsonpath='{.items[0].metadata.name}')
    kubectl -n linkpulse port-forward pod/$POD 16060:9090 >/dev/null 2>&1 & PF=$!; sleep 3
    curl -s -o $L-cpu.pb.gz "http://127.0.0.1:16060/debug/pprof/profile?seconds=30"; kill $PF; echo "$N 프로파일 저장 $(wc -c < $L-cpu.pb.gz)바이트" ) & PROF=$!
  ADMIN_TOKEN="$ADMIN_TOKEN" k6 run -e SCENARIO=spread -e MAX_RPS=800 --out csv=$L.csv.gz $R/capacity.js 2>&1 | tee $L.log
  grep -q 'epoch=' $L.log || { kill $REC $PROF; echo "$N setup 실패 — 멈춘다"; return 1; }
  wait $PROF; sleep 180; kill $REC
  $R/collect-metrics.sh "$(grep -o 'epoch=[0-9]*' $L.log | cut -d= -f2)" $L.csv.gz 800 $LG > $L-metrics.md && echo "$N 수집 완료"
}
[[ $CODE == 200 ]] && run800 e1 \
  && kubectl -n linkpulse patch configmap linkpulse-site --type merge -p '{"data":{"LOG_LEVEL":"warn"}}' && restart && run800 e3 \
  && kubectl -n linkpulse patch configmap linkpulse-site --type merge -p '{"data":{"LOG_LEVEL":"info","GOMAXPROCS":"1"}}' && restart && run800 e4 \
  && kubectl -n linkpulse patch configmap linkpulse-site --type json -p '[{"op":"remove","path":"/data/GOMAXPROCS"}]' && restart && run800 e1b
kubectl -n linkpulse get configmap linkpulse-site -o jsonpath='{.data.LOG_LEVEL} {.data.GOMAXPROCS}{"\n"}'
```

마지막 줄은 `info `(GOMAXPROCS 없음)여야 한다. 프로파일 상위 10개는 창이 끝난 뒤 로컬에서 본다(판정 (e)) — Go 프로파일은 심볼을 품고 있어 바이너리가 필요 없다.

```bash
for e in e1 e3 e4 e1b; do echo "== $e"; go tool pprof -top -nodecount=10 load/k6/results/raw/0015-$e-cpu.pb.gz 2>/dev/null | tail -11; done
```

**5) 재측정 — 세 시나리오를 한 창에서(P5 종결).** 한 셸에서 블록 전체를 붙여 넣는다. 6400까지 8단계라 실행마다 24분, 셋에 약 1시간 45분이다. 4)의 셸을 그대로 써도 된다(`P`·`R`·`LG`·토큰이 남아 있다).
- **실행마다 파드 2개에서 시작한다.** 기본 설정(위 마지막 줄 `info `)이다.
- 수집 표에 CloudWatch 절과 Prometheus 절(파드 CPU 세 출처·스로틀·연결당 요청·**카운터 정합성**·**로그 줄 수**)이 함께 나온다.
- 실행이 끝나면 3분 기다렸다가 수집한다(지표·로그가 늦게 들어온다). 카운터 정합성은 수집 시각이 아니라 teardown의 `DB 클릭 합계` 줄 시각 + 30초에 평가한다. HPA 축소(부하가 떨어지고 약 5분 뒤)보다 앞이라, 수집이 늦어 늘어난 파드가 지워졌어도 판정이 같다.
- setup이 실패하면(시작 epoch 줄이 없으면) 거기서 멈춘다. 대개 예외 IP가 안 맞아 링크 생성이 429로 막힌 것이다.
- `ulimit -n 10240`: macOS 기본 256개로는 VU 500개의 연결을 못 연다. 그러면 발생기 오류가 서버 한계처럼 보인다. 명령은 절대경로로 써서 어느 디렉터리에서 붙여 넣어도 된다(ops-gotchas G-13).

```bash
ulimit -n 10240
P=0015; M=6400; R=/Users/parkjaehee/mini-project/linkpulse/load/k6; LG=/eks/linkpulse-prod/app
[[ -n "$ADMIN_TOKEN" ]] || { read -rs "ADMIN_TOKEN?관리자 토큰: "; echo; }
CODE=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ADMIN_TOKEN" https://lpulse.live/api/admin/links); echo "토큰 확인: $CODE"
[[ $CODE == 200 ]] && for S in spread hot read; do
  until [[ "$(kubectl -n linkpulse get hpa linkpulse -o jsonpath='{.status.currentReplicas}')" == 2 ]]; do echo "파드 2개로 줄기를 기다린다"; sleep 30; done
  echo "$S 시작 파드 수 2 ($(date -u +%H:%M:%S))"
  $R/record-pods.sh $R/results/raw/$P-$S & REC=$!
  ADMIN_TOKEN="$ADMIN_TOKEN" k6 run -e SCENARIO=$S -e MAX_RPS=$M --out csv=$R/results/raw/$P-$S.csv.gz $R/capacity.js 2>&1 | tee $R/results/raw/$P-$S.log
  grep -q 'epoch=' $R/results/raw/$P-$S.log || { kill $REC; echo "$S setup 실패 — 멈춘다"; break; }
  sleep 180
  kill $REC
  echo "$S Fluent Bit 경고 $(kubectl -n logging logs -l app.kubernetes.io/name=fluent-bit --tail=-1 | grep -ciE 'retry|paused|drop|error')건"
  $R/collect-metrics.sh "$(grep -o 'epoch=[0-9]*' $R/results/raw/$P-$S.log | cut -d= -f2)" $R/results/raw/$P-$S.csv.gz $M $LG > $R/results/raw/$P-$S-metrics.md && echo "$S 수집 완료"
done
unset ADMIN_TOKEN MYIP
```

**발생기 CPU 기록(판정 0번).** 4) 전에 **다른 터미널 탭**에서 켜 두고, 5)가 끝나면 Ctrl+C로 멈춘다. 10초 남짓마다 시각·맥 전체 CPU %·k6 CPU %(코어 1개 = 100)를 한 줄씩 적는다.

```bash
L=/Users/parkjaehee/mini-project/linkpulse/load/k6/results/raw/0015-cpu.log; sysctl -n hw.ncpu > $L; while true; do printf '%s %s %s\n' "$(date -u +%s)" "$(top -l 2 -n 0 -s 1 | awk '/CPU usage/ {u=$3; s=$5} END {print u+s}')" "$(ps -A -o %cpu=,comm= | awk '$2 ~ /k6$/ {s+=$1} END {print s+0}')" >> $L; sleep 8; done
```

**6) ALB 접근 로그 내려받기·분류(판정 (f)).** ALB는 로그를 5분 단위로 전달한다 — **마지막 요청 뒤 5분 넘게 기다린 다음** 받는다. 읽기만 한다.
6400 세 실행이면 압축으로 1GB 남짓이다. 분류 결과(`0015-alb-errors.md`)에는 클라이언트 IP가 없다 — 결과 문서에는 이것만 옮긴다.

```bash
B=$(aws s3api list-buckets --query "Buckets[?starts_with(Name, 'linkpulse-prod-alb-logs-')].Name" --output text) && aws s3 sync "s3://$B" /Users/parkjaehee/mini-project/linkpulse/load/k6/results/raw/0015-alb-logs --only-show-errors && du -sh /Users/parkjaehee/mini-project/linkpulse/load/k6/results/raw/0015-alb-logs
python3 -I /Users/parkjaehee/mini-project/linkpulse/load/k6/alb-errors.py /Users/parkjaehee/mini-project/linkpulse/load/k6/results/raw/0015-alb-logs > /Users/parkjaehee/mini-project/linkpulse/load/k6/results/raw/0015-alb-errors.md; head -2 /Users/parkjaehee/mini-project/linkpulse/load/k6/results/raw/0015-alb-errors.md
```

**7) 끄기.** 수집·내려받기가 다 끝난 뒤에 한다(destroy하면 로그 그룹·Prometheus·접근 로그 버킷이 지워진다). `Destroy complete!`까지 컴퓨터를 끄지 않는다(ops-gotchas G-9).
스크립트가 두 스택의 state가 비었는지 확인한다.

```bash
./scripts/full-destroy-prod.sh --drop-dev-db
```

실행마다 남는 것: `load/k6/results/raw/<P>-<이름>.log`(k6 요약·`DB 클릭 합계`), `.csv.gz`, `-metrics.md`(수집 표),
`-cw.json`·`-pool.json`·`-fail.json`·`-cq.json`·`-cons.json`(AWS 원본 응답 — destroy 뒤에도 표를 다시 만들 수 있다),
`-prom.json`·`-lines.json`(plan 0015, Prometheus 응답·파드별 로그 줄 수 — 클러스터가 없어도 수집 표의 Prometheus 절을 다시 만든다),
`-pods.tsv`·`-hpa.tsv`(EKS, 파드 CPU·HPA 30초 기록), 실험은 `-cpu.pb.gz`(CPU 프로파일). 창 전체로 `0015-alb-logs/`·`0015-alb-errors.md`·`0015-cpu.log`.

클릭 정합성:
- **0012(동기 집계):** "302 수 > DB 합계"일 때만 유실로 본다. abort로 끝난 실행은 진행 중 요청이 끊겨 DB 합계가 더 클 수 있다.
- **0013·0014(비동기, 로그 합계):** 수집 표의 "정합성(click queue 통계 합)" 줄. `recorded − dropped` = `applied + unknown` = DB 합계여야 한다. 고부하에서 로그가 빠지면 거짓으로 어긋난다(G-15).
- **0015부터(Prometheus 카운터):** 수집 표의 "정합성 — Prometheus 카운터" 줄로 판정한다(판정 (b)). 정수로 정확히 같아야 하고, 카운터 리셋 합과 실행 중 사라진 파드가 둘 다 0일 때만 유효하다.
  로그 유실은 따로 "로그 유실 — 파드·분당 줄 수" 표로 본다(판정 (c), 유실 의심 칸 0).
  클릭은 응답 전에 기록되므로 클라이언트가 302를 못 받은 요청도 DB에 들어간다. k6 쪽은 `redirects_302 ≤ DB 합계`만 본다.
