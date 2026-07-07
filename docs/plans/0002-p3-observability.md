# P3 — 관측성(P3-1: 알람·Slack 알림·Container Insights) 계획·이행 기록

- 날짜: 2026-07-02
- Phase: **P3 (관측성)** — 이번 범위는 **P3-1: 장애 자동 감지 → Slack 통지**
- 작성 브랜치: `infra/p3-cloudwatch-slack-alerts`
- 상태: **이행 중** — 코드 작성 완료(`terraform fmt`/`validate` 통과). 사용자 `apply` + Slack OAuth 대기.
- 관련 문서: [`docs/adr/0002-alerting-design.md`](../adr/0002-alerting-design.md)(설계 근거).

> 이 디렉터리(`docs/plans/`)는 phase별 **계획 + 이행 기록**을 남긴다.

---

## 1. 배경·목표

P2(CI/CD + 롤백 E2E)가 완결됐다. 로깅은 이미 프로덕션급이다(`app/cmd/server/main.go`의 slog JSON, `app/internal/httpapi/middleware.go`의 `requestLogger`/`recoverer`, `logs.tf`의 awslogs→`/ecs/linkpulse-prod-app`). **빠진 것은 "감지·통지"** — CloudWatch 알람 0개, 알림 채널 0개라 장애가 나도 사람이 `/healthz`를 찌르기 전엔 모른다.

**P3-1 목표:** 장애를 자동 감지해 Slack으로 통지하는 체계 구축. 의도적 장애·회고(runbook)는 알람 발화를 확인한 뒤 **P3-2**로 한다.

## 2. 계획 (확정 결정)

- **경로:** CloudWatch Alarm → SNS → AWS Chatbot(Amazon Q Developer in chat applications) → Slack. Lambda/webhook 시크릿 없이 Terraform 선언만.
- **SNS:** KMS 미사용(민감정보 없음 + 암호화 시 CloudWatch 키정책 함정 회피). `aws_sns_topic_policy`로 CloudWatch publish 한정 + confused-deputy(SourceArn/SourceAccount).
- **Chatbot:** `slack_team_id`/`slack_channel_id`가 둘 다 있을 때만 role/attachment/config 생성(`count = local.slack_enabled`). 없으면 알람+SNS만(중간 마일스톤). guardrail·role 모두 `CloudWatchReadOnlyAccess`로 상한 축소.
- **Container Insights:** `enable_container_insights` default `false`→`true`(RunningTaskCount 등 태스크 메트릭 확보).
- **임계값:** `locals`에 "초기값"으로. 스토리지/RunningTaskCount는 변수(`db_allocated_storage`/`service_desired_count`)에서 파생.

## 3. 이행 기록 (Task별)

| # | 작업 | 파일 | 핵심 내용 |
|---|---|---|---|
| 1 | SNS + 알람 12 | `monitoring.tf`(신규) | SNS(KMS 없음)+policy(confused-deputy, `data.aws_caller_identity.current` 재사용), 알람 12개. ALB dimension 지표별 정확화(ELB 5xx=LB만) |
| 2 | Slack 배선 | `chatbot.tf`(신규) | `local.slack_enabled`(trimspace), role(trust `chatbot.amazonaws.com`)+`CloudWatchReadOnlyAccess`+config, 전부 count-gate, guardrail 명시 |
| 3 | 변수 | `variables.tf` | `enable_container_insights` default→true; `slack_team_id`/`slack_channel_id` + cross-var validation(양쪽-또는-없음, trimspace) |
| 4 | 출력·예시 | `outputs.tf`, `terraform.tfvars.example` | `sns_alarms_topic_arn`+`slack_alerts_enabled`; slack/insights 예시·절차 주석 |
| 5 | 문서 | `docs/adr/0002`, `docs/plans/0002`, `infra/README.md` | 설계 ADR, 이 문서, README 모니터링/Slack 절차·Logs Insights 쿼리 |

### 알람 세트 (12개, 초기값)
- **ALB(5):** Target 5xx≥5/5m · ELB 5xx≥5/5m(LB만) · UnHealthyHostCount≥1/2m · TargetResponseTime p95≥1s/3m(low-sample ignore) · **HealthyHostCount<1/3m(breaching)** ← 무트래픽 무중단 사각지대 차단.
- **ECS(3):** CPU≥80% · Memory≥80% · RunningTaskCount<`service_desired_count`(Insights).
- **RDS(4):** CPU≥80% · FreeStorageSpace≤10%(변수 계산) · FreeableMemory≤100MB · DatabaseConnections≥80.

## 4. 교차검토 반영 (3라운드·총 5팀 — 전건 수용)

**1차(1팀):** SNS "기본 SSE" 부정확→KMS 미사용 명시 · topic policy confused-deputy · Slack 양쪽-또는-없음(cross-var validation) · RunningTaskCount→`service_desired_count` 참조 · p95 `evaluate_low_sample_count_percentiles="ignore"`.

**2차(2팀):** guardrail 미지정=Admin(CFN 문서 확인)→`guardrail_policy_arns` 명시 · 검증 기대=ECS cluster **in-place**(not destroy0) · 완료판정+`slack_alerts_enabled` output · FreeStorageSpace를 `db_allocated_storage`서 계산 · `/invite @Amazon Q`+raw-delivery 끄기 · 인자명 `user_authorization_required`.

**3차(2팀):** `data.aws_caller_identity.current` 중복(github_oidc.tf:6 존재)→**재사용** · ALB dimension 지표별 명시(ELB 5xx=LB만) · 앱 경로 정정(`app/cmd/server`, `app/internal/httpapi`) · Slack disabled 시 IAM role도 count-gate · validation `trimspace` · plan 기대에 `aws_ecs_task_definition.app` 변경 없음.

**추가 발견(수용):** ALB `HealthyHostCount<1` 알람(무트래픽 사각지대) · Chatbot 엔드포인트 fallback 문서화.

**거부:** 없음(전건 타당, 코드 사실 주장 2건은 실제 repo로 직접 확인 후 수용).

**4차(외부 1팀, 2026-07-02) — 2건 수용:**
1. **RDS drift가 P3 apply에 섞임(Medium)** → 실측으로 원인 확정 후 코드 해소: `terraform state show`=immediate인데 AWS `describe-db-parameters` 보고=`pending-reboot`(ApplyType=**dynamic**, Source=system) — 즉 "미적용 변경"이 아니라 **refresh 보고상의 가짜 drift**라, 함께 apply해도 다음 plan에서 재발할 소지. → `rds.tf`에 `apply_method="pending-reboot"` 명시(보고값과 일치 → drift 영구 소멸, 값 "1"·SSL 강제 불변, apply 시 파라미터그룹 API 호출 없음). **별도 커밋 권장.**
2. **Chatbot 권한이 최소권한이라기엔 넓음(낮음)** → 수용(문서화): `user_authorization_required=false` + `CloudWatchReadOnlyAccess`는 채널 멤버 전원이 계정 전역 CW/Logs 읽기를 공유한다는 뜻. private 운영 채널 전제를 ADR·`chatbot.tf` 주석에 명시, 스코프 축소는 **P4(IAM 최소권한)** 로 이관(1인 운영 현 단계선 수용).

**5차(외부 1팀, 2026-07-02) — 2건 수용:**
1. **(High) Chatbot 리전 오판** — ADR의 "ap-northeast-2 지원" 전제가 틀림. Chatbot API는 **us-east-2·us-west-2·ap-southeast-1·eu-west-1 4개 리전에만** 존재. CLI 실측으로 확정: `aws chatbot describe-slack-workspaces`가 ap-northeast-2=**연결 실패**, us-east-2·ap-southeast-1=정상 응답. 현 상태(Slack 변수 비움)선 무해하나 **P3-1 완료를 위해 변수를 넣는 순간 apply 실패**했을 결함 → `chatbot.tf` config에 `region = "us-east-2"` 명시(provider v6 per-resource region, alias 불요. 데이터 글로벌·콘솔 OAuth 워크스페이스는 전 지원 리전서 보임) + ADR 서술 정정. SNS 토픽은 ap-northeast-2 유지(Chatbot이 타 리전 토픽 결합 지원).
2. **(낮음) §5 plan 기록 구본** — "14 add/2 change"+drift 서술이 4차 반영 후 실측(14 add/1 change/0 destroy)과 불일치해 apply 리뷰 때 혼란 소지 → 최신 실측으로 정리(아래 §5).

## 5. 검증 결과 (로컬)

- `terraform fmt` ✅ (monitoring.tf 정렬)
- `terraform validate` → **Success! The configuration is valid.** ✅
  - 확인: `data.aws_caller_identity.current` 중복 없음, 리소스 참조(`arn_suffix`/`identifier`/cluster·service name) 정확, `aws_chatbot_slack_channel_configuration` 스키마(guardrail_policy_arns/user_authorization_required/sns_topic_arns/logging_level) 유효, cross-var validation 문법 통과.
- `terraform plan`(읽기 전용, 에이전트 실행 — 4·5차 검토 반영 후 재실측) → **`Plan: 14 to add, 1 to change, 0 to destroy`**:
  - **add 14** = SNS topic + policy + 알람 12개. (tfvars에 Slack 값이 없어 `local.slack_enabled=false` → chatbot 3종은 count=0. 예상대로 "알람+SNS만" 중간 마일스톤, `slack_alerts_enabled=false`.)
  - **change 1** = `aws_ecs_cluster.main` in-place(containerInsights `disabled→enabled`) ✅ 예측대로. (4차 반영 전 최초 plan은 `14 add/2 change` — rds.force_ssl 가짜 drift 포함이었고, `rds.tf` apply_method 명시로 소멸. §4 참고.)
  - `aws_ecs_task_definition.app` **변경 없음** ✅ (image_tag가 live와 일치), **destroy 0** ✅.
- `apply`는 **사람이 실행**(가드레일 #1).

## 6. 현재 상태 · 남은 사용자 단계

> 코드 작성 완료·validate 통과. `apply`·Slack OAuth는 사람이 수행한다.

1. **plan 확인**: `terraform plan`. 기대 = **14 add**(SNS·policy·알람 12[+Slack 설정 시 chatbot 3종 추가]) + **1 change**(`aws_ecs_cluster.main` in-place, containerInsights=enabled) + `aws_ecs_task_definition.app` **변경 없음**(tfvars `image_tag`를 현재 live SHA와 맞춤) + **destroy 0**.
   - RDS `rds.force_ssl` apply_method drift는 4차 검토(§4)에서 `rds.tf`에 `apply_method="pending-reboot"` 명시로 해소됨 — `aws_db_parameter_group.main`은 이제 plan에 잡히지 않는다(첫 plan 출력의 "changes outside Terraform" 참고 표시는 무해).
2. **Slack 준비**(선택·이번 마일스톤 권장): 콘솔 "Amazon Q Developer in chat applications"서 workspace OAuth → team/channel ID → **채널서 `/invite @Amazon Q`** → `terraform.tfvars`에 두 값 입력. 준비 전이면 비워두고 알람+SNS만 apply.
3. **`terraform apply`.**
4. **검증**: `aws cloudwatch describe-alarms`(12개) · `terraform output slack_alerts_enabled`=true · `aws cloudwatch set-alarm-state ... ALARM` → Slack 수신 → OK 원복 · `RunningTaskCount` 수분 후 생성 · Logs Insights 쿼리(README).

**P3-1 완료 조건:** ① 알람 12개 ② chatbot config 1개 생성(`slack_alerts_enabled=true`) ③ 테스트 통지 Slack 수신. — "알람+SNS만"은 중간 마일스톤.

## 7. 다음 (P3-2)

의도적 장애 주입(나쁜 이미지 배포 → circuit breaker/알람 발화 → P2 롤백 복구) + `/docs` 회고(runbook). 이후 CloudWatch 대시보드(선택), 로그 메트릭 필터(에러율), `SwapUsage`·임계값 튜닝.
