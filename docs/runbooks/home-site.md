# Runbook — 집 사이트(Proxmox·k3s) 켜기·끄기·운영

집 Proxmox 서버의 두 번째 사이트(plan 0017)를 처음부터 세우고, 판정하고, 내리고, 운영하는 절차다.
설계와 그 이유는 [ADR 0010](../adr/0010-home-site.md)·[plan 0017](../plans/0017-home-site-foundation/plan.md)에 있다.

- **표기**: 이 문서의 `terraform apply/destroy`·`ansible-playbook site.yml`·`kubectl create/apply/delete`·Proxmox 화면 작업은 **사람이** 한다(AGENTS.md §2).
  각 단계는 **무엇 → 왜 → 통과 기준** 순서다. 실행 기록은 plan 디렉터리의 `commands-explained.md`(git 밖)에 남긴다.
- **셸은 macOS 기본 zsh 기준이다.** 비밀 입력은 `read -rs "VAR?프롬프트: "`(zsh 문법)로 받고 화면에 찍지 않는다. 터미널 출력을 대화에 붙여도 되게 계정 ID는 `sed`로 가린다.
- **저장소 밖에 두는 것**(맥, `~/.linkpulse-home/`): `pki/`(CA·말단 키), `db-password`, `bin/aws_signing_helper`, `aws-config`. kubeconfig는 `~/.kube/linkpulse-home.yaml`.
  CA 키(`pki/ca.key`)는 비밀번호 관리자 등 오프라인에도 백업한다(분실하면 트러스트 앵커를 새 CA로 바꿔야 한다).

## 컨텍스트 주의 — 집 명령은 `hk`로만

EKS kubeconfig와 섞이면 집 명령이 운영 클러스터를 바꾼다. 집 터미널에서는 먼저 이 함수를 정의하고 **`kubectl` 대신 `hk`만** 쓴다.
`--context linkpulse-home`을 함께 주므로 파일을 잘못 가리키면 명령이 실패한다(바뀌지 않는다).

```bash
hk() { kubectl --kubeconfig "$HOME/.kube/linkpulse-home.yaml" --context linkpulse-home "$@"; }
```

`infra/home-k8s`의 Terraform 공급자도 같은 파일·같은 컨텍스트만 쓴다(`providers.tf`). 그래도 AWS 인프라가 꺼져 있을 때 작업하는 것이 가장 안전하다.

## 순서 한눈에

| 단계 | 스택 | 하는 것 | 끄기(역순) |
| --- | --- | --- | --- |
| 0 | — | 공유기·Proxmox 확인, 토큰, 맥 도구 | — |
| 1 | `scripts/home-pki.sh` → `infra/home-aws` | CA·말단, Roles Anywhere·집 큐 | **늘 켜 둔다**(시간 과금 없음) |
| 2 | `infra/home` | VM 두 대 | `terraform destroy` |
| 3 | `infra/home/ansible` | k3s·PostgreSQL 16 | (VM과 함께 사라진다) |
| 4 | `infra/home-k8s` + Secret 두 개 | kps·ArgoCD·Image Updater·사이트 값·Application | Application 삭제 → `terraform destroy` |

---

## 0. 사전 확인(처음 한 번)

**0-1. 공유기** — 관리 화면에서 게이트웨이 주소와 **DHCP 범위**를 보고, 범위 밖의 주소 두 개(k3s·Postgres)를 고른다.
왜: VM IP는 고정이다(k3s 노드 IP·`pg_hba`가 이 주소에 묶인다). DHCP가 같은 주소를 다른 기기에 주면 충돌한다.

```bash
route -n get default | grep gateway
ping -c 2 -t 2 <고른 k3s IP>; ping -c 2 -t 2 <고른 pg IP>
```
통과: 두 `ping` 모두 `100.0% packet loss`(아무도 쓰지 않는 주소).

**0-2. Proxmox 서버 셸**(웹 화면 → 노드 → Shell)

```bash
pveversion; hostname; pvesm status; grep -A4 '^[a-z]*:' /etc/pve/storage.cfg
```
- `hostname` = `infra/home`의 `node_name`. `pvesm status`의 이름 = `image_datastore_id`(기본 `local`)·`vm_datastore_id`(기본 `local-lvm`).
- 이미지 저장소의 허용 콘텐츠에 **Import**가 없으면 웹 화면 Datacenter → Storage → `local` → Edit → Content에 Import를 더한다. 왜: 클라우드 이미지를 `import` 형식으로 받아 VM 디스크로 가져온다(SSH 없이 API만). PVE 8.4 미만이면 알려 준다(파일 이름 규칙이 다르다).

**0-3. Proxmox API 토큰**(같은 셸). 사용자·역할·토큰을 만든다. 권한은 이 스택이 쓰는 것만 준다(이미지 받기·VM 만들기·켜고 끄기).

```bash
pveum user add terraform@pve --comment "linkpulse infra/home"
pveum role add LinkpulseTerraform -privs "Datastore.AllocateSpace,Datastore.AllocateTemplate,Datastore.Audit,SDN.Use,Sys.Audit,Sys.Modify,VM.Allocate,VM.Audit,VM.Config.CDROM,VM.Config.CPU,VM.Config.Cloudinit,VM.Config.Disk,VM.Config.HWType,VM.Config.Memory,VM.Config.Network,VM.Config.Options,VM.PowerMgmt"
pveum acl modify / --users terraform@pve --roles LinkpulseTerraform
pveum user token add terraform@pve linkpulse --privsep 0
```
- 마지막 명령이 토큰 값을 **한 번만** 보여 준다. 그 출력은 대화에 붙이지 말고 비밀번호 관리자에 `terraform@pve!linkpulse=<값>` 형태로 저장한다.
- `Sys.Audit`·`Sys.Modify`·`Datastore.AllocateTemplate`는 이미지 받기(download-url)가, `SDN.Use`는 브리지(`vmbr0`) 사용이 요구한다(공급자 문서).
- apply가 `403 ... Permission check failed (<경로>, <권한>)`로 멈추면 그 권한 하나만 더한다: `pveum role modify LinkpulseTerraform -append 1 -privs <권한>`. 더한 것은 이 목록과 ops-gotchas에 남긴다.

**0-4. 맥** — 도구와 토큰.

```bash
brew install ansible            # community.general(ufw)·community.postgresql이 들어 있다
ls ~/.ssh/id_ed25519.pub || ssh-keygen -t ed25519
mkdir -p ~/.linkpulse-home/bin && chmod 700 ~/.linkpulse-home
U=https://rolesanywhere.amazonaws.com/releases/1.8.5/Aarch64/MacOS/Sonoma/aws_signing_helper
curl -fsSL -o ~/.linkpulse-home/bin/aws_signing_helper "$U" && \
  echo "ac4b656cd83ffde5a6e9e8f2317ffb90e036c9bb704cc80faa6aee414b55915a  $HOME/.linkpulse-home/bin/aws_signing_helper" | shasum -a 256 -c - && \
  chmod +x ~/.linkpulse-home/bin/aws_signing_helper
read -rs "PROXMOX_VE_API_TOKEN?Proxmox 토큰(terraform@pve!linkpulse=...): "; echo; export PROXMOX_VE_API_TOKEN
curl -fsSk -o /dev/null -w '%{http_code}\n' -H "Authorization: PVEAPIToken=${PROXMOX_VE_API_TOKEN}" https://<Proxmox IP>:8006/api2/json/version
```
통과: `shasum`이 `OK`(체크섬은 2026-10-16에 같은 곳의 `.sha256`과 대조한 값, 서명 도우미 v1.8.5 맥 arm64), 마지막 줄 `200`.
토큰은 이 터미널에만 있다(파일에 쓰지 않는다). 새 터미널이면 `read -rs`부터 다시 한다.

---

## 1. PKI → `infra/home-aws`

```bash
cd ~/mini-project/linkpulse
scripts/home-pki.sh
cd infra/home-aws && terraform init -backend-config=../prod/backend.hcl
terraform test                       # 신뢰 정책·큐 설정 단언(AWS에 닿지 않는다)
terraform plan -out=tfplan
```
- 무엇: 자체 CA·말단 인증서를 만들고(이미 있으면 그대로), 트러스트 앵커·프로필·역할·정책과 집 큐·DLQ·재전달 허용 정책을 만든다.
- 통과(plan): `Plan: 7 to add, 0 to change, 0 to destroy.` 이유 — 처음 만드는 스택이라 바꾸거나 지울 것이 없어야 한다. `change`·`destroy`가 있으면 멈춘다(다른 state를 가리키는 것이다).
- `terraform apply tfplan`(사람).

**1-검증.** 맥에서 서명 도우미로 역할을 맡아 본다. 같은 CA의 다른 CN 인증서는 거부돼야 한다(판정 (c)의 앞 절반).

```bash
cd ~/mini-project/linkpulse
scripts/home-pki.sh --test-leaf linkpulse-home-other
TA=$(terraform -chdir=infra/home-aws output -raw trust_anchor_arn); PA=$(terraform -chdir=infra/home-aws output -raw profile_arn); RA=$(terraform -chdir=infra/home-aws output -raw role_arn)
H=$HOME/.linkpulse-home; P=$H/pki
cat > $H/aws-config <<EOF
[profile home-app]
region = ap-northeast-2
credential_process = $H/bin/aws_signing_helper credential-process --certificate $P/app.crt --private-key $P/app.key --trust-anchor-arn $TA --profile-arn $PA --role-arn $RA
[profile home-other]
region = ap-northeast-2
credential_process = $H/bin/aws_signing_helper credential-process --certificate $P/test-linkpulse-home-other.crt --private-key $P/test-linkpulse-home-other.key --trust-anchor-arn $TA --profile-arn $PA --role-arn $RA
EOF
AWS_CONFIG_FILE=$H/aws-config aws sts get-caller-identity --profile home-app --query Arn --output text | sed -E 's/[0-9]{12}/<계정>/'
AWS_CONFIG_FILE=$H/aws-config aws sts get-caller-identity --profile home-other 2>&1 | sed -E 's/[0-9]{12}/<계정>/g' | tail -3
aws sqs list-queues --queue-name-prefix linkpulse-home --query QueueUrls --output text | sed -E 's/[0-9]{12}/<계정>/g'
```
통과:
- 첫 줄 `arn:aws:sts::<계정>:assumed-role/linkpulse-home-app/<인증서 일련번호>`.
- 둘째는 `AccessDenied`가 든 오류. **트러스트 앵커 오류(서명·신뢰할 수 없는 인증서)가 아니라 역할을 맡지 못한 오류여야** 신뢰 정책의 CN 조건을 시험한 것이다.
- 셋째에 `linkpulse-home-clicks`·`linkpulse-home-clicks-dlq` 두 개.
- 판정 (c)가 끝나면 시험 말단을 지운다: `rm $P/test-linkpulse-home-other.*`.

## 2. `infra/home` — VM 두 대

```bash
cd ~/mini-project/linkpulse/infra/home
cp terraform.tfvars.example terraform.tfvars   # 0단계 값으로 고친다(처음 한 번)
terraform init -backend-config=../prod/backend.hcl
terraform test
terraform plan -out=tfplan
```
- 통과(plan): `Plan: 3 to add, 0 to change, 0 to destroy.`(이미지 1 + VM 2).
- `terraform apply tfplan`(사람). 이미지 약 600MB를 Proxmox가 받는다(몇 분).

```bash
K=$(terraform output -raw k3s_ip); G=$(terraform output -raw pg_ip)
ssh ubuntu@$K cloud-init status --wait; ssh ubuntu@$G cloud-init status --wait
```
통과: 둘 다 `status: done`. 처음 접속이면 호스트 키를 받는다(`accept-new`). VM을 다시 만든 뒤라면 먼저 `ssh-keygen -R $K; ssh-keygen -R $G`.

## 3. Ansible — 공통 → PostgreSQL → k3s

```bash
[ -e ~/.linkpulse-home/db-password ] || (umask 077; openssl rand -hex 32 | tr -d '\n' > ~/.linkpulse-home/db-password)
wc -c < ~/.linkpulse-home/db-password            # 64
cd ~/mini-project/linkpulse/infra/home
terraform output -raw ansible_inventory > ansible/inventory.yml
cd ansible && ansible-playbook -i localhost, tests/render.yml && ansible all -m ping && ansible-playbook site.yml
```
- 무엇: 게스트 에이전트·시간 동기 → PostgreSQL 16(ssl·`wal_level=logical`·`pg_hba` 한 줄·ufw) → k3s(Traefik·ServiceLB 끔, ufw 끔) → kubeconfig를 맥에 쓴다.
- 비밀번호는 hex 64자다(공백·특수문자가 없다 — Ansible `lookup`은 끝 공백을, 앱은 끝 줄바꿈을 지운다). 화면에 찍지 않는다.
- 통과: 마지막 `PLAY RECAP`의 두 호스트 모두 `failed=0`.

**3-검증.**

```bash
hk() { kubectl --kubeconfig "$HOME/.kube/linkpulse-home.yaml" --context linkpulse-home "$@"; }
hk get nodes -o wide
hk -n kube-system get deploy,ds | grep -iE 'traefik|svclb' || echo "Traefik·ServiceLB 없음"
hk -n kube-system rollout status deploy/coredns --timeout=60s
hk run apitest --rm -i --restart=Never --image=curlimages/curl:8.16.0 -- curl -sk https://kubernetes.default.svc/version | grep gitVersion
G=$(terraform -chdir=.. output -raw pg_ip)
hk run pgtest --rm -i --restart=Never --image=postgres:16.10 --env=PGHOST=$G -- sh -c 'read -r PGPASSWORD || true; export PGPASSWORD; psql "dbname=linkpulse user=linkpulse sslmode=require" -Atc "select current_user, ssl from pg_stat_ssl where pid = pg_backend_pid()"' < ~/.linkpulse-home/db-password
ssh ubuntu@$G "sudo -u postgres psql -Atc 'show wal_level' -c \"select datname, pg_get_userbyid(datdba) from pg_database where datname = 'linkpulse'\""
nc -vz -G 3 $G 5432
```
통과:
- 노드 `Ready`, `VERSION v1.37.1+k3s1`. Traefik·ServiceLB 없음. CoreDNS `successfully rolled out`.
- `apitest`: `"gitVersion": "v1.37.1+k3s1"` — 파드 → apiserver 경로(ufw를 끈 이유)가 열려 있다.
- `pgtest`: `linkpulse|t` — k3s 노드(파드는 노드 IP로 나간다)에서 TLS로 붙었다. 비밀번호는 표준 입력으로만 들어가 명령줄·파드 정의에 남지 않는다.
- `ssh` 줄: `logical`과 `linkpulse|linkpulse`(DB 소유자 = linkpulse).
- `nc`: 맥(다른 LAN 기기)에서 5432는 **실패**(`Operation timed out` 또는 `refused`). ufw가 k3s 노드만 받는다.

## 4. `infra/home-k8s` → Secret 두 개

```bash
cd ~/mini-project/linkpulse/infra/home-k8s
cp terraform.tfvars.example terraform.tfvars   # tfstate_bucket(= ../prod/backend.hcl의 bucket), 관리자 해시(= infra/k8s와 같은 값)
terraform init -backend-config=../prod/backend.hcl
terraform plan -out=tfplan
```
- 통과(plan): `Plan: 7 to add, 0 to change, 0 to destroy.`(네임스페이스 2 + helm 릴리스 5). ARN·큐 URL·관리자 해시는 `(sensitive value)`로 보여야 한다.
- `terraform apply tfplan`(사람). 이 스택이 네임스페이스 `linkpulse`를 만든다 — Secret은 **그다음에** 넣는다(먼저 만들면 네임스페이스가 없어 실패한다).
- apply 직후 앱 파드가 `ContainerCreating`(마운트할 Secret 없음)인 것은 정상이다. Secret이 생기면 kubelet이 다시 시도한다.

```bash
hk -n linkpulse create secret generic linkpulse-db-password --from-file=password=$HOME/.linkpulse-home/db-password
hk -n linkpulse create secret generic linkpulse-ra-cert --from-file=cert.pem=$HOME/.linkpulse-home/pki/app.crt --from-file=key.pem=$HOME/.linkpulse-home/pki/app.key
```
키 이름(`password`·`cert.pem`·`key.pem`)은 고정이다 — 앱은 `/var/run/secrets/linkpulse/password`를, 사이드카는 `cert.pem`·`key.pem`을 읽는다. 경로는 `$HOME`으로 쓴다(`--from-file=x=~/…`의 `~`는 펼쳐지지 않는다).

**4-검증.**

```bash
hk -n argocd get application linkpulse
hk -n linkpulse get pods
hk -n linkpulse get pods -o jsonpath='{range .items[*]}{.status.containerStatuses[?(@.name=="app")].imageID}{"\n"}{end}'
T=$(curl -fsS "https://ghcr.io/token?scope=repository:naerrow/linkpulse:pull" | jq -r .token); curl -fsSI -H "Authorization: Bearer $T" -H "Accept: application/vnd.oci.image.index.v1+json" https://ghcr.io/v2/naerrow/linkpulse/manifests/main | grep -i docker-content-digest
hk -n linkpulse logs deploy/linkpulse -c app | grep -oE '"(password_source|click_queue)":"[a-z]+"' | sort -u
hk -n linkpulse port-forward deploy/linkpulse 9090:9090 >/dev/null & sleep 2; curl -s localhost:9090/metrics | grep '^go_sched_gomaxprocs_threads'; kill %1
```
통과: `Synced  Healthy`, 파드 2개 `2/2 Running`(앱 + 사이드카), 파드 `imageID`의 다이제스트 = GHCR `main` 다이제스트(판정 (a)), `password_source":"file"`·`click_queue":"sqs"`, `go_sched_gomaxprocs_threads 1`.

---

## 5. 판정 창

**(b) 클릭 파이프라인** — 관리자 링크 생성 → 리다이렉트 3번 → 15초 안에 클릭 3.

```bash
cd ~/mini-project/linkpulse
hk -n linkpulse port-forward svc/linkpulse 8080:8080 >/dev/null &
read -rs "ADMIN_TOKEN?관리자 토큰: "; echo
C=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{"url":"https://example.com/home-smoke"}' localhost:8080/api/links | jq -r .code); echo "code=$C"
for i in 1 2 3; do curl -s -o /dev/null -w '%{http_code} ' localhost:8080/$C; done; echo
sleep 15; curl -s localhost:8080/api/links/$C | jq .clicks
hk -n linkpulse port-forward deploy/linkpulse 9090:9090 >/dev/null & sleep 2; curl -s localhost:9090/metrics | grep -E '^linkpulse_clicks_(publish|apply)_errors_total'
AWS_PAGER= aws sqs get-queue-attributes --queue-url "$(terraform -chdir=infra/home-aws output -raw click_dlq_url)" --attribute-names ApproximateNumberOfMessages --query Attributes
kill %1 %2; unset ADMIN_TOKEN
```
통과: `302 302 302`, `3`. 발행·반영 오류 카운터는 1분 뒤 다시 봐도 늘지 않는다(기동 직후 사이드카가 듣기 전의 실패는 재시도로 흡수된다). DLQ `"0"`.

**(c) 인증 경계** — 1-검증의 `home-other`가 `AccessDenied`였으면 앞 절반은 통과다. 뒤 절반: 다른 파드에서 앱 파드 IP:9911에 닿지 않는다.

```bash
IP=$(hk -n linkpulse get pod -l app.kubernetes.io/name=linkpulse -o jsonpath='{.items[0].status.podIP}')
hk run ra-reach --rm -i --restart=Never --image=curlimages/curl:8.16.0 -- curl -s -m 3 -X PUT http://$IP:9911/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60'; echo "exit=$?"
```
통과: 연결 거부(`exit` 0이 아님, 토큰 문자열이 나오지 않음). 도우미는 127.0.0.1에만 바인드한다.

**(d) `file` 비밀번호 회전 드릴** — ① Postgres 비밀번호를 바꾸고(새 연결부터 옛 비밀번호 실패) ② Secret을 바꾼다(재기동 없음). 앱은 인증 실패 때 파일을 다시 읽는다(backoff 상한 5초).
다른 탭에서 `/readyz`를 1초마다 기록해 둔다:

```bash
hk -n linkpulse port-forward svc/linkpulse 8080:8080 >/dev/null &
while :; do printf '%s %s\n' "$(date -u +%T)" "$(curl -s -m 2 -o /dev/null -w '%{http_code}' localhost:8080/readyz)"; sleep 1; done | tee ~/.linkpulse-home/readyz-drill.log
```
본 탭에서(시각을 적어 둔다):

```bash
(umask 077; openssl rand -hex 32 | tr -d '\n' > ~/.linkpulse-home/db-password)
date -u +%T; (cd ~/mini-project/linkpulse/infra/home/ansible && ansible-playbook site.yml --limit postgres)   # ① ALTER ROLE
date -u +%T; hk -n linkpulse create secret generic linkpulse-db-password --from-file=password=$HOME/.linkpulse-home/db-password --dry-run=client -o yaml | hk apply -f -   # ②
hk -n linkpulse get pods     # RESTARTS 0
```
잴 것: ①부터 `/readyz`가 다시 200이 될 때까지의 실패 창. 통과: kubelet의 Secret 파일 반영(최대 약 1분 + 캐시) + 5초 안, 파드 재시작 0.
(①과 ② 사이에 기존 연결은 계속 쓰이므로 실패 창이 0일 수도 있다 — 그대로 적는다.) 옛 비밀번호는 파일에 남기지 않는다(덮어썼다).

**(e) 모니터링**

```bash
hk -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9091:9090 >/dev/null & sleep 2
curl -s localhost:9091/api/v1/targets | jq -r '.data.activeTargets[] | "\(.labels.job) \(.health)"' | sort | uniq -c
curl -s localhost:9091/api/v1/rules | jq -r '.data.groups[] | select(.name | startswith("linkpulse")) | "\(.name) \([.rules[].health] | unique)"'
hk -n linkpulse get hpa; kill %1
hk -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80   # 브라우저 localhost:3000 → linkpulse-overview 대시보드
```
통과: 모든 대상 `up`(앱 2·kubelet·node-exporter·ArgoCD 등), linkpulse 규칙 그룹 4개 `["ok"]`, HPA `TARGETS`가 `<unknown>`이 아님, 대시보드가 그려진다.
시끄러운 기본 규칙(k3s에 없는 대상)이 있으면 이름을 결과에 적는다 — 집 값 파일에서 끈다.

**(f) 처음부터 다시 만들기** — 아래 "끄기" 전체 → 2·3·4단계를 다시 → (b). 시작·끝 시각과 사람이 한 일을 적는다(P7 이전의 기준선).

**(g) AWS 무영향** — 이 변경이 `infra/prod`·`infra/k8s`를 바꾸지 않았고(diff), `scripts/check-charts.sh`의 "AWS 렌더가 기준 브랜치와 같다"가 통과했다.

---

## 끄기 — 집 사이트 내리기(`infra/home-aws`는 남긴다)

```bash
hk() { kubectl --kubeconfig "$HOME/.kube/linkpulse-home.yaml" --context linkpulse-home "$@"; }
cd ~/mini-project/linkpulse
K=$(terraform -chdir=infra/home output -raw k3s_ip); G=$(terraform -chdir=infra/home output -raw pg_ip)   # destroy 뒤에는 출력이 없다
hk -n argocd delete application linkpulse --wait --timeout=5m
terraform -chdir=infra/home-k8s destroy
hk get ns
terraform -chdir=infra/home destroy
ssh-keygen -R "$K"; ssh-keygen -R "$G"
```
- Application을 먼저 지우는 이유: finalizer(`resources-finalizer.argocd.argoproj.io`)가 ArgoCD로 앱 리소스를 지운다 — ArgoCD가 먼저 사라지면 Application이 영영 지워지지 않아 destroy가 멈춘다(AWS `full-destroy-prod.sh`와 같은 이유).
- 통과: home-k8s `Destroy complete! Resources: 7 destroyed.`, `hk get ns`에 `linkpulse`·`monitoring`·`argocd`가 없음(`Terminating`으로 남으면 그 네임스페이스의 finalizer 걸린 CR을 본다: `hk api-resources --verbs=list --namespaced -o name | xargs -n1 hk get -n <ns> --ignore-not-found`).
  home `Destroy complete! Resources: 3 destroyed.`
- `infra/home-aws`는 지우지 않는다(시간 과금 없음, 집을 다시 켤 때 그대로 쓴다). 집 데이터(Postgres VM)는 사라진다 — 0017은 시험 데이터다.
- `ssh-keygen -R`: VM을 다시 만들면 호스트 키가 바뀌어 Ansible(`accept-new`)이 거부한다.

## 운영

**인증서 갱신**(말단 1년). 만료 30일 전에 한다 — 집 알림이 밖으로 나가지 않으므로 **매달 첫 주에 만료일을 본다**: `openssl x509 -in ~/.linkpulse-home/pki/app.crt -noout -enddate`.
만료되면 클릭 발행이 실패하고(`linkpulse_clicks_publish_errors_total` 증가) 대기 배치가 상한(10만 코드)을 넘으면 유실된다.

```bash
scripts/home-pki.sh --renew-leaf
hk -n linkpulse create secret generic linkpulse-ra-cert --from-file=cert.pem=$HOME/.linkpulse-home/pki/app.crt --from-file=key.pem=$HOME/.linkpulse-home/pki/app.key --dry-run=client -o yaml | hk apply -f -
hk -n linkpulse rollout restart deploy/linkpulse && hk -n linkpulse rollout status deploy/linkpulse
```

**DB 비밀번호 회전** — 판정 (d)와 같은 순서(파일 → `ansible-playbook site.yml --limit postgres` → Secret `apply`). 비밀번호의 원본은 `~/.linkpulse-home/db-password` 하나다.

**VM 재시작** — Proxmox 화면의 Reboot(게스트 에이전트가 있어 정상 종료된다). k3s·PostgreSQL은 부팅 때 스스로 뜬다. 뒤에 `hk get nodes`가 `Ready`, 앱 파드 `2/2`를 본다.
Ansible을 돌리기 전(게스트 에이전트 없음)에는 Shutdown 대신 Stop을 쓴다 — Shutdown은 에이전트를 기다리다 시간이 초과된다.

**CA 유출** — 즉시 트러스트 앵커를 끈다(모든 집 인증서가 그 순간 무효). 그다음 새 CA로 바꾼다.

```bash
aws rolesanywhere disable-trust-anchor --trust-anchor-id "$(terraform -chdir=infra/home-aws output -raw trust_anchor_arn | awk -F/ '{print $NF}')" --query trustAnchor.enabled
mv ~/.linkpulse-home/pki ~/.linkpulse-home/pki.revoked-$(date +%Y%m%d) && scripts/home-pki.sh
terraform -chdir=infra/home-aws apply   # 앵커의 인증서가 바뀐다(enabled도 true로 되돌린다)
```
이어서 위 "인증서 갱신"의 Secret 교체·재시작을 한다. CA 키를 잃어버린 경우도 `disable` 없이 같은 순서다(집 사이트만 영향).
