---
status: approved # in-review | approved
revision: 2
created: 2026-10-15
---

# 0016. 0015 후속 정리 — GOMAXPROCS 기본값, ArgoCD 알림 오탐, 매 분 :00초 정지 원인, 요청 로그 표본화

## 목표

`load/k6/results/0015-observability.md` "0016으로 넘길 것" 중 네 항목을 끝낸다.

1. **`GOMAXPROCS=1`을 차트 기본값으로 둔다**(사이트 무관). 같은 창 안 비교로 CPU가 줄고 지연이 나빠지지 않는지 확인한다.
2. **`LinkpulseArgoCDAppUnhealthy`가 `Progressing`으로 울리지 않게 한다.** 멈춘 롤링은 계속 잡는다. promtool 테스트로 고정한다.
3. **"3200 p99 2~6배"의 실체인 매 분 :00초 정지의 원인을 가린다.** 0015 원자료로 보면 모든 부하에서 매 분 :00초에 모든 파드가 함께 늦어진다(배경).
   먼저 발생기 쪽인지 서버·AWS 쪽인지 가르고, 정지 중 앱이 어디서 기다리는지 본다. 0015 원자료로 먼저 분석하고 창에서 확인한다.
   고치는 것은 이 plan에서 하지 않는다. 결과에 권고로 남긴다.
4. **요청 로그를 표본화한다.** 성공했고 빠른 요청만 1/N로 남기고, 오류·느린 요청은 전부 남긴다. Fluent Bit CPU와 로그 양이 얼마나 주는지 잰다.

**범위 밖**(다음으로 미룬다): 노드 세대 실험(0015 이월 5), 6400의 벽(HPA 최대·DB 풀·PgBouncer, 이월 6), 외부 dead man's switch(P7), P6 하이브리드.

## 배경/제약

- **0015 결과에서 이 plan이 쓰는 숫자**
  - 파드 CPU 한도 250m에서 Go가 `GOMAXPROCS`를 **2**로 잡았다(E1 실측). E4(`GOMAXPROCS=1`)는 50~400 rps에서 파드 CPU −10~15%였다.
    프로파일의 스케줄러·futex 몫은 30% → 11%로 줄었고, p95는 나빠지지 않았다. 다만 800 rps까지만 쟀다.
  - 3200 단계 서버 쪽 p99: `spread` 28.8 → **53.0**, `hot` 11.0 → **62.3**, `read` 14.0 → **60.8** ms(0014 → 0015). p95는 같았다.
    파드별 CFS 스로틀은 3.9~14.8%였다. 노드 CPU는 42~44%로 여유가 있었다.
  - 0014에서 바뀐 것: 노드 3대, 모니터링 스택, Fluent Bit 파일 버퍼(3200에서 노드 디스크 바쁨 20~38%), 지표 계측, keep-alive 75초.
  - **그 p99의 실체**(plan 리뷰 r1 결함 1 지적, 병합 때 0015 원자료로 다시 확인)
    - 단계 p99는 유지 두 분 중 큰 값이다(`collect-metrics.sh` 단계 표). 0015 3200 단계의 나머지 한 분은 `spread` 20.9, `hot` 14.5, `read` 12.1ms로 0014 수준이다.
    - ALB 접근 로그를 분 안의 초로 나누면 **매 분 :00초에 모든 파드가 함께 늦어진다.** :15·:30·:45초에도 약한 봉우리가 있다.
      - `spread` 3200 유지 구간: :00초 p99 323ms(50ms 초과 26%)다. 다른 초는 대부분 3~14ms다. 400 단계도 :00초 168ms 대 다른 초 3~27ms다.
      - 정각 앞뒤 100ms 단위로 보면, 도착은 정각 직전까지 100ms당 약 320건으로 고르다. 0.0초 구간에 520건으로 조금 몰린다.
        그 0.0초에 도착한 요청부터 대상 처리 시간이 수백 ms로 뛴다(최대 447ms). +1.7~2.3초에 두 번째 정지가 있다.
      - 새 파드와 기존 파드가 똑같이 느리다(리뷰 표본: :00~:10초 p99 새/기존 219/181ms).
      - 1차 창(800 rps)에서도 같다. `GOMAXPROCS=1`인 e4도 :00초 p99가 115ms였다(e1 127ms). 로그를 끈 e3는 44ms로 낮았지만 다른 초도 흔들려 결론을 내기 어렵다.
    - `capacity.js`가 k6 시작을 정각에 맞춘다. 그래서 "k6 경과 60초 주기"와 "벽시계 :00"이 겹친다. 지금 자료로는 발생기 쪽과 서버·공유 자원 쪽을 가를 수 없다.
  - Fluent Bit가 3200 rps에서 노드 3대 합 **719m**를 썼다. 같은 때 앱 파드 6개 합은 약 1,166m였다.
  - `argocd_app_info`가 2시간 중 `Synced/Progressing` 6분이었다. HPA가 파드를 늘릴 때다. 규칙이 `health_status!="Healthy"`라 이것을 잡는다.
- **지금 코드**
  - `requestLogger`(`app/internal/httpapi/middleware.go`)는 요청마다 `slog.Info` 한 줄을 쓴다.
    이 쓰기는 **핸들러가 끝나고 소요 시간을 잰 뒤, net/http가 응답 버퍼를 비우기 전**에 일어난다. 그래서 stdout 쓰기가 막히면 그만큼 응답이 늦는다(아래 H2의 근거).
    - 앱 히스토그램과 로그 `duration_ms`는 로그 쓰기 시간을 빼고 잰다. ALB `target_processing_time`은 그 시간을 포함한다. 둘의 차이가 "핸들러 밖" 지연이다.
    - `slog.JSONHandler`는 핸들러 하나의 뮤텍스 아래에서 쓴다. stdout 쓰기 하나가 막히면 그 파드의 모든 요청이 로그 단계에서 줄을 선다.
  - 요청 로그 줄을 읽는 곳은 다음뿐이다. `collect-metrics.sh`는 요청 줄을 읽지 않는다(`db pool`·`click queue`·클릭 실패 줄만 읽는다).
    - 런북 5xx 쿼리(`status >= 500`)와 느린 요청 쿼리(`duration_ms > 1000`)
    - 런북 P-3의 401 `grep`
    - `infra/README.md`의 예시 쿼리
  - ArgoCD 규칙은 `deploy/helm/linkpulse/files/rules.yaml`의 `linkpulse-argocd` 그룹이다. 테스트는 `scripts/rules-test/linkpulse.test.yaml`이다.
  - 차트 Deployment는 `envFrom`(사이트 ConfigMap → 사이트 Secret)으로 사이트 값을 받는다. 그 뒤의 `env`는 같은 키를 덮어쓴다.
    0015 실험은 사이트 ConfigMap(`linkpulse-site`, `infra/k8s` 소유라 ArgoCD selfHeal이 되돌리지 않는다)을 `kubectl patch`했다.
  - `collect-metrics.sh`의 스로틀은 파드 합계 하나(분 단위)다. 파드별 값, 노드 디스크, Fluent Bit CPU는 수집 표에 없다. 0015 결과의 719m와 디스크 바쁨은 창에서 손으로 물은 값이다.
  - 0015 원자료(로컬, gitignore)
    - ALB 접근 로그 `load/k6/results/raw/0015-alb-logs/`: 요청마다 대상 IP와 `target_processing_time`이 있다.
    - `raw/0015-*-prom.json`의 `alive`: 파드 이름 ↔ IP, 분마다 살아 있었는지.
- **인프라**: Terraform 코드를 바꾸지 않는다. 바뀌는 것은 앱·차트·측정 스크립트·문서다. 차트는 머지 후 ArgoCD가 배포한다.
- **가드레일**(AGENTS.md §2)
  - 측정 창의 `full-apply`·측정 예외 apply·`destroy`·`kubectl patch`/`rollout restart`는 **사람이** 실행한다.
  - 에이전트는 읽기 명령(`kubectl get`, Prometheus 쿼리, `aws` 조회·접근 로그 내려받기)과 로컬 분석·검사까지만 한다.
  - 비밀은 화면에 찍지 않는다. 관리자 토큰은 `read -rs`로 받는다.
- **원칙(AGENTS.md §5 자유 이동)**: 런타임 기본값은 차트에 둔다(사이트 무관). 사이트는 그 값을 덮어쓸 수 있어야 한다. P6 k3s에서도 같은 차트·같은 기본값을 쓴다.

## 설계

### 1. 차트 런타임 기본값 — `GOMAXPROCS`·표본 비율

- 차트에 ConfigMap `linkpulse-defaults`를 새로 둔다. 이름은 다른 리소스처럼 `linkpulse.fullname` 헬퍼로 만든다(`<fullname>-defaults`). 값은 `values.yaml`의 `runtimeDefaults`에서 온다.
  ```yaml
  runtimeDefaults:
    GOMAXPROCS: "1"          # CPU 한도 250m(1코어 미만). 한도를 1코어 이상으로 바꾸면 다시 정한다
    ACCESS_LOG_SAMPLE: "100" # 성공·빠른 요청 로그 1/100. 장애 때 사이트 ConfigMap에 "1"을 넣어 전부 남긴다
  ```
- `envFrom` 순서는 **`linkpulse-defaults` → 사이트 ConfigMap → 사이트 Secret**이다. 같은 키는 뒤의 출처가 이긴다.
  그래서 사이트가 기본값을 덮어쓸 수 있다. 이 규칙은 쿠버네티스 API 문서(`EnvFromSource`)에서 확인해 주석에 출처를 남긴다.
- **`env`가 아니라 ConfigMap인 이유**: `env`는 `envFrom`을 늘 덮어쓴다. 그러면 사이트가 바꿀 수 없다.
  장애 때 표본을 끄거나, 측정 창에서 비교 실행을 하려면 사이트 ConfigMap으로 바꿀 수 있어야 한다.
  차트 ConfigMap은 ArgoCD가 소유하므로 창에서 직접 고치지 않는다(selfHeal이 되돌린다).
- 파드 템플릿에 `checksum/defaults` 주석을 단다. 기본값이 바뀌면 파드가 다시 뜬다. 앱은 기동할 때만 환경 변수를 읽기 때문이다.
- **`GOMAXPROCS=1`의 근거**: 250m 한도에서 Go가 2로 잡는다(0015 E1 실측). 두 스레드가 100ms 주기의 25ms 몫을 더 빨리 쓰고, 스케줄러·futex 비용이 든다.
  E4에서 −10~15%였다. 3200 rps 같은 높은 부하에서 지연이 나빠지지 않는지는 이번 창(R0 대 R1)에서 확인한다.

### 2. 요청 로그 표본화 (앱)

- 환경 변수 `ACCESS_LOG_SAMPLE`=N을 둔다(정수 ≥ 1).
  - 앱 기본값은 1(전부 남김)이다. 로컬·docker-compose 동작은 그대로다.
  - 0·음수·숫자가 아니면 기동에 실패한다(설정 오류).
- **늘 남기는 줄**
  - 상태 ≥ 400(4xx·5xx): 런북의 401 grep과 5xx 쿼리가 계속 동작한다.
  - 소요 ≥ 100ms(SLO 지연 경계): 런북의 `duration_ms > 1000` 쿼리도 여기 포함된다.
- **나머지**(2xx·3xx이고 100ms 미만)는 1/N 확률로 남긴다(`math/rand/v2`).
  - 계수기(N번째마다) 대신 무작위로 하는 이유: 요청 순서가 주기를 가지면(k6가 링크를 차례로 돈다) 특정 링크만 남을 수 있다. 그리고 공유 상태가 없다.
- 지표(`ObserveRequest`)는 표본과 무관하게 모든 요청을 센다. 지표를 먼저 기록한 뒤 로그를 쓸지 정한다.
  - **요청 수는 로그가 아니라 지표(ALB·Prometheus)로 본다.** 이 점을 `infra/README.md` 로그 절과 런북에 적는다.
- 기대 효과: 3200 rps에서 요청 줄이 초당 약 3,200 → 약 32 + (오류·느린 줄)로 준다. 10초 주기 줄(`click queue`·`db pool`)은 그대로다.
  6400 과부하에서는 100ms를 넘는 요청(0015에서 약 1%)이 모두 남는다. 그래도 지금의 수십 분의 1이다.
- 이 변경은 H2(로그 쓰기 경로)의 개입이기도 하다(아래 4).

### 3. ArgoCD 알림 — `Progressing` 제외

- 식을 다음처럼 바꾼다(`for: 15m`과 `max by (name)`은 그대로).
  ```
  max by (name) (argocd_app_info{sync_status!="Synced"} or argocd_app_info{health_status!~"Healthy|Progressing"})
  ```
- **멈춘 롤링을 놓치지 않는 이유**
  - 차트가 `progressDeadlineSeconds`를 두지 않아 기본값 600초다. 그 안에 진행이 없으면 Deployment의 Progressing 조건이 `ProgressDeadlineExceeded`가 된다.
  - 그러면 ArgoCD가 Deployment를 `Degraded`로 본다 → 이 규칙이 15분 뒤 울린다(최악 약 25분).
  - 근거는 구현 때 1차 출처로 확인해 주석에 남긴다: 쿠버네티스 Deployment 문서, gitops-engine `pkg/health/health_deployment.go`(설치한 ArgoCD 버전 태그).
  - **확인 결과가 다르면**(Degraded로 바뀌지 않으면) `Progressing`만 따로 45분 지속 조건으로 두는 규칙으로 바꾸고 plan에 적는다.
  - **멈춘 HPA 확장은 `Degraded`가 되지 않는다**(리뷰 r1 제안).
    - 롤링이 끝난 Deployment에서 확장 파드가 Pending으로 남으면, deployment controller(`syncRolloutStatus`)는 진행 기한 검사를 하지 않는다. ArgoCD는 계속 `Progressing`으로 보고 이 규칙은 울리지 않는다.
    - 이 경우는 kube-prometheus-stack 기본 규칙 `KubeDeploymentReplicasMismatch`·`KubePodNotReady`가 잡는다. 멈춘 롤링 자체도 기본 규칙 `KubeDeploymentRolloutStuck`이 `ProgressDeadlineExceeded`로 바로 잡는다(이중으로 덮인다).
    - 이 경로와 세 규칙 이름도 1차 출처와 렌더된 기본 규칙에서 확인한다. 런북 P-4에 "확장이 멈춘 것은 `KubeDeploymentReplicasMismatch`가 잡는다"를 적는다.
- HPA 확장 중 `Progressing`을 낸 리소스가 Deployment인지 HPA인지 창에서 기록한다(Application `status.resources`의 health). 런북 P-4에 적는다.
- promtool 테스트를 더한다.
  - `Progressing` 6분 → 울리지 않는다(0015 실측 모양).
  - `Progressing` 30분 → 울리지 않는다(설계상 `Degraded` 전환에 맡긴다).
  - `Degraded` 15분 → 울린다.
  - `OutOfSync` 15분 → 울린다(기존 테스트가 있으면 그대로 둔다).

### 4. 매 분 :00초 정지 — 가설과 가르는 법

**현상**(배경): 매 분 :00초에 모든 파드가 함께 수백 ms 늦어진다. :15·:30·:45초에도 약한 봉우리가 있다. 50~3200 모든 부하에서 보인다. 단계 p99는 그 분에 정지가 얼마나 걸렸는지를 잰다.

| | 가설 | 지금 근거 | 가르는 법 |
| --- | --- | --- | --- |
| G | **발생기(k6·노트북).** k6 시작이 정각에 맞춰져 k6 쪽 주기나 노트북의 정각 작업이 요청 흐름을 흔든다 | 약하다. 정각 직전 도착이 고르고, 0.0초 몰림이 작다(100ms당 320 → 520건) | **r0에서 k6 시작을 정각에서 30초 비킨다.** 정지가 :30으로 옮겨 가면 발생기 쪽이다 |
| S | **공유 자원·벽시계 작업.** 모든 파드가 같이 기다리는 것이 정각에 멈춘다(RDS — 리다이렉트·조회 모두 DB를 거친다, ALB, 노드 공통 작업) | 모든 파드가 동시에, 정각 0.0초부터 늦어진다. `read`(클릭 없음)에도 있다 | r0에서 정지가 :00에 남는다. 정지 중 고루틴이 DB 연결·읽기에서 기다린다 |
| H1 | **CFS 스로틀**(파드 국소) | 정지의 원인으로는 약하다. `GOMAXPROCS=1`(e4)에도 :00 정지가 있었다 | 정지 밖 꼬리: r0 → r1. 정지 중 고루틴이 실행 대기(runnable)에 몰리는가 |
| H2 | **로그 쓰기 경로**(파드·노드 국소, 배경) | 0014 → 0015에서 바뀐 것. 정지와의 관계는 모른다(e3는 흔들려 결론 없음) | 정지 중·정지 밖 고루틴이 `slog` 핸들러 뮤텍스·`os.(*File).Write`에서 기다리는가. 앱 `duration_ms`(로그 쓰기 제외)와 ALB 시간의 차이 |

- **창 전 분석(인프라 없이)**: `load/k6/alb-latency.py`를 새로 만든다. `alb-errors.py`와 같은 방식이다(로컬 파일만 읽고, 클라이언트 IP는 찍지 않는다).
  분 단위 출력으로는 1~3초짜리 정지가 보이지 않는다. 그래서 **초 단위로** 낸다.
  - 입력
    - 접근 로그 디렉터리
    - 실행 시작 epoch(단계 3분, `capacity.js`와 같은 계산)
    - 그 실행의 `-prom.json`(`alive`로 IP ↔ 파드 이름과 첫 분)
  - 출력(단계별)
    - 분 안의 초(벽시계 0~59)별 p99와 느린 요청(50ms 초과) 수. 새 파드(첫 요청 뒤 2분 안)와 기존 파드로 나눈다.
    - k6 시작 기준 초별로도 같은 표를 낸다. 시작이 정각이면 두 표가 같고, r0(30초 비킴)에서는 다르다.
    - 정지 초 앞뒤 3초의 도착 수(`request_creation_time`)와 대상 처리 시간을 100ms 단위로 낸다. 발생기가 몰아 보냈는지가 보인다.
    - **정지 p99**(벽시계 :00~:02초 + k6 기준 :00~:02초)와 **정지 밖 p99**(나머지)를 따로 낸다.
  - 먼저 0015 원자료에 돌린다.
    - 재측정 셋(`spread`·`hot`·`read`, 모든 단계)과 1차 창 실험 넷(e1·e3·e4·e1b, 800 rps)이다.
    - e3(로그 끔)·e4(`GOMAXPROCS=1`)의 정지 크기를 e1·e1b와 비교한다. 결과를 결과 문서 초안 "창 전 분석"에 적는다.
  - **이 분석이 위 넷 밖의 원인을 가리키면 창 실험을 바꾸고 plan revision을 올린다.**
- **창 실험 — 같은 창 안에서 하나씩 바꾼다**(`spread`, `MAX_RPS=3200`, 50 → 3200 7단계, 약 21분, 실행마다 파드 2개에서 시작)

  | 실행 | k6 시작 | 사이트 ConfigMap 덮어쓰기 | 상태 |
  | --- | --- | --- | --- |
  | `r0` | **정각 + 30초** | `GOMAXPROCS=2`, `ACCESS_LOG_SAMPLE=1` | 0015와 같은 설정. 발생기 판별 |
  | `r1` | 정각 | `ACCESS_LOG_SAMPLE=1` | `GOMAXPROCS`만 1 |
  | `r2` | 정각 | 없음 | 0016 기본값 |

  - 순서를 r0 → r1 → r2로 두는 이유: 덮어쓰기를 하나씩 지우면 마지막이 기본값이다. 되돌리는 단계가 따로 없다.
  - `spread` 하나만 쓰는 이유: 0015의 세 시나리오가 같은 정지를 보였다. 창 시간을 줄인다.
  - **k6 시작 비키기**
    - `capacity.js`에 환경 변수 `START_OFFSET_S`(기본 0)를 더한다. 시작을 "다음 (정각 + 오프셋)"으로 맞춘다.
    - r0에서 이 값을 30으로 둔다. 실행을 따로 더하지 않으므로 창 시간이 늘지 않는다.
    - **수집 격자**: CloudWatch는 벽시계 분 단위로만 준다. 그래서 r0의 `collect-metrics.sh`에는 epoch − 30(직전 정각)을 준다.
      - 30초 비킨 단계는 램프 30초가 끝나는 정각부터 유지가 2분 30초다. 그래서 수집기가 고르는 유지 두 분(단계 둘째·셋째 분)이 벽시계 두 분과 그대로 겹친다.
      - `alb-latency.py`에는 실제 epoch를 준다.
  - **정지 중 고루틴 스냅숏**(읽기 명령)
    - 실행마다 3200 단계 동안 파드 하나에서 `/debug/pprof/goroutine?debug=1`을 받는다. 시각은 정지 초 + 0.2초와 + 30.2초(대조)다.
    - 요청 고루틴이 어디서 기다리는지 센다.
      - DB 연결 대기·DB 읽기 → S
      - `slog` 핸들러 뮤텍스·`os.(*File).Write` → H2
      - 실행 대기 → H1
      - 네트워크 쓰기 → ALB·회선
  - **앱 쪽 시간**: r0·r1은 모든 요청을 로그로 남긴다. Logs Insights로 정지 초의 `duration_ms` p99를 본다.
    - 앱 시간에도 정지가 들어가면 핸들러 안(DB·CPU)에서 멈춘 것이다.
    - 앱 시간에는 없고 ALB 시간에만 있으면 핸들러 밖(로그 쓰기·연결·네트워크)이다.
  - 같은 설정을 두 번 돌리지 않는다. 대신 정지와 정지 밖을 나눠 본다. 같은 단계 안에서 분별 p99가 2.5~4.3배 흔들리는 것은 대부분 정지가 그 분에 얼마나 걸렸느냐다.
- **판정 순서**
  1. **정지가 재현됐는가**: r1·r2에서 정지 초의 50ms 초과 비율이 다른 초 중앙값의 5배 이상이면 재현이다.
  2. **r0에서 정지가 어디에 있나**: :30(k6 기준 :00)으로 옮겨 갔으면 G다. :00에 남았으면 S 쪽이고, 고루틴 스냅숏과 앱 쪽 시간으로 범위를 좁힌다.
  3. G로 판명되면 결과 문서에 적는다: 0012~0015 p99에 발생기 산물이 섞였다. "0014 대비 2~6배"는 서버 회귀가 아니다.
  4. **정지 밖 꼬리**는 r0 → r1 → r2의 정지 밖 p99로 본다. H2를 "가렸다"고 하려면 다음 중 하나가 있어야 한다. 표본화는 앱 CPU도 줄여(0015 E3 −1~−8%) H1 효과가 H2처럼 보일 수 있기 때문이다.
     - r2에서 스로틀이 그대로였다.
     - 고루틴 스냅숏에 로그 쓰기 대기가 직접 보인다.
  5. 고칠 것은 결과에 권고로만 적는다. CPU 한도·HPA는 6400 벽 plan으로 넘긴다. RDS·ALB 쪽이면 그 근거를 다음 plan의 입력으로 둔다.
- **수집 추가**(`collect-metrics.sh`, `-prom.json`에 저장)
  - 늘 넣는 것(목표 2·4의 판정에 쓴다)
    - Fluent Bit 컨테이너 CPU(노드별, `logging` 네임스페이스)
    - `ALERTS{alertname="LinkpulseArgoCDAppUnhealthy"}`와 `argocd_app_info` health별 분 수
    - CloudWatch 앱 로그 그룹 `IncomingBytes`(분 단위)
    - Logs Insights `http request` 줄 수(분 단위)
  - **파드별 스로틀·노드 디스크 바쁨·`kube_pod_info`는 단계 1 결과를 보고 필요할 때만 넣는다.** 분 단위로는 정지가 보이지 않는다.
  - 표에는 단계별로 Fluent Bit CPU·로그 바이트·요청 줄 수를 더한다. 초 단위 판정은 `alb-latency.py`가 낸다.
  - 없는 지표는 건너뛴다. 0014·0015 원자료를 다시 수집해도 기존 표가 그대로 나와야 한다.

## 실행 단계

1. **창 전 분석** — `load/k6/alb-latency.py`(초 단위, 위 4)를 작성한다. 0015 원자료(재측정 셋 모든 단계, 1차 창 e1·e3·e4·e1b)에 돌린다.
   → 검증:
   - 분별 전체 p99가 0015 `-metrics.md`의 CloudWatch p99와 같은 분에서 10% 안이다(CloudWatch 백분위는 근사라 정확히 같지 않다).
   - `spread` 3200 유지 구간의 :00초 p99가 병합 때 손으로 잰 값(323ms)과 같다.
   - 출력에 클라이언트 IP가 없다(`grep`).
   - 결과(정지 모양, e3·e4 대비, 새/기존 파드, 정각 앞뒤 도착)를 결과 문서 초안 "창 전 분석" 절에 적는다. 단계 4의 조건부 수집을 넣을지 여기서 정한다.
2. **앱** — `config`에 `ACCESS_LOG_SAMPLE`(검증 포함)을 더하고 `requestLogger`에 표본을 붙인다.
   → 검증: `go test -race ./...`. 테스트:
   - N=1이면 모든 요청이 남는다.
   - 표본 판정(난수원)을 주입해 테스트를 결정적으로 만든다(확률에 기대지 않는다). "뽑히지 않음"으로 고정하면 빠른 200·302는 남지 않는다. 그래도 5xx·4xx(401 포함)·100ms 이상은 남는다.
   - 표본으로 빠진 요청도 지표에는 세어진다.
   - 0·음수·문자 값은 설정 로드가 실패한다.

   **변이 검사**(`scratchpad/mut`): 아래 각각에서 테스트가 실패해야 한다.
   - 늘 남기는 조건에서 4xx를 뺀다.
   - 느린 요청 경계를 지운다.
   - N을 무시한다(늘 남긴다).
   - 표본으로 빠질 때 지표도 건너뛴다.
3. **차트·규칙** — `linkpulse-defaults` ConfigMap, `envFrom` 순서, `checksum/defaults`, `runtimeDefaults` 값. ArgoCD 규칙 식과 promtool 테스트 4개.
   → 검증: `scripts/check-charts.sh`를 확장한다.
   - 렌더 단언(이름은 렌더된 `<fullname>-defaults`로 본다): `envFrom` 첫째가 그 ConfigMap이고 그 뒤가 사이트 ConfigMap·Secret이다. ConfigMap에 `GOMAXPROCS: "1"`·`ACCESS_LOG_SAMPLE: "100"`이 있다. `env`에 두 키가 없다(있으면 사이트가 덮어쓸 수 없다). 파드 템플릿에 `checksum/defaults`가 있다.
   - promtool 테스트 통과. CI에서도 돈다.

   **변이 검사**:
   - `envFrom` 순서를 바꾼다.
   - 두 키를 `env`로 옮긴다.
   - 규칙 식을 `health_status!="Healthy"`로 되돌린다(→ "Progressing 30분 미발화"가 실패).
   - `Degraded`까지 빼 버린다(→ "Degraded 발화"가 실패).
4. **측정 도구**
   - `collect-metrics.sh` 수집 추가(위 4. 조건부 항목은 단계 1 결과대로)
   - `capacity.js` `START_OFFSET_S`(기본 0)
   - 고루틴 스냅숏 명령. 정지 초 + 0.2초·+ 30.2초에 맞춰 받는다. 읽기 명령이다
   - `load/k6/README.md` "운영 측정"에 0016 창 절차. 들어갈 것:
     - r0~r2, 실행 사이 ConfigMap 덮어쓰기
     - r0의 오프셋과 `collect-metrics`에 epoch − 30을 주는 이유
     - ArgoCD health 기록, 스냅숏, 정지 초 `duration_ms` Logs Insights 쿼리, `alb-latency.py`
   → 검증:
   - `bash -n`·shellcheck.
   - 가짜 도구(kubectl port-forward·Prometheus 응답 JSON)로 끝까지 돈다.
   - 0015 원자료 재수집 표가 기존과 같다(새 절은 지표가 없으면 건너뛴다).
   - `k6 inspect`로 `capacity.js`가 읽힌다. 시작 시각 계산이 오프셋 0이면 지금과 같고 30이면 "정각 + 30초"임을 작은 계산 확인(노드 한 줄 또는 주석의 예)으로 보인다.
5. **문서**
   - **ADR 0009**(짧게): 요청 로그 표본화(무엇을 늘 남기나, 요청 수는 지표로), 런타임 기본값을 차트 ConfigMap으로 두고 사이트가 덮어쓰는 구조, `GOMAXPROCS`와 CPU 한도의 관계.
   - 런북: P-4 의미(Progressing 제외, 멈춘 롤링은 Degraded로 잡힘, 멈춘 확장은 `KubeDeploymentReplicasMismatch`가 잡음), 5xx·느린 요청 쿼리와 P-3 401 grep이 표본과 무관하다는 한 줄, "장애 때 요청 로그 전부 남기기"(사이트 ConfigMap `ACCESS_LOG_SAMPLE=1` + `rollout restart`) **[사람]**.
   - `infra/README.md` 로그 절: 요청 줄은 표본이다, 요청 수는 지표로 본다.
   → 검증: 문서의 키·이름·명령이 차트·코드와 같다(`grep`).
6. **코드 검토 1라운드(claude-ide) → 커밋·PR·머지 [사람].** 머지 후 CI(GHCR 푸시)가 인프라 없이 성공해야 한다.
7. **측정 창 [사람]** — 약 2시간 30분. 단계마다 확인 지점이 있고, 걸리면 멈춘다.
   1. `full-apply-prod.sh`. 확인:
      - 노드 3대, 모든 파드 Ready, TGB 대상 healthy.
      - 앱 파드 모두 `go_sched_gomaxprocs_threads` = 1.
      - `linkpulse-defaults`가 있다.
      - linkpulse 규칙 그룹이 `health: ok`다.
   2. 측정 예외 apply + ALB 접근 로그(0015 §9-5와 같다).
   3. 클릭 파이프라인 스모크(0015 §9-7과 같다).
   4. r0 → r1 → r2. 실행마다 다음을 한다.
      - 사이트 ConfigMap 덮어쓰기 → `rollout restart` → 파드 2개 대기
      - **확인 한 줄**: 모든 앱 파드의 `go_sched_gomaxprocs_threads`(r0 2, r1·r2 1)와 사이트 ConfigMap의 두 키(0015 e4 마지막 확인 줄과 같은 방식). 다르면 멈춘다
      - k6 `spread` 3200(r0만 `START_OFFSET_S=30`), `record-pods.sh`, ArgoCD health·리소스 기록(30초 간격), 3200 단계 고루틴 스냅숏
      - 수집(r0는 epoch − 30)
   5. 마지막 요청 뒤 5분 넘게 기다렸다가 접근 로그를 내려받는다. 실행 셋에 `alb-latency.py`를 돌린다.
   6. destroy(k8s → prod)하고 state 0을 확인한다.
   → 검증: 아래 판정에 쓴 원자료가 모두 있다. 판정에 쓴 단계가 0012 판정 0번(발생기 한계)에 걸리지 않았다.

   **창 진행 방식**: 명령마다 **무엇·왜·통과 기준**을 먼저 알린다. 실행 기록은 에이전트가 `docs/plans/0016-observability-followups/commands-explained.md`(gitignore)에 이어 쓴다.
   형식은 0015 §9·§10과 같다: 절마다 UTC 시각, 실행한 명령 블록, 부분별 해설 표(무엇·왜), 통과 기준, 멈추는 경우, 결과. 오류는 증상·원인·해결을 따로 적고 `ops-gotchas`에도 남긴다.
8. **결과 기록** — `load/k6/results/0016-followups.md`, docs PR [사람]. 넣을 것:
   - 판정표
   - 창 전 분석
   - r0~r2 단계 표
   - 정지 판정(G/S, 고루틴 대기 위치, 앱 쪽 시간)과 정지 밖 꼬리의 H1·H2 "가렸다/못 가렸다"·근거
   - G로 판명되면 0012~0015 p99에 발생기 산물이 섞였다는 정정
   - 표본화 전후 Fluent Bit CPU·로그 양
   - "다음으로 넘길 것": 6400 벽 plan에 쓸 값(H1이 남으면 CPU 한도 제안 포함), S면 그 공유 자원의 근거, 노드 세대 실험

검토: plan 검토 1라운드(claude-ide) → 구현 → 코드 검토 1라운드 → 창. 블로커가 없으면 라운드를 더 열지 않는다.

## 판정 (측정 창)

| # | 조건 | 통과 기준 |
| --- | --- | --- |
| (a) | `GOMAXPROCS` 기본값 | r2의 앱 파드가 모두 1이고, r0는 2다. 아래 셋을 함께 본다 |
| | | r1의 50~1600 단계 파드당 cAdvisor 밀리코어가 r0보다 낮다. 차이가 6~10%(0015 흔들림)를 넘으면 "확인", 안이면 "흔들림 안"으로 적는다 |
| | | r1의 3200 단계 p95가 r0보다 1ms 넘게 높지 않다 |
| | | **꼬리**: r1의 1600·3200 단계 **정지 밖 p99**(`alb-latency.py`)가 r0보다 2ms와 20% 중 큰 값을 넘게 높지 않다 |
| | | 꼬리나 p95 기준을 넘으면 차트 기본값을 `"2"`로 되돌리는 PR을 낸다(기본값은 머지와 함께 운영에 나가고 창은 사후 확인이라 조치를 미리 정해 둔다). 결과에 적는다 |
| (b) | ArgoCD 알림 | 세 실행(HPA 2 → 6 확장 포함) 동안 `LinkpulseArgoCDAppUnhealthy`가 pending도 되지 않는다. `Progressing`이 있었다면 낸 리소스를 적는다. promtool 테스트가 CI에서 통과한다 |
| (c) | :00 정지 원인 | 위 4 "판정 순서"대로 적는다: 재현 여부(정지 초 50ms 초과 비율이 다른 초 중앙값의 5배 이상), r0에서 정지 위치(G/S), 고루틴 대기 위치, 앱 쪽 시간 포함 여부. 정지 밖 꼬리의 H1·H2는 실행 간 변화와 직접 증거가 같은 쪽일 때만 "가렸다". 나머지는 "못 가렸다"와 이유 |
| (d) | 요청 로그 표본화 | r2 3200 단계의 분당 `http request` 줄 수가 r1의 약 1/100 + (4xx·5xx·100ms 이상 요청 수)와 맞는다. Fluent Bit CPU 합과 로그 그룹 `IncomingBytes`의 r1 대비 감소를 적는다. 주기 줄 유실 검사(0015 (c))가 그대로 통과한다 |
| (e) | 정합성(회귀) | 세 실행 모두 `recorded − dropped = applied + unknown = DB 합계`가 정수로 맞는다(0015 (b)와 같은 계산·무효 조건) |

## 리스크/롤백

| 리스크 | 완화 | 롤백 |
| --- | --- | --- |
| 장애 때 필요한 요청 줄이 표본으로 빠짐 | 4xx·5xx·100ms 이상은 늘 남는다. 요청 수는 지표로 본다 | 사이트 ConfigMap `ACCESS_LOG_SAMPLE=1` + `rollout restart`(런북) |
| `GOMAXPROCS=1`이 높은 부하에서 지연을 늘림(GC·긴 요청이 한 P를 막음) | r0 대 r1 p95·정지 밖 p99 비교, 판정 (a) | 당장은 사이트 ConfigMap `GOMAXPROCS=2`, 이어서 차트 기본값 `"2"` PR |
| `envFrom` 순서 실수로 사이트가 덮어쓰지 못하거나 기본값이 사라짐 | 렌더 단언·변이 검사 | 차트 수정 |
| 기본값을 바꿔도 파드가 옛 값을 씀 | `checksum/defaults` 주석 | `rollout restart` |
| 창에서 차트 ConfigMap을 고쳐 selfHeal이 되돌림 | 실험은 사이트 ConfigMap만 고친다(README·commands-explained에 명시) | — |
| `Progressing` 제외로 멈춘 롤링을 놓침 | `progressDeadlineSeconds` 600 → `Degraded` → 15분 뒤 발화. 1차 출처 확인 | 별도 `Progressing` 45분 규칙 |
| 정지가 창에서 재현되지 않음 | 창 전 분석(0015 원자료 일곱 실행)을 근거로 남긴다 | 결과에 "재현되지 않음"으로 적고 다음 창으로 |
| 정지의 크기가 분마다 달라 실행 간 차이를 원인으로 착각 | 정지와 정지 밖을 나눠 본다. 두 증거가 같은 쪽일 때만 "가렸다" | — |
| k6 오프셋 실행의 수집 격자가 단계와 어긋남 | `collect-metrics`에 epoch − 30(유지 두 분이 벽시계 두 분과 겹친다), `alb-latency`에는 실제 epoch | r0 표를 접근 로그 기준으로만 쓴다 |
| 고루틴 스냅숏이 정지를 놓침(시각 어긋남·port-forward 지연) | 3200 단계 두 분 동안 정지 초마다 받는다, 대조 시각도 받는다 | 앱 쪽 시간(Logs Insights)으로만 판단 |
| 측정 창 시간 | 실행 셋, 각 약 30분(대기·수집 포함) | r2만 하고 r0·r1을 다음 창으로 미룬다(그때 (c)는 미완) |

## 검토 반영 로그

<!-- /plan-merge가 라운드별로 기록. 형식: [rN] 리뷰어#번호 지적요약 → 반영|기각 — 사유 -->

r1 리뷰는 claude-ide 하나다(codex 사용 불가). `reviewed-revision: 1` 일치.

- [r1] claude-ide#1 "3200 p99 2~6배"의 실체는 매 분 :00초 전 파드 동시 정지(모든 부하)이고, 단계 p99는 두 분 중 큰 값이라 H1~H3·r0 재현 기준이 현상을 가르지 못함 → 반영 — 병합 때 0015 원자료로 다시 확인했다. `spread` 3200 유지 :00초 p99 323ms 대 다른 초 3~14ms, 400 단계도 같다. 정각 직전 도착은 고르고, e4(`GOMAXPROCS=1`)에도 정지가 있다. 목표 3·배경·설계 4를 정지 중심으로 다시 썼다: 가설 G(발생기)·S(공유 자원)를 앞에 두고, `alb-latency.py`를 초 단위로, r0에 k6 시작 30초 비킴, 정지/정지 밖 p99 분리, 파드별 스로틀·디스크 수집은 조건부. 리뷰에 없던 것을 더했다: 오프셋 실행은 CloudWatch 분 격자와 어긋나므로 `collect-metrics`에 epoch − 30을 준다(유지 두 분이 벽시계와 겹친다). 정지 중 고루틴 스냅숏과 정지 초 앱 `duration_ms`로 대기 위치를 본다
- [r1] claude-ide#2 판정 (a)에 꼬리 기준이 없고 실패 시 조치가 없음 → 반영 — 1600·3200 정지 밖 p99가 r0 + max(2ms, 20%)를 넘지 않을 것. 넘으면 차트 기본값 `"2"` PR. 리스크 표 롤백도 맞춤
- [r1] claude-ide#3 H2 근거 문장이 코드와 다름(로그는 핸들러 뒤·버퍼 비우기 전) → 반영 — 문장 정정. 앱 시간 대 ALB 시간 차이와 `slog` 핸들러 뮤텍스를 배경에 더했다
- [r1] claude-ide 제안: 멈춘 HPA 확장은 Degraded가 되지 않음 → 반영 — 기본 규칙 `KubeDeploymentReplicasMismatch`·`KubePodNotReady`·`KubeDeploymentRolloutStuck`이 덮는다고 설계 3에 적고, 1차 출처·렌더된 규칙 확인 대상과 런북 P-4에 넣었다
- [r1] claude-ide 제안: r1→r2는 표본화가 CPU도 줄여 H1이 H2처럼 보일 수 있음 → 반영 — H2 "가렸다"에 r2 스로틀 불변 또는 고루틴 스냅숏의 로그 쓰기 대기를 요구
- [r1] claude-ide 제안: 표본 테스트를 결정적으로 → 반영 — 난수원 주입(단계 2)
- [r1] claude-ide 제안: ConfigMap 이름을 `linkpulse.fullname`에서 → 반영 — 설계 1·단계 3 렌더 단언
- [r1] claude-ide 제안: r1·r2 재시작 뒤에도 GOMAXPROCS·ConfigMap 키 확인 → 반영 — 단계 7-4 확인 한 줄(다르면 멈춘다)

**판정(r2):** 결함 3개와 제안 5개를 모두 반영했다. 결함 1은 리뷰어가 낸 수정 방향을 원자료로 다시 확인한 뒤 반영했다. 수집 격자 문제는 리뷰에 없던 것이라 직접 더했다. 남은 쟁점이 없다. "블로커가 없으면 라운드를 더 열지 않는다"에 따라 `approved`.
