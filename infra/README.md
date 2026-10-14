# linkpulse 인프라 (Terraform)

AWS(ap-northeast-2)에 **EKS**로 배포한다(plan 0014 — P1~P4는 ECS Fargate였다. 근거는 [ADR 0007](../docs/adr/0007-ecs-to-eks.md)). **모든 `apply`는 사람이 직접** 수행한다(인프라 변경은 사람이 승인하는 원칙). **Terraform 1.10+** 필요(S3 네이티브 lockfile 사용, DynamoDB 미사용). `aws`·`kubectl` CLI도 필요하다(kubectl은 클러스터 버전 ±1 마이너를 쓴다).

- `bootstrap/` — Terraform state용 S3 버킷 1개만 생성(state는 로컬).
- `prod/` — VPC·ALB·RDS·SQS·EKS(클러스터·노드·애드온)·IAM 등 AWS 리소스(S3 backend, 키 `prod/`).
- `k8s/` — 클러스터 안의 것: kube-prometheus-stack(Prometheus·Alertmanager·Grafana, plan 0015)·LB 컨트롤러·ArgoCD·Image Updater·Fluent Bit·사이트 값(ConfigMap·Secret)·TargetGroupBinding·ArgoCD Application(S3 backend, 키 `k8s/`). 차트 값 중 긴 것은 `k8s/values/*.tftpl`에 있다. helm·kubernetes provider는 클러스터가 있어야 설정되므로 `prod/` 다음에 따로 apply한다.
- 앱 자체는 Terraform이 아니라 **ArgoCD가 git의 차트(`deploy/helm/linkpulse`)로** 배포한다. 이미지는 CI가 GHCR에 올리고, Image Updater가 `main` 다이제스트를 따라간다.

## 배포 순서

1. **state 버킷 생성(bootstrap)**

   ```bash
   cd infra/bootstrap
   terraform init && terraform plan && terraform apply   # 출력된 tfstate_bucket 기록
   ```

2. **prod 초기화(backend 주입)**

   ```bash
   cd ../prod
   cp backend.hcl.example backend.hcl          # bucket 값을 bootstrap 출력으로 채움
   cp terraform.tfvars.example terraform.tfvars
   terraform init -backend-config=backend.hcl
   ```

3. **GHCR 패키지 공개(1회, GitHub 화면)** — CI(`deploy.yml`)가 main 머지마다 `ghcr.io/naerrow/linkpulse:{<sha>,main}`(arm64+amd64)을 올린다. 첫 푸시 뒤 GitHub → Packages → linkpulse → Package settings에서 **Public**으로 바꾼다. 그래야 클러스터가 pull 비밀 없이 받는다. CI에는 AWS 권한이 없다(GitHub repo Variables도 필요 없다).

4. **AWS 리소스 + EKS(prod)** — ACM DNS 검증을 위해 도메인이 Route53에 위임돼 있어야 한다. 약 15~20분.

   ```bash
   terraform plan && terraform apply
   aws eks update-kubeconfig --name "$(terraform output -raw eks_cluster_name)" --region ap-northeast-2
   ```

5. **클러스터 안(k8s)** — 관리자 토큰 해시는 이 스택의 tfvars에 둔다(`k8s/terraform.tfvars.example`).

   ```bash
   cd ../k8s
   cp terraform.tfvars.example terraform.tfvars   # admin_token_sha256 채움(선택)
   terraform init -backend-config=../prod/backend.hcl
   terraform plan && terraform apply
   ```

   순서는 Terraform이 지킨다: kube-prometheus-stack(CRD) → 컨트롤러(LBC·ArgoCD·Image Updater) → site(ConfigMap·Secret·TGB) → gitops(Application·ImageUpdater). 그 뒤 ArgoCD가 앱을 동기화한다.

6. **확인** — `kubectl -n linkpulse rollout status deploy/linkpulse`, `https://lpulse.live/healthz` 200, 단축/리다이렉트 동작, CloudWatch 로그(`/eks/linkpulse-prod/app`) 수신.

4~6을 한 번에 하는 것이 `scripts/full-apply-prod.sh`다(아래).

## 개발 비용 절감: prod 전체 내렸다가 다시 올리기

실사용자가 없고 개발 데이터·로그를 버려도 되는 기간에는 `infra/k8s`·`infra/prod` 스택을 통째로 지워 비용을 거의 0에 가깝게 낮춘다(EKS 컨트롤 플레인·노드·NAT는 켜져 있는 동안 시간 과금). 이 방식은 **운영 데이터 보존용이 아니다.** RDS 최종 스냅샷을 남기지 않고, CloudWatch Logs도 삭제한다.

남기는 것:

- `infra/bootstrap` state 버킷 — 별도 Terraform 스택이므로 건드리지 않는다.
- 도메인 등록과 기존 Route53 hosted zone — `prod`는 hosted zone을 생성하지 않고 조회만 한다.
- GHCR 이미지 — AWS가 아니다. 그래서 다시 켤 때 "이미지 부트스트랩 → CI 배포 대기" 단계가 없다.

삭제하는 것:

- 클러스터 안의 것(`infra/k8s`): Helm 릴리스(kube-prometheus-stack·LBC·ArgoCD·Image Updater·Fluent Bit·site·gitops), 네임스페이스. Prometheus 데이터(emptyDir, 보관 2일)도 함께 사라진다.
- EKS 클러스터·노드 그룹·애드온·Pod Identity 연결, ALB, NAT Gateway/EIP, VPC/Subnet/Route table/Security group
- RDS PostgreSQL 인스턴스와 자동 관리 Secrets Manager 시크릿
- SQS 클릭 큐·DLQ, CloudWatch Logs/Alarms, SNS, Chatbot Slack 설정
- prod 스택이 만든 IAM role/policy, ACM 인증서, Route53 record

plan 0014 전 계정에 남아 있을 수 있는 GitHub OIDC provider는 이제 어느 스택도 관리하지 않는다. 다른 프로젝트가 쓰지 않으면 IAM 콘솔에서 지운다.

### 끄기: full destroy

```bash
./scripts/full-destroy-prod.sh --drop-dev-db
```

스크립트 순서(finalizer 때문에 순서가 중요하다 — ADR 0007):

1. 클러스터가 있으면 ArgoCD Application과 TargetGroupBinding을 **각 컨트롤러가 살아 있을 때** `kubectl delete --wait`로 먼저 지우고, 남은 것이 없는지 확인한다. 이때부터 앱이 내려간다.
2. `infra/k8s` `terraform plan -destroy` → `destroy linkpulse k8s` 입력.
3. `infra/prod` RDS 삭제 보호 해제 plan → `prepare destroy` 입력 → `terraform plan -destroy` → `destroy linkpulse prod` 입력.

finalizer에 걸려 멈추면 [alarm-response.md](../docs/runbooks/alarm-response.md) "EKS 운영"을 본다.

계획만 보고 멈추려면:

```bash
./scripts/full-destroy-prod.sh --drop-dev-db --plan-only
```

### 켜기: full apply

```bash
./scripts/full-apply-prod.sh
```

재생성 순서:

0. GHCR `main` 이미지가 있고 공개인지 확인한다(없으면 Actions `deploy`를 main에서 돌리고 패키지를 공개로).
1. `infra/prod` plan → `apply linkpulse prod` 입력(약 15~20분).
2. kubeconfig를 받는다.
3. `infra/k8s` plan → `apply linkpulse k8s` 입력.
4. ArgoCD가 `deploy/linkpulse`를 만들고 롤아웃이 끝날 때까지 기다린다(Ready = ALB 타깃 healthy, readiness gate).
5. `https://lpulse.live/healthz`를 확인한다.

`--trigger-deploy`는 이제 필요 없다(이미지는 GHCR에 남아 있다). 넘겨도 무시한다.

주의:

- `terraform apply` 자체에는 비용이 붙지 않는다. 비용은 생성된 AWS 리소스가 존재하는 시간부터 발생한다.
- RDS 데이터가 필요한 시점이 오면 이 절차를 쓰면 안 된다. 그때는 최종 스냅샷/복원 절차를 별도 runbook으로 분리한다.
- 반복 삭제하는 개발 운용 방식이므로 `terraform.tfvars`에 비밀값을 넣지 않는다. 예외는 관리자 토큰 **해시**(`infra/k8s/terraform.tfvars`, 원문 아님)다.
- 측정 때 레이트리밋 예외 IP는 `infra/k8s` apply의 `-var rate_limit_exempt_ips=...`로 넣고 `kubectl -n linkpulse rollout restart deploy/linkpulse`(envFrom은 Secret 변경을 다시 읽지 않는다).

## 운영 안전 주의

- **state 버킷을 destroy하지 말 것.** `prevent_destroy=true`로 잠겨 있다. bootstrap state는 로컬이라 분실 시 `terraform import`로 복구한다.
- **RDS 철거**는 `deletion_protection=true`·`skip_final_snapshot=false`로 이중 보호된다. 정말 지우려면 두 변수를 풀어야 하며, 최종 스냅샷 이름(`linkpulse-prod-pg-final`)이 이미 있으면 충돌하므로 정리 후 진행한다.
- **비밀번호**는 RDS가 Secrets Manager에 생성·관리하고, 앱이 기동 때와 인증 실패 때 Secrets Manager에서 직접 읽는다(`DB_PASSWORD_SOURCE=aws`, 권한은 Pod Identity 파드 역할). 코드·state·tfvars·쿠버네티스 Secret에 평문으로 두지 않는다.
- `terraform plan` 출력은 PR/기록에 보관한다.

## CI/CD 경계 (plan 0014 — 자세히는 [ADR 0007](../docs/adr/0007-ecs-to-eks.md). ECS 시절 경계는 ADR 0001)

- 인프라는 Terraform(사람 `apply`). CI(GitHub Actions)는 테스트 → 이미지 빌드 → **GHCR 푸시만** 한다. CI에는 AWS·클러스터 권한이 없다(OIDC 배포 역할을 지웠다).
- 배포는 클러스터 안에서 일어난다: Image Updater가 GHCR `main` 다이제스트 변경을 보고 ArgoCD Application의 `image.tag` 파라미터를 바꾸고(git에 쓰지 않는다), ArgoCD가 롤링 배포한다.
- 그래서 **인프라가 꺼져 있어도 main 머지 후 CI는 성공한다.** 켜면 그때의 `main` 이미지로 뜬다.
- 롤백은 Image Updater를 멈추고 이미지를 고정한다(runbook R-1).

## 모니터링 및 알림 (P3 — 자세히는 `docs/adr/0002-alerting-design.md`)

장애를 자동 감지해 Slack으로 통지한다. 경로: **CloudWatch Alarm → SNS(`linkpulse-prod-alarms`) → AWS Chatbot(Amazon Q Developer in chat applications) → Slack**. 알람 정의는 `monitoring.tf`, Slack 배선은 `chatbot.tf`.

- **알람(초기값):** ALB 5개(target 5xx·ELB 5xx·unhealthy host·p95 지연·**healthy host 0=다운**), RDS 4개(CPU·여유 스토리지·여유 메모리·연결 수), 클릭 큐 2개(`sqs.tf`), 시크릿 이벤트 관찰 1개(`secret-events.tf`). 임계값은 실트래픽 보며 조정한다.
- ECS 알람 3개(CPU·Memory·RunningTaskCount)는 plan 0014에서 지웠다. Container Insights는 켜지 않는다.
- **클러스터 안 Prometheus 알림(plan 0015, [ADR 0008](../docs/adr/0008-in-cluster-prometheus.md))**: SLO 소진·클릭 파이프라인·관리자 401·ArgoCD 동기화와 kube-prometheus-stack 기본 규칙(CrashLoop·HPA 최대·TargetDown 등). 경로는 Alertmanager → SNS(`linkpulse-prod-prom-alerts`, SMS 구독 없음) → 같은 Chatbot 채널 → **Slack만**이다. 사람을 깨우는 SMS는 위 CloudWatch 알람과 canary(클러스터 밖)에만 있다. 대응은 `docs/runbooks/alarm-response.md` "Prometheus 알림".

### Slack 연동 절차 (콘솔 OAuth는 사람이 1회)

1. AWS 콘솔 **"Amazon Q Developer in chat applications"** → **Configure new client → Slack** → workspace **OAuth 승인**.
2. `slack_team_id`(workspace ID)와 `slack_channel_id`(채널 우클릭→링크 복사 끝의 `C...` 문자열)를 확보한다.
3. **통지받을 Slack 채널에서 `/invite @Amazon Q` 실행**(private 채널은 필수 — 안 하면 "Terraform은 됐는데 Slack에 안 옴").
4. `terraform.tfvars`에 두 값을 넣고(둘 다 필수 — 한쪽만이면 apply 전 validation 에러) `terraform apply`.
5. 확인: `terraform output slack_alerts_enabled` = `true`.

> Slack 준비 전이면 두 값을 비워 알람+SNS만 먼저 apply할 수 있다(통지는 나중에 값 넣고 재 apply). 이 상태는 **중간 마일스톤**이지 P3-1 완료가 아니다.

**통지가 안 오면:** ① 채널에 `@Amazon Q` 앱이 초대됐는지 ② **실제 구독 존재** 확인 — `slack_alerts_enabled=true`는 team/channel 변수가 설정됐다는 Terraform 계산값일 뿐이므로, `aws sns list-subscriptions-by-topic --topic-arn "$(terraform output -raw sns_alarms_topic_arn)"`(또는 SNS 콘솔)로 구독이 실제로 걸렸는지 본다 ③ 그 구독에 **raw message delivery가 켜져 있지 않은지**(꺼야 한다)를 본다. ④ **canary 알람(무트래픽 방어선, 아래)은 us-east-1 전용 토픽**이라 별도 확인 — `aws sns list-subscriptions-by-topic --topic-arn "$(terraform output -raw sns_canary_topic_arn)" --region us-east-1`. 이 토픽 구독이 없으면 canary 다운 카드만 조용히 누락된다(12개 알람은 정상인데 canary만 침묵).

### 배포 실패 알림 (plan 0014에서 지움)

ECS 배포 실패 → EventBridge → Slack 경로(ADR 0003, `eventbridge.tf`)는 ECS와 함께 지웠다. ArgoCD 동기화 실패 알림은 plan 0015의 Prometheus 규칙 `LinkpulseArgoCDAppUnhealthy`(15분, Slack)가 맡는다.
그 사이 배포 실패는 사용자에게 번지지 않는다 — 새 파드가 ALB에서 healthy가 되지 않으면 롤링이 멈추고 옛 파드가 남는다(readiness gate, surge 1·unavailable 0).
확인은 `kubectl -n argocd get application linkpulse`와 `kubectl -n linkpulse rollout status deploy/linkpulse`.

### 무트래픽 방어선 canary (P4 — 자세히는 `docs/adr/0004-notraffic-canary.md`)

실사용 트래픽이 0인 시간대(새벽 등)의 다운을 잡는 **무트래픽 방어선**이다(회고 A-5·A-10). Route53 헬스체크가 `lpulse.live/readyz`를 외부에서 상시(30초) 프로빙해, (a) 무트래픽 다운이어도 ALB가 503을 뱉어 `alb-elb-5xx`가 발화하게 하고, (b) `HealthCheckStatus`(1=정상/0=다운) 지표로 트래픽과 무관한 직접 신호를 준다. 리소스는 `synthetic-canary.tf`, us-east-1 provider 별칭은 `providers.tf`.

- **무엇을 프로빙하나 (P4(d)-2에서 `/healthz` → `/readyz`로 변경):** 이 프로젝트의 두 엔드포인트는 역할이 다르다. `/healthz`는 **liveness** — 프로세스가 살아 요청을 처리하는지만 보고 **DB를 조회하지 않는다.** `/readyz`는 **readiness** — DB 핑까지 하고 실패하면 503이다. 2026-08-16 RDS 비밀번호 로테이션 장애 때 canary와 ALB 타깃그룹이 **둘 다 `/healthz`** 를 보고 있어, 앱이 DB 신규 연결에 전면 실패하는 66시간 내내 양쪽 모두 200을 받고 침묵했다. canary를 `/readyz`로 옮겨 이 공백을 닫았다. **liveness 백스톱은 잃지 않는다** — 프로세스가 죽으면 `/readyz`도 응답하지 못하므로 `/readyz`는 `/healthz`의 상위집합이다. **이 변경은 canary에만 적용했고 ALB 타깃그룹 헬스체크는 `/healthz` 그대로다**(`alb.tf`) — 타깃그룹까지 바꾸면 DB 장애 시 두 타깃이 모두 unhealthy가 돼 ECS 태스크 교체가 도는 별개의 트레이드오프가 생기므로 이 plan의 범위에서 제외했다. **EKS(plan 0014)에서도 같은 이유로 파드 readiness를 `/healthz`로 뒀다** — TargetGroupBinding은 NotReady 파드를 타깃에서 빼므로 `/readyz`면 DB 장애 때 파드가 전부 빠진다(ADR 0007). 그 결과 **DB 장애에서는 타깃이 healthy를 유지해 `alb-elb-5xx`가 울리지 않고 `alb-target-5xx`가 1순위**가 된다(runbook §13).
- **응답 시간 제약:** Route53은 **연결 후 2초 안에**(= TLS 핸드셰이크 포함) 2xx/3xx를 받아야 healthy로 본다. `/readyz`의 DB 핑 상한도 2초면 앱이 503을 확정하는 순간과 Route53이 포기하는 순간이 겹쳐 503이 전달되지 않고 로그에도 실제 DB 에러 대신 취소·deadline 계열 오류(`context canceled` 등)가 남을 수 있으므로 **1초**로 낮췄다(`app/internal/httpapi/health.go` `readinessTimeout`). **오탐 방지가 아니라 판정 일치가 목적이다** — 정상 DB 핑은 수 ms라 상한 1초/2초의 결과가 같다. 같은 이유로 앱 배포가 canary 경로 변경보다 **먼저** 나가야 한다.

- **크로스리전:** Route53 헬스체크 지표는 **us-east-1에만 발행**되므로 `canary_down` 알람·전용 SNS 토픽도 us-east-1에 둔다. 이 토픽을 기존 Chatbot config에 크로스리전으로 **추가** 바인딩해 같은 Slack 채널로 통지한다(ADR 0002 패턴). 그래서 canary 관련 CLI 조회는 전부 `--region us-east-1`.
- **경로:** **Route53 health check(`HealthCheckStatus<1` 3분) → CloudWatch 알람(us-east-1) → SNS(`linkpulse-prod-canary-alarms`, us-east-1) → Chatbot → Slack**. 대응 절차는 [`runbook §13`](../docs/runbooks/alarm-response.md), 예정된 비밀번호 로테이션 대응은 [`secret-rotation-bridge.md`](../docs/runbooks/secret-rotation-bridge.md).
- **최초 apply 오탐 레이스:** 지표 최초 발행 전 공백이 `breaching`으로 잡혀 정상인데 down 카드가 1회 올 수 있다. **첫 canary ALARM은 `list-metrics`/헬스체크 상태로 진위 확인 전까지 장애로 단정하지 않는다**(2단계 apply로 회피 가능 — ADR 0004).
- **비용:** 헬스체크 HTTPS ≈ $1.50/월 + 알람 ~$0.10/월 ≈ **$1.6/월**(string-match·10초 주기 미사용으로 최소화).
- **종단 검증 ① 전면 다운 경로 (P4(c), 완료):** us-east-1 지표 발행 실측(`list-metrics --region us-east-1 --namespace AWS/Route53`) → 비파괴 preflight(`set-alarm-state --region us-east-1`로 Slack canary 카드 수신 확인) → 수동 curl 없이(무트래픽) chaos 이미지로 desired=0 다운 → **`canary_down` 수분 내 Slack 다운 카드** + `alb-elb-5xx` MTTD 기록 → 복구 시 OK 카드. 절차·chaos 자산은 `load/chaos/README.md`·`docs/postmortems/2026-07-06-gameday-01.md` 재사용. **이 드릴은 프로세스가 사라지는 liveness 경로만 재검증한다.**
- **종단 검증 ② DB 장애 경로 (P4(d)-2, 미검증):** `/readyz` 전환의 핵심 분기는 **DB는 끊겼는데 `/healthz`는 200인 상태**이고, `desired=0` 드릴은 이 분기를 전혀 밟지 않는다. 의도적 DB 장애로 `canary_down`·`alb-target-5xx` 발화를 실측하는 드릴은 **plan 0010**이 맡는다. 그때까지 이 경로는 **넣었으나 실증되지 않은 상태**다. 2026-08-23 회전 창에는 [`secret-rotation-bridge.md`](../docs/runbooks/secret-rotation-bridge.md)를 쓰지만, **그 브릿지도 안전을 담보하지 못한다** — 회전 창이 약 24시간이라 사람의 폴링이 야간을 덮지 못하고, 다운 시간의 **상한만 줄인다.**

### 알람 테스트 (실제 발화·통지 확인)

```bash
# 알람 하나를 임시로 ALARM으로 만들어 Slack 통지가 오는지 확인 → 원복
aws cloudwatch set-alarm-state --alarm-name linkpulse-prod-alb-target-5xx \
  --state-value ALARM --state-reason "manual test"
aws cloudwatch set-alarm-state --alarm-name linkpulse-prod-alb-target-5xx \
  --state-value OK --state-reason "reset"

# canary 알람(§13, 무트래픽 방어선)은 us-east-1 전용 토픽 → --region us-east-1 필수(--state-reason도 CLI 필수).
# 주의: 이 테스트 상태는 다음 지표 평가에서 자동 복귀하며, 실제 상태가 OK면 ok_actions가 한 번 더 발화해
# 여분의 OK 카드가 올 수 있다(정상).
aws cloudwatch set-alarm-state --alarm-name linkpulse-prod-canary-down --region us-east-1 \
  --state-value ALARM --state-reason "manual test"
aws cloudwatch set-alarm-state --alarm-name linkpulse-prod-canary-down --region us-east-1 \
  --state-value OK --state-reason "reset"
```

### CloudWatch Logs Insights 조회 (로그 그룹 `/eks/linkpulse-prod/app`)

앱은 JSON 구조화 로그(`method`/`path`/`status`/`duration_ms`)를 낸다. Fluent Bit가 앱 JSON을 최상위 필드로 풀고(`Merge_Log`) `kubernetes.pod_name` 등을 더해 보낸다. `task_id`는 파드 이름이다.
ECS 시절 그룹(`/ecs/linkpulse-prod-app`)은 destroy와 함께 사라졌다.

```
# 에러(5xx)만
fields @timestamp, status, method, path, duration_ms | filter status >= 500 | sort @timestamp desc | limit 50

# 느린 요청(1초 초과)
fields @timestamp, path, status, duration_ms | filter duration_ms > 1000 | sort duration_ms desc | limit 50

# 특정 status (예: 404)
fields @timestamp, method, path | filter status = 404 | sort @timestamp desc | limit 50

# panic 복구 로그
fields @timestamp, error, path | filter msg = "panic recovered" | sort @timestamp desc
```
