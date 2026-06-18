# AGENTS.md — linkpulse

> 이 파일은 이 저장소에서 작업하는 모든 코딩 에이전트(Claude Code, Codex 등)가 가장 먼저 읽고 따르는 단일 기준입니다.
> Claude Code는 `CLAUDE.md`도 읽으므로, 루트에 `CLAUDE.md`를 만들고 한 줄(`See AGENTS.md`)만 넣어 이 파일을 가리키게 하세요.

## 1. 프로젝트 개요
- 이름: **linkpulse** — 빠른 URL 단축 + 실시간 클릭 분석 서비스.
- 문제: 기존 단축 서비스는 분석이 빈약하거나 무겁다. 링크가 *실제로 어떻게 쓰이는지*(클릭 수·유입 경로·시간대)를 가볍고 빠르게 보여주는 서비스를 만들고 직접 운영한다.
- 목표: 기능 자체보다 **작은 서비스를 프로덕션급으로 신뢰성 있게 운영하는 것**. 자동 배포·모니터링·장애 대응 체계를 갖추고 실제 트래픽을 받으며 운영한다.
- 1순위 원칙: **완성 우선**. 화려함보다 끝까지 동작하고 운영되는 것.
- 운영 책임 원칙: 이 서비스는 사람이 직접 운영한다. 따라서 생성된 모든 코드·인프라는 사람이 이해하고 설명할 수 있는 상태여야 한다(이해 못 하는 것은 운영할 수 없다).
- 단계: 클라우드 단독(ECS Fargate)으로 먼저 완성 → 이후 EKS·홈랩(k3s) 하이브리드로 확장.

## 2. 절대 규칙 (가드레일) — 위반 금지
1. **인프라 변경은 사람이 승인한다.** `terraform apply`, `aws ... (mutating)`, `kubectl apply`, 리소스 생성/삭제/과금 발생 명령을 **에이전트가 자율 실행하지 않는다.** `terraform plan`까지만 하고, 변경 계획을 사람에게 보여주고 멈춘다. (Codex: suggest 모드 / Claude Code: plan 모드 유지)
2. **비밀값을 코드·커밋에 절대 넣지 않는다.** DB 비밀번호·토큰·키는 AWS Secrets Manager 또는 환경변수로만. `.env`, `*.tfvars`(민감), `*.pem`은 `.gitignore`에 추가.
3. **자기 작업 디렉터리/브랜치 밖을 건드리지 않는다.** 현재 task에 배정된 경로 외 파일 수정 금지(필요하면 사람에게 먼저 알린다).
4. **이해 가능한 산출물.** 자명하지 않은 인프라 결정(서브넷 배치, IAM 정책, 보안그룹 규칙 등)은 *왜 그렇게 했는지* 1~3줄 주석 또는 PR 설명에 남긴다. 이 서비스를 직접 운영할 사람이 이해할 수 있어야 한다.
5. **작게, 자주 커밋.** 한 커밋 = 한 가지 일. 동작하지 않는 코드를 메인에 머지하지 않는다.
6. **커밋·푸시는 사람이 직접 한다.** 에이전트는 `git commit`/`git push`를 **스스로 실행하지 않는다.** 코드/문서 변경을 만들고 `git add`로 스테이징한 뒤, 제안 커밋 메시지를 보여주고 멈춘다. 실제 커밋·푸시는 사람이 수행한다. `git reset`·`git restore` 등으로 기존 커밋/변경을 되돌리는 것도 사람의 명시적 지시가 있을 때만 한다.

## 3. 기술 스택 (확정)
- 앱: (사용자가 정함 — 권장 Go) / DB: PostgreSQL / 캐시: Redis(ElastiCache)
- 컨테이너: Docker / 레지스트리: ECR
- IaC: Terraform (HCL) / 클라우드: AWS (리전 ap-northeast-2)
- 런타임: ECS Fargate(코어) → EKS(업그레이드)
- 인그레스: ALB + ACM(HTTPS) + Route53
- CI/CD: GitHub Actions(코어) → ArgoCD/GitOps(EKS 업그레이드 시)
- 관측성: CloudWatch(코어) → Prometheus/Grafana(업그레이드)
- 부하 테스트: k6

## 4. 저장소 구조 (모노레포)
```
/app            # 애플리케이션 코드 + 단위 테스트
/infra          # Terraform (VPC, ECS, ALB, RDS, ElastiCache, IAM)
/.github/workflows  # GitHub Actions (CI/CD)
/docs           # 아키텍처 노트, 장애 회고(runbook), ADR
/load           # k6 부하 테스트 스크립트
docker-compose.yml  # 로컬 전체 스택
AGENTS.md  CLAUDE.md  README.md
```

## 5. 컨벤션
- 커밋: Conventional Commits (`feat:`, `fix:`, `chore:`, `docs:`, `infra:`).
- 브랜치: `feat/<topic>`, `infra/<topic>`. 메인 직접 푸시 금지, PR로만.
- 모든 앱 변경에는 테스트. 모든 인프라 변경에는 `terraform plan` 출력 첨부.
- 문서: 비자명한 결정은 `/docs/adr/`에 짧은 ADR(맥락·결정·트레이드오프)로 남긴다.

## 6. Phase 계획 (순차 의존)
- **P0** 앱 코어: 단축·리다이렉트·클릭집계, `docker-compose`로 로컬 완결.
- **P1** 라이브: Terraform로 VPC/ECS/ALB/RDS/IAM, HTTPS 도메인. (첫 완성 마일스톤)
- **P2** CI/CD: GitHub Actions → 빌드·테스트·ECR·ECS 롤링 배포.
- **P3** 관측성: 구조화 로깅, CloudWatch 지표·알람, 의도적 장애 + 회고(`/docs`).
- **P4** 하드닝: IAM 최소권한, Secrets 로테이션, RDS 백업+복원 리허설, Redis 캐시, 레이트리밋, k6 부하. (클라우드 단독 완성본)
- **P5+** 업그레이드: ECS→EKS, ArgoCD, Prometheus/Grafana, 홈랩 k3s 하이브리드 + DR.

## 7. 에이전트 역할 (고정)
- **Claude Code = 메인.** 설계·구현·의사결정을 주도한다. 인프라/Terraform, 앱, CI/CD 전반의 1차 작성자이자 책임자.
- **Codex = 보조 + 검토.** (1) Claude가 올린 변경(diff)을 리뷰해 보안·IAM·비용·버그·약점을 지적한다. (2) 테스트 작성, 반복 작업, 독립적인 보조 task를 맡는다. 작업 트리를 임의로 바꾸지 않고, 큰 변경은 Claude/사람을 거친다.
- 흐름: Claude 작성 → Codex 리뷰(`/review`, 수정 없이 지적만) → Claude가 사람 승인 후 반영 → 사람이 "지적 N개 중 반영분"을 diff로 재확인.

## 8. 작업 분담표 (Phase 시작 시마다 채운다)
> 두 에이전트에게 동시에 일을 줄 경우, 담당 경로가 겹치지 않는지 먼저 확인한다. 같은 파일은 반드시 순차로.

| 에이전트 | 역할 | 브랜치 / worktree | 담당 경로 | 이번 task |
|---|---|---|---|---|
| Claude Code | 메인(작성) | `feat/...` 또는 `infra/...` | 해당 Phase 전체 | (예: P0 앱 코어) |
| Codex | 검토/보조 | 리뷰는 브랜치 비고정 | (보조 task 시 명시) | (예: 위 PR 리뷰 + 테스트 보강) |
