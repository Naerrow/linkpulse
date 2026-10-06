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
