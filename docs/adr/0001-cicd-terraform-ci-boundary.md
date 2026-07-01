# ADR 0001 — Terraform과 CI의 배포 경계

- 상태: 채택 (2026-06-30)
- 관련: AGENTS.md 가드레일 #1(인프라 변경은 사람 승인), Phase P2(CI/CD)

## 맥락
P2에서 GitHub Actions로 자동 배포를 도입한다. 그런데 가드레일 #1은 "`terraform apply`·리소스 생성/변경은 사람이 승인"을 요구한다 — "자동 배포"와 "사람 승인 인프라"가 충돌하는 것처럼 보인다.

핵심 구분: **앱 이미지 교체는 인프라 변경이 아니라 앱 배포다.** VPC/RDS/IAM/ALB를 바꾸는 게 아니라, 이미 만들어진 ECS 서비스가 실행할 이미지(task definition revision)만 바꾼다.

## 결정
1. **Terraform = 인프라.** VPC/ALB/RDS/IAM/ECR/ECS 클러스터·서비스의 생성·변경은 Terraform이 소유하고 **사람이 `plan`→`apply`** 한다. CI는 `terraform apply`를 실행하지 않는다.
2. **CI = 앱 배포.** GitHub Actions가 OIDC로 assume한 배포 role로 `aws ecs register-task-definition`(새 이미지) + `aws ecs update-service`(롤링)만 수행한다.
3. **경계 장치**: `aws_ecs_service.app`에 `lifecycle { ignore_changes = [task_definition] }`. CI가 서비스를 새 revision으로 바꿔도 Terraform이 되돌리지 않는다.
4. **IAM 최소권한**: 배포 role은 `ecs:RegisterTaskDefinition`을 `task-definition/linkpulse-prod-app:*` family ARN으로, `ecs:UpdateService`/`DescribeServices`를 서비스 ARN으로, `iam:PassRole`을 execution/task 2개 role로 한정한다. (`ecs:DescribeTaskDefinition`·`ecr:GetAuthorizationToken`은 AWS가 리소스 레벨 권한을 지원하지 않아 `*`)
   - 근거: AWS Service Authorization Reference의 `RegisterTaskDefinition` 항목은 `task-definition` 리소스 타입을 지원한다(family ARN으로 제한 가능). **만약 첫 CI 배포에서 `RegisterTaskDefinition` AccessDenied가 나면** 이 statement의 resource를 `*`로 완화하면 된다 — register는 "정의만 만드는" 권한이라 `*`라도 실질 배포 통제는 `UpdateService`(service ARN)+`PassRole`(2 role)이 쥔다.

## 결과 / 트레이드오프 (운영자가 알아야 할 함정)
- **Terraform에서 task definition을 바꾸면(예: DB env 추가) 즉시 라이브에 반영되지 않는다.** `ignore_changes` 때문에 새 revision은 등록되지만 running 서비스는 옛 revision을 계속 쓴다. **반드시 CI 배포를 1회 트리거**(main 푸시 또는 `workflow_dispatch`)해야 라이브에 적용된다. CI는 `describe-task-definition`으로 family의 **최신 active revision**(= Terraform이 방금 등록한 것)을 base로 이미지만 교체하므로, 이 순서면 env 변경이 보존된다.
- **이미지 태그**: CI는 git sha 불변 태그를 쓴다. Terraform의 `image_tag` 변수는 **baseline/초기 task definition용**이다. `service.task_definition`이 `ignore_changes`라 **`terraform apply -var image_tag=<sha>`로는 running 서비스가 옮겨지지 않는다**(새 revision만 등록). 따라서 평상시 배포·비상 배포·롤백은 모두 **CI(main push 또는 `workflow_dispatch`)** 또는 직접 **`aws ecs update-service`** 로 한다. 평상시 Terraform이 인식하는 이미지와 라이브가 다를 수 있으나 의도된 것이다.
- **롤백**: `workflow_dispatch`에 과거 sha를 입력해 재배포한다. 단 **ECR lifecycle 보관(최근 30개) 범위 내** 태그만 가능하다.
- **arm64 빌드**: 배포 워크플로는 `ubuntu-24.04-arm` 러너 단일 경로로 구현한다(public preview). 러너 장애/제한 시 x86 러너 + `docker/setup-qemu-action` + `docker buildx build --platform linux/arm64`로 전환한다(이 fallback은 문서 절차로만 두고 평상시 워크플로엔 넣지 않는다).
