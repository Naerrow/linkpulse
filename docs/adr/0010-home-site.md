# ADR 0010 — 집 사이트: AWS와 같은 세 층, 사이트 큐, Roles Anywhere 사이드카, 비밀은 kubectl로

- 상태: 채택 (2026-10-16, plan 0017). 집 창 판정(a)~(g) 전이다 — 결과는 판정 뒤 이 줄에 적는다.
- 관련: AGENTS.md §5(P6 하이브리드·자유 이동), [plan 0017](../plans/0017-home-site-foundation/plan.md), [ADR 0006](0006-runtime-db-credentials.md)(비밀번호 출처 aws|file),
  [ADR 0007](0007-ecs-to-eks.md)(차트는 사이트를 모른다), [ADR 0008](0008-in-cluster-prometheus.md)(클러스터 안 Prometheus), 런북 [home-site.md](../runbooks/home-site.md).

## 맥락

- P6의 첫 단계는 집 Proxmox 서버에 **두 번째 사이트**를 세우는 것이다. 같은 이미지·같은 Helm 차트가 사이트 값만 바꿔 돌아야 한다(자유 이동 원칙).
- 집에는 공인 IP·공유기 사양을 아직 모른다 → 0017은 집 → AWS 방향의 인터넷(HTTPS)만 쓴다. VPN은 0018이다.
- 집 앱도 클릭 큐(SQS)를 쓴다. AWS 밖이라 EKS Pod Identity가 없고, 장기 액세스 키는 두고 싶지 않다.
- AWS 사이트(infra/prod)는 시간 과금이라 작업할 때만 켜고 통째로 지운다. 집 사이트는 그와 무관하게 켜져 있을 수 있어야 한다.

## 결정

1. **AWS와 같은 세 층**으로 나눈다. 층마다 state가 따로다(같은 S3 버킷, 키만 다르다).
   | 층 | AWS | 집 |
   | --- | --- | --- |
   | 바탕 | `infra/prod` | `infra/home`(Proxmox VM 두 대) + `infra/home/ansible`(k3s·PostgreSQL 16) |
   | 바탕의 AWS 쪽 | — | `infra/home-aws`(Roles Anywhere·집 클릭 큐) — 시간 과금이 없어 늘 켠다 |
   | 클러스터 안 기반 | `infra/k8s` | `infra/home-k8s`(같은 버전 kube-prometheus-stack·ArgoCD·Image Updater, site-home, 같은 gitops 차트) |
   | 앱 | `values-aws.yaml` | `values-home.yaml` |
   - `infra/home-k8s`는 앞 스택의 값(Postgres IP·ARN·큐 URL)을 **원격 state**로 읽는다. AWS의 `infra/k8s`는 이름으로 찾지만, Roles Anywhere에는 Terraform 데이터 소스가 없다(트러스트 앵커 ID가 난수).
   - Postgres는 클러스터 밖 VM이다 — RDS처럼 클러스터를 다시 만들어도 데이터가 남고, 0019 논리 복제의 대상이 된다.
2. **사이트마다 큐·워커·DB가 한 묶음이다.** 집 앱이 보낸 클릭은 집 큐(`linkpulse-home-clicks`)로 가고 집 워커가 집 DB에 반영한다. 두 사이트가 한 큐를 나누는 것(워커 이동)은 0019의 실험이다.
3. **AWS 인증은 IAM Roles Anywhere, 서명 도우미는 앱 파드의 네이티브 사이드카다.**
   - 자체 CA(맥의 `~/.linkpulse-home/pki`, `scripts/home-pki.sh`)를 트러스트 앵커로 올린다. AWS Private CA는 쓰지 않는다(고정 비용). Roles Anywhere 자체는 추가 요금이 없다.
   - 역할 신뢰 정책은 `aws:SourceArn` = 그 트러스트 앵커와 `x509Subject/CN` = `linkpulse-home-app` 두 조건을 건다(없으면 계정의 어느 앵커 인증서로든 역할을 맡는다). 권한은 집 큐 세 동작뿐이다.
   - 도우미 `serve`는 127.0.0.1에만 바인드하는 IMDSv2 호환 엔드포인트다 → 앱과 같은 네트워크 네임스페이스(사이드카)여야 하고, 파드 밖에서는 닿지 않는다.
     앱은 `AWS_EC2_METADATA_SERVICE_ENDPOINT`로 그곳을 본다(SDK 기본 체인의 IMDS 단계). **앱 이미지·코드는 바꾸지 않는다.**
   - 차트의 선택 기능(`awsRolesAnywhere.enabled`, 기본 끔)이다. 끄면 렌더가 이전과 바이트 단위로 같다(`check-charts.sh`가 main 렌더와 비교).
4. **DB 비밀번호와 말단 개인 키는 Terraform이 아니라 사람이 `kubectl`로 넣는다.** Terraform으로 넣으면 S3 state에 평문으로 남는다. Secret 키 이름은 고정이다(`password`, `cert.pem`·`key.pem`).
   CA 개인 키는 클러스터에도 AWS에도 넣지 않는다. Proxmox API 토큰은 환경 변수로만 준다.

## 트레이드오프 (받아들인 위험)

- **사이드카에 probe가 없다.** 127.0.0.1 바인드라 kubelet probe가 닿지 않고, 이미지에 셸도 없다. 도우미가 듣기 전의 첫 SQS 호출은 실패할 수 있다 — 배처는 같은 배치 ID로 다시 보내고 워커는 다시 받는다. 판정 (b)는 "기동 30초 뒤부터 발행 실패가 늘지 않는다"로 본다.
- **CRL을 쓰지 않는다.** 말단 유효 기간 1년과 트러스트 앵커 교체로 대신한다. 말단이 만료되면 클릭 발행이 실패하고 유실이 쌓인다 — 0017은 집 알림의 외부 전달이 없으므로 런북 정기 점검(만료 30일 전)에 기댄다.
- **k3s VM에 방화벽이 없다**(k3s 문서 권장). 집 LAN 안·공유기 NAT 뒤라 밖에서 닿지 않는다. VLAN 분리는 P7이다. Postgres VM만 ufw를 켠다(5432는 k3s 노드에서만).
- **단일 노드·백업 없음.** 노드 장애 = 집 사이트 장애, 집 데이터는 시험 데이터다. 다중 노드·집 밖 백업은 P7이다.
- **집 알림은 Alertmanager 화면까지만.** "감시는 집 밖에서"(P7)와 함께 정한다.

## 결과

- 같은 이미지(GHCR `main` 다이제스트)·같은 차트·같은 버전의 클러스터 구성 요소가 두 사이트에서 돈다. 다른 것은 사이트 값 파일과 사이트 차트(site-aws·site-home)뿐이다.
- 집 사이트를 켜고 끄는 것은 AWS와 독립적이다. 집 → AWS 의존은 Roles Anywhere·SQS(공개 엔드포인트)뿐이다.
- 판정은 집 창: (a) 같은 이미지·차트, (b) 클릭 스모크, (c) 다른 CN 거부, (d) file 회전 드릴, (e) 모니터링, (f) 처음부터 다시 만들기, (g) AWS 무영향.
