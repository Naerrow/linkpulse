---
status: approved # in-review | approved
revision: 2
created: 2026-10-13
---

# 0014. ECS → EKS — Helm·ArgoCD·GHCR·HPA

## 목표

1. 앱을 ECS Fargate에서 **EKS**로 옮긴다.
   - 배포: **같은 이미지(GHCR)·같은 Helm 차트**를 **ArgoCD**가 git에서 끌어와 배포한다(GitOps).
   - 확장: **HPA**가 CPU로 파드 수를 늘린다. ECS 오토스케일링은 건너뛴다(AGENTS.md §5).
2. **P6(집 k3s)로 그대로 가져갈 수 있는 모양**을 만든다.
   - 차트는 사이트를 모른다. AWS가 만든 값(RDS 주소·시크릿 ARN·큐 URL·타깃 그룹)은 사이트 쪽 "접착제"로 넣는다.
   - DB 비밀번호 출처를 `aws|file` 중에서 고른다.
3. EKS에서 **세 시나리오를 한 측정 창에서** 다시 재 0012·0013과 비교한다.
   - 0013에서 같은 코드인데도 창마다 태스크 CPU가 달랐다. 그래서 세 시나리오를 한 창에서 잰다.
   - HPA가 다음 병목(앱 CPU)을 넘기는지 본다.

## 배경/제약

- **0013 결과**(`load/k6/results/0013-async-clicks.md`): 세 시나리오 모두 3200 rps 이상이었다. **다음 병목은 앱 CPU**다(`spread` 3200에서 태스크 하나 97.5%). 1600~3200에서 p99가 풀 대기로 10~30ms까지 오른다.
- **지금 ECS에 묶인 것**(이 plan이 바꾸거나 지운다):

  | 파일 | ECS 의존 | 처리 |
  | --- | --- | --- |
  | `ecs.tf` | 클러스터·서비스·태스크 정의(env·시크릿 주입·awslogs) | EKS로 대체 |
  | `iam.tf` | 실행 역할(시크릿 주입)·태스크 역할(Secrets Manager·SQS) | Pod Identity 역할로 대체 |
  | `ecr.tf`, `github_oidc.tf`, `.github/workflows/deploy.yml` | ECR 푸시·ECS 배포·OIDC 배포 역할 | GHCR 푸시로 대체, AWS 배포 권한 삭제 |
  | `rotation-redeploy.tf` | 회전 시 ECS 강제 재배포 Lambda(Step 1) | Step 1 리소스만 삭제(아래 "회전"). 같은 파일의 `local.rotation_secret_arn`과 `secret_events_*` 관찰 로그·알람은 남긴다 |
  | `eventbridge.tf` | ECS 배포 실패 → SNS | 삭제(ArgoCD로 대체는 0015) |
  | `monitoring.tf` | `ecs_cpu_high`·`ecs_memory_high`·`ecs_running_tasks_low` | 삭제. 가용성은 ALB 알람·canary가 맡고, CPU 알람은 0015 Prometheus로 |
  | `logs.tf`, `dbcreds.TaskID` | awslogs 로그 그룹, ECS 메타데이터의 task ID | Fluent Bit 로그, 파드 이름 |
  | `scripts/full-apply-prod.sh`·`full-destroy-prod.sh` | ECS 0대로 만들고 CI 배포를 기다림 | 두 스택 순서로 다시 쓴다 |
  | `outputs.tf`·`variables.tf`, GitHub Variables | ECS·ECR 출력과 변수, `ECR_REPOSITORY_*`·`ECS_*`·`AWS_DEPLOY_ROLE_ARN` | 삭제(GitHub Variables는 [사람]) |

- **그대로 두는 것:** VPC·서브넷·NAT, RDS, **ALB·리스너·타깃 그룹(ip)·ACM·Route53**, SQS, SNS·Chatbot, canary(us-east-1), 시크릿 이벤트 관찰 로그, ALB·RDS 알람.
  - 예외 하나: 타깃 그룹 헬스체크를 간격 30초·healthy 3회에서 **10초·2회**로 줄인다. readiness gate가 ALB healthy를 기다리므로, 새 파드가 트래픽을 받기까지 60~90초가 약 20초로 준다(HPA·롤링 모두).
- **레이트리밋 토폴로지를 지킨다.** `clientIP`는 XFF 맨 오른쪽 한 홉(ALB)을 믿는다(`rlTrustedHops = 1`).
  ALB가 파드로 바로 보내야 이 값이 유지된다. 그래서 nginx 같은 인그레스 컨트롤러를 ALB 뒤에 두지 않는다.
- **AGENTS.md 가드레일:** `terraform apply`·`destroy`, `kubectl apply`, 리소스 생성은 사람이 한다. 에이전트는 `fmt`·`validate`, `helm lint`·`helm template`까지만 하고, `terraform plan`도 사람이 실행한다.
  AWS로 가는 설명 문자열은 ASCII만 쓴다.
- **비용**(작업할 때만 켠다): EKS 컨트롤 플레인 시간당 0.10달러 + 노드 t4g.medium 2대(대략 시간당 0.08달러). 지금 ECS 구성보다 시간당 약 0.15달러 더 든다.
- **공개 저장소다.** 차트·values·ArgoCD 앱 정의는 공개된다. 비밀은 넣지 않는다(비밀번호는 Secrets Manager, 관리자 해시는 클러스터 안 Secret).

## 설계

### 클러스터

- **EKS + 관리형 노드 그룹**: t4g.medium(ARM64, 2 vCPU·4GiB) **2대 고정**(최소·최대·희망 모두 2). 앱 서브넷(사설, NAT 경유)에 둔다.
  - 버전은 구현 때 표준 지원 중 최신을 CLI(`aws eks describe-cluster-versions`)로 확인해 고정한다. `preStop` sleep 동작을 지원하는지도 함께 본다.
  - EKS Auto Mode나 Karpenter, Cluster Autoscaler는 쓰지 않는다. 오토스케일러가 없으면 "최대 3"은 쓰이지 않으므로 2대로 고정한다. 무엇이 도는지 보이는 구성을 고른다.
  - **노드 2대에 다 들어가야 한다.** 앱 최대 7개(HPA 6 + surge 1)와 시스템 파드(ArgoCD, LBC 2, Image Updater, metrics-server, coredns 2, DaemonSet 4종)의 요청 합이 할당 가능량 안이고, 파드 수가 노드당 17개(t4g.medium ENI 한도) 안이어야 한다.
    ArgoCD는 dex·notifications·applicationset을 끈다. 단계 2에서 요청 합을 계산하고, 9-1에서 `kubectl describe nodes`로 확인한다.
  - t4g는 버스트형이다. T4g 기본값인 `unlimited` 크레딧인지 9-1에서 확인한다(`standard`면 크레딧이 떨어질 때 기준선 20%로 묶여 측정이 틀어진다). 결과 문서에 Fargate(비버스트)와 다르다고 적는다.
- **인증:** 액세스 엔트리(API 모드)로 운영자 IAM 주체에만 클러스터 관리자를 준다. `aws-auth` ConfigMap은 쓰지 않는다.
  API 엔드포인트는 공개 + 사설이다. 공개 쪽은 IAM 인증이 막는다. 운영자 IP가 회사·집을 오가므로 CIDR 제한은 두지 않는다. 이 점은 트레이드오프로 ADR에 적는다.
- **파드 IAM:** EKS Pod Identity(에이전트 add-on). 서비스 어카운트별로 역할을 붙인다.
  - `linkpulse`: `secretsmanager:GetSecretValue`(회전 시크릿 하나), `sqs:SendMessage`·`ReceiveMessage`·`DeleteMessage`(클릭 큐 하나). 지금 태스크 역할과 같다.
  - `aws-load-balancer-controller`: 공식 정책(버전 고정).
  - 로그 수집기: `logs:PutLogEvents` 등.
- **애드온:** vpc-cni, coredns, kube-proxy, eks-pod-identity-agent, metrics-server(HPA용).
  metrics-server가 EKS add-on으로 없으면 Helm으로 설치한다(구현 때 `aws eks describe-addon-versions`로 확인).

### ALB 연결 — 기존 ALB를 그대로 두고 TargetGroupBinding

- AWS Load Balancer Controller가 **ALB를 새로 만들지 않게** 한다. 지금 Terraform의 타깃 그룹(ip 타입)에 **`TargetGroupBinding`으로 파드 IP만 등록**한다.
  - ALB·리스너·ACM·Route53·ALB 알람·canary·`collect-metrics.sh`의 ALB 지표가 그대로 산다.
  - destroy 때 컨트롤러가 만든 ALB·SG가 남아 VPC 삭제를 막는 일이 없다(컨트롤러는 타깃 등록만 한다).
  - XFF 홉도 그대로다(클라이언트 → ALB → 파드).
- **보안 그룹:** VPC CNI라 파드는 노드의 SG(EKS 클러스터 SG)를 쓴다. t4g는 파드별 SG를 지원하지 않는다. `app` SG는 지운다.
  - 수신은 옮긴다: 클러스터 SG에 "ALB SG에서 8080". 그리고 상대쪽 참조도 바꾼다 — ALB SG의 `alb_to_app` egress와 RDS SG의 `data_from_app` 수신 출처가 클러스터 SG가 된다.
  - **egress 제한(P4, "HTTPS/DNS/DB만")은 잃는다.** EKS가 만든 클러스터 SG는 모든 egress를 허용한다. 노드 전용 SG를 시작 템플릿으로 붙여 제한을 지키려면 컨트롤 플레인·노드 간·웹훅 규칙을 직접 관리해야 한다.
    노드 2대 구성에 그 복잡도는 맞지 않아 **전체 허용을 받아들이고 ADR 0007 트레이드오프에 적는다.** 원래 규칙도 443은 0.0.0.0/0이었다.
  - TGB의 `networking` 자동 규칙은 쓰지 않는다 — SG는 Terraform만 바꾼다.
- **무중단 롤링:** 네임스페이스에 `elbv2.k8s.aws/pod-readiness-gate-inject=enabled`를 붙인다. 새 파드가 ALB에서 healthy가 된 뒤에 옛 파드가 빠진다.
  - 종료 때는 `preStop` 10초 대기 뒤 SIGTERM을 받는다. 그동안 ALB 등록 해제가 끝난다. `terminationGracePeriodSeconds`는 45초다(앱 종료: HTTP 10초 + 마지막 flush 5초).
  - **`preStop`은 `lifecycle.preStop.sleep.seconds: 10`(kubelet이 실행)로 둔다.** 이미지가 distroless라 셸·`sleep`이 없다. exec 훅은 실패해도 `FailedPreStopHook` 이벤트만 남기고 바로 SIGTERM으로 가서, 10초가 조용히 사라진다.
  - 롤링은 `maxSurge: 1`, `maxUnavailable: 0`이다(연결 수 계산, 아래 "DB 연결 풀").

### Helm 차트 — `deploy/helm/linkpulse` (사이트를 모른다)

- Deployment, Service(ClusterIP), HPA, PodDisruptionBudget(minAvailable 1), ServiceAccount, 프로브(liveness·readiness 모두 `/healthz`).
- **readiness는 `/readyz`(DB 핑)가 아니라 `/healthz`다.** 지금 ECS 타깃 그룹 헬스체크와 같은 의미를 지킨다.
  - TGB에서는 readiness가 실패한 파드를 LB 컨트롤러가 타깃 그룹에서 등록 해제한다. `/readyz`로 두면 회전 503 구간(약 61초)에 파드가 모두 빠진다.
    다시 등록돼 healthy가 될 때까지 더 걸려 회복이 130~160초로 늘고, 9-3 회전 드릴이 Step 2가 아니라 프로브 때문에 실패한다.
  - DB는 모든 파드가 같이 쓴다. DB 장애 때 파드를 빼도 갈 곳이 없다.
  - 서버는 DB 핑·스키마 적용이 끝난 뒤에야 리슨한다(`cmd/server/main.go`). 그래서 `/healthz` 200은 "초기화 끝"을 뜻한다.
  - `/readyz`는 canary 전용으로 남긴다. 이 결정은 ADR 0007에 적는다.
- **HPA를 켜면 Deployment에 `spec.replicas`를 렌더하지 않는다.** 렌더하면 ArgoCD selfHeal이 HPA가 늘린 값을 drift로 보고 되돌린다.
- **사이트 값은 이름으로만 참조한다.** 환경값은 `envFrom`으로 사이트가 넣은 ConfigMap(`linkpulse-site`)과 Secret(`linkpulse-site-secret`)에서 읽는다.
  차트 values에는 그 이름·이미지·리소스·HPA 범위만 둔다.
  - `values.yaml`(기본): 이미지 `ghcr.io/naerrow/linkpulse`, CPU 요청·한도 250m(Fargate와 같은 크기), 메모리 512Mi, HPA 최소 2·최대 6·CPU 60%.
  - `values-aws.yaml`: AWS 사이트에서 다른 값만. P6에서 `values-home.yaml`을 더한다.
- **DB 연결 풀:** 지금 태스크당 25개 고정이다. 파드 6개면 150개로 t4g.micro 한도(약 112)와 알람(80)을 넘는다.
  - 그래서 `DB_MAX_OPEN_CONNS` env로 열고(기본 25, 로컬·ECS 동작 불변), 차트는 **11**로 둔다.
  - 롤링 surge까지 넣어 계산한다: (HPA 최대 6 + surge 1) × 11 = 77 < 80.
  - 종료 중인 파드는 surge에 세지 않아 잠깐 8개가 겹칠 수 있다(88). 그래도 한도 112 아래이고, 알람은 5분 평균이라 잠깐 겹침에는 울리지 않는다.
- **HPA 기준:** CPU 요청의 60%. 0013에서 태스크 CPU가 50~70%일 때 p99가 오르기 시작했다. 그 전에 늘린다.

### 사이트 접착제 — `infra/k8s` (새 Terraform 스택, AWS 전용)

AWS가 만든 값은 apply마다 바뀐다(타깃 그룹 ARN, 시크릿 ARN 등). git(차트)에 넣을 수 없어서, 클러스터 안에 Terraform이 넣는다.

| 무엇 | 방법 |
| --- | --- |
| 네임스페이스 `linkpulse`(readiness gate 주입 라벨 포함) | `kubernetes_namespace` — `helm_release`의 `create_namespace`는 라벨을 붙이지 못한다 |
| AWS Load Balancer Controller, ArgoCD, ArgoCD Image Updater, Fluent Bit | `helm_release`(차트 버전 고정) |
| ConfigMap `linkpulse-site`(DB_HOST·DB_SECRET_ARN·CLICK_QUEUE_URL·AWS_REGION 등), Secret `linkpulse-site-secret`(ADMIN_TOKEN_SHA256) | 작은 로컬 차트(`infra/k8s/charts/site-aws`)를 `helm_release`로 |
| TargetGroupBinding(타깃 그룹 ARN) | 같은 로컬 차트 |
| ArgoCD Application(`linkpulse`), Image Updater 설정(v1.x면 `ImageUpdater` CR) | `argocd-apps` 차트(또는 같은 로컬 차트)를 `helm_release`로 |

- **스택을 나누는 이유:** helm·kubernetes provider는 클러스터가 있어야 설정된다. 클러스터를 만드는 스택과 한 번에 돌리면 처음 생성 때 provider 설정이 미정 값이 된다.
  그래서 `infra/prod`(AWS) → `infra/k8s`(클러스터 안) 순서로 따로 apply한다. state 키도 따로다.
- **CRD를 plan 때 요구하지 않게 한다.** TGB·Application은 `kubernetes_manifest`가 아니라 `helm_release`로 넣는다(`kubernetes_manifest`는 plan 때 CRD가 있어야 한다).
- **스택 안 순서(CRD·웹훅·finalizer):**
  - 생성: TGB는 LBC의 CRD·검증 웹훅이 준비된 뒤, Application은 ArgoCD가, `ImageUpdater` CR은 Image Updater가 준비된 뒤에 들어가야 한다.
    그래서 사이트 차트·Application 릴리스가 LBC·ArgoCD·Image Updater 릴리스와 네임스페이스를 `depends_on`한다. 컨트롤러 릴리스는 `wait = true`다.
  - 삭제: TGB(LBC)와 Application(ArgoCD resources-finalizer)은 finalizer가 있어 컨트롤러가 살아 있어야 지워진다. `depends_on`이 있으면 terraform이 역순으로 지운다.
    그래도 helm uninstall이 finalizer 처리를 기다리지 않을 수 있으니, `full-destroy`는 terraform destroy 전에 Application·TGB를 `kubectl delete --wait`로 먼저 지우고 남지 않았는지 확인한다.
  - 이 순서를 단계 6의 런북에 적는다.
- **공개 PR에 붙는 plan 출력:** `infra/k8s`로 옮기는 `rate_limit_exempt_ips`(집 IP)와 `admin_token_sha256`은 새 스택에서도 `sensitive = true`로 선언하고, helm 값으로는 `set_sensitive`로 넘긴다.

### 배포 흐름 — GHCR + ArgoCD + Image Updater

```
main 머지 ─▶ GitHub Actions: 테스트 → arm64+amd64 이미지 → ghcr.io/naerrow/linkpulse:{<sha>, main}
                                                            │
ArgoCD Image Updater: `main` 태그의 다이제스트가 바뀌면 ──────┘
   → ArgoCD Application의 image 파라미터를 그 다이제스트로 바꾼다(git에 쓰지 않음, write-back=argocd)
   → ArgoCD가 차트를 다시 렌더해 롤링 배포
```

- **GitHub Actions에서 AWS 권한이 사라진다.** CI는 GHCR에만 푸시한다(`GITHUB_TOKEN`의 `packages: write`). 클러스터 API를 CI에 열지 않는다.
  ECR·ECS 배포 단계와 OIDC 배포 역할·provider를 지운다.
- **Image Updater를 쓰는 이유:**
  - CI가 git에 태그를 커밋하면 "main 직접 푸시 금지" 규칙과 부딪힌다.
  - CI가 ArgoCD API를 부르면 ArgoCD를 인터넷에 열어야 한다.
  - Image Updater는 클러스터 안에서 GHCR을 읽기만 한다.
- **GHCR 패키지는 공개로 둔다.** 저장소가 공개이고 이미지에 비밀이 없다. 그러면 EKS와 P6 k3s 모두 pull 비밀 없이 받는다(한 번만 GitHub 화면에서 [사람]).
- **이미지는 `linux/arm64,linux/amd64` 멀티 아키텍처로 만든다.** P6 집 서버는 x86일 가능성이 높고, "같은 이미지" 원칙(AGENTS.md §5)을 지금 지키는 편이 싸다.
  Dockerfile이 이미 `BUILDPLATFORM` 크로스 컴파일이라 QEMU가 필요 없다.
- **Image Updater는 차트 버전을 고정한다.** v1.x는 설정이 Application 주석이 아니라 `ImageUpdater` CR이다. 고정한 버전의 방식으로 설정하고 롤백 런북도 그 방식으로 쓴다.
- **GHCR은 인프라 destroy와 무관하다.** 그래서 `full-apply`가 더는 "부트스트랩 이미지 → CI 배포 대기"를 하지 않는다. 지금 스크립트의 2단계가 사라진다.
- **ArgoCD 접근:** 인터넷에 열지 않는다. 운영자는 `kubectl port-forward`로 본다.
- **롤백:** `argocd app set linkpulse -p image.tag=<예전 sha>`로 고정하고, Image Updater가 이 앱을 건드리지 않게 잠시 끈다(v1.x면 `ImageUpdater` CR을 지우거나 대상에서 뺀다). 런북에 적는다.

### DB 비밀번호 출처 — `DB_PASSWORD_SOURCE=aws|file`

- 지금 ECS는 기동 때 실행 역할이 Secrets Manager 값을 `DB_PASSWORD` env로 넣어 주고(seed), 앱은 인증 실패 때 Secrets Manager에서 다시 읽는다(ADR 0006, Step 2).
  EKS에는 이 주입이 없다.
- **`aws`:** 기동 때 앱이 Secrets Manager에서 직접 읽는다(seed 없음, 재시도). 회전 뒤 다시 읽는 경로는 지금과 같다. EKS의 기본값이다.
- **`file`:** `DB_PASSWORD_FILE` 경로에서 읽는다. 쿠버네티스 Secret을 파일로 마운트하면 회전 때 kubelet이 파일을 바꾸고, 앱은 인증 실패 때 파일을 다시 읽는다. P6 k3s용이다.
- 두 방식 모두 `dbcreds.Provider`의 `FetchFunc`만 다르다(Secrets Manager 호출이냐, 파일 읽기냐). `config`는 `APP_ENV=production`에서 `DB_PASSWORD` env 대신 출처 설정을 요구한다.
- 로컬 compose(`DATABASE_URL`, 정적)는 그대로다.

### 회전(Step 1 Lambda) 정리

- `rotation-redeploy` Lambda는 회전 때 **ECS 서비스를 강제 재배포**한다(Step 1). EKS에서는 그대로 쓸 수 없다.
- Step 2(앱이 인증 실패 때 비밀번호를 다시 읽음)가 10-07부터 라이브다. 온디맨드 회전 실측에서 503 약 61초 뒤 스스로 회복했다.
  그래서 **Step 1을 EKS로 옮기지 않고 지운다**(Lambda·EventBridge·멱등 테이블·DLQ·관련 알람). 시크릿 이벤트 관찰 로그는 남긴다.
- **측정 창에서 회전 드릴로 확인한다**(단계 9). 그 전까지는 위험이다. 4번의 회전 장애 이력이 있어 이것을 리스크 표 맨 위에 둔다.

### 로그·지표

- **앱 로그:** Fluent Bit(DaemonSet)가 CloudWatch Logs `/eks/linkpulse-prod/app`로 보낸다.
  쿠버네티스 필터의 `Merge_Log`로 앱 JSON을 **최상위 필드로 풀어** 지금 쿼리(`msg`, `click queue`, `db pool`)가 그대로 돌게 한다. 실제 필드 모양은 측정 창 스모크에서 확인한다.
  - upstream Fluent Bit 차트를 쓰고, **EKS Pod Identity 자격증명을 지원하는 버전으로 고정**한다(오래된 기반 이미지는 지원하지 않을 수 있다).
- **task ID:** ECS 메타데이터가 없으면 `POD_NAME`(다운워드 API) → `HOSTNAME` 순으로 쓴다. `collect-metrics.sh`의 `count_distinct(task_id)`가 **분마다 파드 수(HPA 효과)**를 보여 준다.
- **파드 CPU:** metrics-server는 CloudWatch에 남지 않는다. Container Insights는 켜지 않는다.
  측정 동안 `kubectl top pods`와 `kubectl get hpa`를 30초마다 원자료 디렉터리에 찍는 기록 스크립트로 대신한다. 0015에서 Prometheus로 정리한다.
- `collect-metrics.sh`: ECS 지표를 파드 지표로 바꾸고 로그 그룹을 인자로 받는다. 0012·0013 원자료 재수집은 기존 기본값으로 된다.

### 재측정

- 0013과 같은 도구다. 다만 **세 시나리오를 한 창에서**, `MAX_RPS=6400`(단계 하나 추가)으로 잰다. HPA가 늘리는 구간을 보려고서다.
  - 6400 rps에서 `maxVUs` 500이면 평균 지연 약 78ms까지 버틴다. 발생기는 0013 3200에서 k6 코어 0.6개였다 — 판정 0번으로 본다.
  - **각 실행은 파드 2개에서 시작한다.** HPA 축소 안정화가 기본 5분이라, 실행 사이에 `kubectl get hpa`가 2를 보일 때까지 기다리고 시작 파드 수를 기록한다.
- 판정은 0012의 판정표에 다음을 더한다.
  - (a) HPA가 늘어난 파드 수(분 단위)
  - (b) 3200에서 p99가 0013보다 내려갔는가. 교란 요인이 있다 — 풀 25→11, Fargate→t4g 노드, HPA가 함께 바뀐다. 결론에 이것을 같이 적는다.
  - (c) RDS 연결 ≤ 80(풀 11 × (최대 6 + surge 1) = 77)
  - (d) 정합성 불변식

## 실행 단계

1. **앱** — `DB_PASSWORD_SOURCE=aws|file`(기동 때 직접 읽기, 파일 다시 읽기), `DB_MAX_OPEN_CONNS`, `TaskID`의 `POD_NAME`·`HOSTNAME` 폴백.
   → 검증: 단위 테스트(`-race`).
   - 출처별 기동·인증 실패 뒤 다시 읽기: 파일을 바꾸면 새 값, aws는 가짜 fetch
   - 운영에서 출처 미설정이면 기동 오류
   - 풀 크기 env, task ID 폴백 순서

   **변이 검사:** 파일을 다시 읽지 않게, 기동 fetch를 빼게, 폴백 순서를 바꾸게 했을 때 각 테스트가 실패해야 한다.
   compose 로컬 동작이 그대로인지 기존 E2E로 확인한다 [사람].
2. **Helm 차트** — `deploy/helm/linkpulse`, `values-aws.yaml`.
   → 검증: `helm lint`, `helm template` 결과를 `kubeconform`(쿠버네티스 스키마)으로 검사.
   - CRD(TGB·Application) 스키마는 CRDs-catalog를 스키마 위치로 주고, 없으면 `-ignore-missing-schemas`로 넘긴다.
   - 렌더 결과로 확인한다: HPA를 켜면 `spec.replicas`가 없다, `preStop`이 `sleep`이다, readiness가 `/healthz`다, 롤링이 `maxSurge: 1`·`maxUnavailable: 0`이다.
   - 노드 2대 수용 계산(요청 합·노드당 파드 수)을 결과에 남긴다.
   (선택 [사람]) 로컬 k3d에서 `file` 출처 + compose DB로 차트가 뜨는지 본다 — P6 미리보기다.
3. **CI** — `deploy.yml`을 GHCR 빌드·푸시(`linux/arm64,linux/amd64`)로 바꾼다. AWS 단계를 지운다.
   → 검증: actionlint. 머지 후 workflow_dispatch로 GHCR에 `<sha>`·`main`이 두 아키텍처로 올라가는지 본다 [사람]. 패키지를 공개로 바꾸고, 쓰지 않게 된 GitHub Variables를 지운다 [사람].
4. **Terraform `infra/prod`** — EKS 클러스터·노드 그룹·애드온·액세스 엔트리·Pod Identity 역할과 연결, SG 규칙 이동(`app` SG 삭제, 클러스터 SG·ALB SG·RDS SG 참조 변경), 타깃 그룹 헬스체크 10초·2회.
   ECS·ECR·OIDC 배포 역할·rotation-redeploy의 Step 1 리소스·deploy_failed·ECS 알람·`logs.tf`, `outputs.tf`·`variables.tf`의 ECS·ECR 항목을 지운다.
   → 검증: `fmt`·`validate`, tfsec 또는 checkov(IAM `*` 없음 확인). `terraform plan` [사람](인프라가 꺼져 있으면 전체 생성으로 나온다).
   AWS 설명 문자열 ASCII 확인. IAM 정책 주장은 Service Authorization Reference로 확인한다.
5. **Terraform `infra/k8s`** — 네임스페이스(라벨)·LB 컨트롤러·ArgoCD·Image Updater·Fluent Bit·사이트 차트(ConfigMap·Secret·TGB)·Application, `depends_on` 순서.
   → 검증: `fmt`·`validate`, 로컬 사이트 차트 `helm template`·kubeconform. `plan`은 클러스터가 있어야 해서 측정 창에서 [사람].
   민감 변수(`rate_limit_exempt_ips`·`admin_token_sha256`)가 `sensitive`이고 `set_sensitive`로 넘어가는지 코드로 확인하고, 9-1의 첫 plan 출력에서 helm 값이 가려졌는지 본다.
6. **스크립트·문서** — `full-apply-prod.sh`(prod → k8s → ArgoCD 동기화·파드 준비 대기 → 헬스체크), `full-destroy-prod.sh`(Application·TGB `kubectl delete --wait`·잔존 확인 → k8s → prod).
   런북 정리:
   - ECS 명령 → `kubectl`
   - 지운 알람 절 정리
   - 롤백(이미지 고정, 고정한 Image Updater 버전의 방식)
   - ArgoCD 접근(port-forward)
   - `infra/k8s` 생성·삭제 순서와 finalizer에 걸렸을 때 조치

   **ADR 0007**(EKS 전환: TGB, Image Updater, Step 1 폐기, 공개 API 엔드포인트, 스택 분리, readiness=`/healthz`, 노드 SG egress 전체 허용)을 쓴다.
   AGENTS.md §5의 "인프라가 꺼져 있으면 머지 후 배포 실패가 정상" 문구를 고친다(CI가 GHCR 푸시만 하므로 꺼져 있어도 성공한다).
   → 검증: `bash -n`·shellcheck. 단계 순서를 런북과 대조한다.
7. **수집 도구** — `collect-metrics.sh` 파드 지표·로그 그룹 인자, `capacity.js`의 `ALL_RATES`에 6400, `kubectl top`·`get hpa` 기록 스크립트.
   → 검증: 가짜 `aws`로 끝까지 실행, 0013 원자료 재수집이 같은 표를 내는지 확인. `MAX_RPS=6400`에서 단계 목록에 6400이 들어가는지 확인.
8. **코드 검토 1라운드(claude-ide) → 커밋·PR·머지 [사람].** 머지 후 CI는 GHCR 푸시만 하므로, 인프라가 꺼져 있어도 성공해야 한다(이번부터 "배포 실패가 정상"이 아니다).
9. **측정 창 [사람]** — 약 2시간 30분.
   1. `full-apply-prod.sh` — 두 스택, ArgoCD 동기화, 파드 2개 Ready, TGB 대상 healthy. 노드 수용(`kubectl describe nodes`)과 노드 크레딧 `unlimited`를 확인한다.
   2. 스모크: 0013 3-1(리다이렉트 3번 → 클릭 3). 그리고 로그 필드 확인 — `click queue`·`db pool`이 Logs Insights에서 최상위 필드로 나오는가.
   3. **회전 드릴:** `aws secretsmanager rotate-secret`(온디맨드) → `/readyz` 503이 2분 안에 스스로 회복하는가. 실패하면 측정하지 않고 멈춘다.
   4. 측정 예외: `rate_limit_exempt_ips` 변수가 `infra/prod`의 ECS env에서 `infra/k8s`의 사이트 ConfigMap으로 옮겨 간다.
      `infra/k8s` apply(`-var`) + `infra/prod` apply(`alarm_sms_number=`) → `kubectl rollout restart deploy/linkpulse`(envFrom은 ConfigMap 변경을 다시 읽지 않는다) → `rate_limit_exempt_count=1`.
   5. `spread`·`hot`·`read`를 **한 창에서** `MAX_RPS=6400`으로 측정·수집. 실행마다 파드 2개에서 시작한다(위 "재측정").
   6. destroy(k8s → prod), state 0개 확인.

   → 검증: 세 실행 모두 원자료가 있고, 판정에 쓴 단계가 판정 0번에 걸리지 않았다.
10. **결과 기록** — `load/k6/results/0014-eks.md`: 0012·0013과 단계 비교, HPA 파드 수, 회전 드릴 결과, 0015로 넘길 것. docs PR [사람].

검토: 사용자 확인 → plan 검토 1라운드(claude-ide) → 구현 → 코드 검토 1라운드 → 측정. 블로커가 없으면 라운드를 더 열지 않는다.

## 리스크/롤백

| 리스크 | 완화 | 롤백 |
| --- | --- | --- |
| **회전 대응 후퇴**(Step 1 Lambda 삭제) — 회전 장애 4회 이력 | Step 2가 라이브로 실측 회복(61초). readiness를 `/healthz`로 둬 503 구간에 파드가 타깃 그룹에서 빠지지 않게 한다. 측정 창의 회전 드릴을 통과해야 진행 | 드릴 실패 시 측정 중단, `kubectl rollout restart`를 런북 즉시 조치로. 먼저 TGB 대상이 빠졌는지(프로브 결합) 보고, 아니면 Step 1을 EKS용(Lambda → `rollout restart`)으로 다시 만드는 plan을 연다 |
| 처음 쓰는 구성 요소가 많다(EKS·LB 컨트롤러·ArgoCD·Image Updater·Fluent Bit) | 버전 고정, 단계별 확인 지점(9-1·9-2), ADR로 이유 기록 | git revert → ECS 구성으로 `full-apply` |
| destroy가 남는 리소스에 막힘(ENI·SG) | ALB를 컨트롤러가 만들지 않는다(TGB). destroy는 k8s 스택 먼저 | ops-gotchas에 기록, 수동 정리 절차 |
| destroy가 finalizer(TGB·Application)에 걸려 멈춤 | `depends_on`으로 컨트롤러보다 먼저 지우고, `full-destroy`가 terraform 전에 `kubectl delete --wait`·잔존 확인 | 런북: 컨트롤러가 이미 없으면 finalizer를 지우는 `kubectl patch` |
| 노드 SG egress 전체 허용(P4 제한 상실) | 수신은 ALB에서만. ADR 0007에 트레이드오프 | 노드 전용 SG(시작 템플릿)로 바꾸는 plan |
| 로그 필드 모양이 달라 수집·런북 쿼리가 깨짐 | Fluent Bit `Merge_Log`, 스모크에서 확인(9-2) | 쿼리를 실제 필드에 맞춘다 |
| Image Updater 오동작(엉뚱한 이미지·배포 안 됨) | 차트 버전 고정, `main` 다이제스트만 추적. 이미지 고정 롤백 절차 | Image Updater를 끄고(v1.x면 CR 삭제) 태그 고정 |
| RDS 연결 초과 | 풀 11 × (HPA 최대 6 + surge 1) = 77 | `DB_MAX_OPEN_CONNS`·`maxReplicas` 낮춤 |
| 공개 API 엔드포인트 | IAM 인증·액세스 엔트리 최소화. ADR에 트레이드오프 | CIDR 제한 변수 추가 |
| 비용 증가(시간당 약 0.15달러 추가) | 작업할 때만 켠다 | — |
| 배포 실패 알림 공백(ECS 이벤트 규칙 삭제) | 가용성은 ALB 알람·canary가 잡는다. ArgoCD 동기화 실패 알림은 0015 | 0015 |

## 검토 반영 로그

<!-- /plan-merge가 라운드별로 기록. 형식: [rN] 리뷰어#번호 지적요약 → 반영|기각 — 사유 -->

r1 리뷰는 claude-ide 하나다(codex 사용 불가). `reviewed-revision: 1` 일치.

- [r1] claude-ide#1 readiness `/readyz`면 회전 503 구간에 TGB가 파드를 모두 빼 회복 130~160초, 드릴 오진 → 반영 — readiness·liveness 모두 `/healthz`, `/readyz`는 canary 전용. 서버가 DB 핑·스키마 뒤에 리슨함을 `main.go`·`db.Open`으로 확인. ADR 0007·리스크 표에 적음
- [r1] claude-ide#2 HPA와 ArgoCD selfHeal이 `replicas`를 서로 되돌림 → 반영 — HPA를 켜면 `spec.replicas`를 렌더하지 않음. 단계 2 렌더 확인에 넣음
- [r1] claude-ide#3 distroless라 exec `preStop sleep`이 돌지 않고 10초가 조용히 사라짐 → 반영 — `lifecycle.preStop.sleep.seconds: 10`. kubelet이 실행한다는 점을 k8s 문서로 확인. 클러스터 버전 고정 때 지원 확인
- [r1] claude-ide#4 헬스체크 30초×3이라 새 파드가 60~90초 뒤에야 트래픽을 받아 HPA 효과가 단계 안에 안 드러남 → 반영 — 타깃 그룹 헬스체크 10초·2회("그대로 두는 것"의 예외로 명시). 실행마다 파드 2개에서 시작, 시작 파드 수 기록
- [r1] claude-ide#5 `infra/k8s` 생성·삭제 순서(CRD·웹훅·finalizer)·네임스페이스 라벨 누락 → 반영 — `kubernetes_namespace`(라벨), 사이트 차트·Application이 컨트롤러 릴리스를 `depends_on`, `full-destroy`가 terraform 전에 Application·TGB `kubectl delete --wait`·잔존 확인. 런북·리스크 표에 적음
- [r1] claude-ide#6 클러스터 SG는 egress 전체 허용이라 P4 egress 제한이 사라짐, ALB·RDS SG 참조도 바뀜 → 반영 — 전체 허용을 받아들이고 ADR 0007에 트레이드오프로 적음(노드 전용 SG는 컨트롤 플레인·노드 간·웹훅 규칙을 직접 관리해야 해 2대 구성에 과함). `app` SG 삭제와 `alb_to_app`·`data_from_app` 참조 변경을 명시
- [r1] claude-ide 제안: Image Updater v1.x는 `ImageUpdater` CR → 반영 — 차트 버전 고정, 고정 버전 방식으로 설정·롤백 런북. 리스크 표 문구도 버전 중립으로
- [r1] claude-ide 제안: Fluent Bit은 Pod Identity 지원 버전으로 고정 → 반영
- [r1] claude-ide 제안: CloudWatch Observability 애드온을 후보로 비교 → 기각 — 자체 로그 그룹·형식이라 지금 쿼리 유지 설계(`Merge_Log`)와 어긋나고 관측 단위 과금이 붙는다. 파드 CPU는 `kubectl top`·`get hpa` 30초 기록으로 정했고(구현 때 정하기로 미뤘던 것을 지금 확정), 0015 Prometheus로 정리
- [r1] claude-ide 제안: 오토스케일러 없으면 최대 3은 안 쓰임, 노드 2대 수용 확인 → 반영 — 2대 고정(최소·최대·희망 2), ArgoCD 불필요 컴포넌트 끔, 단계 2 계산·9-1 확인
- [r1] claude-ide 제안: t4g `cpu_credits = "unlimited"` 명시 → 반영(방식 변경) — T4g 기본이 `unlimited`라 시작 템플릿을 만들지 않고 9-1에서 확인. 결과 문서에 Fargate와 차이 기록
- [r1] claude-ide 제안: (b) 판정의 교란 요인 → 반영 — 판정 (b)에 풀·노드·HPA 동시 변경을 적음
- [r1] claude-ide 제안: 롤링 surge를 연결 수에 넣음 → 반영 — `maxSurge: 1`·`maxUnavailable: 0`에 풀 12→11로 (6+1)×11=77<80. 종료 중 파드 겹침(88)은 한도 112 아래·알람 5분 평균이라 허용
- [r1] claude-ide 제안: 멀티 아키텍처 이미지 → 반영 — `linux/arm64,linux/amd64`. AGENTS.md §5 "같은 이미지" 원칙, Dockerfile이 이미 크로스 컴파일
- [r1] claude-ide 제안: `infra/k8s`의 민감 변수 → 반영 — `sensitive`·`set_sensitive`, 단계 5·9-1에서 가려짐 확인
- [r1] claude-ide 제안: `ALL_RATES` 6400 → 반영 — 단계 7
- [r1] claude-ide 제안: `rotation-redeploy.tf` 통째 삭제 금지 → 반영 — Step 1 리소스만 삭제, `rotation_secret_arn`·`secret_events_*` 유지(파일 확인)
- [r1] claude-ide 제안: `outputs.tf`·`variables.tf`·GitHub Variables 정리 → 반영 — ECS 의존 표와 단계 3·4에 넣음
- [r1] claude-ide 제안: kubeconform CRD 스키마 → 반영 — CRDs-catalog 우선, 없으면 `-ignore-missing-schemas`
- [r1] claude-ide 제안: AGENTS.md §5 "배포 실패가 정상" 문구 → 반영 — 단계 6

**판정(r2):** 결함 6개 모두 리뷰어가 제시한 수정대로 반영했고 남은 쟁점이 없다. "블로커가 없으면 라운드를 더 열지 않는다"에 따라 `approved`.
