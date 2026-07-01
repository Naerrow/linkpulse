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
