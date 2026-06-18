# linkpulse

**Fast URL shortener with real-time click analytics.**
Built and operated as a production service, with a focus on reliability and observability.

링크를 짧게 만들고, 그 링크가 *실제로 어떻게 쓰이는지* — 클릭 수, 유입 경로, 시간대 — 를 바로 볼 수 있는 가볍고 빠른 서비스입니다.

## 왜 만들었나

기존 URL 단축 서비스는 분석이 빈약하거나 무겁다고 느꼈습니다. linkpulse는 단축이라는 단순한 기능에 더해 *링크의 실제 사용 흐름*을 가볍게 보여주는 데 집중합니다.
그리고 기능보다 중요하게 둔 목표는 따로 있습니다 — **작은 서비스라도 끝까지 직접, 안정적으로 운영하는 것.** 그래서 자동 배포·모니터링·장애 대응 체계를 갖추고 실제 트래픽을 받으며 운영합니다.

## 주요 기능

- 긴 URL → 짧은 키 발급, 짧은 키 → 원본 리다이렉트
- 클릭 이벤트 수집 및 분석(클릭 수, referrer, 시간대 등)
- 읽기 위주 트래픽에 맞춘 캐싱

## 아키텍처 (클라우드)

사용자 → Route53 → ALB(HTTPS, ACM) → ECS Fargate(app) → RDS(PostgreSQL) / ElastiCache(Redis)
모든 리소스는 Terraform으로 관리(IaC)되며, VPC의 퍼블릭/프라이빗 서브넷으로 분리되어 있습니다.

- IaC: Terraform · 클라우드: AWS(ap-northeast-2)
- 런타임: ECS Fargate · 인그레스: ALB + ACM
- 데이터: RDS PostgreSQL · 캐시: ElastiCache Redis
- CI/CD: GitHub Actions(빌드·테스트·ECR·롤링 배포)
- 관측성: CloudWatch(구조화 로깅·지표·알람)

> 진행 단계: 클라우드(ECS Fargate)에서 안정 운영 → EKS·홈랩(k3s) 하이브리드로 확장 예정.

## 운영 (operations)

- 자동 배포: `main` 머지 시 GitHub Actions가 빌드·테스트 후 배포
- 모니터링/알림: 핵심 지표(요청·오류율·지연·DB 커넥션)와 알람
- 장애 대응 기록: 실제 장애와 대응 과정을 `/docs`에 회고로 정리
- 백업: RDS 자동 백업 및 복원 리허설

## 로컬 실행

```bash
docker compose up
```

## 저장소 구조

```
/app    애플리케이션 + 테스트
/infra  Terraform (VPC, ECS, ALB, RDS, ElastiCache, IAM)
/.github/workflows  CI/CD
/docs   아키텍처 노트 · 장애 회고 · ADR
/load   k6 부하 테스트
```

## 라이선스

MIT (예정)
