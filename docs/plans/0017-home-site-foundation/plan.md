---
status: approved # in-review | approved
revision: 2
created: 2026-10-15
---

# 0017. P6 하이브리드 1단계 — 집 사이트 기반(Proxmox·k3s·같은 차트·Roles Anywhere)

## 목표

집 Proxmox 서버에 **두 번째 사이트**를 세운다. AWS와 같은 이미지·같은 앱 차트가 사이트 값만 바꿔 집에서 돈다.

1. **IaC로 VM 두 대**: Terraform(Proxmox 공급자)으로 k3s VM과 Postgres VM을 만든다. Ansible로 k3s와 PostgreSQL 16을 설치한다.
2. **같은 이미지·같은 차트**: GHCR `main` 이미지를 `deploy/helm/linkpulse` 차트 + `values-home.yaml`로 배포한다. ArgoCD·Image Updater·kube-prometheus-stack도 AWS와 같은 버전이다.
3. **집 데이터는 집 Postgres**, 비밀번호 출처는 `file`(plan 0014가 P6용으로 만든 경로)이다.
4. **AWS 인증은 IAM Roles Anywhere**: 장기 키 없이 X.509 인증서로 임시 자격증명을 받는다. 트러스트 앵커는 자체 CA다(AWS Private CA는 쓰지 않는다). **앱 이미지는 바꾸지 않는다** — 서명 도우미를 파드 사이드카로 두고, SDK가 IMDS 대신 그 사이드카를 보게 한다.
5. **집 사이트 전용 클릭 큐**(SQS): 집 앱이 보내고 집 워커가 집 DB에 반영한다.
6. **검증**: 클릭 파이프라인 스모크, 다른 CN 인증서 거부, `file` 비밀번호 회전 드릴, 모니터링 규칙, 처음부터 다시 만들기 1회.

**범위 밖**(다음 plan):
- **0018**: AWS↔집 VPN·라우팅. 공인 IP와 공유기 사양을 확인한 뒤 설계한다.
- **0019**: 워커를 집으로 옮기기·VPN 끊김 실험, RDS → 집 Postgres 논리 복제, 읽기 전용 DR(RTO·RPO).
- **P7**: 공개 노출(DNS 전환·Cloudflare Tunnel), 집 밖 감시, 집 밖 백업, VLAN 분리.
- 집 로그를 CloudWatch로 보내기(Fluent Bit). 0017에서는 `kubectl logs`로 본다.
- 집 알림의 외부 전달(Slack). 0017에서는 Alertmanager 화면까지만 본다 — "감시는 집 밖에서"(P7)와 함께 정한다.

## 배경/제약

- **사용자 환경**(2026-10-15 답): 집에 Proxmox가 설치된 x86 서버가 있다(메모리 16GB 이상). 공인 IP·공유기 사양은 모른다 → 0017은 집 → AWS 방향의 인터넷(HTTPS)만 쓴다.
  집 LAN은 `192.168.0.x`다(측정 창 오류 출력의 맥 주소). AWS VPC `10.0.0.0/16`, k3s 기본 파드 `10.42.0.0/16`·서비스 `10.43.0.0/16`과 겹치지 않는다 — 0018 라우팅의 전제를 지금 지킨다.
- **로드맵 원칙**(AGENTS.md §5 자유 이동)
  - 앱이 AWS SDK를 직접 부르지 않는다(비밀번호 공급자·큐는 인터페이스 뒤) — 이미 그렇다.
  - 순수 Postgres만, 모든 테이블에 PK — 집 Postgres도 같은 스키마를 앱이 기동 때 적용한다.
  - 같은 이미지·같은 Helm 차트, 사이트별 값만 다르게. 레지스트리는 양쪽에서 닿는 GHCR(공개 패키지, pull 비밀 없음).
  - 양쪽 다 IaC: AWS는 Terraform, 집은 Proxmox Terraform 공급자 + Ansible.
- **1차 출처로 확인한 것**
  - **Roles Anywhere 서명 도우미**(aws/rolesanywhere-credential-helper v1.8.5, 2026-08-24): `serve`는 "IMDSv2 호환 로컬 엔드포인트"를 연다(기본 포트 9911). README: "any process that can reach 127.0.0.1 can retrieve AWS credentials" → **127.0.0.1에서만 듣는다** → 앱과 같은 네트워크 네임스페이스(파드 사이드카)여야 한다.
    공식 이미지 `public.ecr.aws/rolesanywhere/credential-helper`, 고정 태그 `<version>-<platform>-<timestamp>`(amd64·arm64).
  - **앱 SDK**: `aws-sdk-go-v2/config` v1.33.4 `env_config.go`가 `AWS_EC2_METADATA_SERVICE_ENDPOINT`를 읽는다. SQS 클라이언트는 `LoadDefaultConfig`(기본 체인, IMDS 포함)다 — 환경 변수 하나로 사이드카를 본다. 앱 코드 변경 없음.
  - **Roles Anywhere 신뢰 모델**(userguide `trust-model.html`)
    - 트러스트 앵커 CA 인증서: X.509v3, Basic Constraints `CA: true`, Key Usage `Certificate Sign`, 서명 SHA256 이상.
    - 말단 인증서: X.509v3, `CA: false`, Key Usage `Digital Signature`, SHA256 이상. 빈 Subject는 안 된다.
    - 역할 신뢰 정책: 주체 `rolesanywhere.amazonaws.com`, 동작 `sts:AssumeRole`·`sts:TagSession`·`sts:SetSourceIdentity`.
      권장 조건: `aws:SourceArn` = 트러스트 앵커 ARN, 인증서 필드(`aws:PrincipalTag/x509Subject/CN`). 조건이 없으면 계정의 어느 트러스트 앵커 인증서로든 그 역할을 맡을 수 있다.
    - CRL은 가져오기(ImportCrl)만 된다. 이번에는 쓰지 않고 말단 인증서 유효 기간과 트러스트 앵커 교체로 대신한다(리스크 표).
  - **Proxmox 공급자** bpg/proxmox: 2주 규칙으로 **v0.114.0**(2026-09-21). v0.115.0은 10-02라 2주가 안 됐다.
    API 토큰 인증, `proxmox_virtual_environment_vm`의 `initialization`(cloud-init `ip_config`·`user_account` — SSH 없이 API만), `proxmox_virtual_environment_download_file`(클라우드 이미지 받기 — `Datastore.AllocateTemplate`·`Sys.Audit`·`Sys.Modify` 권한, `import` 콘텐츠 형식은 저장소에서 켜야 한다).
  - **k3s**: 2주 규칙으로 **v1.37.1+k3s1**(2026-09-30). EKS(1.37)와 같은 마이너다. 서버 옵션 `--disable`로 내장 Traefik·ServiceLB를 끈다(0017은 공개 노출이 없다). 내장 metrics-server는 HPA가 쓴다.
  - **k3s 방화벽**(docs.k3s.io `installation/requirements`): "It is recommended to turn off ufw". 켜 두려면 `6443/tcp`·`from 10.42.0.0/16`·`from 10.43.0.0/16`을 열어야 한다.
  - **구현 때 확인할 것**(plan에서 단정하지 않는다): Roles Anywhere 요금(가격 페이지), 서명 도우미 이미지의 정확한 amd64 태그·다이제스트와 실행 사용자, 맥용 서명 도우미 v1.8.5 바이너리와 체크섬(B0·B1 시험용), Ubuntu 24.04 클라우드 이미지의 날짜 고정 URL·체크섬, Proxmox 토큰에 줄 최소 권한(공급자 문서).
- **지금 코드**
  - 앱 차트는 `dbPasswordSecret.name`이 있으면 Secret을 디렉터리로 마운트한다(subPath 없음 — 회전 때 kubelet이 파일을 바꾼다). 앱은 인증 실패 때 파일을 다시 읽는다(plan 0014, `dbcreds`).
  - 차트는 `envFrom`으로 차트 기본값 → 사이트 ConfigMap → 사이트 Secret을 읽는다(ADR 0009). `monitoring.enabled`로 ServiceMonitor·규칙·대시보드를 켠다.
  - AWS 사이트의 클러스터 층은 `infra/k8s`(Terraform helm: kube-prometheus-stack 91.8.2·ArgoCD 10.10.3·Image Updater 1.3.2·Fluent Bit·LBC·site-aws·gitops)다. `infra/k8s/charts/gitops`는 `valueFiles`를 값으로 받는다 — 집에서 그대로 쓴다.
  - AWS 앱 역할 권한은 클릭 큐 `sqs:SendMessage`·`ReceiveMessage`·`DeleteMessage`뿐이다(`infra/prod/iam.tf` `pod_app`). 집 역할도 같은 세 동작을 집 큐에만 준다.
  - CI는 Terraform을 돌리지 않는다. `terraform validate`·tfsec은 로컬에서 한다(지금까지와 같다).
- **가드레일**(AGENTS.md §2)
  - `terraform apply`(세 스택 모두), `ansible-playbook`(변경 실행), `kubectl create/patch/delete`, Proxmox 화면의 토큰·저장소 설정은 **사람이** 한다.
  - 에이전트는 `terraform validate`·`plan`(사람이 만든 로컬 tfvars가 있을 때), `ansible-playbook --syntax-check`·`ansible-lint`, `helm template`, `scripts/check-charts.sh`, 읽기 명령까지만 한다.
  - 비밀(Proxmox 토큰, DB 비밀번호, CA·말단 개인 키, 관리자 해시)은 저장소·커밋·화면·Terraform state에 넣지 않는다(아래 설계 6). 집 IP는 사설 주소라 문서에 적어도 되지만 공인 IP는 찍지 않는다.

## 설계

### 1. 층과 디렉터리 — AWS와 같은 세 층

| 층 | AWS(지금) | 집(새로) | 상태 저장 |
| --- | --- | --- | --- |
| ① 바탕 | `infra/prod`(VPC·RDS·EKS…) | `infra/home`(Proxmox VM 두 대) + `infra/home/ansible`(k3s·PostgreSQL) | S3 state 버킷, 키 `home/…` |
| ① 의 AWS 쪽 | — | **`infra/home-aws`**(Roles Anywhere 트러스트 앵커·프로필·역할, 집 클릭 큐·DLQ) | 같은 버킷, 키 `home-aws/…` |
| ② 클러스터 안 기반 | `infra/k8s` | **`infra/home-k8s`**(kube-prometheus-stack·ArgoCD·Image Updater·site-home·gitops) | 같은 버킷, 키 `home-k8s/…` |
| ③ 앱 | ArgoCD가 `deploy/helm/linkpulse` + `values-aws.yaml` | ArgoCD가 같은 차트 + **`values-home.yaml`** | git |

- **`infra/home-aws`를 `infra/prod`와 나누는 이유**: `infra/prod`는 시간 과금이라 작업할 때만 켜고 통째로 지운다(`full-destroy-prod.sh`). 집 사이트는 늘 켜 둘 수 있어야 하고, 그 AWS 쪽(Roles Anywhere·SQS·IAM)은 시간 과금이 없다. `infra/prod`에 두면 AWS를 끌 때 집도 같이 멈춘다.
- **집 사이트 전용 큐를 두는 이유**: 클릭은 그 사이트의 DB에 반영돼야 한다. 사이트마다 큐·워커·DB가 한 묶음이다. 두 사이트가 한 큐를 나누는 것(워커 이동)은 0019의 실험이다.

### 2. VM과 k3s

- VM 두 대(Ubuntu 24.04 LTS 클라우드 이미지, cloud-init):
  - `lp-k3s-1`: 4 vCPU, 8GiB, 디스크 40GiB — k3s 서버 하나(단일 노드). 파드 슬롯은 HPA 최대 6 + 모니터링·ArgoCD에 충분하다(EKS 노드 3대의 요청 합 약 1.75 vCPU·3GiB).
  - `lp-pg-1`: 2 vCPU, 2GiB, 디스크 20GiB — PostgreSQL 16 하나.
  - 합 6 vCPU·10GiB. 16GB 서버에서 Proxmox 호스트에 여유를 남긴다.
  - CPU 형식은 `x86-64-v2-AES`로 적는다(공급자 기본 `qemu64`는 AES-NI 등이 빠진다. Proxmox 8의 기본값과 같다).
- **IP는 고정**(cloud-init `ip_config`): 공유기 DHCP 범위 밖의 두 주소. k3s 노드 IP와 `pg_hba`가 IP에 묶이므로 바뀌면 안 된다. 범위는 사람이 공유기 화면에서 확인한다(실행 단계 0).
- k3s: Ansible이 고정 버전 설치 스크립트(`INSTALL_K3S_VERSION=v1.37.1+k3s1`)로 설치한다. `--disable=traefik,servicelb`, kubeconfig는 맥의 `~/.kube/linkpulse-home.yaml`(저장소 밖)로 가져온다.
  **컨텍스트를 섞지 않는다**: 집 명령은 모두 `--kubeconfig ~/.kube/linkpulse-home.yaml`(또는 `KUBECONFIG`)를 명시한다. EKS 컨텍스트에 집 명령이 가면 운영 클러스터를 바꾼다.
- 공개 노출 없음: 앱 Service는 ClusterIP 그대로다. 스모크는 `kubectl port-forward`로 한다.
- **k3s VM은 ufw를 끈다**(k3s 문서 권장). 켜 두면 파드 → apiserver(`10.43.0.1` → 노드 6443, 출발지가 파드 IP), 맥 → 6443, Prometheus → node-exporter 9100·kubelet 10250, metrics-server → kubelet이 막힌다.
  집 LAN 안이고 공유기 NAT 뒤라 밖에서 닿지 않는다. 서비스 서버 VLAN 분리는 P7 과제다. ufw는 Postgres VM에만 켠다(설계 3).

### 3. 집 Postgres

- 별도 VM에 OS 패키지 `postgresql-16`(RDS 메이저 16과 같다). 클러스터 안 StatefulSet이 아니라 VM인 이유: RDS처럼 데이터가 클러스터 밖에 있어 클러스터를 다시 만들어도 남고, 0019의 논리 복제 대상이 된다.
- 설정(Ansible):
  - `ssl = on`(자체 서명 인증서) — 앱이 `sslmode=require`로 붙는다(AWS와 같은 사이트 값).
  - `wal_level = logical` — 0019 논리 복제 준비. 바꾸려면 재시작이 필요하므로 지금 켠다.
  - `listen_addresses`는 VM의 LAN 주소, `pg_hba`는 `hostssl linkpulse linkpulse <k3s 노드 IP>/32 scram-sha-256`만. 파드 트래픽은 k3s(flannel)가 노드 IP로 바꿔 나가므로 노드 IP 하나면 된다.
  - 방화벽(ufw): 5432는 k3s 노드에서만, 22는 LAN에서만.
- DB `linkpulse`의 **소유자는 역할 `linkpulse`**다. PostgreSQL 15부터 PUBLIC은 `public` 스키마에 CREATE 권한이 없어, `postgres` 소유로 만들면 앱의 기동 스키마 적용이 `permission denied for schema public`으로 실패한다(RDS는 마스터 사용자가 곧 앱 사용자라 문제가 없었다).
- DB 비밀번호는 맥에서 만들어(`openssl rand`) `~/.linkpulse-home/db-password`에 두고, Ansible이 그 파일을 읽어 역할에 설정한다. 비밀번호를 다루는 태스크는 `no_log: true` — 화면에 찍지 않는 것을 코드로 지킨다.
- 백업은 범위 밖이다(P7 "집 밖 저장소"). 0017의 집 데이터는 시험 데이터라 잃어도 된다 — 리스크 표에 적는다.

### 4. AWS 인증 — Roles Anywhere 사이드카

```
[app 컨테이너] --IMDSv2(127.0.0.1:9911)--> [서명 도우미 사이드카] --CreateSession(인증서 서명)--> Roles Anywhere --> 임시 자격증명(역할 linkpulse-home-app)
```

- **PKI**(`scripts/home-pki.sh`, openssl): 맥의 `~/.linkpulse-home/pki/`(저장소 밖)에 만든다.
  - CA: EC P-256, 5년, `CA:true`·`keyCertSign, cRLSign`, CN `linkpulse-home-ca`. **CA 개인 키는 맥 밖으로 나가지 않는다**(사람이 비밀번호 관리자에 따로 백업).
  - 말단: EC P-256, 1년, `CA:false`·`digitalSignature`, CN `linkpulse-home-app`. 개인 키는 k8s Secret으로만 클러스터에 들어간다.
  - 스크립트는 인증서 내용(`openssl x509 -text`)을 위 조건과 대조해 맞지 않으면 실패한다.
  - `--test-leaf <CN>`: **같은 CA**로 다른 CN의 시험 말단을 저장소 밖 경로에 발급한다(판정 (c)). 다른 CA로 만든 인증서는 트러스트 앵커 검증에서 먼저 거부돼, 신뢰 정책의 CN 조건을 시험하지 못한다.
- **`infra/home-aws`**
  - 트러스트 앵커: 소스 `CERTIFICATE_BUNDLE`, CA 인증서(공개 정보 — 경로를 로컬 tfvars로 받는다).
  - 역할 `linkpulse-home-app`: 신뢰 정책은 위 세 동작 + 조건 `aws:SourceArn` = 그 트러스트 앵커, `aws:PrincipalTag/x509Subject/CN` = `linkpulse-home-app`. 권한은 집 큐에 `sqs:SendMessage`·`ReceiveMessage`·`DeleteMessage`만.
  - 프로필: 그 역할 하나, 세션 1시간(기본).
  - 집 클릭 큐 `linkpulse-home-clicks` + DLQ: AWS 큐와 같은 설정(가시성 30초, 재전달 5회, 보존 4일/14일, SSE).
- **앱 차트 — 사이드카는 선택 기능**(`awsRolesAnywhere.enabled`, 기본 `false`)
  - 켜면 서명 도우미를 **네이티브 사이드카**(`initContainers` + `restartPolicy: Always`)로 넣는다. 앱 컨테이너보다 먼저 시작한다.
  - **사이드카에 probe를 두지 않는다.** 도우미는 127.0.0.1에 고정으로 바인드한다(`aws_signing_helper/serve.go` `LocalHostAddress = "127.0.0.1"`, 주소 플래그 없음). kubelet의 `tcpSocket`·`httpGet` probe는 노드 네트워크에서 파드 IP로 붙어 루프백에 닿지 못한다 — startupProbe를 두면 영영 성공하지 못해 앱이 시작되지 않는다.
    probe가 없으면 사이드카는 시작 즉시 "시작됨"으로 보고 앱을 띄운다. 도우미가 듣기 시작하기 전의 첫 SQS 호출은 실패할 수 있지만, 배처는 실패한 배치를 같은 ID로 다음 tick에 다시 보내고(`app/internal/clicks/batcher.go`) 워커도 다시 받는다. 도우미가 죽으면 `restartPolicy: Always`가 다시 띄운다.
    (이미지에 셸과 HTTP 도구가 있으면 파드 안에서 도는 `exec` probe를 쓸 수 있다 — 구현 때 이미지 내용물을 보고, 없으면 probe 없이 간다.)
  - 인자: `serve --certificate /var/run/secrets/rolesanywhere/cert.pem --private-key /var/run/secrets/rolesanywhere/key.pem`(Secret 마운트) `--trust-anchor-arn $(RA_TRUST_ANCHOR_ARN) --profile-arn $(RA_PROFILE_ARN) --role-arn $(RA_ROLE_ARN) --port 9911`.
    ARN 세 개만 사이트 ConfigMap에서 `configMapKeyRef`로 받는다(`envFrom` 통째가 아니다 — 사이드카 환경에 필요 없는 값이 들어가지 않게). **계정 ID가 든 ARN을 공개 저장소의 values 파일에 넣지 않는다.**
  - 앱 컨테이너에 `AWS_EC2_METADATA_SERVICE_ENDPOINT=http://127.0.0.1:9911`을 차트가 넣는다(사이드카와 포트가 한 묶음이라 차트가 정한다).
  - 이미지는 다이제스트로 고정한다. 자원 요청은 작게(10m·32Mi). 보안 컨텍스트는 앱과 같은 원칙(루트 아님, 읽기 전용 루트 파일 시스템, 권한 상승 없음, 모든 capability 제거).
  - 끄면 렌더 결과가 지금과 같아야 한다 — AWS 사이트는 Pod Identity를 그대로 쓴다(렌더 단언·변이로 지킨다).
- 자격증명은 파드 안에서만 닿는다(127.0.0.1). 클러스터 안 다른 파드가 가져갈 길이 없다.

### 5. `values-home.yaml`과 site-home

- `values-home.yaml`(공개 저장소, ARN·IP·비밀 없음): `monitoring.enabled: true`, `dbPasswordSecret.name: linkpulse-db-password`, `awsRolesAnywhere.enabled: true`(+ Secret 이름·이미지). 나머지(파드 크기·HPA·연결 예산·런타임 기본값)는 차트 기본값 그대로다.
  연결 예산(파드 11 × 최대 7 = 77)은 RDS 기준이지만 집 PG16 기본 `max_connections` 100 안이다 — 이 근거를 `values-home.yaml` 주석에 남긴다.
- `infra/home-k8s/charts/site-home`(site-aws에서 TargetGroupBinding을 뺀 모양): ConfigMap `linkpulse-site`
  - `PUBLIC_BASE_URL`(0017은 `http://localhost:8080` — port-forward 스모크용, 공개 주소는 P7), `LOG_LEVEL`, `SHORT_CODE_LENGTH`
  - `DB_HOST`(Postgres VM IP)·`DB_PORT`·`DB_NAME`·`DB_USER`·`DB_SSLMODE=require`, `DB_PASSWORD_SOURCE=file`, `DB_PASSWORD_FILE`(마운트 경로)
  - `AWS_REGION`, `CLICK_QUEUE_URL`(집 큐), `RA_TRUST_ANCHOR_ARN`·`RA_PROFILE_ARN`·`RA_ROLE_ARN`(Terraform이 `infra/home-aws` 출력에서 채운다 — 저장소에 남지 않는다)
- Secret `linkpulse-site-secret`의 `ADMIN_TOKEN_SHA256`은 AWS와 같은 방식(로컬 tfvars, `set_sensitive`)이다.

### 6. 비밀 다루기

| 비밀 | 만드는 곳 | 클러스터로 넣는 법 | 남지 않는 곳 |
| --- | --- | --- | --- |
| Proxmox API 토큰 | 사람(Proxmox 화면) | — (`infra/home` 로컬 tfvars) | 저장소·화면 |
| DB 비밀번호 | `openssl rand` → `~/.linkpulse-home/db-password` | 사람: `kubectl create secret generic linkpulse-db-password --from-file=password=$HOME/.linkpulse-home/db-password` | 저장소·화면·Terraform state |
| 말단 인증서·키 | `scripts/home-pki.sh` | 사람: `kubectl create secret generic linkpulse-ra-cert --from-file=cert.pem=$HOME/.linkpulse-home/pki/app.crt --from-file=key.pem=$HOME/.linkpulse-home/pki/app.key` | 저장소·Terraform state |
| CA 키 | 같은 스크립트 | 클러스터에 넣지 않는다 | 맥 밖 전부(사람이 오프라인 백업) |
| 관리자 해시 | AWS와 같은 값 | `infra/home-k8s` 로컬 tfvars, `set_sensitive` | 저장소·plan 출력 |

- DB 비밀번호·개인 키를 Terraform이 아니라 `kubectl`로 넣는 이유: Terraform으로 넣으면 S3 state에 평문으로 남는다.
- **Secret 키 이름은 고정이다**(`--from-file=<키>=<경로>`): DB는 `password` → 앱 `DB_PASSWORD_FILE=/var/run/secrets/linkpulse/password`(차트 `dbPasswordSecret.mountPath` 기본값), 인증서는 `cert.pem`·`key.pem` → 사이드카 인자 경로. 어긋나면 앱은 기동 때 "비밀번호 파일 읽기 실패"로, 사이드카는 시작하지 못하고 멈춘다.
  경로는 `$HOME`으로 쓴다(`--opt=~/…`의 `~`는 zsh·bash 모두 펼치지 않는다). 이미 있는 Secret을 바꿀 때(회전 드릴·인증서 갱신)는 `create`가 실패하므로 `kubectl create secret … --dry-run=client -o yaml | kubectl apply -f -`로 한다.
- `.gitignore`에 `~` 밖 경로는 필요 없지만, 실수 방지로 `infra/home*/**/*.pem`·`*.key`가 저장소에 들어오지 않게 패턴을 더한다(`*.pem`은 이미 있다).

### 7. 모니터링

- kube-prometheus-stack 91.8.2(AWS와 같은 버전). 집 값 파일은 AWS 값과 같은 원칙이다: 컨트롤 플레인·kube-proxy 수집과 기본 규칙 끔, 세 `*SelectorNilUsesHelmValues=false`(앱 차트 CR 선택), 보관 2일 `emptyDir`, kubelet `/metrics/resource` 수집.
  다른 점: Alertmanager 수신처는 `null`(외부 전달은 범위 밖), SNS·Pod Identity 없음.
- 앱 차트의 규칙·대시보드·ServiceMonitor는 그대로 쓴다(사이트 무관).

## 실행 단계

**A. 구현(에이전트) — 코드는 main에 머지돼야 ArgoCD가 집에 배포한다.**

1. **PKI 스크립트** `scripts/home-pki.sh`: CA·말단 만들기, 조건 대조(설계 4), 이미 있으면 덮어쓰지 않음, 말단만 다시 발급(`--renew-leaf`), 같은 CA로 시험 말단(`--test-leaf <CN>`).
   → 검증: scratchpad 경로로 돌려 `openssl x509 -text` 대조가 통과한다. 조건을 하나씩 어긴 인증서(`CA:true` 말단, `digitalSignature` 없음, SHA1)를 스크립트의 검사가 거부한다(변이). `shellcheck`.
2. **`infra/home-aws`**: 트러스트 앵커·프로필·역할·정책, 집 큐·DLQ·재전달 허용 정책. S3 backend(키 `home-aws/terraform.tfstate`).
   → 검증: `terraform validate`, tfsec 새 지적 없음. 역할 신뢰 정책 문서를 렌더해 세 동작과 두 조건이 있는지 단언(`terraform console` 또는 plan의 정책 JSON). 큐 설정이 `infra/prod/sqs.tf`와 같은지 대조.
3. **`infra/home`**: bpg/proxmox v0.114.0, 클라우드 이미지 받기(날짜 고정 URL·체크섬), VM 두 대(설계 2), 출력(IP·이름). **`infra/home/ansible`**: 역할 `common`(qemu-guest-agent·ufw·시간 동기), `postgres`(설계 3), `k3s`(설계 2, kubeconfig 가져오기).
   → 검증: `terraform validate`, `ansible-playbook --syntax-check`, `ansible-lint`. Postgres 설정 템플릿을 렌더해 `ssl`·`wal_level`·`pg_hba` 한 줄·DB 소유자를 단언. 비밀번호 태스크에 `no_log: true`. ufw 태스크가 Postgres 호스트에만 걸리고 k3s 호스트에는 `ufw disable`인지 단언. VM `cpu.type` = `x86-64-v2-AES`.
4. **앱 차트**: `awsRolesAnywhere` 선택 기능(설계 4), `values-home.yaml`(설계 5).
   → 검증(`scripts/check-charts.sh` 확장):
   - AWS 렌더(`values-aws.yaml`)에 사이드카·`AWS_EC2_METADATA_SERVICE_ENDPOINT`가 **없다**(지금과 같다).
   - 집 렌더(`values-home.yaml`): `initContainers`에 서명 도우미가 있고 `restartPolicy: Always`, **`tcpSocket`·`httpGet` probe가 없다**, 이미지가 다이제스트, 인자가 `$(RA_*_ARN)`이고 그 env가 `configMapKeyRef` 세 개뿐(`envFrom` 없음), 인증서 Secret(`cert.pem`·`key.pem`)이 읽기 전용 마운트, 앱 env에 `AWS_EC2_METADATA_SERVICE_ENDPOINT=http://127.0.0.1:9911`, DB 비밀번호 Secret 디렉터리 마운트(subPath 없음), ServiceMonitor·규칙 있음, `values-home.yaml`·`site-home` 차트에 `arn:aws` 문자열 없음.
   - **AWS 렌더가 이번 변경 전과 같다**: main 워크트리의 `helm template … -f values-aws.yaml`과 브랜치 렌더의 `diff`가 0(판정 (a)·(g)의 근거).
   - kubeconform(집 렌더 포함).
   - 변이: 사이드카를 일반 컨테이너로(`restartPolicy` 제거), 사이드카에 `tcpSocket` startupProbe 추가, 기본값을 `enabled: true`로, env를 사이드카 없이 남김, ARN을 values에 하드코딩, 사이드카에 `envFrom` 추가 — 각각 단언이 실패한다.
5. **`infra/home-k8s`**: helm 릴리스(kps·ArgoCD·Image Updater 같은 버전), `charts/site-home`, gitops(기존 차트, `valueFiles: [values-home.yaml]`), kps 집 값 파일.
   → 검증: `terraform validate`, `helm lint`·kubeconform(site-home), kps 집 값 렌더 단언(선택자 `{}`·컨트롤 플레인 끔·`null` 수신처), `check-charts.sh`에 편입.
6. **문서**: ADR 0010(집 사이트 — 세 층, 사이트 큐, Roles Anywhere 사이드카, 비밀 경로), 런북 `docs/runbooks/home-site.md`(켜기·끄기·컨텍스트 주의·인증서 갱신·DB 비밀번호 회전·VM 재시작), `infra/README.md`·`README.md` 한 줄.
   → 검증: 문서의 이름·경로·명령이 코드와 같다(`grep`).
7. **코드 검토 1라운드(claude-ide) → 커밋·PR·머지 [사람].** 머지 후 CI(GHCR 푸시) 성공.

**B. 집 창 [사람] — 단계마다 무엇·왜·통과 기준을 먼저 알리고, 실행 기록은 `commands-explained.md`(gitignore)에 남긴다.**

0. **사전 확인**: `pveversion`, 노드 이름, 저장소 이름과 `import` 콘텐츠 허용, 공유기의 게이트웨이·DHCP 범위 → 고정 IP 두 개 정하기, Proxmox API 토큰 만들기(최소 권한), `brew install ansible`, 맥용 서명 도우미 v1.8.5(릴리스 체크섬 확인, 저장소 밖 경로).
   → 검증: 토큰으로 `curl …/api2/json/version`이 200. 고른 두 IP에 `ping` 응답 없음.
1. `scripts/home-pki.sh` → `infra/home-aws` plan(에이전트) → apply(사람).
   → 검증: 맥에서 서명 도우미 `credential-process`로 `aws sts get-caller-identity`가 `assumed-role/linkpulse-home-app/…`. **같은 CA로 서명한 CN `linkpulse-home-other` 시험 인증서(`--test-leaf`)는 `AccessDenied`로 거부된다**(트러스트 앵커 오류가 아니라 역할을 맡지 못한 오류여야 신뢰 정책의 CN 조건을 시험한 것이다). 집 큐 두 개가 있다.
2. `infra/home` plan(에이전트) → apply(사람).
   → 검증: plan `… to add, 0 to change, 0 to destroy`. 두 VM에 SSH, `cloud-init status` = done.
3. Ansible(사람): `common` → `postgres` → `k3s`.
   → 검증: 노드 `Ready`·버전 `v1.37.1+k3s1`·Traefik/ServiceLB 없음, CoreDNS Ready, 시험 파드에서 `kubernetes.default` 접근 성공(파드 → apiserver 경로). k3s 노드에서 `psql "sslmode=require …"` 접속 성공·`SHOW wal_level` = `logical`·`\l`에서 `linkpulse` 소유자 = `linkpulse`. 다른 LAN 기기에서 5432 접속 거부.
4. `infra/home-k8s` plan(에이전트) → apply(사람) → Secret 두 개(`kubectl create secret`, 사람, 설계 6의 고정 키 이름).
   - 이 순서인 이유: `infra/home-k8s`가 네임스페이스 `linkpulse`를 만든다(AWS `infra/k8s`와 같은 모양). 먼저 Secret을 만들면 네임스페이스가 없어 실패하고, 사람이 네임스페이스를 먼저 만들면 Terraform이 "already exists"로 실패한다.
   - site·gitops 릴리스의 `wait`는 앱 파드를 기다리지 않아 apply는 끝난다. **Secret을 만들기 전의 앱 파드 `ContainerCreating`(마운트할 Secret 없음)은 정상이다** — Secret이 생기면 kubelet이 다시 시도해 뜬다.
   → 검증: ArgoCD `Synced/Healthy`, 앱 파드 2개 Ready(사이드카 포함 2/2), 이미지 다이제스트 = GHCR `main` 다이제스트, 기동 로그 `password_source: file`·`click_queue: sqs`, `go_sched_gomaxprocs_threads` = 1.
5. **판정 창**(아래 판정표): 스모크 → 모니터링 확인 → 회전 드릴 → 다시 만들기.

## 판정 (집 창)

| # | 조건 | 통과 기준 |
| --- | --- | --- |
| (a) | 같은 이미지·같은 차트 | 집 앱 이미지 다이제스트 = 그때의 GHCR `main` 다이제스트. ArgoCD 소스가 같은 경로·같은 리비전, 값 파일만 `values-home.yaml`. AWS 렌더는 이번 변경 전과 같다(check-charts) |
| (b) | 클릭 파이프라인(Roles Anywhere·집 큐·집 DB) | port-forward 스모크: 관리자 링크 생성 → 리다이렉트 3번 → 15초 안에 통계 클릭 3. 파드 기동 30초 뒤부터 `linkpulse_clicks_publish_errors_total`·`linkpulse_clicks_apply_errors_total`이 늘지 않는다(기동 직후 사이드카가 듣기 전의 실패는 재시도로 흡수된다 — 설계 4). 집 큐 DLQ 0 |
| (c) | 인증 경계 | 같은 CA로 서명한 CN `linkpulse-home-other` 인증서로 `CreateSession` → `AccessDenied`(앵커 오류 아님). 파드 밖에서 9911에 닿지 않는다(다른 파드에서 앱 파드 IP:9911 `curl` 실패 — 도우미가 127.0.0.1에만 바인드) |
| (d) | `file` 비밀번호 회전 | 순서: ① Postgres `ALTER ROLE linkpulse PASSWORD …`(새 연결부터 옛 비밀번호 실패) → ② Secret 교체(`--dry-run=client -o yaml \| kubectl apply -f -`, 재기동 없음). 앱은 인증 실패 때 파일을 다시 읽는다(backoff 상한 5초, `dbcreds` `maxBackoff`).
잴 것: ①부터 회복까지 `/readyz` 실패 창. 기준은 kubelet의 Secret 파일 반영(최대 약 1분 + 캐시) + 5초 안. 파드 재시작 0 |
| (e) | 모니터링 | Prometheus 대상 모두 up(앱 2·kubelet·node-exporter·ArgoCD 등), linkpulse 규칙 그룹 4개 `ok`, Grafana 대시보드가 그려진다. HPA `TARGETS`가 `<unknown>`이 아니다(metrics-server) |
| (f) | 처음부터 다시 만들기 | 사전 단계: 집 kubeconfig를 명시해 `kubectl -n argocd delete application linkpulse --wait`(finalizer `resources-finalizer.argocd.argoproj.io` — AWS `full-destroy-prod.sh`와 같은 이유)와 finalizer가 있는 monitoring CR 정리 확인.
그다음 `infra/home-k8s` destroy → `infra/home` destroy → 같은 순서로 다시 만들어 (b)가 다시 통과한다. VM을 다시 만들면 SSH 호스트 키가 바뀌므로 `ssh-keygen -R <IP>` 후 Ansible을 돈다. 걸린 시간과 사람이 한 일을 적는다(P7 이전의 기준선) |
| (g) | AWS 무영향 | `infra/prod`·`infra/k8s`를 이번에 바꾸지 않았다(diff에 없음). AWS 차트 렌더 단언 통과 |

## 리스크/롤백

| 리스크 | 완화 | 롤백 |
| --- | --- | --- |
| 집 명령이 EKS 컨텍스트로 가서 운영 클러스터를 바꿈 | 집 명령마다 `--kubeconfig ~/.kube/linkpulse-home.yaml` 명시, 런북·README 절차에 박는다. 인프라가 꺼져 있을 때 작업한다 | AWS는 IaC로 다시 맞춘다(`infra/k8s` apply) |
| 고정 IP가 공유기 DHCP와 겹침 | 단계 0에서 범위 확인·`ping` | `infra/home` 변수로 IP 변경 → apply |
| Proxmox 저장소에 `import` 콘텐츠가 꺼져 이미지 받기 실패 | 단계 0에서 확인 | `iso` 형식으로 받고 디스크로 가져오기(공급자 문서의 다른 예) |
| 서명 도우미가 앱보다 늦게 떠 시작 직후 SQS 실패 | 네이티브 사이드카(앱보다 먼저 시작). probe는 둘 수 없다(루프백 바인드). 배처·워커 재시도가 흡수한다 | 30초 넘게 계속 실패하면 사이드카 로그 확인 후 `rollout restart` |
| k3s VM에 방화벽이 없음 | 집 LAN 안·공유기 NAT 뒤라 밖에서 닿지 않는다. k3s 문서 권장 | VLAN 분리(P7), 필요하면 k3s 문서의 ufw 규칙 세 줄 |
| VM을 다시 만들어 SSH 호스트 키가 바뀌어 Ansible이 멈춤 | (f)와 런북에 `ssh-keygen -R <IP>` | 같은 명령 |
| 말단 인증서 만료 → SQS 인증 실패 → 클릭 발행 실패가 쌓여 유실(상한 10만 코드) | 1년 유효, 만료일을 런북에 적고 `--renew-leaf` 절차. 0017은 외부 알림이 없으므로 만료 30일 전 확인을 런북 정기 점검에 넣는다 | 새 말단 발급 → Secret 교체 → `rollout restart` |
| CA 개인 키 유출 | 맥 밖으로 내보내지 않음, 클러스터에 넣지 않음 | 트러스트 앵커 비활성화(즉시 모든 집 인증서 무효) → 새 CA → 앵커 교체 |
| CA 개인 키 분실 | 사람이 오프라인 백업 | 새 CA로 앵커 교체(집 사이트만 영향) |
| Roles Anywhere에 요금이 있음 | 구현 때 가격 페이지 확인, 있으면 plan에 적고 사람 확인 | `infra/home-aws` destroy(집은 SQS 없이 못 뜬다 — 그때는 집을 끈다) |
| k3s 단일 노드라 노드 장애 = 집 사이트 장애 | 0017의 목적은 기반 확인이다. 다중 노드는 P7에서 정한다 | VM 재시작, 안 되면 (f) 절차로 다시 만들기 |
| 집 Postgres 데이터 유실(백업 없음) | 시험 데이터만 둔다 | 다시 만들기(스키마는 앱이 적용) |
| kps 기본 규칙이 k3s에서 시끄러움(컨트롤 플레인 대상 없음 등) | AWS와 같이 컨트롤 플레인 수집·규칙 끔 | 시끄러운 규칙 이름을 결과에 적고 집 값 파일에서 끈다 |
| 서명 도우미 이미지가 바뀌거나 사라짐 | 다이제스트 고정 | 고정 다이제스트를 GHCR로 복제해 두는 것은 다음에 검토 |

## 검토 반영 로그

<!-- /plan-merge가 라운드별로 기록. 형식: [rN] 리뷰어#번호 지적요약 → 반영|기각 — 사유 -->

r1 리뷰는 claude-ide 하나다(codex 사용 불가). `reviewed-revision: 1` 일치.

- [r1] claude-ide#1 사이드카 9911 TCP startupProbe는 성공할 수 없다(도우미는 127.0.0.1 고정 바인드, kubelet probe는 노드 네트워크에서 파드 IP로 붙는다) → 반영 — probe를 두지 않는다(설계 4). 시작 순서는 네이티브 사이드카, 첫 호출 실패는 배처·워커 재시도에 맡긴다. 이미지에 셸·HTTP 도구가 있으면 exec probe를 쓸 수 있다는 것을 구현 확인 항목으로 남겼다. 판정 (b)를 "기동 30초 뒤부터 발행 실패가 늘지 않는다"로, 리스크 표를 고쳤다. A4 단언·변이에 "tcpSocket·httpGet probe 없음"을 더했다
- [r1] claude-ide#2 B4 순서(Secret → home-k8s apply)는 네임스페이스가 없어 실행할 수 없다 → 반영 — 순서를 "home-k8s apply → Secret → Ready"로 바꾸고, Secret 전의 `ContainerCreating`이 정상이라고 적었다. 네임스페이스는 AWS와 같은 모양으로 Terraform이 만든다
- [r1] claude-ide#3 k3s VM 방화벽 규칙이 없다 → 반영 — k3s 문서(1차 출처 확인: "It is recommended to turn off ufw")대로 k3s VM은 ufw를 끄고, 이유(LAN 안·NAT 뒤, VLAN은 P7)를 적었다. ufw는 Postgres VM에만. B3에 CoreDNS Ready·파드 → `kubernetes.default`를 더했다
- [r1] claude-ide#4 CN 거부 시험이 다른 CA 인증서면 앵커 검증에서 먼저 거부돼 CN 조건을 시험하지 못하고, 시험 인증서·맥용 도우미 설치 경로가 없다 → 반영 — `home-pki.sh --test-leaf <CN>`(같은 CA), 판정 (c)·B1의 기대 오류를 `AccessDenied`로, B0에 맥용 도우미 v1.8.5(체크섬) 설치를 더했다
- [r1] claude-ide#5 (f) destroy가 Application finalizer에 걸린다, VM 재생성 뒤 SSH 호스트 키 불일치 → 반영 — (f)·런북 끄기에 `kubectl -n argocd delete application linkpulse --wait`와 monitoring CR 확인, `ssh-keygen -R`을 넣었다
- [r1] claude-ide#6 Secret 키 이름이 검사 없이 사람 명령에 달려 있다 → 반영 — 설계 6에 키(`password`·`cert.pem`·`key.pem`)와 경로를 고정하고, `$HOME` 사용, 바꿀 때 `--dry-run=client -o yaml | kubectl apply -f -`를 적었다
- [r1] claude-ide#7 집 DB 소유자가 정해져 있지 않다(PG15+ public 스키마 CREATE) → 반영 — 설계 3에 소유자 = `linkpulse`, B3·A3 검증에 소유자 확인
- [r1] claude-ide 제안: 사이드카 env는 ARN 세 키만 `configMapKeyRef` → 반영 — 설계 4·A4 단언·변이(`envFrom` 추가 시 실패)
- [r1] claude-ide 제안: "AWS 렌더가 변경 전과 같다"를 main 대 브랜치 렌더 diff 0으로 → 반영 — A4 검증
- [r1] claude-ide 제안: `arn:aws` 단언 범위를 `values-home.yaml`·`site-home`으로 → 반영
- [r1] claude-ide 제안: VM `cpu.type`을 `qemu64`가 아니라 `x86-64-v2-AES`로 → 반영 — 설계 2·A3
- [r1] claude-ide 제안: 연결 예산 근거(PG16 기본 `max_connections` 100 > 77)를 `values-home.yaml` 주석에 → 반영 — 설계 5
- [r1] claude-ide 제안: 비밀번호 Ansible 태스크에 `no_log: true` → 반영 — 설계 3·A3
- [r1] claude-ide 제안: 회전 드릴의 순서와 잴 것, backoff 상한 5초 → 반영 — 판정 (d)

**판정(r2):** 결함 7개와 제안 7개를 모두 반영했다. 결함 1은 리뷰가 낸 두 방법 중 (ii)를 기본으로, (i)을 구현 확인 항목으로 남겼다. 방화벽(결함 3)은 k3s 1차 출처를 확인한 뒤 반영했다. 남은 쟁점이 없다 — "블로커가 없으면 라운드를 더 열지 않는다"에 따라 `approved`.
