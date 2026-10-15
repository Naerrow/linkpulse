# ADR 0008 — 클러스터 안 Prometheus·Grafana, SLO 알림, 알림 등급 분리

- 상태: 채택 (2026-10-14, plan 0015). **측정 창 통과(2026-10-14·15)** — 수집·규칙·알림 경로(Slack 발화·해소)·카운터 정합성·로그 유실 0을 확인했다. 결과: [`load/k6/results/0015-observability.md`](../../load/k6/results/0015-observability.md).
- 관련: AGENTS.md §5 P5(0015 Prometheus/Grafana, 자유 이동 원칙), [plan 0015](../plans/0015-prometheus-grafana/plan.md),
  [ADR 0002](0002-alerting-design.md)(CloudWatch 알람·Chatbot), [ADR 0007](0007-ecs-to-eks.md)(EKS·노드 수용).

## 맥락

- 0014에서 넘어온 것: 요청당 CPU가 ECS의 2~4배(원인 미확정), 정합성 판정이 로그 합계에 기대는데 고부하에서 로그가 빠졌다(G-15),
  클릭 발행 실패 알림(0013 이월), ArgoCD 동기화 실패 알림·파드 CPU·파드 수 알람 공백(ADR 0007), ALB 502·타임아웃 미분류.
- P6·P7(집 k3s)에서 **같은 차트·같은 구성**을 써야 한다. 시간 과금 외 고정 비용은 늘리지 않는다.

## 결정

1. **클러스터 안 kube-prometheus-stack(91.8.2 — operator v0.94.1·Prometheus v3.15.0·Alertmanager v0.34.1).** AMP·AMG를 쓰지 않는다 — 집에서 같은 구성을 쓸 수 없고 고정 비용이 붙는다.
   - 저장소는 `emptyDir`, 보관 2일. 인프라를 작업할 때만 켜므로 EBS CSI·PVC를 들이지 않는다. 측정 원자료는 `collect-metrics.sh`가 쿼리 응답을 파일로 남긴다.
   - EKS에서 닿지 않는 컨트롤 플레인(etcd·controller-manager·scheduler)과 127.0.0.1에만 여는 kube-proxy는 수집과 기본 규칙을 함께 끈다. 나머지 기본 규칙(KubePodCrashLooping·KubeHpaMaxedOut·TargetDown 등)은 켠다 — ADR 0007의 파드 알람 공백을 이것이 닫는다.
   - kubelet `/metrics/resource`(metrics-server와 같은 출처)도 긁는다. 요청당 CPU 실험 E1이 세 출처를 비교한다.
2. **선택자 계약(차트 ↔ 사이트).** 규칙·대시보드·ServiceMonitor는 앱 차트에 있고(사이트 무관), 수신처는 사이트 값이다(`infra/k8s`).
   - 차트는 사이트의 Prometheus 릴리스 이름을 모르므로 `release` 라벨을 넣지 않는다. 사이트가 `serviceMonitor`·`podMonitor`·`ruleSelectorNilUsesHelmValues=false`로 모든 네임스페이스의 CR을 고른다. ArgoCD 차트의 ServiceMonitor도 같다.
   - 대시보드 ConfigMap 라벨은 Grafana 사이드카 기본값(`grafana_dashboard: "1"`)과 맞춘다.
   - 이 계약은 렌더 단언(`scripts/check-charts.sh`)으로 지키고, 측정 창에서 `/api/v1/rules`·대상 `up`으로 확인한다. 빠지면 규칙이 조용히 평가되지 않는다.
   - 앱 차트의 모니터링 CR은 `monitoring.enabled`(기본 false)로 켠다. CRD가 없는 사이트에서 ArgoCD 동기화가 깨지지 않게 한다. 그래서 `infra/k8s`는 kube-prometheus-stack을 ArgoCD·Application보다 먼저 넣는다(ArgoCD 차트도 CRD가 있어야 ServiceMonitor를 렌더한다).
3. **앱 지표는 내부 포트 9090(`/metrics`·`net/http/pprof`)에만 낸다.** 공개 포트 8080에는 없다. ALB SG → 클러스터 SG는 8080만 열려 있어 밖에서 닿지 않는다.
   - `route` 라벨은 원시 경로가 아니라 ServeMux 패턴(`GET /{code}`)이다. mux 앞에서 끝났거나(429) 맞는 패턴이 없으면 `none`. 메서드는 표준 7개 밖이면 `other`로 묶는다 — 둘 다 클라이언트가 시계열 수를 늘릴 수 없게 한다.
   - 요청 히스토그램 하나(`route`만)와 카운터 하나(`route·method·code`)로 요청 경로 비용을 제한한다. 버킷에 지연 SLO 경계 0.1초가 있다.
   - 클릭 통계(`clicks.Stats`)는 긁을 때 원자 카운터를 그대로 읽는 수집기다. 정합성 판정이 이 값을 정수로 비교한다. 로그 줄(`click queue`)은 0012~0014 비교를 위해 남긴다.
   - 서버 keep-alive(`IdleTimeout`)를 60초 → 75초로 늘린다. ALB 유휴 타임아웃(60초)보다 길어야 한다는 AWS 권고를 따른다. 0014 502의 원인인지는 접근 로그로 가른다.
4. **SLO 두 개와 다중 창 소진 알림**(SRE 워크북). 대상은 리다이렉트·링크 조회(`GET /{code}`·`GET /api/links/{code}`)다.
   - 가용성 99.5%(5xx가 아닌 비율, 404·410은 정상). 지연 99%(서버 쪽 100ms 이하).
   - 빠른 소진 14.4배(1시간 **그리고** 5분) → critical, 느린 소진 6배(6시간 **그리고** 30분) → warning.
   - 저트래픽 보호: 1시간 요청률이 초당 0.2 미만이면 평가하지 않는다.
   - 그 밖에 클릭 유실(critical)·발행 실패 5분 지속·반영 실패, 관리자 401 10분 20건, ArgoCD 15분 비정상(warning). 규칙은 promtool 단위 테스트로 지킨다(`scripts/rules-test/`).
5. **알림 등급 분리.** 사람을 깨우는 경로(SMS)는 클러스터 밖(CloudWatch 알람·canary)에만 둔다. 클러스터가 죽으면 그 안의 Prometheus 알림도 함께 죽기 때문이다.
   - Prometheus 알림은 Alertmanager → 새 SNS 토픽 `linkpulse-prod-prom-alerts`(SMS 구독 없음) → 기존 Chatbot 채널 → **Slack만**.
   - 자격증명은 EKS Pod Identity(그 토픽 `sns:Publish`만). Alertmanager v0.34.1의 SNS 수신기는 access_key가 없으면 aws-sdk-go-v2 기본 체인을 쓰고, 그 체인이 Pod Identity의 컨테이너 자격증명을 읽는다(소스 `notify/sns/sns.go`로 확인).
   - 메시지는 Chatbot 사용자 지정 알림 JSON(`version 1.0`·`source custom`·`content.description`)이다. Alertmanager 템플릿의 `toJson`·`dict`로 만들어 주석의 따옴표·줄바꿈이 JSON을 깨지 않는다. SNS 제목은 ASCII 100자 제한 때문에 상태와 알림 이름만 넣는다.
   - `Watchdog`·`InfoInhibitor`는 보내지 않는다.
6. **Grafana는 `port-forward`로만 본다.** 익명 Viewer를 켜고, 관리자 비밀번호는 고정 문자열이 아니라 Grafana 차트가 만든 난수다(고정 버전 91.8.2는 `prom-operator`를 넘기지 않는다 — 렌더 단언으로 지킨다). 대시보드는 저장소 JSON으로만 바꾼다.
7. **Fluent Bit tail을 파일 시스템 버퍼로 바꾼다**(G-15). `storage.type filesystem`(버퍼가 차도 입력을 멈추지 않는다), `DB`(읽은 위치), `Rotate_Wait 30`. 디스크 버퍼는 노드마다 1G 상한, 메모리에 올리는 청크는 16개(한도 128Mi 안).
8. **ALB 접근 로그는 측정 apply에서만 켜고, 그때 버킷(7일 만료·SSE-S3·공개 차단·`force_destroy`)을 이 스택이 만든다.** 버킷 정책은 서비스 주체 `logdelivery.elasticloadbalancing.amazonaws.com`에 이 계정·리전 ALB(`aws:SourceArn`)만 허용한다. ALB는 정책이 먼저 있어야 켜진다(`depends_on`).
9. **노드 3대**(t4g.medium). 2대면 파드 슬롯 33~34/34로 꽉 찬다. 같은 노드 종류라 0014와 CPU 비교 조건이 덜 바뀐다. 계산은 ADR 0007 "노드 수용".

## 트레이드오프 (받아들인 위험)

- **Prometheus가 죽고 앱은 살아 있으면 Prometheus 알림이 조용히 끊긴다.** 가용성은 클러스터 밖 알람·canary가 보지만, 클릭 유실·발행 실패 알림은 멈춘다. 외부 dead man's switch(Watchdog을 밖에서 받는 것)는 P7("감시는 집 밖에서")로 넘긴다.
- **pprof는 클러스터 안에서 닿는다.** 클러스터 안의 누군가가 프로파일 요청으로 CPU를 쓰게 할 수 있다. 클러스터 SG가 같은 SG끼리 전체를 허용하는 구성(ADR 0007)이라 내부 포트도 같다. 지표·프로파일에 비밀은 없다. 문제가 되면 pprof 핸들러를 뺀다.
- **처음 생기는 라벨 조합의 첫 긁기 전 증가분은 세지 않는다.** 파드에서 그 조합(예: `GET /{code}`의 500, 관리자 경로의 401)이 처음 나오면 첫 긁기 때 이미 N인 채로 보이고, `rate()`·`increase()`는 그 N을 빼고 센다. 빠지는 것은 파드·조합마다 긁기 한 간격(15초)분이다. 미리 0으로 만들려면 라우터 패턴을 지표 쪽에 다시 적어야 해서(바뀌면 조용히 어긋난다) 하지 않았다.
- **30일 SLO인데 보관은 2일이다.** 대시보드의 버짓 잔량은 "있는 데이터 기준"이라고 패널 설명에 적었다. 장기 SLO 보고는 이 구성의 목적이 아니다.
- **6400 단계는 3분이라 빠른 소진의 1시간 창이 채워지지 않을 수 있다.** 측정 중 발화하지 않아도 실패가 아니다(결과에 이유를 적는다).
- **SNS 토픽은 암호화하지 않는다.** 기존 알람 토픽과 같다(알림에 민감정보 없음).
- **CI의 차트 검사가 고정 버전 차트를 네트워크로 받는다**(kube-prometheus-stack·fluent-bit 렌더 단언). 기존 CRD 카탈로그 스키마도 네트워크를 쓴다.
- **비용**: 노드 1대(켜 둔 동안 시간당 약 0.04달러), 접근 로그 S3(측정 창 동안만).

## 결과

- ADR 0007의 공백 둘(ArgoCD 동기화 실패 알림, 파드 CrashLoop·HPA 최대 알림)과 0013 이월(클릭 발행 실패 알림)을 닫는다.
- 정합성은 로그 합계가 아니라 Prometheus 카운터로 판정한다(정수 정확, 리셋·사라진 파드가 있으면 무효). 로그 유실은 파드·분당 줄 수로 따로 본다.
- 검증은 기계적 게이트로 받는다: 앱 `-race`·변이 검사, `scripts/check-charts.sh`(렌더 단언·kubeconform·promtool 단위 테스트·amtool 설정 검사, CI에서도 돈다), `terraform validate`·tfsec. Slack 도착은 측정 창에서 규칙 경로 시험 알림(`infra/k8s/manifests/test-alert-rule.yaml`)으로 확인한다.
