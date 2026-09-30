# CI/CD와 변경 책임

[프로젝트로 돌아가기](../README.md)

## PR 검증

[ci.yml](../.github/workflows/ci.yml)은 PR 및 `workflow_call`을 지원합니다. PR에서는 다음을 확인합니다.

- Go 형식 검사, `go vet`, `make test`
- Python 3.13 환경의 로테이션 Lambda 단위 테스트
- 배포와 같은 ARM64 Docker 크로스빌드 경로

## main 배포

[deploy.yml](../.github/workflows/deploy.yml)은 `main` push 또는 수동 실행 시 동작합니다. 현재 파일은 reusable CI를 직접 호출하지 않고, 단일 배포 job 안에 Go 검증을 정의합니다. 따라서 PR CI의 Lambda 테스트·Docker smoke test와 배포 job의 검증 범위는 동일하지 않습니다.

1. 신규 이미지 배포라면 Go 형식·정적 검사·단위 테스트를 수행합니다.
2. OIDC로 AWS 배포 역할을 사용합니다.
3. 커밋 SHA 태그가 ECR에 있으면 이미지를 재사용하고, 없으면 x86 GA 러너에서 ARM64 이미지를 빌드·푸시합니다.
4. 최신 active task definition을 가져와 이미지 참조를 바꾸고 새 revision을 등록합니다.
5. ECS 서비스의 안정화를 기다립니다. 동시 배포는 직렬화합니다.

Terraform은 ECS deployment circuit breaker의 자동 롤백을 선언합니다. 이것과 사람이 기존 이미지를 선택하는 수동 롤백은 별도 경로입니다.

## 롤백

수동 실행의 `image_tag`에 기존 ECR 태그를 지정하면 새 코드 검증·빌드를 건너뛰고 해당 이미지를 재배포합니다. 태그가 존재하지 않으면 실패합니다. [ADR 0001](adr/0001-cicd-terraform-ci-boundary.md)에 기재된 ECR 보관 범위를 넘긴 이미지는 사용할 수 없습니다.

## Terraform과 배포의 경계

Terraform은 VPC·ALB·RDS·IAM·ECS 등 인프라를 관리하고, 앱 이미지 교체는 GitHub Actions가 수행합니다. `terraform apply`는 CI에서 실행하지 않습니다.

ECS 서비스의 `task_definition`에는 `ignore_changes`가 설정되어 있습니다. Terraform으로 환경 변수 등 task definition을 바꿔도 running 서비스에 즉시 반영되는 것은 아니므로, 후속 앱 배포가 필요합니다. 이 동작과 초기 구성 순서는 [인프라 안내](../infra/README.md)와 [ADR](adr/0001-cicd-terraform-ci-boundary.md)을 확인하세요.

이번 포트폴리오 문서 작업에서는 AWS 배포나 Terraform apply를 수행하지 않았습니다.
