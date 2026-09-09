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
붙여넣은 명령 뒤의 `# 설명`이 그대로 인자가 됐다.

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
