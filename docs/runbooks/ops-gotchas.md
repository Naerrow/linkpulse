# 작업 중 걸린 함정 모음 (증상 → 원인 → 해결)

실제로 막혔던 것만 적는다. 추측·일반론은 넣지 않는다.
각 항목은 **다시 만났을 때 30초 안에 빠져나오는 것**이 목적이다.

---

## G-1. `gh pr merge`가 권한 오류 — 계정이 둘이다

**증상**
```
GraphQL: jaeheeparkpb does not have the correct permissions to execute `MergePullRequest`
```

**원인** — `gh`에 GitHub 계정이 **두 개** 로그인돼 있고, 활성 계정이 저장소 소유자가 아니다.
`jaeheeparkpb`는 이 저장소에 `READ` 권한만 있다.

```bash
gh auth status                              # 로그인된 계정 전부와 Active 표시
gh repo view --json owner,viewerPermission  # 소유자와 내 권한
```

**해결**
```bash
gh auth switch --user Naerrow
```

**재발 방지** — PR 생성·머지·코멘트 전에 활성 계정을 먼저 본다. 증상이 머지에만 나는 것이 아니라
**PR 생성도 막힌다.** 읽기 명령(`gh pr view`, `gh pr checks`)은 READ로도 되기 때문에 중간까지는 멀쩡해 보인다.

**2차 발생 (2026-10-06, Step 2 PR)** — `git push`는 성공하고 `gh pr create`만 실패했다.
**git과 gh가 서로 다른 자격증명을 쓰기 때문**이다. 브랜치는 원격에 올라가 있으니 `git status -sb`로는
정상으로 보이고, PR이 없다는 사실은 따로 확인해야 드러난다.
```bash
git status -sb | head -1                                   # 푸시만 확인 — PR 유무는 모른다
gh pr list --state all --head "$(git branch --show-current)"  # [] 면 PR이 없는 것이다
```
→ **`gh`로 쓰기 동작을 하기 전에는 활성 계정 확인을 명령 앞에 붙인다.**
```bash
gh auth switch --user Naerrow && gh repo view --json viewerPermission
# "ADMIN" 또는 "WRITE" 를 확인한 뒤에만 gh pr create / merge / comment
```

**3차 발생 (2026-10-07, k6 PR)** — `GraphQL: must be a collaborator (createPullRequest)`. 메시지 모양은 달라도 같은 원인이다
(활성 계정 `jaeheeparkpb`). 그날 앞선 PR 두 개는 `Naerrow`로 잘 만들어졌다 — **세션 중간에 활성 계정이 바뀔 수 있다.**
→ 위 확인을 "세션 시작 때 한 번"이 아니라 **`gh` 쓰기 명령마다** 붙인다.

---

## G-2. `terraform init`/`plan`이 자격증명 만료로 실패

**증상**
```
Error: No valid credential sources found
Error: failed to refresh cached credentials, create oauth2 token:
       login session has expired, please reauthenticate
```

**원인** — SSO 세션 만료. S3 백엔드 접근에 자격증명이 필요하다.
`terraform validate`·`fmt`는 **자격증명 없이도 통과**하므로 이것만 보고 "괜찮다"고 판단하면 안 된다.

**해결**
```bash
aws sso login
```

**재발 방지** — `plan`을 돌려야 하는 단계(1-1 검증, apply 직전 게이트) 앞에서 먼저 재인증한다.

---

## G-3. zsh에서 `#` 주석이 명령어 인자로 들어간다

**증상**
```
aws: [ERROR]: Unknown options: target, 0개면, 원문을, 못, 받는다, #
```
붙여넣은 명령 뒤의 `# 설명`이 그대로 인자가 됐다. 주석에 괄호가 있으면 메시지가 달라진다 —
zsh가 `(...)`를 **glob qualifier**로 읽기 때문이다. 같은 원인이니 같은 칸에서 찾는다.
```
zsh: unknown file attribute: ^-      # docker compose stop db   (포트 미노출)
```

**원인** — zsh **대화형 셸**은 기본적으로 `#`을 주석으로 보지 않는다(`INTERACTIVE_COMMENTS` 미설정).
bash와 다르고, 스크립트 파일로 실행할 때와도 다르다.

**해결** — 둘 중 하나
```bash
setopt interactive_comments        # 이 셸에서만. ~/.zshrc에 넣으면 영구
```
또는 붙여넣을 때 `#` 뒤 주석을 지운다.

**⚠️ 위험한 점** — 이 실패는 **조용히 넘어간다.** 여러 명령을 한 번에 붙여넣으면 그중 하나만
에러 나고 나머지는 정상 실행돼, **검증 항목 하나가 실행되지 않은 채 "다 통과했다"고 읽기 쉽다.**
실제로 2026-09-09 1-3 검증에서 *"광역 rule target 확인"* 한 줄이 이렇게 누락됐다
(target이 0개면 회전 원문을 한 건도 못 받는 항목이었다).
→ **여러 명령을 붙여넣었으면 출력 개수가 명령 개수와 맞는지 센다.**

**같은 모양의 재발 (2-4 통합 테스트 실행)** — 블록 중간의 `cd app`이 *"이미 `app/`에 있어서"*
실패했고, `cd app && go test ...`가 `&&`로 묶여 있어 **게이트 테스트가 아예 실행되지 않았다.**
그런데 앞뒤 명령(`docker run`, `docker stop`)은 정상 출력을 내서 블록 전체가 돈 것처럼 보였다.
→ **상대 경로 `cd`를 블록 안에 넣지 않는다.** 저장소 루트를 절대경로로 한 번 잡거나
`go test -C app ...`처럼 **명령 자체에 디렉터리를 준다.**

**재발 (2026-10-07, 30초 회수)** — 에이전트가 준 블록의 `terraform plan -out=… # 기대: …`가
`Error: Too many command line arguments`로 실패했다. 그다음 줄 `apply`는 plan 파일이 없어 실패했지만,
**마지막 줄 `gh workflow run deploy.yml`은 정상 실행돼** 회수 전 설정(30초)으로 배포가 한 번 헛돌았다.
이미 G-3에 있는 함정인데 **명령을 건네는 쪽이 다시 만들었다.**
→ **남에게 건네는 명령 블록에는 인라인 주석을 넣지 않는다.** 기대값은 블록 밖 문장으로 쓴다.

---

## G-4. `terraform plan`에 `2 to destroy` — 브랜치가 안 머지돼 있었다

**증상** — 신규 리소스만 추가하는 브랜치인데 무관한 리소스 2개가 삭제 대상으로 뜬다.
```
Plan: 23 to add, 0 to change, 2 to destroy
  # aws_sns_topic_subscription.alarms_sms[0] will be destroyed
  # (because aws_sns_topic_subscription.alarms_sms is not in configuration)
Warning: Value for undeclared variable "alarm_sms_number"
```

**원인** — **프로덕션에 apply는 됐는데 코드가 main에 머지되지 않은 브랜치**가 있었다
(`docs/p4d2-rotation-outages-3-4`). state에는 리소스가 있고 현재 브랜치 config에는 선언이 없으니
Terraform이 "설정에서 사라졌다 = 지워라"로 읽는다.

**해결** — 그 브랜치를 main에 머지 → 현재 브랜치 rebase → `plan` 재실행으로 `0 destroy` 확인.

**재발 방지 — 이게 핵심이다**
- **apply한 것은 반드시 머지한다.** "apply했으니 됐다"가 이 함정을 만든다.
- `Warning: Value for undeclared variable`은 **같은 원인의 조기 신호다.** tfvars에 값이 남아 있는데
  변수 선언이 없다는 뜻이라, 그 브랜치에 코드가 안 들어와 있다는 것을 `plan` 전에 알려 준다.
- **apply 전 하드 게이트: `0 destroy`가 아니면 멈춘다.**

---

## G-5. `describe-alarms`의 OK 액션 필드명은 `OKActions`다

**증상** — JMESPath로 `OkActions`를 뽑으면 전부 `null`이라 정상 알람이 불합격으로 보인다.

**원인** — CloudWatch API 응답 필드는 **`OKActions`**(K 대문자). JMESPath는 대소문자를 구분하고,
없는 필드는 에러가 아니라 `null`을 준다.

**해결** — `--query 'MetricAlarms[].{Ok:OKActions}'`

**재발 방지** — 검증 스크립트가 "전부 불합격"을 내면 대상을 의심하기 전에 **쿼리 필드명부터 확인한다.**
조용히 `null`이 되는 API 필드는 검증을 통째로 무의미하게 만든다.

---

## G-6. `git rev-parse --short`는 리비전을 하나만 받는다

**증상** — 두 SHA를 한 번에 비교하려다 아무것도 못 얻는다.
```
$ git rev-parse --short HEAD origin/main
fatal: Needed a single revision
```
`--short=7`로 길이를 명시해도 같다. 각각 따로 부르면 정상이다.

**원인** — `--short`는 단일 리비전 출력 전용이다. 인자가 둘이면 개수 검사에서 먼저 실패한다.

**해결**
```bash
git rev-parse --short HEAD; git rev-parse --short origin/main   # 따로
git rev-parse HEAD origin/main                                   # 또는 --short 없이(전체 SHA)
```

**⚠️ 위험한 점 — 검증이 조용히 무의미해진다.** 이 명령을 `echo`나 파이프라인 안에 넣어 두면
에러가 stderr로 빠지고 **본문은 빈 줄로 보인다.** 실제로 2026-09-09에 *"머지·push가 됐는지"*
확인하던 두 번이 이렇게 빈 출력을 냈고, **PR이 머지 안 된 상태를 "동기화됨"으로 넘길 뻔했다.**

**대신 쓸 것** — 브랜치 동기 상태는 이게 낫다.
```bash
git status -sb | head -1
#  ## main...origin/main            <- ahead/behind 표시 없으면 동기화됨
#  ## main...origin/main [ahead 1]  <- push 안 됨
```

**교훈(G-5와 같다)** — 검증 명령 자체가 틀리면 검증은 통과처럼 보인다.
**빈 출력을 "이상 없음"으로 읽지 않는다.** 기대한 값이 나왔는지를 본다.

---

## G-7. 통합 테스트가 compose가 아닌 **호스트의 다른 Postgres**에 붙었다

**증상** — `docker compose up -d db`로 DB를 띄우고 `localhost:5432`로 통합 테스트를 돌렸는데
연결 자체가 인증 실패한다. DB 컨테이너는 분명히 `Started`이고 healthcheck도 통과한다.
```
관리 연결 핑 실패: failed SASL auth: FATAL: password authentication failed
for user "linkpulse" (SQLSTATE 28P01)
```

**원인** — `docker-compose.yml`의 `db` 서비스에는 **`ports:`가 없다.** 컨테이너 내부 5432만 열려
있고 호스트로는 publish되지 않는다(`app`만 `8080`을 매핑한다 — app은 compose 네트워크에서
서비스명 `db`로 붙으므로 publish가 필요 없다). 따라서 `localhost:5432`는 **compose의 DB가 아니라
호스트에 이미 떠 있던 다른 Postgres**다. 그쪽 `linkpulse` 비밀번호가 다르니 28P01이 난 것이다.

```bash
docker compose port db 5432     # :0  ← publish 안 됨. 매핑이 있으면 0.0.0.0:5432 가 나온다
docker ps --format '{{.Names}}\t{{.Ports}}'   # 5432/tcp (→로 시작하는 매핑이 없다)
```

**해결** — 통합 테스트는 **비표준 포트의 일회용 컨테이너**를 쓴다. compose의 dev DB와 섞지 않는다.
```bash
docker run -d --rm --name linkpulse-it-pg -e POSTGRES_PASSWORD=itpw -p 55432:5432 postgres:16-alpine
LINKPULSE_IT_DSN='postgres://postgres:itpw@localhost:55432/postgres?sslmode=disable' \
  go test -tags=integration -v -timeout 5m ./internal/db/
docker stop linkpulse-it-pg
```

**⚠️ 위험한 점 — 인증 실패가 오히려 다행이었다.** 이 테스트는 대상 서버에 `CREATE ROLE`과
`CREATE DATABASE`를 한다. 비밀번호가 우연히 맞았다면 **남의 Postgres에 역할과 DB를 만들었을
것이고**, 실패도 나지 않아 눈치채지 못했을 것이다.

**재발 방지** — 두 겹으로 막았다.
1. `rotation_integration_test.go`가 DSN 호스트를 검사해 `localhost`/`127.0.0.1`/유닉스 소켓이
   아니면 즉시 중단한다(운영 RDS DSN을 실수로 넣는 경로를 끊는다).
2. 포트를 5432가 아닌 **55432**로 둔다. 기본 포트를 피하면 "이미 떠 있던 무언가"에 붙지 않는다.

**교훈** — `docker compose up -d db`가 성공했다는 것은 **컨테이너가 떴다**는 뜻이지
**내가 그 컨테이너에 닿는다**는 뜻이 아니다. 접속 실패를 보면 서비스 상태보다 **포트 매핑**을 먼저 본다.

---

## G-8. `aws logs tail --since`가 빈 결과 — 배포가 범위보다 먼저 끝났다

**증상** — 배포 뒤 기동 로그를 확인하려고 `aws logs tail … --since 10m | grep conn_max_lifetime`을 돌렸는데
**아무것도 안 나온다.** 에러도 없다. 두 번 연속 같은 일이 났다(30분 범위도 한 번 빗나갔다).

**원인** — 새 태스크의 기동 로그는 **배포가 끝난 시각이 아니라 태스크가 뜬 시각**에 찍힌다. 배포 완료를 기다렸다가
확인하러 오면 그 사이 시간이 이미 `--since` 범위를 넘어 있다(이번엔 기동 01:08 → 조회 01:31).
`grep`이 0줄이면 *"로그가 없다"*와 *"설정이 안 먹었다"*가 구별되지 않는다.

**해결** — 범위를 넉넉히 잡고 마지막 줄만 본다.
```bash
aws logs tail /ecs/linkpulse-prod-app --region ap-northeast-2 --since 2h | grep conn_max_lifetime | tail -4
```
배포 시각은 `gh run list --workflow deploy.yml --limit 3`의 `updatedAt`으로 바로 확인된다.

**같은 축 — 셸 변수는 터미널을 따라가지 않는다.** 회전 직전에 `T0=…`를 잡은 터미널이 아닌 새 창에서
`--start-time "$T0"`을 쓰면 `invalid int value: ''`로 실패한다. 태스크별 로그 스트림을 조회할 때는
`--start-time 0`이면 충분하다(스트림 자체가 그 태스크의 기동 이후다).


---

## G-9. destroy 도중 컴퓨터를 꺼서 state 저장 실패 + 잠금이 남았다

**증상** — 다음 날 `./scripts/full-destroy-prod.sh --drop-dev-db`를 다시 돌리자 첫 plan이 잠금 오류로 멈췄다(2026-10-08).
```
Error: Error acquiring the state lock
... StatusCode: 412 ... PreconditionFailed
Lock Info:
  ID:        d20e6b95-29f2-1df2-f74d-d95989622af0
  Operation: OperationTypeApply
  Created:   2026-10-07 07:58:00 UTC
```
전날 destroy 창의 마지막 출력은 이랬다.
```
Error: ... dial tcp: lookup ec2.ap-northeast-2.amazonaws.com: no such host
Error: Failed to save state
  ... the state has been written to the file "errored.tfstate" in the current working directory.
  Running "terraform apply" again at this point will create a forked state
Error: Error releasing the state lock
```

**원인** — 10-07 destroy가 도는 중에 컴퓨터를 껐다. 네트워크가 끊겨 AWS 호출(DNS 조회)이 실패했다.
terraform은 마지막 state를 S3에 올리지 못해 `infra/prod/errored.tfstate`에 따로 저장했고, 잠금도 풀지 못했다.
그래서 **S3의 state는 destroy 이전 것이고, 실제 AWS와 맞는 최신 state는 로컬 `errored.tfstate`에만 있다**
(serial 110, 106개 중 25개 남음 — NAT·EIP는 삭제됐고 RDS·ALB·VPC·ECS 서비스는 남았다).
이 상태에서 다른 작업을 하면 state가 갈라진다(forked state).

**해결** — 순서가 중요하다. 잠금을 풀고, 로컬 state를 먼저 올린 뒤에 destroy를 이어 간다.
```bash
ps -axo pid,command | grep -i '[t]erraform'   # 아무것도 안 나와야 한다
cd infra/prod
terraform force-unlock d20e6b95-29f2-1df2-f74d-d95989622af0
terraform state push errored.tfstate
cd ../.. && ./scripts/full-destroy-prod.sh --drop-dev-db
```
destroy가 끝나면 `errored.tfstate`를 지운다. 남겨 두면 나중에 같은 파일을 다시 올리는 사고가 날 수 있다.

**재발 방지** — destroy·apply 중에는 컴퓨터를 끄거나 잠자기에 들게 하지 않는다. 끝까지(`Destroy complete!`) 본다.
**잠금 오류가 나면 바로 `force-unlock`하지 말고, `infra/prod/errored.tfstate`가 있는지부터 본다.** 있으면 그게 최신 state다.
잠금만 풀고 S3의 옛 state로 다시 돌리면 이미 지운 리소스를 기준으로 계획이 세워진다.

---

## G-10. 로컬 k6 setup이 401 — `.env`가 compose 기본 관리자 해시를 덮어쓴다

**증상** — plan 0012 로컬 검증에서 `capacity.js`의 setup이 첫 링크 생성부터 실패했다(2026-10-08).
```
Error: 링크 생성 실패: 401 {"error":{"code":"unauthorized","message":"관리자 토큰이 필요합니다"}}
```

**원인** — `docker-compose.yml`의 `ADMIN_TOKEN_SHA256: ${ADMIN_TOKEN_SHA256:-<dev-token 해시>}`는 기본값일 뿐이다.
저장소 루트 `.env`(gitignore)에 `ADMIN_TOKEN_SHA256`이 있으면 compose가 그 값을 쓴다. 그래서 `dev-token`은 틀린 토큰이 된다.
README가 `-e ADMIN_TOKEN=dev-token`을 기본처럼 안내한 게 잘못이었다.

**해결** — 로컬도 운영과 같이 토큰을 `read -rs`로 받는다(`load/k6/README.md` 로컬 검증).
```bash
read -rs "ADMIN_TOKEN?관리자 토큰: "; echo
ADMIN_TOKEN="$ADMIN_TOKEN" k6 run -e BASE_URL=http://localhost:8080 ... load/k6/capacity.js
```

**재발 방지** — 로컬 401이면 먼저 `.env`에 `ADMIN_TOKEN_SHA256`이 있는지 본다(값은 찍지 않는다).
1이 나오면 `.env`의 해시가 쓰이니 그 토큰을 넣는다.
```bash
grep -c '^ADMIN_TOKEN_SHA256=' .env
```

---

## G-11. `collect-metrics.sh`의 CloudWatch 열이 전부 `-` — CLI가 시각을 로컬 시간대로 준다

**증상** — plan 0012 측정 뒤 수집 표에서 ALB·ECS·RDS 열이 모두 비었다(2026-10-12). 같은 표의 Logs Insights 열(`in_use`)은 정상이었다.

**원인** — CloudWatch API가 CBOR 프로토콜이라, AWS CLI v2가 받은 epoch를 **로컬 시간대**로 바꿔 찍는다.
```
"Timestamps": ["2026-10-12T09:36:00+09:00", ...]
```
스크립트는 앞 19자만 잘라 UTC로 읽어서 모든 분이 9시간 어긋났다. Logs Insights의 `bin(1m)`은 서비스가 UTC 문자열로 줘서 영향이 없었다.

**해결** — 오프셋(`+09:00`·`Z`·`-05:00`)을 반영해 UTC epoch로 바꾼다(`collect-metrics.sh`의 `cwts`). 원본 응답은 `<CSV 이름>-cw.json`으로 남긴다.

**재발 방지** — AWS CLI 출력의 시각은 문자열 그대로 비교하지 않는다. 오프셋까지 해석하거나 `--start-time`처럼 epoch로 맞춘다.

---

## G-12. Logs Insights `stats sum(x) as x` — 별칭이 필드 이름과 같으면 그 열이 결과에서 빠진다

**증상** — 같은 수집에서 풀 대기 열(`wait_count`·`wait_ms` 합계)만 비었다. 원본 응답에 `in_use_max`·`in_use_avg`·`tasks`는 있고 대기 합계는 아예 없었다.

**원인** — 쿼리가 `stats sum(wait_count) as wait_count, sum(wait_ms) as wait_ms`였다. 별칭이 원래 필드 이름과 같은 열만 빠졌다(`max(in_use) as in_use_max`처럼 이름이 다른 열은 나왔다).

**해결** — 별칭을 `waits`·`waited_ms`로 바꿨다. 다음 수집에서 값이 채워졌다.

**재발 방지** — Logs Insights 집계 별칭은 원래 필드 이름과 다르게 짓는다.

---

## G-13. 새 터미널 탭에 붙여 넣은 명령 앞뒤에 `[200~`·`~`가 붙었다

**증상** — 새 탭에 두 줄을 붙여 넣자 이렇게 됐다(2026-10-12).
```
zsh: bad pattern: [200~cd
while>
```
첫 줄 `cd`가 실패해 홈 디렉터리에 남았고, 마지막이 `done~`이 돼 zsh가 `done`을 기다렸다. 아무것도 실행되지 않았다.

**원인** — 터미널의 붙여넣기 표시 문자(bracketed paste)가 셸에 처리되지 않고 그대로 들어갔다.

**해결** — Ctrl+C로 빠져나온 뒤 **`cd` 없이 절대경로만 쓰는 한 줄**로 다시 붙여 넣었다. 계속 섞이면 `printf '\e[?2004l'`로 그 탭의 표시 모드를 끈다.

**재발 방지** — 건네는 명령은 한 줄·절대경로로 쓴다(G-3의 "상대 경로 `cd`를 블록에 넣지 않는다"와 같은 원칙).
붙여 넣은 뒤 `while>`·`quote>` 같은 이어쓰기 프롬프트가 보이면 실행되지 않은 것이다.

---

## G-14. 차트 스키마 검사가 CR의 모르는 필드·오타를 통과시켰다

**증상** — plan 0014 차트 검사(`scripts/check-charts.sh`)를 만들며 변이 검사를 했다(2026-10-13). 두 가지가 나왔다.
1. CRDs-catalog의 `ImageUpdater` 스키마로 검사하자 올바른 CR이 실패했다.
   ```
   ImageUpdater linkpulse is invalid: ... at '/spec': missing property 'namespace'
   ```
2. 거꾸로, 고정 버전 CRD에서 뽑은 스키마로 바꾸자 `spec`에 없는 필드(`namespace`)나 오타(`manifestTarget`)를 넣어도 **통과했다.**

**원인**
1. 카탈로그 스키마가 옛 Image Updater 버전 것이었다. 고정한 차트(1.3.2)의 CRD에는 `spec.namespace`가 아예 없다.
2. CRD의 `openAPIV3Schema`에는 `additionalProperties: false`가 없다. API 서버도 모르는 필드를 오류 없이 **버린다** — 오타가 클러스터에서도 조용히 무시된다.

**해결** — `ImageUpdater` 스키마는 고정 버전 CRD에서 만들어 저장소에 둔다(`scripts/schemas/`). 만들 때 properties가 있는 모든 객체에 `additionalProperties: false`를 넣는다(`scripts/schemas/crd-to-jsonschema.py`). TGB·Application은 카탈로그 스키마가 이미 모르는 필드를 거부해서 그대로 쓴다(오타 변이로 확인).

**재발 방지** — 스키마 검사를 새로 붙이면 **틀린 입력(모르는 필드·오타)으로 실패하는지 먼저 본다.** 통과만 보면 검사가 아무것도 막지 않는 것을 모른다. 차트 버전을 올리면 그 버전의 CRD로 스키마를 다시 만든다(`check-charts.sh` 머리 주석).

## G-15. 고부하에서 파드 하나의 앱 로그가 2분 동안 통째로 빠졌다 — 정합성이 틀린 것처럼 보였다

**증상** — plan 0014 `hot` 6400 rps(2026-10-13 06:43~06:44 UTC). `collect-metrics.sh`의 정합성 절이 이렇게 나왔다.
```
recorded 2083594 − dropped 0 = 2083594
applied 2087746 + unknown 0 = 2087746
DB 합계(teardown) = 2189636
```
세 값이 다 달라 클릭이 사라진 것처럼 보였다. 하지만 DB 합계는 k6의 `redirects_302`(2,189,634)와 맞았다. 분 단위로 k6 요청 수와 로그 `recorded_clicks`를 비교했다. 06:43에 53,395건, 06:44에 63,590건이 빠졌다 — 파드 6개 중 **하나의 몫**(06:44 요청 ÷ 6 = 63,704)이다. 06:45부터는 맞았다. 그 창의 로그 레코드 수도 요청 수보다 약 10만 적었다. 같은 6400의 `spread`·`read`는 빠지지 않았다.

**원인** — 클릭이 아니라 **로그가 빠졌다.** 그 파드는 초당 약 1,060줄을 썼다. Fluent Bit tail 입력은 `Mem_Buf_Limit 5MB`, 메모리 버퍼만 쓰고, 읽은 위치를 저장하는 DB가 없다.
증거(`load/k6/results/raw/0014-fluentbit.txt`, 측정 직후 수집)는 이렇다.
- Fluent Bit 두 개 모두 **재시작 0회**다. 재시작해서 파일 끝부터 다시 읽었다는 가설은 빠진다.
- 6400 부하에서 두 노드의 tail 입력이 **초마다 `paused (mem buf overlimit)` → `resume`을 반복했다.** 남은 마지막 40줄이 `read` 6400의 끝(07:15~07:16)이다. 06:43 당시의 줄은 남지 않았다.
- kubelet은 컨테이너 로그를 **10Mi마다 회전**하고 5개까지 둔다(`containerLogMaxSize`·`containerLogMaxFiles`). 6400에서는 파드당 약 10초마다 회전한다. 같은 로그에 `handle rotation()` 줄이 있다.

입력이 멈춘 사이 파일이 회전되면, Fluent Bit는 회전된 파일을 `Rotate_Wait`(기본 5초) 동안만 더 읽는다. 그 안에 다 읽지 못하면 남은 부분을 놓친다. 06:43의 한 파드가 이렇게 빠진 것으로 본다.

**해결** — 측정 판정은 DB 합계와 `302 ≤ DB ≤ 302 + 실패 요청`으로 했다(`0014-eks.md`). 로그로 계산한 열은 그 두 분 동안 약 1/6 적게 잡혔다고 문서에 적었다.

**재발 방지** — **로그 합계로 정합성을 판정하는 방식은 고부하에서 믿지 않는다.** 불일치가 나면 먼저 DB 합계와 k6 302 수를 비교하고, 분 단위로 k6 요청 수 대 로그 수를 비교해 로그 유실인지 가른다.
0015에서 클릭 통계를 지표 카운터(Prometheus)로 옮긴다. Fluent Bit는 `storage.type filesystem` 버퍼·tail DB·`Rotate_Wait` 연장 중에서 고른다.
측정 중에는 `kubectl -n logging logs -l app.kubernetes.io/name=fluent-bit | grep -ciE 'retry|paused|drop|error'`가 0이 아니면 그 줄을 바로 본다(README 3-1).

## G-16. 차트 검사 단언이 큰 입력에서 Broken pipe로 실패했다 — 그리고 `exit`가 든 단언은 ❌ 없이 스크립트를 끝냈다

**증상** — plan 0015에서 `scripts/check-charts.sh`에 kube-prometheus-stack 렌더 단언을 더했다(2026-10-14). 맞는 값인데 이렇게 나왔다.
```
scripts/check-charts.sh: line 36: printf: write error: Broken pipe
❌ Alertmanager 서비스 어카운트 = infra/prod Pod Identity 키
```
변이 검사에서는 반대 문제가 나왔다. 선택자 값을 지운 변이에서 스크립트가 **❌ 줄 없이** 끝났다(종료 코드만 0이 아니었다).

**원인**
1. 단언은 `printf "%s" "$K" | grep -q ...` 꼴이다. `grep -q`는 첫 일치에서 바로 끝난다. 입력(렌더 수천 줄)이 파이프 버퍼보다 크면 `printf`가 남은 것을 쓰다가 SIGPIPE를 받고, `set -o pipefail`이라 파이프 전체가 실패한다. 작은 렌더에서는 드러나지 않는다.
2. 단언은 `eval`로 돈다. 단언 안의 `for ...; do ... || exit 1; done`의 `exit`가 서브셸이 아니라 **스크립트 자체**를 끝냈다. 그래서 그 뒤의 `❌` 출력과 나머지 단언이 모두 빠졌다.

**해결** — 큰 렌더는 임시 파일로 두고 `grep ... 파일`로 본다(파이프 없음). 앱 렌더(대시보드 JSON으로 커졌다)는 HPA 문서만 떼어 단언한다. 반복문 단언은 `( ... )` 서브셸로 감싼다.

**재발 방지** — 단언을 더할 때 **틀린 값으로 실패하는지(❌가 찍히는지)와 맞는 값으로 통과하는지를 둘 다** 본다(G-14와 같은 원칙). 큰 입력에는 `printf | grep -q`를 쓰지 않는다. `eval` 안에서 `exit`를 쓰지 않는다.

## G-17. promtool 테스트에서 `increase(x[1m])`가 비었다 — Prometheus 3은 범위 창의 왼쪽 끝을 뺀다

**증상** — plan 0015 규칙 단위 테스트(`scripts/rules-test/linkpulse.test.yaml`, 1분 간격 합성 시계열)에서 발행 실패 알림(`increase(...[1m]) > 0`, `for: 5m`)이 8분에도 울리지 않았다.

**원인** — Prometheus 3.0부터 범위 선택자는 왼쪽이 열린 구간 `(t−창, t]`이다. 1분 간격 표본에 `[1m]` 창이면 표본이 하나뿐이고, `increase()`·`rate()`는 표본이 둘 이상이어야 값을 낸다. 운영(긁기 15초)에서는 창에 표본이 4개라 드러나지 않는다.

**해결** — 그 규칙의 창을 `[2m]`으로 늘렸다. 긁기 간격이 1분이어도 표본이 둘 이상 든다.

**재발 방지** — 창 길이는 "긁기 간격의 2배 이상"으로 잡는다. 규칙을 합성 시계열로 시험할 때는 운영과 다른 간격(1분)으로도 통과하는지 본다. 이름 정규식으로 여러 카운터를 한 번에 `resets()`하면 이름이 빠져 라벨이 겹친다("vector cannot contain metrics with the same labelset") — 카운터마다 따로 계산해 더한다(`collect-metrics.sh` 정합성 절).

## G-18. `terraform fmt -check -diff`를 `infra/prod` 디렉터리째 돌리자 gitignore된 `terraform.tfvars`의 값이 화면에 찍혔다

**증상** — plan 0015 작업 중 `terraform fmt -check -diff .`(infra/prod)를 돌렸다(2026-10-14). 커밋 대상이 아닌 `terraform.tfvars`의 정렬 차이가 diff로 나오면서 Slack ID·SMS 번호 줄이 그대로 출력됐다.

**원인** — `fmt`는 디렉터리의 `*.tf`뿐 아니라 `*.tfvars`도 검사하고, `-diff`는 그 내용을 찍는다. gitignore는 상관없다.

**해결** — 그 출력은 대화·문서에 붙이지 않았다. 검사는 파일을 지정해서 한다: `terraform fmt -check infra/prod/*.tf infra/k8s/*.tf`.

**재발 방지** — 비밀이 든 디렉터리(`infra/prod`·`infra/k8s`)에서는 `fmt`에 디렉터리나 `-diff`를 주지 않는다. CI는 tfvars가 없어 영향이 없다.

## G-19. `full-apply-prod.sh` 3단계(infra/k8s) plan이 차트 저장소 타임아웃으로 멈췄다

**증상** — plan 0015 측정 창의 처음부터 켜기(2026-10-14). 1단계 infra/prod는 끝났고(94개 생성, 노드 3대 Ready), 3단계 `terraform plan`이 이렇게 끝났다.
```
Error: Error locating chart  (helm_release.fluent_bit)
Unable to locate chart fluent-bit: looks like "https://fluent.github.io/helm-charts" is not a valid chart repository
or cannot be reached: read tcp …->185.199.109.153:443: read: operation timed out
```

**원인** — 차트가 아니라 네트워크다. helm 공급자는 plan 때 저장소의 `index.yaml`을 받는다. 그 순간 집 회선에서 GitHub Pages(185.199.108~111.153)로 가는 연결 하나가 읽기 타임아웃이 났다. 바로 뒤에 같은 맥에서 fluent·prometheus-community·argo·eks-charts 네 저장소의 `index.yaml`이 모두 200(0.1초 안팎)이었다.

**해결** — plan에서 멈췄으므로 infra/k8s에는 아무것도 적용되지 않았다. `./scripts/full-apply-prod.sh`를 다시 돌린다. 1단계 plan은 `No changes`여야 한다(그대로 확인 문구를 넣으면 빈 apply다). 변경이 보이면 거기서 멈춘다.

**재발(2026-10-15, plan 0016 측정 창 2)** — 측정 예외 apply(`terraform -chdir=infra/k8s apply -var rate_limit_exempt_ips=…`)에서 같은 증상이 argo-helm 저장소로 났다(`helm_release.argocd`, `185.199.110.153:443 read: operation timed out`).
바꾸려던 것은 `helm_release.site` 하나였지만 helm 공급자는 모든 릴리스의 차트 저장소를 읽는다 — 그중 하나만 끊겨도 멈춘다.
적용된 것은 없었다: site helm 리비전 1 그대로, 사이트 Secret에 `RATE_LIMIT_EXEMPT_IPS` 키 없음, 접근 로그 버킷 없음(`&&`로 이어져 뒤 명령이 돌지 않았다). 바로 뒤 네 저장소 `index.yaml` 모두 200(0.04~0.24초). 같은 명령을 다시 돌렸다.

**재발 방지** — 스크립트는 다시 돌려도 안전하다(두 스택 모두 plan → 확인 → apply). 3단계 plan의 저장소 오류는 먼저 `curl -sS -o /dev/null -w '%{http_code}\n' <저장소>/index.yaml`로 접속부터 본다.

## G-20. infra/k8s apply에서 LBC 설치가 "the server is currently unable to handle the request"로 실패했다 — metrics-server 애드온의 깨진 OpenAPI

**증상** — plan 0015 측정 창의 처음부터 켜기(2026-10-14). 3단계 apply에서 Fluent Bit·kube-prometheus-stack·ArgoCD·Image Updater는 만들어졌는데 LBC만 실패했다. 다시 돌려도(LBC 혼자) 같았다.
```
Error: installation failed  (helm_release.aws_load_balancer_controller)
unable to build kubernetes objects from release manifest: error validating "": error validating data:
the server is currently unable to handle the request
```
LBC는 CRD 7개만 남고 Deployment·helm 릴리스 기록·state가 없다(다시 돌리면 새로 만든다).

**원인** — EKS metrics-server 애드온 기본 버전 `v0.9.0-eksbuild.12`가 OpenAPI v2 스펙을 깨진 JSON으로 낸다.
- `/openapi/v2`를 JSON으로 받으면 200(6.4MB), **protobuf로 받으면 늘 503(0바이트)**이다(`kubectl proxy` + `Accept: application/com.github.proto-openapi.spec.v2@v1.0+protobuf`).
  API 서버는 합친 스펙을 protobuf로 바꿀 때 gnostic으로 파싱하는데, 같은 파서로 JSON을 돌리면 정의 20개가 `has invalid properties: ArbitraryKeys, SchemaProps, SwaggerSchemaProps`로 실패한다.
  20개 모두 metrics-server가 합쳐 넣은 정의다(`metrics.k8s.io/v1beta1` 타입, `Duration`, 이름이 겹쳐 `_v2`가 붙은 meta 타입). EKS 확장 API(`com.amazonaws.eks.metrics.v1.*`)는 정상이다.
- helm(kubectl과 같은 코드)은 v3로 서버 측 필드 검증이 되지 않는 객체를 v2 protobuf로 검증한다. LBC 차트의 `kind: List`(IngressClassParams·IngressClass 묶음)가 그것이라 LBC만 걸렸다 — 그 문서를 빼면 검증이 통과한다.
- **ArgoCD도 같은 곳에 걸린다.** 클러스터 캐시를 동기화할 때 `LoadOpenAPISchema`(v2 protobuf)가 실패하면 `failed to load open api schema while syncing cluster cache`로 멈춘다(argo-cd v3.5.5 `gitops-engine/pkg/cache/cluster.go`·`pkg/utils/kube/ctl.go`). LBC만 우회하면 앱이 배포되지 않는다.
- eksbuild.12 이미지는 2026-10-12 21:19 UTC 빌드로 Go 1.27.2, .11은 2026-09-14 빌드로 Go 1.26.6이다(이미지 바이너리 `go version -m`, kube-openapi 같은 버전). 같은 kube-openapi의 `spec.Schema`를 `json.Marshal`하면 Go 1.26.6은 `{"description":…}`, **Go 1.27.2는 `{"ArbitraryKeys":{},"SchemaProps":{…},"SwaggerSchemaProps":{}}`**다 — Go 1.27의 `encoding/json`에서 그 타입의 사용자 정의 직렬화가 적용되지 않는다. 0014(10-13)는 기본 버전이 바뀌기 전이었던 것으로 본다(당시 버전 기록은 없다).
- 처음엔 kube-prometheus-stack CRD 등록과 겹친 탓으로 잘못 봤다. LBC 혼자 돌려도 같아서 버렸다 — 시각이 겹친다는 것만으로 원인을 정하지 않는다.

**해결** — `infra/prod/eks.tf`의 `addon_version_pins`로 metrics-server를 `v0.9.0-eksbuild.11`에 고정하고, 애드온을 교체한다(`terraform apply -replace='aws_eks_addon.after_nodes["metrics-server"]'` — 이전 버전으로의 in-place 업데이트 허용 여부가 문서에 없어 지우고 다시 만든다). 그 뒤 protobuf v2가 200인지 보고 `full-apply-prod.sh`를 다시 돌린다(1단계 `No changes`, 3단계 `3 to add`).

**재발 방지** — 애드온 기본 버전은 AWS가 예고 없이 바꾼다. 3단계(infra/k8s)에서 검증 오류가 나면 먼저 protobuf v2를 본다.
```bash
kubectl proxy --port=18001 & sleep 2; curl -s -o /dev/null -w '%{http_code}\n' -H 'Accept: application/com.github.proto-openapi.spec.v2@v1.0+protobuf' http://127.0.0.1:18001/openapi/v2; kill %1
```
고정을 풀거나 올릴 때도 이 값이 200인지 먼저 확인한다.

## G-21. k6 setup의 링크 생성 하나가 18초 멈췄다가 `request timeout` — 요청이 ALB에 닿지 않았다(집 회선)

**증상** — plan 0015 측정 창 4) e3 시작(2026-10-14 05:37:28 UTC). setup이 링크 100개를 만들다 7번째에서 멈췄다.
```
level=warning msg="Request Failed" error="Post \"https://lpulse.live/api/links\": request timeout"
level=error msg="Error: 링크 생성 실패: 0 null …" (05:37:46)
```
`run800`이 `setup 실패 — 멈춘다`로 끝났고, 실험 묶음이 `&&`로 이어져 있어 e4·e1b도 돌지 않았다.

**원인** — 서버가 아니라 집 회선 ↔ AWS 구간이다.
- 그 구간(05:37:00~05:38:30) ALB 접근 로그 54건이 모두 2xx·3xx이고, 링크 생성 6건은 05:37:28에 3~9ms에 201이었다. **멈춘 7번째 요청은 로그에 없다** — 클라이언트가 먼저 끊은 요청도 ALB가 받았다면 460으로 남는다. ALB에 닿지 못했다.
- 같은 때 HPA가 파드를 3 → 2로 줄였지만(05:37) 원인이 아니다. 종료 중인 파드로 갔다면 ALB에 502·504가 남는다.
- 같은 날 같은 모양이 두 번 더 있었다: G-19(GitHub Pages `read: operation timed out`), G-20 진단 중 EKS API `read tcp … read: operation timed out`. 이 맥의 집 회선에서 연결 하나가 가끔 멈춘다.

**해결** — `load/k6/capacity.js` setup이 응답을 아예 못 받은 경우(status 0)만 세 번까지 다시 보낸다(1초 간격, `console.warn`으로 남긴다). 그 요청은 ALB에 닿지 않아 링크가 생기지 않았고, 닿았더라도 남는 것은 쓰지 않는 링크 하나다. 닫힌 로컬 포트로 setup을 돌려 3회 재시도 후 실패하는 것을 확인했다. 실패한 e3 파일은 `raw/0015-e3-setupfail/`로 옮기고 e3부터 다시 돌렸다.

**재발 방지** — k6 실패가 서버 쪽인지는 **ALB 접근 로그에 그 요청이 있는지**로 가른다. 없으면 발생기·회선 쪽이다(결과 문서의 판정 0번 — 발생기 한계와 같은 쪽). 부하 중의 같은 실패는 k6 `http_req_failed`에만 잡히고 ALB 5xx에는 없다 — 둘을 나란히 본다.

## G-22. 측정 중 고루틴 스냅숏이 하나도 남지 않았다 — 새 스크립트에 실행 권한이 없었다

**증상** — plan 0016 측정 창(2026-10-15) r0·r1·r2 세 실행이 다 끝났는데 `raw/0016-r*-gr-*.txt`(3200 단계 고루틴 스냅숏)가 하나도 없었다. 다른 원자료(수집 표·`-argo.tsv`·`-appsec.json`)는 모두 있었다.

**원인** — `load/k6/goroutine-snap.sh`가 git 모드 `100644`(실행 권한 없음)로 커밋됐다. README 4) 블록은 이것을 백그라운드 서브셸에서 `$R/goroutine-snap.sh …`로 부른다 — `permission denied`가 백그라운드 출력에 한 줄 섞여 지나갔고, 블록은 `wait`에서 그냥 끝났다.
`shellcheck`·`bash -n`은 통과했다. 실행 권한은 문법 검사로 잡히지 않는다. 같은 블록의 `record-pods.sh`·`collect-metrics.sh`는 예전부터 `100755`라 돌았다.

**해결** — `chmod +x load/k6/goroutine-snap.sh`(git이 `mode change 100644 => 100755`로 본다). 같은 창에서 실제 클러스터에 대고 한 번 돌려 스냅숏 5개가 남는 것을 확인했다.

**재발 방지** — 새 실행 스크립트는 **실제로 한 번 실행해 본다**(문법 검사만으로 끝내지 않는다). 커밋 전에 `git ls-files -s load/k6/*.sh scripts/*.sh`로 모드가 `100755`인지 본다.

## G-23. 변이 시험이 전부 "실패 ✅"였는데, 단언이 아니라 도구가 죽은 것이었다

**증상** — plan 0017 A3 변이 시험(2026-10-16). 집 Ansible 렌더 단언(`infra/home/ansible/tests/render.yml`) 변이 10종과 `infra/home` `terraform test` 변이 3종이 모두 "실패 ✅"로 나왔다. 실패한 단언 이름을 찍으려 하자 빈칸이었다. 출력 원문은 이랬다.
```
TimeoutError: Local RPC server did not start.            ← ansible-playbook
panic: unexpected error marshalling value: value has marks, so it cannot be serialized as JSON   ← terraform test
```

**원인**
1. Ansible: 변이 하네스가 출력 경로를 바꾸려고 `TMPDIR`을 scratchpad의 긴 경로로 덮어썼다. ansible-core 2.21은 시작할 때 `TMPDIR` 아래에 로컬 RPC 소켓을 만드는데, 경로가 길어 소켓을 못 열고 플레이북이 시작도 못 했다. 원본으로 돌렸어도 같은 오류였다.
2. Terraform: 단언이 `for v in proxmox_virtual_environment_vm.vm : v.cpu[0].type == ...`처럼 **VM 객체 전체**를 반복했다. 그 객체에는 민감 속성(`user_account.password`)이 있어 값에 "mark"가 붙는다. 단언이 **실패할 때만** Terraform이 값을 출력하려다 패닉한다(통과할 때는 멀쩡하다).

두 경우 모두 종료 코드가 0이 아니라서 "변이를 잡았다"로 읽혔다.

**해결** — Ansible은 `TMPDIR` 대신 `-e out=<경로>`로 출력 경로만 바꾼다. 단언은 VM을 키로 하나씩 가리킨다(`vm["lp-k3s-1"].cpu[0].type`) — 민감 속성이 든 객체 전체를 단언에서 반복하지 않는다. 고친 뒤 13종이 각각 **그 단언 이름으로** 실패하고, 원본은 통과하는 것을 확인했다.

**재발 방지** — 변이 시험은 "실패했다"로 끝내지 않고 **어느 단언에서 실패했는지**를 본다. 도구 오류(Traceback·panic)는 실패가 아니라 무효로 센다. G-14·G-16과 같은 원칙(틀린 값으로 ❌, 맞는 값으로 ✅)의 세 번째 사례다.

## G-24. `set -u` 스크립트가 한국어 메시지에서 `unbound variable`로 죽었다

**증상** — `scripts/home-pki.sh --test-leaf linkpulse-home-app`(일부러 거부되는 입력)이 거부 메시지 대신 이렇게 끝났다.
```
scripts/home-pki.sh: line 129: APP_CN�: unbound variable
```

**원인** — 메시지가 `"... $APP_CN과 달라야 한다"`였다. UTF-8 환경의 bash는 변수 이름 바로 뒤의 한글 바이트를 이름의 일부로 읽어 `APP_CN과`라는 없는 변수를 찾았고, `set -u`라 그 자리에서 끝났다. `shellcheck`는 잡지 못했다.

**해결** — 한글이 바로 붙는 변수는 중괄호로 감싼다(`${APP_CN}과`). `grep -nP '\$[A-Za-z_]+[\x{AC00}-\x{D7A3}]' <파일>`로 남은 곳이 없는지 본다.
같은 날 `scripts/check-charts.sh`에 새 단언을 넣다가 `"... $BASE_REF와 같다"`로 **한 번 더** 걸렸다(`line 72: BASE_REF�: unbound variable`, 그 뒤 단언이 모두 빠졌다). 같은 grep으로 `scripts/*.sh`를 다시 훑어 남은 곳이 없음을 확인했다.

**재발 방지** — 새 셸 스크립트의 **거부 경로도 한 번 실행한다**(G-22와 같은 원칙 — 정상 경로만 돌리면 메시지 줄은 실행되지 않는다).
