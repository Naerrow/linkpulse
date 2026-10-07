# 2-5 진입 — 인프라 재기동이 곧 apply ①②다

> 이 문서는 **일회성 절차**다. 2-5가 끝나면 지운다.
> 출처: Step 2 코드 검토 round-1 claude-ide #2(apply ① 기록 공백) → round-4에서 전제를 다시 확인해 단순화했다.

## 왜 "apply ① 먼저" 게이트를 없앴나

옛 게이트는 *"2-2a(`skip_destroy`)를 먼저 apply하지 않으면 task definition 교체 때 직전 revision이
INACTIVE가 돼 롤백 대상이 사라진다"*는 전제였다. **이 저장소의 배포 구조에서는 그 일이 일어나지 않는다.**

- **Terraform은 state의 revision 하나만 deregister한다.** AWS provider 6.52.0
  `internal/service/ecs/task_definition.go`의 Read·Delete가 state의 `arn`만 다룬다(`track_latest` 미사용).
- **서비스가 실제로 도는 revision은 항상 CI가 등록한 것이다.** `deploy.yml`이 최신 ACTIVE를 base로
  새 revision을 등록하고, `ecs.tf`의 service는 `ignore_changes = [task_definition]`이다.
  → CI revision은 Terraform state에 들어가지 않으므로 Terraform이 지울 수 없다.
- Terraform state의 revision은 이미지가 `:bootstrap`/`:v1`이다. 복구 후 ECR에는 없는 태그라
  애초에 롤백 대상이 아니다. **롤백은 이전 이미지로의 CI 재배포**다(아래 "롤백").

게다가 **지금은 인프라가 내려가 있어 state가 비어 있다.** 이 브랜치에서 새로 만들면 교체(destroy)가
일어날 대상 자체가 없으므로, 재기동 한 번이 apply ①과 ②를 동시에 끝낸다.

## 절차 (B 단계 AWS 창의 첫 순서)

### 1. 이 브랜치에서 인프라를 다시 만든다

```bash
git switch feat/p4d2-secret-provider && git pull
./scripts/full-apply-prod.sh --trigger-deploy
```

스크립트가 두 번 확인을 묻는다. **입력 전에 plan 출력을 본다.**

| 스크립트 단계 | 기대 plan | 다르면 |
| --- | --- | --- |
| Step 1/4 (`apply linkpulse prod`) | `0 to change, 0 to destroy`(전부 신규). 신규 목록에 `aws_iam_role_policy.ecs_task_secrets`가 있다 | 멈춘다 — state가 비어 있지 않다는 뜻이다 |
| Step 3/4 (`scale linkpulse prod`) | `aws_ecs_task_definition.app` **교체 1건**(이미지 태그 `bootstrap → v1`) + in-place 2건(`aws_ecs_service.app`의 desired count, 그 값을 임계로 쓰는 `monitoring.tf` 알람). `1 to destroy`는 그 교체분이다 | destroy 대상이 task definition이 아니면 멈춘다 |

- Step 2/4의 배포는 **main 이미지**다(`--ref main`). 이 이미지는 `DB_SECRET_ARN`·`AWS_REGION`을
  읽지 않으므로 env가 미리 들어가 있어도 평소처럼 뜬다. Step 2 코드는 머지 때 들어간다.
- Step 3/4의 교체는 `skip_destroy = true`라 옛 revision이 ACTIVE로 남는다. 무해하다.

### 2. 머지 전 env 게이트 (plan 2-5 3번)

```bash
aws ecs describe-task-definition --task-definition linkpulse-prod-app --region ap-northeast-2 \
  --query 'taskDefinition.containerDefinitions[0].environment[?name==`DB_SECRET_ARN` || name==`AWS_REGION`]' \
  --output table
```

두 값이 다 있고 `DB_SECRET_ARN`의 리전이 `ap-northeast-2`면 통과. 하나라도 없으면 머지하지 않는다.

### 3. PR #20 정리 후 머지

- PR 본문의 `- [ ] apply ① 실행 기록` 체크박스를 닫고, 근거로 *"빈 state에서 브랜치 HCL로 재기동(apply ①② 동시)"*
  한 줄과 Step 1/4의 `Plan:` 줄만 붙인다. **전체 plan 출력은 붙이지 않는다** — 공개 저장소라 계정 ID·ARN이 들어간다.
- 머지하면 CI가 Step 2 이미지를 배포한다. 이후는 plan 2-5의 5번(smoke + `secret_provider_mode=secret` 로그)부터.

## 롤백

Step 2 이미지가 깨지면 **머지 직전 main 커밋의 SHA**로 되돌린다(ECR은 최근 30개 이미지를 보존한다).

```bash
gh workflow run deploy.yml --ref main -f image_tag=<머지 직전 main SHA>
```

이 경로는 최신 ACTIVE(두 env 포함)를 base로 옛 이미지를 다시 등록한다. 옛 이미지는 두 env를 무시하므로 그대로 뜬다.
