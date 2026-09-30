# 문서 안내

[프로젝트 README](../README.md)

## 처음 읽는 순서

1. [아키텍처](architecture.md): 요청 흐름, 네트워크, 비용·가용성의 선택
2. [CI/CD](deployment.md): PR 검증, 앱 배포, 롤백, Terraform 경계
3. [운영 사례](operations-case-study.md): 탐지와 복구를 구분한 장애 분석

## 의사결정 기록

- [0001 — CI/CD와 Terraform의 경계](adr/0001-cicd-terraform-ci-boundary.md)
- [0002 — 알림 설계](adr/0002-alerting-design.md)
- [0003 — 배포 실패 알림](adr/0003-deploy-failure-alerts.md)
- [0004 — 무트래픽 canary와 readiness](adr/0004-notraffic-canary.md)
- [0005 — 백업 복원 검증 범위](adr/0005-backup-restore-dr.md)

## 운영 런북

- [알람 대응](runbooks/alarm-response.md)
- [비밀번호 회전 복구](runbooks/secret-rotation-bridge.md)
- [RDS 복원 검증](runbooks/rds-restore.md)
- [운영 시 주의점](runbooks/ops-gotchas.md)

## 장애 회고

- [GameDay 회고](postmortems/2026-07-06-gameday-01-retro.md)
- [배포 러너 배정 실패](postmortems/2026-07-09-deploy-runner-acquisition.md)
- RDS 회전 장애: [08-16](postmortems/2026-08-16-db-password-rotation-outage.md) · [08-23](postmortems/2026-08-23-rds-rotation-outage.md) · [08-30](postmortems/2026-08-30-rds-rotation-outage.md) · [09-06](postmortems/2026-09-06-rds-rotation-outage.md)

## 개선 계획과 검증 상태

[로테이션 대응 계획](plans/0009-p4d2-rotation-resilience/plan.md)과 [탐지 드릴 계획](plans/0010-p4d2-detection-drill/plan.md)은 계획 문서입니다. 체크 항목과 실제 검증 증거를 확인하기 전에는 완료된 성과로 해석하지 않습니다. [백업 복원 검증 계획](plans/0007-p4-rds-restore-drill/plan.md) 역시 설계와 실측 완료를 구분합니다.

다이어그램은 README의 Mermaid로 관리하므로 별도의 빈 `assets/` 폴더는 만들지 않았습니다.
