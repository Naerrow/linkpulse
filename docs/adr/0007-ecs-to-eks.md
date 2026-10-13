# ADR 0007 — ECS Fargate에서 EKS로 (Helm·ArgoCD·GHCR·HPA)

- 상태: 채택 (2026-10-13, plan 0014). **측정 창 전** — 클러스터 위 실측(스모크·회전 드릴·재측정)은 plan 0014 단계 9가 맡는다.
- 관련: AGENTS.md §5 P5(EKS·HPA, 자유 이동 원칙), [plan 0014](../plans/0014-ecs-to-eks/plan.md),
  대체: [ADR 0001](0001-cicd-terraform-ci-boundary.md)의 CI 배포 경계, [ADR 0003](0003-deploy-failure-alerts.md)의 ECS 배포 실패 알림,
  [ADR 0006](0006-runtime-db-credentials.md)의 Step 1(회전 재배포 Lambda). Step 2(런타임 재조회)는 그대로다.

## 맥락

- 0013 재측정에서 세 시나리오 모두 3200 rps를 넘었고, **다음 병목은 앱 CPU**였다(`spread` 3200에서 태스크 하나 97.5%). 수평 확장이 다음 단계다.
- AGENTS.md §5는 ECS 오토스케일링을 건너뛰고 EKS의 HPA로 한 번만 하기로 정했다. P6(집 k3s)에서 **같은 이미지·같은 Helm 차트**를 쓰는 것이 목표다.
- 그래서 이번 전환은 "ECS를 EKS로 옮긴다"보다 **"사이트를 모르는 차트 + 사이트별 접착제"로 나누는 것**이 핵심이다.

## 결정

1. **EKS + 관리형 노드 그룹(t4g.medium 2대 고정).** Auto Mode·Karpenter·Cluster Autoscaler는 쓰지 않는다 — 노드 2대에는 관리할 것만 늘고, 무엇이 도는지 보이는 구성을 고른다. 오토스케일러가 없으니 최대치도 2다.
2. **기존 ALB·리스너·타깃 그룹을 그대로 두고 TargetGroupBinding으로 파드 IP만 등록한다.** LB 컨트롤러는 ALB를 만들지 않는다.
   - ALB 알람·canary·ACM·Route53·`collect-metrics.sh`의 ALB 지표가 그대로 산다.
   - destroy 때 컨트롤러가 만든 ALB·SG가 남아 VPC 삭제를 막는 일이 없다.
   - XFF 홉이 그대로다(클라이언트 → ALB → 파드). 레이트리밋의 `rlTrustedHops = 1`이 맞는다. 그래서 nginx 같은 인그레스를 ALB 뒤에 두지 않는다.
   - TGB의 `networking`은 쓰지 않는다. SG는 Terraform만 바꾼다.
3. **readiness 프로브는 `/readyz`가 아니라 `/healthz`다.** TGB는 NotReady 파드를 타깃 그룹에서 뺀다. `/readyz`(DB 핑)로 두면 회전 503 구간(약 61초)에 파드가 전부 빠지고, 다시 등록돼 healthy가 되기까지 더 걸려 회복이 130~160초로 는다. DB는 모든 파드가 같이 쓰므로 빼도 갈 곳이 없다. 서버는 DB 핑·스키마 적용 뒤에야 리슨하므로 `/healthz` 200은 "초기화 끝"이다. ECS 타깃 그룹 헬스체크와 같은 의미다. `/readyz`는 canary 전용이다.
   - 프로브 타임아웃은 ALB 헬스체크와 같은 5초다(readiness 5초 × 3회, liveness 10초 × 6회 ≈ 1분). 2초로 두면 측정의 포화 구간(HPA가 늘린 파드가 일하기 전)에 응답이 잠깐 느려진 파드가 빠지고, 남은 파드로 부하가 몰려 재시작이 번진다. 그러면 판정이 HPA가 아니라 프로브를 잰다. 이 앱의 liveness는 교착을 잡을 일이 없어 느슨하게 둔다.
   - 파드 `securityContext`에 숫자 UID(65532)를 적는다. 이미지 USER가 이름(`nonroot`)이라 `runAsNonRoot`만 두면 kubelet이 root가 아님을 확인하지 못해 컨테이너를 만들지 않는다.
4. **무중단 롤링은 readiness gate + surge 1·unavailable 0 + preStop sleep 10초.**
   - 네임스페이스 라벨로 LB 컨트롤러가 readiness gate를 넣어, 새 파드가 ALB에서 healthy가 된 뒤에 옛 파드가 빠진다.
   - preStop은 **kubelet이 실행하는 `sleep` 동작**이다. 이미지가 distroless라 셸·`sleep`이 없어, exec 훅은 실패하고 `FailedPreStopHook` 이벤트만 남긴 채 바로 SIGTERM으로 간다(10초가 조용히 사라진다).
   - 타깃 그룹 헬스체크를 10초·healthy 2회로 줄였다. readiness gate가 이 값만큼 기다리므로, 새 파드가 트래픽을 받기까지 60~90초가 약 20초가 된다(HPA·롤링 모두).
5. **HPA(CPU 요청의 60%, 2~6개)를 켜면 Deployment에 `replicas`를 렌더하지 않는다.** 렌더하면 ArgoCD selfHeal이 HPA가 늘린 값을 drift로 보고 되돌린다.
6. **DB 연결 예산은 차트가 정한다.** 풀은 `DB_MAX_OPEN_CONNS`(기본 25, 차트 11). (HPA 최대 6 + 롤링 surge 1) × 11 = 77 < 알람 80 < 한도 약 112. 종료 중인 파드는 surge에 세지 않아 잠깐 8개(88)가 겹칠 수 있지만, 한도 아래이고 알람은 5분 평균이다.
7. **Terraform 스택을 둘로 나눈다** — `infra/prod`(AWS 리소스·EKS) → `infra/k8s`(클러스터 안). helm·kubernetes provider는 클러스터가 있어야 설정되기 때문이다. 두 스택은 원격 state가 아니라 **이름 규칙**으로만 묶인다(`linkpulse-prod-eks`, `-tg`, `-pg`, `-clicks`).
   - TGB·Application·ImageUpdater는 `kubernetes_manifest`가 아니라 로컬 차트를 `helm_release`로 넣는다(`kubernetes_manifest`는 plan 때 CRD가 있어야 한다).
   - **순서**: 컨트롤러(LBC·ArgoCD·Image Updater, `wait`) → `site`(ConfigMap·Secret·TGB) → `gitops`(Application·ImageUpdater). TGB가 파드보다 먼저 있어야 readiness gate가 붙는다.
   - **삭제**: TGB(LB 컨트롤러)와 Application(ArgoCD resources-finalizer)은 finalizer가 있어 컨트롤러가 살아 있어야 지워진다. `depends_on`이 역순 삭제를 만들지만 helm uninstall이 finalizer 처리를 기다리지 않을 수 있어, `full-destroy`가 terraform 전에 둘을 `kubectl delete --wait`로 지우고 잔존을 확인한다.
8. **배포는 GHCR + ArgoCD + Image Updater(write-back=argocd).**
   - CI는 테스트 후 `ghcr.io/naerrow/linkpulse:{<sha>,main}`을 **arm64+amd64**로 푸시만 한다(`GITHUB_TOKEN`의 `packages: write`). **CI에서 AWS 권한·OIDC 배포 역할이 사라졌다.** 클러스터 API도 CI에 열지 않는다.
   - Image Updater가 `main`의 다이제스트가 바뀌면 Application의 helm 파라미터(`image.tag`)를 바꾼다. git에 쓰지 않으므로 "main 직접 푸시 금지"와 부딪히지 않고, CI가 ArgoCD API를 부를 일도 없다.
   - GHCR 패키지는 공개다(공개 저장소, 이미지에 비밀 없음). EKS·k3s 모두 pull 비밀이 필요 없다.
   - 그래서 **인프라가 꺼져 있어도 머지 후 CI가 성공한다.** `full-apply`의 "부트스트랩 이미지 → CI 배포 대기" 단계가 사라졌다.
   - 롤백은 Image Updater를 0개로 줄이고 Application에 이미지를 고정한다(runbook R-1). 되돌리기도 명령 두 줄이다.
9. **DB 비밀번호 출처를 `DB_PASSWORD_SOURCE=aws|file`로 고른다.** ECS는 실행 역할이 기동 때 `DB_PASSWORD`를 넣어 줬지만(seed) EKS에는 그 주입이 없다.
   - `aws`(EKS): 앱이 기동 때 Secrets Manager에서 직접 읽고(최대 5회 재시도), 인증 실패 때 다시 읽는다. 권한은 Pod Identity.
   - `file`(P6 k3s): 쿠버네티스 Secret을 디렉터리로 마운트하고, 인증 실패 때 파일을 다시 읽는다(kubelet이 회전 때 파일을 바꾼다).
   - 두 출처는 `dbcreds.FetchFunc`만 다르다. 회전 대응 경로(provider·connector, ADR 0006)는 같다. `APP_ENV=production`은 출처를 요구한다.
10. **Step 1(회전 때 ECS 강제 재배포 Lambda)은 EKS로 옮기지 않고 지운다.** Step 2가 10-07부터 라이브로 회전을 스스로 견뎠다(온디맨드 실측 503 약 61초). 회전 이벤트 관찰 로그와 그 알람은 남긴다. EKS에서의 확인은 측정 창의 회전 드릴(단계 9-3)이다 — 그 전까지는 위험으로 둔다(회전 장애 4회 이력).
11. **파드 IAM은 EKS Pod Identity.** 서비스 어카운트마다 역할 하나(앱·LB 컨트롤러·Fluent Bit). 신뢰 정책이 세션 태그(`kubernetes-namespace`·`kubernetes-service-account`)까지 확인해, 연결을 잘못 만들어도 다른 파드가 그 역할을 받지 못한다.
12. **로그는 Fluent Bit(DaemonSet) → CloudWatch Logs `/eks/linkpulse-prod/app`.** 앱 네임스페이스 로그만 보낸다. kubernetes 필터의 `Merge_Log`가 앱 JSON을 최상위 필드로 풀어 ECS 시절 Logs Insights 쿼리가 그대로 돈다. `task_id`는 파드 이름이라 `count_distinct(task_id)`가 분마다 파드 수(HPA 효과)를 보여 준다. 파드 CPU는 측정 동안 `kubectl top`·`get hpa` 기록으로 보고, 0015에서 Prometheus로 정리한다(Container Insights는 켜지 않는다).

## 트레이드오프 (받아들인 것)

- **API 엔드포인트 공개 + 사설.** 공개 쪽은 IAM 인증·액세스 엔트리(클러스터를 만든 운영자만 관리자)가 막는다. 운영자 IP가 회사·집을 오가 CIDR 제한을 두지 않았다. 문제가 되면 `public_access_cidrs` 변수를 더한다.
- **노드 SG(EKS 클러스터 SG)는 egress를 전부 허용한다 — P4의 "HTTPS/DNS/DB만" 제한을 잃었다.** t4g는 파드별 SG를 지원하지 않아 파드가 노드 SG를 쓴다. 노드 전용 SG(시작 템플릿)로 제한을 지키려면 컨트롤 플레인·노드 간·웹훅 규칙을 직접 관리해야 해서, 노드 2대 구성에 그 복잡도를 들이지 않았다. 원래 규칙도 443은 0.0.0.0/0이었다. 수신은 여전히 ALB에서 8080만 더한다.
- **LB 컨트롤러는 공식 IAM 정책을 그대로 쓴다**(v3.6.0, `infra/prod/policies/`). TGB만 쓰는 데 비해 넓지만, 기동·조정 때 부르는 API를 손으로 줄이면 버전마다 깨질 수 있다. 차트 버전을 올리면 같은 태그의 정책으로 바꾼다.
- **배포 실패 알림 공백.** ECS 배포 실패 → Slack(ADR 0003)이 없어졌다. ArgoCD 동기화 실패 알림은 0015다. 그 사이 배포 실패는 사용자에게 번지지 않는다 — 새 파드가 healthy가 되지 않으면 롤링이 멈추고 옛 파드가 남는다. 가용성은 ALB 알람·canary가 본다.
- **파드 CPU·파드 수 알람 공백**(ECS 알람 3개 삭제). 0015에서 Prometheus로 둔다.
- **t4g는 버스트형이다.** T4g 기본 크레딧 `unlimited`라 기준선(20%)에 묶이지 않는다(9-1에서 확인). Fargate(비버스트)와 다르다는 점을 결과에 적는다.
- **비용**: EKS 컨트롤 플레인 시간당 0.10달러 + 노드 2대. 작업할 때만 켠다.
- **tfsec 지적 중 받아들인 것**(`infra/prod/eks.tf`에 ignore 주석): 공개 엔드포인트·0.0.0.0/0(위), Secret 암호화 미설정(EKS 1.28+는 모든 API 데이터를 AWS 소유 키로 기본 봉투 암호화한다 — EKS 사용자 가이드 *Default envelope encryption*), 컨트롤 플레인 로그 미설정(켜고 끄는 측정 클러스터라 비용만 는다). 나머지 지적(ALB 공개, SNS 암호화 등)은 이번 변경 전부터 있던 것이다.

## 노드 수용 (t4g.medium 2대, 노드당 파드 17개)

앱 최대 7개(HPA 6 + surge 1)와 시스템 파드가 들어가야 한다. 요청(requests) 기준 계산이다. 애드온 기본 요청은 추정이 섞여 있어 측정 창 9-1에서 `kubectl describe nodes`로 확인한다.

| 구성 요소 | 파드 | CPU 요청 | 메모리 요청 |
| --- | --- | --- | --- |
| 앱(최대) | 7 | 7 × 250m = 1750m | 7 × 512Mi = 3584Mi |
| DaemonSet(aws-node·kube-proxy·pod-identity-agent·fluent-bit) | 4 × 2 = 8 | 약 450m | 약 200Mi |
| coredns | 2 | 200m | 140Mi |
| metrics-server | 1~2 | 200m 이하 | 400Mi 이하 |
| LB 컨트롤러 | 2 | (차트 기본 없음) | — |
| ArgoCD(controller·repo-server·server·redis, appset 0개·dex·notifications 끔) | 4 | (차트 기본 없음) | — |
| Image Updater | 1 | — | — |
| **합** | **최대 26 / 34** | **약 2.6 / 할당 약 3.8 vCPU** | **약 4.3 / 할당 약 6.7 GiB** |

ArgoCD·LB 컨트롤러는 요청이 없어 스케줄은 막지 않지만 실제 메모리를 쓴다(합쳐 1GiB 안팎으로 본다). 여유가 모자라면 앱 `maxReplicas`부터 낮춘다(연결 예산도 같이 다시 계산).

## 결과

- ECS·ECR·OIDC 배포 역할·Step 1 Lambda·ECS 알람·배포 실패 EventBridge가 사라졌다. 남은 AWS 경계는 ALB·RDS·SQS·관찰 장치다.
- 같은 차트(`deploy/helm/linkpulse`)를 P6에서 `values-home.yaml` + `file` 출처로 쓴다. 사이트 값은 차트 밖(ConfigMap·Secret)에 있다.
- 검증은 기계적 게이트로 받는다: 앱 `-race` 테스트·변이 검사, `scripts/check-charts.sh`(렌더 단언·kubeconform·CRD 스키마, CI에서도 돈다), `terraform validate`·tfsec. 클러스터 위 동작은 측정 창(plan 0014 단계 9)에서 확인한다.
