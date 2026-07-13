# linkpulse 인프라 (P1, Terraform)

AWS(ap-northeast-2)에 ECS Fargate로 배포한다. **모든 `apply`는 사람이 직접** 수행한다(인프라 변경은 사람이 승인하는 원칙). **Terraform 1.10+** 필요(S3 네이티브 lockfile 사용, DynamoDB 미사용).

- `bootstrap/` — Terraform state용 S3 버킷 1개만 생성(state는 로컬).
- `prod/` — VPC·ALB·ECS·RDS·IAM 등 본 인프라(S3 backend).

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

3. **인프라 생성(태스크 0개)** — 이미지가 아직 없으므로 `-var service_desired_count=0`으로 VPC/RDS/ALB/ECR/IAM(CI 배포 role 포함)을 먼저 만든다. (`service_desired_count` 기본값은 운영용 2이므로 최초엔 0을 명시한다.) ACM DNS 검증을 위해 도메인이 Route53에 위임돼 있어야 한다.

   ```bash
   terraform plan -var service_desired_count=0 && terraform apply -var service_desired_count=0
   ```

4. **GitHub repo Variables 등록** — 이후 이미지 빌드·배포는 CI가 맡으므로(P2), `terraform output` 값을 GitHub repo Variables에 넣는다.

   ```bash
   terraform output -raw github_actions_role_arn     # AWS_DEPLOY_ROLE_ARN
   terraform output -raw ecr_repository_url           # ECR_REPOSITORY_URL
   terraform output -raw ecr_repository_name          # ECR_REPOSITORY_NAME
   terraform output -raw ecs_cluster_name             # ECS_CLUSTER_NAME
   terraform output -raw ecs_service_name             # ECS_SERVICE_NAME
   terraform output -raw ecs_task_definition_family   # ECS_TASK_DEFINITION_FAMILY
   ```

5. **첫 배포 + 스케일** — `service.task_definition`은 `ignore_changes`라 **Terraform이 아니라 CI가** task definition을 실제 이미지로 갱신한다(`docs/adr/0001`).

   1. CI 배포를 1회 트리거한다(main 푸시 또는 `workflow_dispatch`). CI가 이미지를 빌드→ECR push→task definition을 실제 이미지(git sha)로 갱신한다. 이 시점 `desired_count`는 0이라 태스크는 아직 뜨지 않는다.
   2. 이어서 `terraform apply`(기본 `desired_count=2`)로 태스크 2개로 스케일한다. `task_definition`은 ignore되어 CI가 설정한 실제 이미지가 유지된다.

   ```bash
   # (1) CI 배포(Actions)가 성공한 뒤:
   terraform apply
   ```

6. **확인** — `https://lpulse.live/healthz` 200, 단축/리다이렉트 동작, CloudWatch 로그 수신.

## 개발 비용 절감: prod 전체 내렸다가 다시 올리기

실사용자가 없고 개발 데이터·로그·이미지를 버려도 되는 기간에는 `infra/prod` 스택을 통째로 지워 비용을 거의 0에 가깝게 낮춘다. 이 방식은 **운영 데이터 보존용이 아니다.** RDS 최종 스냅샷을 남기지 않고, CloudWatch Logs와 ECR 이미지도 삭제한다.

남기는 것:

- `infra/bootstrap` state 버킷 — 별도 Terraform 스택이므로 건드리지 않는다.
- 도메인 등록과 기존 Route53 hosted zone — `prod`는 hosted zone을 생성하지 않고 조회만 한다.
- GitHub repo Variables — GitHub 쪽 설정이므로 Terraform destroy 대상이 아니다.

삭제하는 것:

- ECS, ALB, NAT Gateway/EIP, VPC/Subnet/Route table/Security group
- RDS PostgreSQL 인스턴스와 자동 관리 Secrets Manager 시크릿
- CloudWatch Logs/Alarms, SNS, Chatbot Slack 설정
- ECR repository와 이미지
- prod 스택이 만든 IAM role/policy, GitHub OIDC provider, ACM 인증서, Route53 record

다른 프로젝트도 같은 AWS 계정의 GitHub OIDC provider를 공유한다면, full destroy 전에 `existing_github_oidc_provider_arn` 방식으로 소유권을 분리해야 한다. 지금 프로젝트만 쓰는 개발 계정이면 그대로 지워도 된다.

### 끄기: full destroy

```bash
./scripts/full-destroy-prod.sh --drop-dev-db
```

스크립트는 먼저 `terraform plan`으로 RDS 삭제 보호 해제와 ECR force delete 설정만 보여준 뒤, 확인 문구를 요구한다. 이후 다시 `terraform plan -destroy`를 보여주고, `destroy linkpulse prod`를 직접 입력해야 실제 삭제를 진행한다.

계획만 보고 멈추려면:

```bash
./scripts/full-destroy-prod.sh --drop-dev-db --plan-only
```

### 켜기: full apply

```bash
./scripts/full-apply-prod.sh
```

재생성 순서:

1. `service_desired_count=0`, `image_tag=bootstrap`으로 인프라를 먼저 만든다.
2. GitHub Actions `deploy` workflow를 `main` 브랜치로 실행해 실제 이미지를 ECR에 push하고 ECS service의 task definition을 갱신한다.
3. workflow 성공 후 스크립트에서 Enter를 눌러 정상 desired count로 스케일한다.
4. 스크립트가 `https://lpulse.live/healthz`를 확인한다.

GitHub CLI가 로그인되어 있으면 workflow 트리거까지 같이 할 수 있다.

```bash
./scripts/full-apply-prod.sh --trigger-deploy
```

인프라만 만들고 수동으로 배포·스케일하려면:

```bash
./scripts/full-apply-prod.sh --no-scale
```

주의:

- `terraform apply` 자체에는 비용이 붙지 않는다. 비용은 생성된 AWS 리소스가 존재하는 시간부터 발생한다.
- RDS 데이터가 필요한 시점이 오면 이 절차를 쓰면 안 된다. 그때는 최종 스냅샷/복원 절차를 별도 runbook으로 분리한다.
- `infra/prod` 전체를 반복 삭제하는 개발 운용 방식이므로, `terraform.tfvars`에 비밀값을 넣지 않는다.

## 운영 안전 주의

- **state 버킷을 destroy하지 말 것.** `prevent_destroy=true`로 잠겨 있다. bootstrap state는 로컬이라 분실 시 `terraform import`로 복구한다.
- **RDS 철거**는 `deletion_protection=true`·`skip_final_snapshot=false`로 이중 보호된다. 정말 지우려면 두 변수를 풀어야 하며, 최종 스냅샷 이름(`linkpulse-prod-pg-final`)이 이미 있으면 충돌하므로 정리 후 진행한다.
- **비밀번호**는 RDS가 Secrets Manager에 생성·관리하고, ECS가 `DB_PASSWORD`로만 주입한다. 코드·state·tfvars에 평문으로 두지 않는다.
- `terraform plan` 출력은 PR/기록에 보관한다.

## P2 CI/CD 경계 (요약 — 자세히는 `docs/adr/0001-cicd-terraform-ci-boundary.md`)

- 인프라는 Terraform(사람 `apply`), 앱 이미지 배포는 GitHub Actions(OIDC)가 `register-task-definition`+`update-service`로 수행한다. **CI는 `terraform apply`를 실행하지 않는다.**
- `aws_ecs_service.app`는 `ignore_changes=[task_definition]`이라, **Terraform으로 task definition을 바꾸면 다음 CI 배포(main 푸시/`workflow_dispatch`)가 1회 돌아야 라이브에 반영된다.**
- 운영 가동값은 `service_desired_count=2`. **최초 부트스트랩(이미지 없음) 때만 `-var service_desired_count=0`** 으로 인프라를 먼저 만든다.
- **apply 전**: 로컬 `terraform.tfvars`가 prod를 0/잘못된 태그로 떨구지 않는지 확인하고, `image_tag`를 현재 라이브 태그(`aws ecs describe-services`→`describe-task-definition`)에 맞춘다. OIDC 공급자가 계정에 이미 있으면 `existing_github_oidc_provider_arn`을 지정한다.
- 배포 후 `terraform output`의 `github_actions_role_arn`/`ecr_repository_url`/`ecr_repository_name`/`ecs_cluster_name`/`ecs_service_name`/`ecs_task_definition_family`를 GitHub repo Variables에 등록한다.

## 모니터링 및 알림 (P3 — 자세히는 `docs/adr/0002-alerting-design.md`)

장애를 자동 감지해 Slack으로 통지한다. 경로: **CloudWatch Alarm → SNS(`linkpulse-prod-alarms`) → AWS Chatbot(Amazon Q Developer in chat applications) → Slack**. 알람 정의는 `monitoring.tf`, Slack 배선은 `chatbot.tf`.

- **알람 12개(초기값):** ALB(target 5xx·ELB 5xx·unhealthy host·p95 지연·**healthy host 0=다운**), ECS(CPU·Memory·RunningTaskCount<desired), RDS(CPU·여유 스토리지·여유 메모리·연결 수). 임계값은 실트래픽 보며 조정한다.
- **Container Insights**는 `enable_container_insights=true`(default)로 켠다. RunningTaskCount 등 태스크 레벨 메트릭이 나온다(활성 직후 수 분은 INSUFFICIENT_DATA가 정상).

### Slack 연동 절차 (콘솔 OAuth는 사람이 1회)

1. AWS 콘솔 **"Amazon Q Developer in chat applications"** → **Configure new client → Slack** → workspace **OAuth 승인**.
2. `slack_team_id`(workspace ID)와 `slack_channel_id`(채널 우클릭→링크 복사 끝의 `C...` 문자열)를 확보한다.
3. **통지받을 Slack 채널에서 `/invite @Amazon Q` 실행**(private 채널은 필수 — 안 하면 "Terraform은 됐는데 Slack에 안 옴").
4. `terraform.tfvars`에 두 값을 넣고(둘 다 필수 — 한쪽만이면 apply 전 validation 에러) `terraform apply`.
5. 확인: `terraform output slack_alerts_enabled` = `true`.

> Slack 준비 전이면 두 값을 비워 알람+SNS만 먼저 apply할 수 있다(통지는 나중에 값 넣고 재 apply). 이 상태는 **중간 마일스톤**이지 P3-1 완료가 아니다.

**통지가 안 오면:** ① 채널에 `@Amazon Q` 앱이 초대됐는지 ② **실제 구독 존재** 확인 — `slack_alerts_enabled=true`는 team/channel 변수가 설정됐다는 Terraform 계산값일 뿐이므로, `aws sns list-subscriptions-by-topic --topic-arn "$(terraform output -raw sns_alarms_topic_arn)"`(또는 SNS 콘솔)로 구독이 실제로 걸렸는지 본다 ③ 그 구독에 **raw message delivery가 켜져 있지 않은지**(꺼야 한다)를 본다. ④ **canary 알람(무트래픽 방어선, 아래)은 us-east-1 전용 토픽**이라 별도 확인 — `aws sns list-subscriptions-by-topic --topic-arn "$(terraform output -raw sns_canary_topic_arn)" --region us-east-1`. 이 토픽 구독이 없으면 canary 다운 카드만 조용히 누락된다(12개 알람은 정상인데 canary만 침묵).

### 배포 실패 알림 (P4 — 자세히는 `docs/adr/0003-deploy-failure-alerts.md`)

ECS 롤링 배포가 실패해 서킷브레이커가 롤백하면 Slack으로 자동 통지한다. 경로: **EventBridge(`ECS Deployment State Change`/`SERVICE_DEPLOYMENT_FAILED`) → SNS(`linkpulse-prod-alarms`, 재사용) → Chatbot → Slack**. 규칙·타깃·실행 role은 `eventbridge.tf`.

- **커버리지 갭:** 이 경로는 **ECS단 배포 실패만** 잡는다. GitHub Actions단 실패(빌드/테스트 실패, **러너 미획득/플랫폼 장애**)는 ECS까지 도달하지 않아 못 잡는다 — `job not acquired`는 githubstatus.com부터 본다(`docs/postmortems/2026-07-09-deploy-runner-acquisition.md`).
- **필터 사전 검증(apply 전):** `docs/plans/0005-p4-deploy-failure-alerts/fixtures`의 `aws events test-event-pattern`으로 event_pattern이 실패 이벤트를 잡는지 확인한다(`{ "Result": true }` 기대).
- **종단 검증(apply 후):** `slack_alerts_enabled=true` 확인 → 의도적 나쁜 이미지 배포로 서킷브레이커 롤백 유발 → **Slack 배포 실패 카드 수신**(서비스 ARN·이벤트명·`reason` 정상 렌더) → 서킷브레이커가 직전 정상 taskdef로 자동 롤백하므로 재배포 없이 복구(원복 확인만). 나쁜 이미지·주입 절차는 기존 GameDay 자료 재사용: `load/chaos/README.md`(chaos 이미지, `/healthz` 404), `docs/postmortems/2026-07-06-gameday-01.md`(known-good taskdef 기준선·진행 중 run 부재·healthz 폴링 preflight).
- **`reason` escaping 잔여 리스크(수용):** `reason`에 개행이 들어가면 카드가 조용히 드롭될 수 있으나, ECS 통제 단일 줄 문자열이라 수용한다(근거·검증법은 ADR 0003).

### 무트래픽 방어선 canary (P4 — 자세히는 `docs/adr/0004-notraffic-canary.md`)

실사용 트래픽이 0인 시간대(새벽 등)의 다운을 잡는 **무트래픽 방어선**이다(회고 A-5·A-10). Route53 헬스체크가 `lpulse.live/healthz`를 외부에서 상시(30초) 프로빙해, (a) 무트래픽 다운이어도 ALB가 503을 뱉어 `alb-elb-5xx`가 발화하게 하고, (b) `HealthCheckStatus`(1=정상/0=다운) 지표로 트래픽과 무관한 직접 liveness 신호를 준다. 리소스는 `synthetic-canary.tf`, us-east-1 provider 별칭은 `providers.tf`.

- **크로스리전:** Route53 헬스체크 지표는 **us-east-1에만 발행**되므로 `canary_down` 알람·전용 SNS 토픽도 us-east-1에 둔다. 이 토픽을 기존 Chatbot config에 크로스리전으로 **추가** 바인딩해 같은 Slack 채널로 통지한다(ADR 0002 패턴). 그래서 canary 관련 CLI 조회는 전부 `--region us-east-1`.
- **경로:** **Route53 health check(`HealthCheckStatus<1` 3분) → CloudWatch 알람(us-east-1) → SNS(`linkpulse-prod-canary-alarms`, us-east-1) → Chatbot → Slack**. 대응 절차는 [`runbook §13`](../docs/runbooks/alarm-response.md).
- **최초 apply 오탐 레이스:** 지표 최초 발행 전 공백이 `breaching`으로 잡혀 정상인데 down 카드가 1회 올 수 있다. **첫 canary ALARM은 `list-metrics`/헬스체크 상태로 진위 확인 전까지 장애로 단정하지 않는다**(2단계 apply로 회피 가능 — ADR 0004).
- **비용:** 헬스체크 HTTPS ≈ $1.50/월 + 알람 ~$0.10/월 ≈ **$1.6/월**(string-match·10초 주기 미사용으로 최소화).
- **종단 검증(apply 후):** us-east-1 지표 발행 실측(`list-metrics --region us-east-1 --namespace AWS/Route53`) → 비파괴 preflight(`set-alarm-state --region us-east-1`로 Slack canary 카드 수신 확인) → 수동 curl 없이(무트래픽) chaos 이미지로 desired=0 다운 → **`canary_down` 수분 내 Slack 다운 카드** + `alb-elb-5xx` MTTD 기록 → 복구 시 OK 카드. 절차·chaos 자산은 `load/chaos/README.md`·`docs/postmortems/2026-07-06-gameday-01.md` 재사용.

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

### CloudWatch Logs Insights 조회 (로그 그룹 `/ecs/linkpulse-prod-app`)

앱은 JSON 구조화 로그(`method`/`path`/`status`/`duration_ms`)를 낸다.

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
