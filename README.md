# linkpulse

**Fast URL shortener with real-time click analytics.**
Built and operated as a production service, with a focus on reliability and observability.

링크를 짧게 만들고, 그 링크가 *실제로 어떻게 쓰이는지* — 클릭 수, 유입 경로, 시간대 — 를 바로 볼 수 있는 가볍고 빠른 서비스입니다.

## 왜 만들었나

기존 URL 단축 서비스는 분석이 빈약하거나 무겁다고 느꼈습니다. linkpulse는 단축이라는 단순한 기능에 더해 *링크의 실제 사용 흐름*을 가볍게 보여주는 데 집중합니다.
그리고 기능보다 중요하게 둔 목표는 따로 있습니다 — **작은 서비스라도 끝까지 직접, 안정적으로 운영하는 것.** 그래서 자동 배포·모니터링·장애 대응 체계를 갖추고 실제 트래픽을 받으며 운영합니다.

## 주요 기능

- 긴 URL → 짧은 키 발급, 짧은 키 → 원본 리다이렉트(302 — 매 방문을 서버가 받아야 집계가 맞다)
- 링크별 클릭 수 집계와 조회(`GET /api/links/{code}`)
- **링크 생성은 운영자만** 한다. 방문자는 화면에서 **링크를 요청**하고, 운영자가 주소를 직접 보고 승인하면
  요청 때 받은 확인 링크로 결과를 본다. 개인정보는 받지 않는다([plan 0011](docs/plans/0011-owner-ui-and-link-requests/plan.md))
- 화면 한 장(`/`) — Go 바이너리에 내장돼 인프라·비용이 늘지 않는다
- IP별 레이트리밋(쓰기·통계·읽기 3단계)

> 아직 없는 것: 유입 경로(referrer)·시간대 분석, Redis 캐시·분산 레이트리밋 — P5 이후.

## 아키텍처 (클라우드)

사용자 → Route53 → ALB(HTTPS, ACM) → ECS Fargate(app, 태스크 2개) → RDS(PostgreSQL)
모든 리소스는 Terraform으로 관리(IaC)되며, VPC의 퍼블릭/프라이빗 서브넷으로 분리되어 있습니다.

- IaC: Terraform · 클라우드: AWS(ap-northeast-2)
- 런타임: ECS Fargate · 인그레스: ALB + ACM
- 데이터: RDS PostgreSQL(마스터 비밀번호는 Secrets Manager가 7일마다 자동 회전)
- CI/CD: GitHub Actions(빌드·테스트·ECR·롤링 배포)
- 관측성: CloudWatch(구조화 로깅·지표·알람)

> 진행 단계: **P0~P4 완료(2026-10-07, Redis 캐시만 P5로 이관)** — 클라우드(ECS Fargate) 단독 운영 체계를 갖췄다.
> 다음은 **P5 AWS 완성**(한계 측정 → 클릭 집계 비동기화 → EKS·ArgoCD → Prometheus/Grafana) → **P6 하이브리드**(홈랩 k3s + VPN)
> → **P7 온프레미스**(전부 집으로 이전). 최종적으로 셋 사이를 양방향으로 자유롭게 이전할 수 있게 만든다.

## 운영 (operations)

- 자동 배포: `main` 머지 시 GitHub Actions가 빌드·테스트 후 ECS 롤링 배포(OIDC, 장기 키 없음). 이전 이미지로의 롤백은 `workflow_dispatch` 한 번
- 탐지: CloudWatch 알람(ALB 5xx·지연·ECS·RDS) + 외부 카나리(Route53 헬스체크가 `/readyz`를 30초마다 호출) → Slack·SMS
- **비밀번호 회전 자동 복구**: 회전이 일어나면 앱이 새 비밀번호를 런타임에 다시 읽어 스스로 회복하고, 동시에 자동 재배포가 뒤를 받친다. 실측 503 약 61초, 사람 개입 없음([ADR 0006](docs/adr/0006-runtime-db-credentials.md)). 이전에는 회전마다 수 시간~수 일 다운됐다
- 백업: RDS 자동 백업 + 복원 리허설([ADR 0005](docs/adr/0005-backup-restore-dr.md), [`rds-restore.md`](docs/runbooks/rds-restore.md))
- 장애 대응: 알람별 런북([`alarm-response.md`](docs/runbooks/alarm-response.md)), 실제 장애·GameDay 회고([`docs/postmortems/`](docs/postmortems/))
- 부하: k6로 한도 안 p95 10ms, 한도 밖 429 보호, 클릭 집계 유실 0 확인([`load/k6/`](load/k6/))

## 로컬 실행

전체 스택(app + Postgres)을 docker-compose로 띄웁니다.

```bash
docker compose up --build
```

브라우저에서 <http://localhost:8080>을 열면 방문자 화면(링크 요청)이 뜹니다.
운영자 화면은 **<http://localhost:8080/#admin>**이고(방문자에게는 링크가 없다), 로컬 운영자 토큰은 **`dev-token`**입니다
(`docker-compose.yml`에는 그 해시만 들어 있고, 운영 토큰과는 무관합니다).

```bash
# 단축 생성(관리자) → {"code":"...","short_url":"http://localhost:8080/...", ...}
curl -X POST localhost:8080/api/links -H 'Authorization: Bearer dev-token' -d '{"url":"https://example.com"}'
# 리다이렉트 (위 응답의 code 사용)
curl -i localhost:8080/<code>
# 방문자 링크 요청 → {"id":"...","status":"pending","status_url":"http://localhost:8080/#r=...", ...}
curl -X POST localhost:8080/api/requests -d '{"url":"https://example.com/wanted","note":"메모"}'
```

| API | 누가 | 내용 |
| --- | --- | --- |
| `POST /api/links` | 관리자 | 직접 생성 |
| `GET /{code}` · `GET /api/links/{code}` | 누구나 | 리다이렉트 · 클릭 수 |
| `POST /api/requests` · `GET /api/requests/{id}` | 누구나 | 링크 요청 · 결과 확인 |
| `GET /api/admin/requests` | 관리자 | 대기 목록 |
| `POST /api/admin/requests/{id}/approve` · `/reject` | 관리자 | 승인(링크 발급) · 거절 |
| `GET /api/admin/links` · `POST /api/admin/links/{code}/disable` · `/enable` | 관리자 | 최근 링크 · 중단(410) · 다시 사용 |

관리자 인증은 `Authorization: Bearer <토큰>`입니다. 서버에는 토큰의 SHA-256 해시만 `ADMIN_TOKEN_SHA256`으로 주고, 비어 있으면 관리자 기능이 꺼집니다.

- 데이터는 Postgres에 저장되며 `pgdata` 볼륨으로 재시작 후에도 유지됩니다.
- 종료: `docker compose down` (데이터까지 지우려면 `docker compose down -v`).
- 호스트 8080이 이미 쓰이면: `APP_HOST_PORT=8088 docker compose up --build` (단축 URL도 그 포트로 맞춰집니다).

DB 없이 앱만 빠르게 띄우려면(인메모리, 재시작 시 데이터 소실):

```bash
cd app && ADMIN_TOKEN_SHA256=c91cbbedf8c712e8e2b7517ddeca8fe4fde839ebd8339e0b2001363002b37712 go run ./cmd/server
```

`ADMIN_TOKEN_SHA256`은 `dev-token`의 해시입니다. 빼고 띄우면 관리자 기능이 꺼집니다.

## 저장소 구조

```
/app    애플리케이션 + 테스트
/infra  Terraform (VPC, ECS, ALB, RDS, IAM, 알람·카나리, 회전 자동 재배포 Lambda)
/.github/workflows  CI/CD
/docs   ADR · 계획(plans) · 런북 · 장애 회고
/load   k6 부하 테스트 · GameDay 장애 주입 이미지
/scripts  prod 전체 재기동·삭제 스크립트
```

## 인프라 배포 (P1, Terraform)

AWS(ap-northeast-2) ECS Fargate 배포의 상세 런북은 [`infra/README.md`](infra/README.md)에 있습니다. 요약:

- `infra/bootstrap` → state용 S3 버킷, `infra/prod` → 본 인프라(VPC·ALB·ECS·RDS·IAM).
- **모든 `apply`는 사람이 직접**, **Terraform 1.10+** 필요(S3 네이티브 lockfile, DynamoDB 미사용).
- 순서: bootstrap apply → prod init → `-var service_desired_count=0` apply → GitHub Variables 등록 → CI(GitHub Actions)가 이미지 빌드·배포 → `apply`로 desired=2.
- **이미지 배포는 GitHub Actions(P2)가, `terraform apply`는 인프라·스케일만** 담당합니다(경계: [`docs/adr/0001-cicd-terraform-ci-boundary.md`](docs/adr/0001-cicd-terraform-ci-boundary.md)).

## 라이선스

MIT (예정)
