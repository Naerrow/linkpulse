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

3. **인프라 생성(태스크 0개)** — 이미지가 아직 없으므로 `service_desired_count=0`(기본값)으로 VPC/RDS/ALB/ECR을 먼저 만든다. ACM DNS 검증을 위해 도메인이 Route53에 위임돼 있어야 한다.

   ```bash
   terraform plan && terraform apply
   ```

4. **이미지 빌드 → ECR push** — 태스크 아키텍처(`task_cpu_architecture`, 기본 ARM64)와 빌드 플랫폼을 맞춘다.

   ```bash
   ECR=$(terraform output -raw ecr_repository_url)
   aws ecr get-login-password --region ap-northeast-2 | docker login --username AWS --password-stdin "${ECR%/*}"
   docker buildx build --platform linux/arm64 -t "$ECR:v1" ../../app --push
   ```

5. **서비스 기동(2 태스크)**

   ```bash
   terraform apply -var image_tag=v1 -var service_desired_count=2
   ```

6. **확인** — `https://lpulse.live/healthz` 200, 단축/리다이렉트 동작, CloudWatch 로그 수신.

## 운영 안전 주의

- **state 버킷을 destroy하지 말 것.** `prevent_destroy=true`로 잠겨 있다. bootstrap state는 로컬이라 분실 시 `terraform import`로 복구한다.
- **RDS 철거**는 `deletion_protection=true`·`skip_final_snapshot=false`로 이중 보호된다. 정말 지우려면 두 변수를 풀어야 하며, 최종 스냅샷 이름(`linkpulse-prod-pg-final`)이 이미 있으면 충돌하므로 정리 후 진행한다.
- **비밀번호**는 RDS가 Secrets Manager에 생성·관리하고, ECS가 `DB_PASSWORD`로만 주입한다. 코드·state·tfvars에 평문으로 두지 않는다.
- `terraform plan` 출력은 PR/기록에 보관한다.
