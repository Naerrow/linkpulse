# 아키텍처와 설계 선택

[프로젝트로 돌아가기](../README.md) · [배포 흐름](deployment.md)

## 요청과 데이터 흐름

1. Route53 DNS와 ACM 인증서를 사용하는 ALB가 HTTPS 요청을 받습니다.
2. ALB는 프라이빗 app 서브넷의 ECS Fargate 태스크로 전달합니다.
3. Go `net/http` API가 URL을 검증하고 PostgreSQL에 단축 코드·원본 URL·클릭 수를 저장합니다.
4. `GET /{code}`는 302로 리다이렉트하고 클릭 수를 증가시킵니다. 클릭 증가가 실패하면 오류를 기록하되 리다이렉트를 계속합니다. 따라서 클릭 수는 완전한 과금용 이벤트 원장이 아닙니다.
5. `GET /api/links/{code}`는 저장된 클릭 수를 조회합니다.

[라우터](../app/internal/httpapi/router.go) · [서비스 로직](../app/internal/links/service.go) · [DB 스키마](../app/internal/db/schema.sql)

## 네트워크와 데이터 보호

- public/app/data 계층을 두 AZ에 배치합니다. data 라우팅에는 인터넷 경로가 없습니다.
- app은 NAT Gateway를 통해 외부 서비스에 접근하며 공인 IP를 부여하지 않습니다.
- RDS 비밀번호는 Secrets Manager 참조로 ECS 태스크에 주입합니다. 이 문서는 실제 비밀번호·계정 ID·내부 주소를 포함하지 않습니다.
- 애플리케이션은 기동 시 받은 자격증명을 사용합니다. 회전된 비밀번호가 기존 프로세스에 자동으로 반영되는 구조로 가정하면 안 됩니다.

[네트워크](../infra/prod/network.tf) · [보안 그룹](../infra/prod/security_groups.tf) · [태스크 구성](../infra/prod/ecs.tf) · [DB 연결](../app/internal/db/db.go)

## 비용과 가용성의 선택

| 선택 | 의미 / 제약 |
| --- | --- |
| 단일 NAT Gateway | 비용을 줄이지만 NAT가 위치한 AZ의 장애가 app 아웃바운드에 영향을 줄 수 있음 |
| Single-AZ RDS | 자동 Multi-AZ failover를 제공하는 구성으로 설명하지 않음 |
| RDS 백업 정책 | 백업 설정과 서비스 전체 복구 검증은 별도. [ADR 0005](adr/0005-backup-restore-dr.md)의 측정 절은 플레이스홀더 |
| 기동 시 시크릿 주입 | 비밀번호 회전 시 기존 연결·새 연결의 동작을 별도로 다뤄야 함 |
| Terraform / CI 분리 | task definition 변경 반영 순서가 중요. [ADR 0001](adr/0001-cicd-terraform-ci-boundary.md) 참고 |

## 관측과 후속 자동 복구

로그·서비스 지표는 CloudWatch로 수집하고, 외부 `/readyz` 점검으로 DB 의존성 실패를 탐지합니다. [자동 재배포 Terraform](../infra/prod/rotation-redeploy.tf)과 [Lambda](../infra/prod/lambda/rotation_redeploy/handler.py)는 Secrets Manager 이벤트에 반응해 ECS 재배포를 요청하도록 구현되어 있습니다. 이는 현재 운영 상태를 조회한 결과가 아니므로 적용 및 드릴 완료와 구분합니다.

Redis/ElastiCache, EKS 및 k3s는 위 구현 아키텍처에 포함하지 않습니다.
