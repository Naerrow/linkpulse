---
status: approved # in-review | approved
revision: 2
created: 2026-10-14
---

# 0015. Prometheus·Grafana 관측성 — 앱 지표, SLO 알림, 0014 이월 항목 정리

## 목표

1. **앱이 Prometheus 지표를 낸다.** 사이트와 무관한 같은 차트로 수집·알림·대시보드를 붙인다.
   - 요청(RED)·클릭 파이프라인·DB 풀·Go 런타임·프로세스 CPU를 낸다.
   - 지표는 공개 경로가 아닌 내부 포트로 낸다.
2. **SLO 두 개와 에러 버짓 소진 알림.** 알림이 Slack에 도착하는 것까지 확인한다.
3. **0014에서 넘어온 것을 처리한다**(`load/k6/results/0014-eks.md` "0015로 넘길 것"). 각 항목의 처리:
   - 요청당 CPU 2~4배 → 원인을 데이터로 가른다.
   - 정합성 → 로그 합계가 아니라 지표 카운터로 판정한다.
   - Fluent Bit 유실 → 고친다.
   - 클릭 발행 실패 알림 → 둔다.
   - 502·타임아웃 → 원인을 분류한다.
4. **P5 종결을 겸한다.** 측정 창 하나에서 두 가지를 확인한다.
   - `full-apply-prod.sh`로 처음부터 재현되는가.
   - 세 시나리오를 다시 재 0012 기준선과 비교한다(AGENTS.md §5 "종결").

## 배경/제약

- **0014 결과**(`load/k6/results/0014-eks.md`):
  - 세 시나리오 모두 한계 ≥6400이었고, 무릎은 6400(HPA 최대 6 × 250m 포화)이었다.
  - 넘어온 항목은 6개다.
    1. EKS 파드의 요청당 CPU가 ECS의 2~4배다. 원인 후보는 넷이다: 지표 출처, 노드 세대, CPU 한도·`GOMAXPROCS`, 로그 경로.
    2. HPA 최대와 DB 연결 예산.
    3. 정합성 판정이 로그 합계에 기대고 있다. 그런데 `hot` 6400에서 파드 하나의 로그가 2분 빠졌다(G-15).
       원인은 Fluent Bit `Mem_Buf_Limit 5MB`로 입력이 멈춘 사이에 kubelet이 10Mi마다 회전한 것이다.
    4. SQS 메시지 나이 알림.
    5. ALB 502 12건(모두 정각 0초)과 60초 타임아웃 18건.
    6. 클릭 발행 실패 알림. 0013에서 이월됐다.
- **이미 있는 것**
  - CloudWatch 알람: ALB 5xx(타깃·ELB)·지연·unhealthy, RDS, SQS DLQ·가장 오래된 메시지 5분, canary(us-east-1). 경로는 SNS → Chatbot(Slack) + SMS다.
  - `monitoring.tf` 주석: "파드 CPU·파드 수 알람은 0015". 0014 리스크 표: "ArgoCD 동기화 실패 알림은 0015".
  - 앱 `clicks.Stats`(원자 카운터 10개)와 DB 풀 통계는 지금 로그로만 나간다. `requestLogger`는 요청마다 JSON 한 줄을 쓴다.
  - `infra/prod` 변수 `enable_alb_access_logs`·`alb_access_logs_bucket`이 있다. 버킷은 밖에서 만든다는 전제다.
- **노드 수용**(ADR 0007): t4g.medium 2대, 노드당 파드 17개다. 앱 최대 7개를 넣으면 **26/34**다.
  kube-prometheus-stack을 더하면 operator·Prometheus·Alertmanager·Grafana·kube-state-metrics와 노드마다 node-exporter가 붙는다(약 7~8개).
  그러면 33~34/34로 꽉 찬다. 메모리 요청도 약 4.3 → 5.9 / 6.7GiB가 된다. **노드를 3대로 늘린다**(아래 설계).
- **가드레일**(AGENTS.md §2)
  - `terraform plan`·`apply`·`destroy`, `kubectl apply`·`patch`, 변경형 `aws`는 사람이 실행한다. 에이전트는 `fmt`·`validate`·tfsec·`helm lint`·`helm template`·kubeconform까지다.
  - AWS로 가는 description은 ASCII만 쓴다.
  - IAM·AWS 서비스 동작에 대한 단정은 1차 출처(Service Authorization Reference, 서비스 문서)로 확인하고 출처를 주석에 남긴다.
- **원칙(AGENTS.md §5 자유 이동)**: 알림 규칙·대시보드·ServiceMonitor는 차트에 둔다(사이트 무관). 알림을 어디로 보낼지(수신처)는 사이트 값이다(`infra/k8s`). P6 k3s에서 같은 차트를 그대로 쓴다.

## 설계

### 수집 구성 — 클러스터 안 kube-prometheus-stack

- `infra/k8s`에 `helm_release`로 kube-prometheus-stack을 넣는다(네임스페이스 `monitoring`). 차트 버전은 구현 때 1차 출처(GitHub 릴리스)로 확인해 고정한다.
- **AMP·AMG를 쓰지 않는 이유**: P6·P7(집 k3s)에서 같은 구성을 써야 한다. 그리고 시간 과금 외 고정 비용을 늘리지 않는다.
- **Prometheus 저장소는 `emptyDir`, 보관 2일.** 인프라를 매일 지우므로 오래 둘 이유가 없다. EBS CSI 드라이버·PVC를 들이지 않는다.
  측정 원자료는 수집 스크립트가 쿼리 결과를 파일로 남긴다(지금 Logs Insights와 같은 방식).
- EKS는 컨트롤 플레인을 긁을 수 없다. 그래서 etcd·controller-manager·scheduler·kube-proxy 수집과 그 기본 규칙을 끈다. 나머지 기본 규칙(CrashLoop·HPA 최대·TargetDown·노드 디스크 등)은 켠다.
- **선택자 계약(차트 ↔ 사이트).** kube-prometheus-stack은 기본값에서 `release: <릴리스 이름>` 라벨이 붙은 ServiceMonitor·PodMonitor·PrometheusRule만 고른다.
  - 앱 차트는 사이트를 모르므로 그 라벨을 넣을 수 없다. ArgoCD 차트의 ServiceMonitor도 마찬가지다.
  - 그래서 사이트 값(`infra/k8s`)에서 `serviceMonitorSelectorNilUsesHelmValues`·`podMonitorSelectorNilUsesHelmValues`·`ruleSelectorNilUsesHelmValues`를 `false`로 둔다. 그러면 모든 네임스페이스의 CR을 고른다.
  - 대시보드 ConfigMap 라벨은 Grafana 사이드카 기본값(`grafana_dashboard: "1"`)과 맞춘다.
  - 이것이 빠지면 대상이 수집되지 않고 규칙도 조용히 무시된다. 그래서 단계 4 렌더 단언과 7-2 규칙 확인으로 막는다.
- Grafana에는 익명 Viewer를 켜고 `kubectl port-forward`로만 접근한다. 밖에 노출하지 않는다.
  대시보드는 코드(JSON)로만 바꾼다.
  - **관리자 비밀번호**: kube-prometheus-stack은 Grafana 하위 차트에 고정 문자열(`prom-operator`)을 넘긴다. 그러면 클러스터 안 아무 파드나 그 공개된 값으로 관리자 로그인을 할 수 있다(데이터 소스 프록시 포함).
  - 그래서 `grafana.adminPassword`를 비워 하위 차트가 난수 Secret을 만들게 한다. 고정한 차트 버전의 values에서 기본값을 확인해 주석에 남긴다. 그 Secret은 화면에 찍지 않는다.
- **노드 3대**(`node_count` 기본 2 → 3)로 늘린다. 파드 슬롯은 51개, 메모리 할당은 약 10GiB가 된다. 비용은 켜 둔 동안 시간당 약 0.04달러가 늘어난다.
  t4g.large 2대(슬롯 70개)보다 싸다. 같은 노드 종류라 0014와 CPU 비교 조건도 덜 바뀐다. ADR 0007 노드 수용 표를 다시 계산해 고친다.

### 앱 지표 (`/metrics`, 내부 포트 9090)

- `prometheus/client_golang`을 쓴다. 히스토그램·Go 런타임 수집기를 직접 만들 이유가 없다.
- **공개 포트 8080에는 `/metrics`가 없다.** ALB·TGB는 8080만 본다. 내부 포트에는 `/metrics`와 `net/http/pprof`(`/debug/pprof/`)만 둔다.
  - 클러스터 SG가 같은 SG끼리 전체를 허용하므로 Prometheus가 다른 노드의 파드를 긁을 수 있다. 밖(ALB)에서는 9090에 닿지 않는다(ALB SG → 클러스터 SG는 8080만).
  - pprof는 클러스터 안에서만 닿는다. 클러스터 안 공격자가 프로파일 요청으로 CPU를 쓰게 할 수 있다는 위험은 받아들이고 ADR에 적는다.
- 지표 목록. 이름 접두는 `linkpulse_`다.
  - `http_requests_total{route,method,code}`, `http_request_duration_seconds{route}`(히스토그램).
    - `route`는 ServeMux 패턴(`r.Pattern`, 예: `GET /{code}`)이다. 원시 경로를 쓰지 않는다 — 카디널리티 폭발과 요청 ID 노출을 막는다(plan 0011 `logPath`와 같은 이유).
    - 리미터(429)처럼 mux 앞에서 끝난 요청, mux가 맞추지 못한 요청(패턴이 빈 값)은 `route="none"`이다.
    - **전제**: 바깥 미들웨어가 `next` 뒤에 `r.Pattern`을 읽는다. 지금 체인(`requestLogger → recoverer → rateLimit → mux`)에는 요청을 복제하는 곳(`r.WithContext` 등)이 없어 mux가 쓴 값이 보인다.
      나중에 미들웨어가 요청을 복제하면 모두 `none`이 된다. 이 전제를 주석에 적고 단계 1 테스트로 지킨다.
    - 버킷은 SLO 경계 100ms를 반드시 포함한다(예: 5·10·25·50·100·250·500ms·1·2.5s).
  - `http_connections_opened_total` — `http.Server.ConnState`의 `StateNew`를 센다. 요청 수와 비교하면 ALB가 연결을 재사용하는지 보인다. 요청당 CPU 가설의 보조 신호다.
  - `clicks_*_total` — 기존 `clicks.Stats` 카운터 10개를 그대로 낸다(recorded·dropped·published·publish_errors·applied·duplicate·unknown·apply_errors 등). `clicks_pending_codes`는 게이지다.
    로그 줄(`click queue`)은 남긴다. 0012·0013 비교와 런북 쿼리가 쓴다.
  - DB 풀: `collectors.NewDBStatsCollector`(`database/sql` 통계).
  - Go 런타임(`go_sched_gomaxprocs_threads` 포함)·프로세스(`process_cpu_seconds_total`) 수집기.
- **서버 `idleTimeout` 60초 → 75초.** AWS는 대상의 keep-alive를 ALB 유휴 타임아웃(60초)보다 길게 두라고 권한다. 같은 값이면 ALB가 막 닫히는 연결에 요청을 보내 502가 날 수 있다.
  0014의 502가 이것 때문인지는 접근 로그로 가른다(아래). 이 변경은 원인과 무관하게 맞춘다.

### SLO와 알림 규칙 (차트의 `PrometheusRule`)

| SLO | SLI | 목표(30일) |
| --- | --- | --- |
| 가용성 | `GET /{code}`·`GET /api/links/{code}` 응답 중 5xx가 아닌 비율. 404·410은 정상 응답이다 | 99.5% |
| 지연 | 같은 경로에서 100ms 이하로 끝난 비율(서버 쪽, 0012 판정 경계와 같다) | 99% |

- 소진율 알림은 SRE 워크북의 다중 창 방식이다.
  - 빠른 소진: 14.4배, 1시간 **그리고** 5분 → `severity=critical`.
  - 느린 소진: 6배, 6시간 **그리고** 30분 → `severity=warning`.
  - 저트래픽 보호: 1시간 요청률이 초당 0.2 미만이면 평가하지 않는다. 요청 하나의 5xx로 알림이 울리지 않게 한다.
- **앱 쪽 SLI의 빈틈**: ALB가 만든 502·504와 연결 실패는 앱 지표에 없다. 기존 CloudWatch `alb_elb_5xx`·canary 알람이 그대로 맡는다.
- 그 밖의 규칙:
  - 클릭: `dropped` 증가 > 0(유실 = critical), `publish_errors`가 5분 동안 계속 증가(warning), `apply_errors` 증가 > 0(warning). 이로써 0013의 이월 알림을 닫는다.
  - 관리자 인증 실패: 관리자 경로 401이 10분에 20건을 넘으면 warning(하드닝 백로그). 임계값은 초기값이다.
  - ArgoCD: `argocd_app_info`가 15분 넘게 `Synced`·`Healthy`가 아니면 warning(0014 리스크 표). ArgoCD 차트의 지표·ServiceMonitor를 켠다.
  - 파드 CrashLoop·HPA 최대·대상 다운은 기본 규칙(`KubePodCrashLooping`·`KubeHpaMaxedOut`·`TargetDown`)이 맡는다. `monitoring.tf` 주석의 "0015"를 닫는다.
- **SQS 나이 알림은 새로 두지 않는다.** 기존 CloudWatch 알람(가장 오래된 메시지 5분 = 워커 멈춤)으로 충분하다.
  0014 `spread` 6400의 69초는 과부하를 큐가 설계대로 흡수한 것이다. 이 판단을 결과 문서에 적는다.

### 알림 경로 — 등급 분리

- **사람을 깨우는 경로(SMS)는 클러스터 밖에만 둔다.** CloudWatch 알람과 canary는 그대로다. 클러스터가 죽으면 Prometheus 알림도 함께 죽기 때문이다.
- **Prometheus 알림은 Slack으로만 보낸다.** 흐름: Alertmanager → 새 SNS 토픽 `linkpulse-prod-prom-alerts` → 기존 Chatbot 채널 설정에 토픽 추가 → Slack.
  - SMS를 구독하지 않는 별도 토픽이라, Chatbot용 JSON이 문자로 가지 않는다.
  - Alertmanager는 **EKS Pod Identity**로 `sns:Publish`(그 토픽 하나)를 받는다. `infra/prod/iam.tf`의 `pod_identities`에 `alertmanager`(monitoring/서비스 어카운트)를 더한다.
  - 메시지는 Chatbot **사용자 지정 알림 JSON**(`version`·`source: custom`·`content`)으로 Alertmanager 템플릿에서 만든다.
    스키마와, Alertmanager SNS 수신기가 Pod Identity 자격증명을 읽는지는 구현 때 1차 출처로 확인한다. 확인이 안 되면 Slack Incoming Webhook(Secret)으로 바꾸고 plan에 적는다.
- `Watchdog`·`InfoInhibitor`는 null 수신기로 보낸다. 나머지는 모두 Slack이다. 측정 창 첫 30분에 기본 규칙 노이즈를 보고, 시끄러운 규칙은 이름을 결과에 남기고 끈다.
- **받아들이는 위험**: Prometheus가 죽고 앱은 살아 있으면 Prometheus 알림이 조용히 끊긴다.
  가용성은 클러스터 밖 알람이 보지만, 클릭 유실 알림은 멈춘다. 외부 dead man's switch는 P7("감시는 집 밖에서")로 넘긴다. ADR에 적는다.

### 대시보드

- Grafana 대시보드 한 장(JSON, 차트의 ConfigMap, 라벨 `grafana_dashboard: "1"`, Grafana 사이드카가 모든 네임스페이스에서 읽는다). 패널 구성:
  - RED(경로별 요청률·5xx·p95/p99)와 SLO 버짓 잔량·소진율
    - 보관이 2일이고 클러스터가 몇 시간만 산다. 그래서 30일 버짓 잔량은 "있는 데이터 기준"이다. 패널 설명에 그렇게 적는다.
  - 클릭 파이프라인(recorded·applied·pending·실패)
  - DB 풀(in_use·대기)
  - CPU 세 출처(cAdvisor 컨테이너, 앱 `process_cpu_seconds_total`, metrics-server)와 CFS 스로틀 비율, `GOMAXPROCS`
  - HPA 현재/목표 파드 수, 요청당 연결 수

### Fluent Bit 유실 수정 (G-15)

- tail 입력에 `storage.type filesystem`(버퍼가 차면 멈추지 않고 디스크로 넘긴다)을 둔다.
- `DB`(읽은 위치 저장)와 `Rotate_Wait 30`(회전된 파일을 30초 더 읽는다)을 둔다. 서비스 절에는 `storage.path`를 둔다.
- 경로는 차트가 이미 hostPath로 붙이는 `/var/log` 아래다(예: `/var/log/flb-storage/`, `/var/log/flb_kube.db`). 쓰기 가능 마운트인지 `helm template`으로 확인한다.
- 요청 로그 양(실행 하나에 약 1.5GB)을 줄일지는 아래 CPU 실험(`LOG_LEVEL=warn`)의 결과로 다음 plan에서 정한다. 이번에는 바꾸지 않는다.

### 요청당 CPU 조사 (측정 창 실험)

0014의 가설 넷 중, EKS 안에서 가를 수 있는 것을 가른다. ECS가 없으니 "ECS와 같은 출처로 비교"는 할 수 없다. 대신 **CPU가 어디에 쓰이는지**를 직접 본다.

| 실험 | 방법 | 가르는 가설 |
| --- | --- | --- |
| E1 출처 비교 | 같은 구간에서 cAdvisor·`process_cpu_seconds_total`·metrics-server를 비교 | 지표 출처. 셋이 10% 안이면 "EKS에서 실제로 CPU를 그만큼 쓴다" |
| E2 프로파일 | 800 rps 유지 구간에 파드 하나에서 30초 CPU 프로파일(`curl …/debug/pprof/profile?seconds=30`) | 상위 함수의 비중으로 로그·런타임 스케줄링·GC·pgx·HTTP를 가른다 |
| E3 로그 끄기 | 사이트 ConfigMap `LOG_LEVEL=warn` + `rollout restart` 뒤 같은 800 실행 | 요청 로그 경로의 비용 |
| E4 `GOMAXPROCS=1` | 사이트 ConfigMap에 `GOMAXPROCS=1` 추가 + `rollout restart` 뒤 같은 800 실행 | CPU 한도 250m 아래의 스케줄러·스로틀 비용(스로틀 비율도 함께 본다) |

- **E3의 부작용**: `click queue`·`db pool` 줄도 Info라 함께 사라진다. 그 실행의 로그 기반 표(정합성·풀·로그 줄 수)는 비어 나오는 것이 정상이다. 정합성은 Prometheus 카운터로 본다.
- **E4의 비교 대상**: Go 1.25부터 런타임이 cgroup CPU 한도로 `GOMAXPROCS` 기본값을 정한다. 250m 한도면 기본값은 하한(2)일 가능성이 크다.
  기준 실행에서 `go_sched_gomaxprocs_threads`로 실제 값을 먼저 확인하고 결과 문서에 적는다(그러면 E4는 "그 값 대 1" 비교다).

- 실행은 `SCENARIO=spread MAX_RPS=800`(15분)을 네 번 한다(E1·E2는 기준 실행 하나에서 같이 본다).
- 사이트 ConfigMap은 ArgoCD가 아니라 `infra/k8s`가 소유한다. 그래서 `kubectl patch`가 selfHeal로 되돌려지지 않는다.
  실험 뒤에는 원래 값으로 되돌리고 `rollout restart`한다(destroy로도 사라진다).
- 이번 plan은 실험 결과로 코드·설정을 바꾸지 않는다. 결과와 다음 조치(요청 로그 줄이기, `GOMAXPROCS`·CPU 한도, HPA 최대·연결 예산)는 결과 문서에 적고 다음 plan으로 넘긴다.
  0014 이월 2번(HPA 최대·연결 예산)은 이 결과가 먼저라 **범위 밖**이다.

### 502·타임아웃 분류

- ALB 접근 로그를 측정 동안만 켠다. `infra/prod`가 `enable_alb_access_logs=true`일 때 S3 버킷을 직접 만든다. 지금의 "기존 버킷 이름" 변수는 지운다.
  - 버킷 설정: `force_destroy`, 7일 만료, SSE-S3, 공개 차단.
  - 버킷 정책: 서울 리전의 ELB 로그 전달 주체를 쓴다. 계정 ID 방식인지 서비스 주체 방식인지는 1차 출처로 확인한다.
  - ALB는 접근 로그를 켤 때 버킷에 시험 파일을 써서 권한을 확인한다. 그래서 `aws_lb`가 버킷 정책을 `depends_on`한다. 정책보다 먼저 켜지면 apply가 실패한다.
- 측정이 끝나면 destroy 전에 로그를 내려받는다 [사람]. 로그는 5분 단위로 전달되므로 마지막 요청 뒤 5분 넘게 기다린 다음 받는다. 502·`-` 상태 줄의 `error_reason`·`target_status_code`·처리 시간·대상 IP를 분류한다(로컬 파일 분석).
- `idleTimeout` 75초에서 502가 0014(12건)보다 줄었는지 함께 본다.

### 수집 도구

- `collect-metrics.sh`에 Prometheus 절을 더한다. 지금 CloudWatch·Logs Insights 절은 그대로 둔다(0012~0014 비교용).
  - 접근: 스크립트가 `kubectl -n monitoring port-forward`를 백그라운드로 열고, 끝날 때 닫는다(trap).
  - 쿼리:
    - 분 단위 파드 CPU(세 출처)·스로틀 비율·파드 수(kube-state-metrics)
    - 앱 쪽 p95/p99
    - 요청당 연결 수
    - **클릭 카운터 정합성**
  - 정합성 계산: 구간 끝의 `max_over_time`에서 구간 시작 값을 뺀 값의 합이다(`@` 수식어). 실행 중에 새로 뜬 파드는 시작 값이 0이다. extrapolation이 있는 `increase()`는 쓰지 않는다 — 정확히 같은 정수여야 한다.
    - 이 계산은 카운터 리셋(컨테이너 재시작)과 실행 중 사라진 파드(마지막 스크랩 뒤 증가분이 빠진다)를 다루지 못한다.
    - 그래서 같은 구간의 `resets()` 합과, 시작~끝 사이에 사라진 파드 수를 함께 낸다. 둘 중 하나라도 0이 아니면 그 실행의 (b)는 **무효**로 적는다.
  - **로그 유실 검사 — 줄 개수.** 파드마다 분당 `click queue`·`db pool` 줄이 각각 6개(10초 tick)여야 한다.
    - 그 분 전체에 살아 있던 파드만 센다. tick 위상 때문에 5~7은 허용하고, 4 이하를 유실로 본다.
    - 트래픽과 무관해 `read`에도 쓸 수 있다.
    - k6 요청 수 대 `recorded_clicks` 분 단위 표(0014에서 손으로 한 계산)는 참고 표로 둔다. 10초 증가분의 위상과 서버에 닿지 못한 요청 때문에 경계가 흔들린다.
- `record-pods.sh`는 이번 창에서도 같이 돌린다. E1의 metrics-server 출처다.
- README "운영 측정" 절차를 이 창에 맞게 고친다. 바뀌는 단계:
  - 모니터링 확인
  - 실험 E1~E4
  - 측정 apply의 `-var enable_alb_access_logs=true`
  - 접근 로그 내려받기
  - 프로파일 저장

## 실행 단계

1. **앱 지표** — 내부 포트 서버(9090: `/metrics`·pprof), 요청 미들웨어의 지표, `ConnState` 카운터, `clicks.Stats`·DB 풀 수집기, `idleTimeout` 75초, 차트에 9090 포트·`METRICS_ADDR`(기본 `:9090`).
   → 검증: `go test -race ./...`.
   - 리다이렉트 1건이 `route="GET /{code}"`로 세어진다(원시 코드가 라벨에 없다).
   - 공개 핸들러에 `/metrics`가 없다(내용이 노출되지 않는다).
   - 클릭 카운터가 `Stats`와 같은 값을 낸다.
   - 429와 mux가 맞추지 못한 요청은 `route="none"`이다. 바깥 미들웨어가 `next` 뒤에 `r.Pattern`을 읽는 전제를 테스트가 지킨다(미들웨어 체인을 통과한 요청의 라벨이 패턴이다).
   - `idleTimeout` > 60초를 테스트로 고정한다(ALB 값과의 관계를 주석에).

   **변이 검사**(`scratchpad/mut`): 아래 각각에서 테스트가 실패해야 한다.
   - 라벨에 원시 경로를 넣는다.
   - 지표 핸들러를 공개 mux에 붙인다.
   - 카운터 하나를 다른 필드에 묶는다.
   - `idleTimeout`을 60초로 되돌린다.
2. **앱 차트** — `monitoring.enabled`(기본 false, `values-aws.yaml`에서 true) 아래에 ServiceMonitor·PrometheusRule(SLO·클릭·관리자 401·ArgoCD)·대시보드 ConfigMap을 둔다.
   → 검증: `scripts/check-charts.sh` 확장.
   - 렌더 단언: false면 셋 다 없다. true면 ServiceMonitor가 9090 `/metrics`를 본다.
   - kubeconform에 CRD 스키마를 붙인다. G-14대로 **모르는 필드·오타가 실패하는지 먼저 본다.**
   - **`promtool test rules`**: 합성 시계열로 빠른 소진 발화·저트래픽 미발화·느린 소진 창 조건·클릭 유실 발화를 단언한다. promtool은 고정 버전을 체크섬으로 받는다.
   - 대시보드 JSON은 `jq` 유효성을 본다.
   - CI(`ci.yml`)에서도 돈다.
3. **Terraform `infra/prod`**
   - `node_count` 3
   - SNS 토픽 `prom-alerts`(SMS 구독 없음)와 Chatbot `sns_topic_arns`에 추가
   - Pod Identity `alertmanager` 역할(그 토픽 `sns:Publish`만)
   - ALB 접근 로그 버킷(조건부)과 버킷 정책. 변수 `alb_access_logs_bucket` 삭제
   → 검증: `fmt`·`validate`·tfsec(무시 항목은 사유와 함께). description은 ASCII.
   IAM 액션·조건 키·ELB 로그 전달 주체는 1차 출처로 확인하고 주석에 남긴다. `terraform plan` [사람](꺼져 있으면 전체 생성).
4. **Terraform `infra/k8s`**
   - `monitoring` 네임스페이스와 kube-prometheus-stack `helm_release`
     - EKS 컨트롤 플레인 수집 끔
     - Grafana 익명 Viewer, 사이드카 모든 네임스페이스
     - Prometheus `emptyDir`·보관 2일·자원 요청
     - Alertmanager 서비스 어카운트 이름 고정, SNS 수신기·라우트
   - ArgoCD 지표·ServiceMonitor 켬
   - Fluent Bit `storage.type filesystem`·`DB`·`Rotate_Wait`
   - `depends_on`: kube-prometheus-stack(CRD)이 gitops(Application)보다 먼저
   → 검증: `fmt`·`validate`. 로컬 `helm template`으로 kube-prometheus-stack 값을 렌더해 아래를 단언한다.
   - 컨트롤 플레인 ServiceMonitor 없음
   - 세 `*SelectorNilUsesHelmValues`가 `false`(모든 네임스페이스의 CR 선택)
   - Alertmanager 설정에 `sns_configs`와 토픽 ARN 자리
   - Grafana 익명 Viewer, `adminPassword`가 고정 문자열이 아님, 대시보드 사이드카 라벨이 차트 ConfigMap과 같음
   - Fluent Bit 설정에 세 항목, `/var/log` 마운트가 쓰기 가능
   차트 버전은 1차 출처로 고정하고 날짜를 적는다. `plan`은 측정 창에서 [사람].
5. **스크립트·문서**
   - `full-destroy-prod.sh`: `monitoring` 네임스페이스에 finalizer 객체가 없는지 확인을 더한다. 없으면 그대로다.
   - `collect-metrics.sh`(Prometheus 절·로그 유실 표)
   - README 운영 측정 절차
   - 런북에 Prometheus 알림 절. 알림별 의미·첫 조치와 Grafana·Prometheus `port-forward` 접근을 적는다.
   - 7-2의 시험 규칙 파일(`vector(1)`, `severity=warning`, 시험 라벨). kubeconform으로 검사하고 런북에서 넣고 빼는 명령을 적는다.
   - **ADR 0008**: 클러스터 안 Prometheus, 알림 등급 분리, SLO 정의와 저트래픽 보호, 내부 포트·pprof, 받아들이는 위험
   - ADR 0007 노드 수용 표를 3대로 고친다.
   - `monitoring.tf` 주석의 "0015"를 정리한다.
   → 검증: `bash -n`·shellcheck. `collect-metrics.sh`는 가짜 `kubectl`(port-forward)·가짜 Prometheus 응답 JSON으로 끝까지 돌린다.
   0014 원자료 재수집이 기존 표를 그대로 내는지 확인한다(Prometheus 절이 없을 때 건너뛰는지).
6. **코드 검토 1라운드(claude-ide) → 커밋·PR·머지 [사람].** 머지 후 CI(GHCR 푸시)가 인프라 없이 성공해야 한다.
7. **측정 창 [사람]** — 약 4시간이다. 단계마다 확인 지점이 있고, 걸리면 멈춘다.
   1. `full-apply-prod.sh`(처음부터, P5 종결 재현 확인). 노드 3대, 모든 파드 Ready, TGB 대상 healthy.
   2. 모니터링 확인
      - Prometheus 대상(앱 파드 2·ArgoCD·kube-state-metrics·node-exporter 3·kubelet/cAdvisor)이 모두 `up`이다.
      - Grafana 대시보드가 그려진다.
      - Prometheus `/api/v1/rules`에 linkpulse 규칙 그룹(SLO·클릭·관리자 401·ArgoCD)이 있고 `health`가 `ok`다.
      - **테스트 알림은 규칙 경로로 보낸다.** 늘 발화하는 시험 PrometheusRule(`vector(1)`, 저장소의 파일)을 `kubectl apply`로 잠깐 넣는다.
        그러면 rule → Alertmanager → SNS → Chatbot → Slack을 한 번에 증명한다. 도착하면 지운다. `amtool alert add`는 규칙을 거치지 않아 쓰지 않는다.
      - Fluent Bit 설정이 적용됐다.
      - 기본 규칙의 노이즈를 30분 동안 본다.
      **Slack 도착이 안 되면 측정은 하고, 알림 경로는 결과에 실패로 남긴다**(측정과 독립이다).
   3. 측정 예외 apply. 0014 2) 절차에 `-var enable_alb_access_logs=true`를 더한다.
   4. 실험 E1~E4. `MAX_RPS=800` 네 번(각 15분)이다. 프로파일은 `raw/0015-e*-cpu.pb.gz`로 저장하고, 실험 사이에 ConfigMap을 되돌린다.
   5. 재측정: `spread`·`hot`·`read`, `MAX_RPS=6400`, 기본 설정이다. 실행마다 파드 2개에서 시작한다. 수집에는 CloudWatch와 Prometheus를 함께 쓴다.
   6. 마지막 요청 뒤 5분 넘게 기다렸다가 ALB 접근 로그를 내려받는다(`aws s3 sync`, 읽기).
   7. destroy(k8s → prod)하고 state 0을 확인한다.
   → 검증: 아래 판정에 쓴 원자료가 모두 있고, 판정에 쓴 단계가 0012 판정 0번(발생기 한계)에 걸리지 않았다.
8. **결과 기록** — `load/k6/results/0015-observability.md`, docs PR [사람]. 넣을 것:
   - 판정표
   - 0012·0013·0014 대비 단계 비교(P5 종결)
   - 요청당 CPU 실험 결과와 다음 조치
   - 502·타임아웃 분류
   - 알림 노이즈 목록

검토: plan 검토 1라운드(claude-ide) → 구현 → 코드 검토 1라운드 → 측정. 블로커가 없으면 라운드를 더 열지 않는다(코드 검토는 claude-ide 1인 + 변이 검사·`-race`·promtool 단위 테스트).

## 판정 (측정 창)

| # | 조건 | 통과 기준 |
| --- | --- | --- |
| (a) | 지표 수집 | Prometheus 대상 전부 `up`. 공개 URL `https://<도메인>/metrics`가 지표를 내지 않는다(404) |
| (b) | 정합성(카운터) | 세 실행 모두 Prometheus 카운터로 `recorded − dropped = applied + unknown = DB 합계`가 **정수로 정확히** 맞는다. 그 구간의 `resets()` 합이 0이고 사라진 파드가 없을 때만 유효하다 |
| (c) | 로그 유실 | 세 실행의 모든 유지 분에서, 그 분 전체에 살아 있던 파드마다 `click queue`·`db pool` 줄이 각각 5개 이상이다(기대 6) |
| (d) | 알림 경로 | 규칙 경로의 시험 알림이 Slack에 도착하고, linkpulse 규칙 그룹이 `health: ok`다. 측정 중 발화한 알림 목록과 SLO 소진 알림 발화 여부를 기록한다. 6400 단계는 3분이라 빠른 소진의 1시간 창 조건이 채워지지 않을 수 있다 — 발화하지 않으면 그 이유(창 길이)를 함께 적는다 |
| (e) | 요청당 CPU | E1~E4 결과로 넷 중 어느 가설이 남는지 서술한다. 프로파일 상위 10개를 결과에 둔다. "가렸다/못 가렸다"를 정직하게 적는다 |
| (f) | 502·타임아웃 | 접근 로그로 원인을 분류한다. 502 수를 0014(12건)와 비교한다 |
| (g) | P5 종결 | `full-apply` 한 번에 재현된다. 세 시나리오의 단계 표가 0012 기준선과 비교된다. 계측 추가의 CPU 영향을 0014 같은 단계와 비교해 적는다(창이 달라 참고값) |

## 리스크/롤백

| 리스크 | 완화 | 롤백 |
| --- | --- | --- |
| 노드·메모리 부족(Prometheus가 예상보다 큼) | 노드 3대, Prometheus 요청·한도 명시, 보관 2일 | `maxReplicas`가 아니라 Prometheus 자원·보관부터 줄인다. 그래도 모자라면 `node_count` 4 |
| 기본 규칙 노이즈로 Slack이 시끄러움 | 창 첫 30분 관찰, EKS에 맞지 않는 규칙 끔 | 규칙 그룹 끄기(값 한 줄) |
| Alertmanager SNS·Pod Identity·Chatbot JSON이 안 됨 | 1차 출처 확인, 규칙 경로 시험 알림으로 실측 | Slack Incoming Webhook(Secret)으로 수신기 교체 |
| 규칙·대상이 선택자에 걸러져 조용히 평가되지 않음 | 세 `*SelectorNilUsesHelmValues=false`, 렌더 단언, 7-2 `/api/v1/rules`·대상 `up` 확인 | 값 수정 후 재apply |
| 클러스터 안 감시라 클러스터가 죽으면 알림도 없음 | 가용성·SMS는 CloudWatch(클러스터 밖)에 남긴다 | — (P7 외부 감시) |
| `/metrics`·pprof 노출 | 내부 포트, ALB·TGB는 8080만, 공개 404를 판정 (a)로 확인 | pprof 핸들러 제거 |
| 계측이 요청 경로 CPU를 늘림 | 히스토그램 하나·라벨 3개로 제한 | 히스토그램 버킷 축소 |
| destroy가 monitoring 네임스페이스에서 멈춤 | operator CR에는 기본 finalizer가 없다 — 창 destroy에서 확인 | 런북의 finalizer 조치 |
| Fluent Bit 디스크 버퍼가 노드 디스크를 채움 | 6400 한 실행 약 1.5GB, `storage.total_limit_size`로 상한 | 상한 낮춤 |
| 측정 창이 길다(약 4시간) | 단계별 확인 지점, 실험은 15분 단위 | 실험 E3·E4를 다음 창으로 미룬다 |
| 비용(노드 1대, S3 접근 로그) | 켤 때만, 접근 로그는 측정 apply에서만, 7일 만료 | — |

## 검토 반영 로그

<!-- /plan-merge가 라운드별로 기록. 형식: [rN] 리뷰어#번호 지적요약 → 반영|기각 — 사유 -->

r1 리뷰는 claude-ide 하나다(codex 사용 불가). `reviewed-revision: 1` 일치.

- [r1] claude-ide#1 kube-prometheus-stack 기본 선택자(`release` 라벨)가 사이트를 모르는 차트의 ServiceMonitor·PrometheusRule을 고르지 않고, 규칙 누락을 드러내는 검증이 없음 → 반영 — 세 `*SelectorNilUsesHelmValues=false`(사이트 값)와 대시보드 라벨 계약을 수집 구성에 적음. 단계 4 렌더 단언, 7-2 `/api/v1/rules` health 확인, 시험 알림을 `amtool` 대신 규칙 경로(`vector(1)` 시험 규칙)로 바꿈. 판정 (d)·리스크 표 갱신
- [r1] claude-ide#2 판정 (c) 요청 수 대 `recorded_clicks`는 `read`에서 성립하지 않고 tick 위상·미도달 요청으로 경계가 흔들림 → 반영 — 파드·분당 `click queue`·`db pool` 줄 개수(기대 6, 5~7 허용)로 바꿈. 요청 수 비교는 참고 표
- [r1] claude-ide#3 kube-prometheus-stack이 Grafana 관리자 비밀번호를 고정 문자열로 넘김 → 반영 — `grafana.adminPassword` 비워 난수 Secret, 고정 버전 values로 확인. 단계 4 단언에 넣음
- [r1] claude-ide 제안: ALB 접근 로그 켜기가 버킷 정책보다 먼저면 apply 실패, 전달 5분 지연 → 반영 — `aws_lb`가 버킷 정책 `depends_on`, 7-6 내려받기 전 5분 대기
- [r1] claude-ide 제안: 카운터 정합성이 리셋·사라진 파드를 다루지 못함 → 반영 — `resets()` 합·사라진 파드 수를 함께 내고 0이 아니면 (b) 무효
- [r1] claude-ide 제안: `route` 라벨이 `next` 뒤 `r.Pattern` 읽기에 기대는 전제·패턴 빈 값 처리 → 반영 — 전제를 주석·단계 1 테스트로 지키고, 빈 패턴도 `none`
- [r1] claude-ide 제안: Go 1.25+ cgroup 기반 `GOMAXPROCS` 기본값이라 E4는 "2 대 1"일 수 있음 → 반영 — 기준 실행에서 `go_sched_gomaxprocs_threads`를 먼저 확인해 결과에 적음
- [r1] claude-ide 제안: E3에서 `click queue`·`db pool` 줄도 사라짐 → 반영 — 그 실행의 로그 기반 표가 비는 것이 정상이라고 적음
- [r1] claude-ide 제안: 30일 버짓 잔량 패널은 보관 2일·짧은 수명 때문에 "있는 데이터 기준" → 반영 — 패널 설명에 적음
- [r1] claude-ide 제안: 6400 3분으로는 빠른 소진 1시간 창이 안 찰 수 있음 → 반영 — 판정 (d)에 미발화 이유를 함께 적게 함

**판정(r2):** 결함 3개를 리뷰어가 제시한 수정대로 반영했고, 제안 7개도 모두 반영했다. 남은 쟁점이 없다. "블로커가 없으면 라운드를 더 열지 않는다"에 따라 `approved`.
