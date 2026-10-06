# 2-5 진입 게이트 — apply ①이 끝났는지 실측으로 가른다

> 이 문서는 **일회성 판정 절차**다. 2-5가 끝나면 지운다.
> 출처: Step 2 코드 검토 round-1 claude-ide #2(코드로 닫을 수 없어 게이트로 올린 유일한 지적).

## 왜 필요한가

plan 2-2는 HCL 작성을 둘로 쪼갰다.

- **2-2a** = `aws_ecs_task_definition.app`에 **`skip_destroy = true`만** → apply ①의 기대값은
  **in-place(`false/null → true`) · replacement 없음 · `0 destroy`**
- **2-2b** = task role 정책 + `DB_SECRET_ARN`·`AWS_REGION` env → task definition **`-/+`**(새 revision)

쪼갠 이유는 *"모든 HCL 변경이 이미 작업 트리에 있으면 일반 `terraform apply`가 env replacement와
정책까지 함께 적용하므로 그 게이트를 만족할 수 없다"*는 것이었다(r8 codex-cli#4).

**그런데 이 브랜치에는 2-2a(`3f8667a`)와 2-2b(`5ae7a11`)가 둘 다 있다.** 두 커밋 사이에 ~15시간이
있어 apply ①이 그사이 끝났을 수도 있지만 **저장소에 기록이 없다.**

⚠️ **왜 그냥 넘기면 안 되나.** Terraform의 destroy 단계는 **prior state**를 근거로 판정한다.
2-2a가 apply되지 않은 상태에서 두 변경을 한 번에 apply하면, replacement의 destroy가
`skip_destroy = false`(prior state)로 판정돼 **직전 revision이 INACTIVE가 될 여지**가 있다.
그러면 `ecs.tf:12-15`가 막으려던 바로 그 일이 일어난다 — **롤백 대상이 사라진다.**
Step 1의 `force-new-deployment`는 같은 이미지를 다시 띄울 뿐이라 코드 결함을 되돌리지 못하므로,
**직전 ACTIVE revision이 Step 2의 유일한 백스톱**이다.

리뷰어도 자격증명이 없어 단정하지 않았다. **실측으로 가른다.**

## 절차 (B 단계 AWS 창의 **첫 순서**)

### 0. 롤백 대상을 먼저 적어 둔다 [read-only]

```bash
aws ecs list-task-definitions --family-prefix linkpulse-prod-app --status ACTIVE \
  --region ap-northeast-2 --query 'taskDefinitionArns[-3:]' --output table
```

여기 나온 revision이 백스톱이다. **판정 전후로 이 목록이 줄어들면 안 된다.**

### 1. state에 `skip_destroy`가 이미 들어갔는지 본다 [read-only]

```bash
terraform -chdir=infra/prod state show aws_ecs_task_definition.app | grep -E 'skip_destroy|revision'
```

### 2. 현재 트리로 plan을 찍는다 [read-only]

```bash
terraform -chdir=infra/prod plan -no-color | tee /tmp/plan-2-5.txt
grep -E '^  # |Plan: ' /tmp/plan-2-5.txt
```

## 판정표

| state의 `skip_destroy` | plan 출력 | 판정 | 다음 |
| --- | --- | --- | --- |
| **`true`** | task definition `-/+`(새 revision) + task role 정책 신설, **`0 to destroy`** | ✅ **apply ①은 이미 끝났다** | plan **요약**을 PR에 첨부하고(아래 종료 조건) **apply ②로 간다** |
| **`false`·없음** | 무엇이든 | ⛔ **apply ①이 안 끝났다** | 아래 (A)로 먼저 apply ①을 통과시킨다 |
| `true` | **`1 to destroy` 이상** | ⛔ **중단** | destroy 대상이 무엇인지 확인. task definition이면 `skip_destroy`가 안 먹는 것이다 |

### (A) apply ①을 뒤늦게 통과시키는 법 — 임시 revert 없이

`3f8667a`는 **정확히 "2-2a까지만 반영된 트리"**다. 그 커밋을 별도 워크트리로 꺼내
거기서 apply하면 plan이 금지한 임시 revert나 `-target` 없이 게이트를 그대로 만족한다.

```bash
git worktree add /tmp/apply1 3f8667a
terraform -chdir=/tmp/apply1/infra/prod init
terraform -chdir=/tmp/apply1/infra/prod plan -no-color | tee /tmp/plan-apply1.txt
# 기대값: task definition in-place(false/null → true), replacement 없음, 0 to destroy
# [사람] 확인 후에만:
terraform -chdir=/tmp/apply1/infra/prod apply
git worktree remove /tmp/apply1
```

⚠️ **워크트리의 state는 같은 원격 state다**(`backend.tf` 동일). 즉 여기서 apply하면 실제 인프라가
바뀐다. plan 출력을 사람이 읽고 기대값과 일치할 때만 apply한다 — AGENTS.md 가드레일 #1.

## 종료 조건

- **요약만** PR #20에 첨부한다 — AGENTS.md §4가 모든 인프라 변경에 `terraform plan` 출력을 요구하지만,
  **이 저장소는 PUBLIC이다.** 전체 출력에는 계정 ID·role/시크릿 ARN·RDS 엔드포인트가 들어 있고,
  그것은 round-1 #4로 방금 걷어낸 값들이다.
  ```bash
  grep -E '^  # |^Plan: ' /tmp/plan-2-5.txt | sed -E 's/[0-9]{12}/<ACCOUNT_ID>/g'
  ```
  전체 파일은 로컬에 두고 필요하면 사람이 직접 확인한다.
- 0번의 ACTIVE revision 목록이 **줄지 않았음**을 재확인한다.
- PR 본문의 `- [ ] apply ① 실행 기록` 체크박스를 닫는다.
