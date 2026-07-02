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

**통지가 안 오면:** ① 채널에 `@Amazon Q` 앱이 초대됐는지 ② **실제 구독 존재** 확인 — `slack_alerts_enabled=true`는 team/channel 변수가 설정됐다는 Terraform 계산값일 뿐이므로, `aws sns list-subscriptions-by-topic --topic-arn "$(terraform output -raw sns_alarms_topic_arn)"`(또는 SNS 콘솔)로 구독이 실제로 걸렸는지 본다 ③ 그 구독에 **raw message delivery가 켜져 있지 않은지**(꺼야 한다)를 본다.

### 알람 테스트 (실제 발화·통지 확인)

```bash
# 알람 하나를 임시로 ALARM으로 만들어 Slack 통지가 오는지 확인 → 원복
aws cloudwatch set-alarm-state --alarm-name linkpulse-prod-alb-target-5xx \
  --state-value ALARM --state-reason "manual test"
aws cloudwatch set-alarm-state --alarm-name linkpulse-prod-alb-target-5xx \
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
