---
status: approved
revision: 23
created: 2026-08-24
---

# 0009. P4(d)-2 로테이션 근본 대응 — 앱이 비밀번호 회전을 견디게 한다

## ✅ 실행 결과 (2026-10-07) — 완료

**목표를 달성했다: 회전이 돌아도 사람 개입 없이 회복된다.** 온디맨드 회전 1회로 2-6과 1-7을 함께 닫았다.

| 항목 | 결과 |
| --- | --- |
| 2-5 배포 | 인프라를 이 브랜치에서 재기동(빈 state → apply ①② 동시) → env 게이트 통과 → PR #20 머지 → 두 태스크 `secret_provider_mode=secret` |
| 2-6 Step 2 실전 | ✅ 회전 전 태스크 **2개 모두** `auth_failed_observed(1) → secret_refreshed(1→2) → credential_recovered(2)`. 라벨 이동 후 **0~3초** 회복 |
| 1-7 Step 1 | ✅ `redeploy_submitted` 1건(중복 없음), 롤아웃 `COMPLETED`까지 **3분 8초**(기준 10분) |
| 서비스 영향 | 503 **약 61초**(44건) — 거의 전부 회전 Lambda 내부 구간(`T_set`→`T_label`). Step 1만 있던 09-09 드릴(1-6) **약 3분 49초**, 둘 다 없던 09-07 수동 대응 **12분 34초** 대비 |
| 알람 | `canary_down` 발화 없음. `alb-target-5xx`는 ALARM 후 약 15분 유지 — CloudWatch 평가 범위 동작(오류 지속 아님, 런북에 기록) |
| 30초 투입·회수 | 임시 브랜치에서 apply → 드릴 → main에서 다시 apply. 최종 `conn_max_lifetime=5m0s` 확인 |

상세 시각표: [`secret-rotation-bridge.md`](../../runbooks/secret-rotation-bridge.md) "프로덕션 실측",
설계 근거: [ADR 0006](../../adr/0006-runtime-db-credentials.md). 아래 본문은 결정 과정의 기록으로 남긴다.

## 목표

**RDS 마스터 비밀번호 자동 로테이션이 돌아도 사람이 개입하지 않고 서비스가 회복된다.**

두 단계로 나눈다. 단계마다 완료 조건이 독립적이며, Step 1만으로도 다음 회전 창의 위험이 실질적으로 닫힌다.

- **Step 1 (기한 엄수)** — `Secret Label Updated` 이벤트 → EventBridge rule → Lambda →
  `UpdateService(forceNewDeployment)`. 다운 시간을 **13시간~118시간(4회 실측 범위) → 수 분**으로 줄인다.
  **앱 코드 변경 0.** ⚠️ **그 폭이 Step 1의 가치를 말한다** — 복구 동작 자체는 네 번 다 2~6분이었고,
  다운 시간을 정한 것은 **사람이 시작하기까지**였다.
- **Step 2 (근본)** — 앱이 커넥션을 열 때마다 **현재** 비밀번호를 쓰게 한다.
  **다운타임(`T_fail`→`T_ready`)을 수 분 → 트래픽 있으면 수 초**로 줄이고, 재기동 자체를 불필요하게 만든다.
  무트래픽(canary만)에서는 **`T_avail` 이후 약 24초**가 **관측 목표**다.
  ⚠️ **"다운타임"과 "관측 목표 24초"는 서로 다른 구간이다**(r13 claude-ide#2) — 앞은 `T_fail` 기준
  (실측 가능), 뒤는 `T_avail` 기준(관측 불가). revision 13의 헤드라인은 둘을 *"다운 시간"* 한 단어로 불렀다.
  ⚠️ **시각을 여섯으로 나눠 쓴다 — 어느 시각부터 세는지가 숫자보다 중요하다**(r10 codex-cli#1 / codex-ide#1
  → r11 codex-cli#1 / codex-ide#1 → **r12에서 3인 전원이 같은 곳을 다시 지적했다**).
  **`T_set`**(DB 비밀번호 변경) → **`T_fail`**(앱이 첫 `28P01`을 관찰) → **`T_label`**(`AWSCURRENT` 이동) →
  **`T_avail`**(새 값이 **조회 가능**해짐 — 아무도 아직 안 읽음, **관측 불가**) →
  **`T_visible`**(provider가 **처음 받음** — refresh 로그) → **`T_ready`**(새 값으로 dial 성공).
  ⚠️ **`T_avail`과 `T_visible`은 다르다**(r12 3인 전원). 앞은 *"물어보면 새 값이 온다"*, 뒤는
  *"물어봤고 새 값이 왔다"*이고, **그 사이에 backoff 게이트 + probe + refresh가 들어간다.**
  **약 24초는 `T_avail`→`T_ready`**(관측 목표), **`T_visible`→`T_ready`는 약 2~7초**(실측 가능)다.
  **`T_set`→`T_label`의 회전 중 창은 이 plan이 상한을 줄 수 없고**(Secrets Manager 회전 lambda 내부),
  **Step 1도 그 구간을 못 받는다**(라벨 확인이 `UpdateService`의 선행 조건이라 `AWSCURRENT` 이동을 기다린다).
  **`T_label`→`T_avail`(전파)에도 상한이 없다.** 자세한 계약은 2-0의 시계 표를 따른다.

**백스톱 관계는 한 방향뿐이다**(r2 claude-ide#1): Step 1은 **Step 2의 refresh 실패**(회전 후 SDK 장애 등)를
받아준다. 그러나 **Step 2의 코드 결함은 Step 1이 못 받는다** — `force-new-deployment`는 같은 task definition,
즉 **같은 이미지**를 다시 띄우므로 깨진 코드는 그대로 깨진 채 뜬다(0008 `:474` 승계).

**⏰ 절대 기한 — 두 개를 분리한다** (r4 codex-ide#2 / claude-ide#3. revision 4는 헤드라인 기한이
*"08-30 전"*인데 본문이 *"08-30까지 불가능하다고 확정"*해 **선언된 기한이 작성 시점에 이미 위반**돼 있었다):

| 무엇 | 기한 | 주체 |
| --- | --- | --- |
| **브릿지 사전 무장** | ⛔ **09-06 창은 지났고 4차 장애가 났다(13시간 10분).** 다음 대상 **2026-09-14 09:00 KST 전**(09-07 창 이동 반영) | [사람] — ⚠️ **이 수단은 네 번 연속 실패했다.** 4차에는 **SMS 문자까지 도착했는데도 13시간**이었다. *"막는다"*가 아니라 **"실패가 기본값"**으로 읽는다 |
| **1-1a 광역 관찰 rule apply** | ✅ **해당 없음 — (a) 채택으로 1-1에 흡수**(ⓑ·ⓒ로 확정) | 광역 rule이 **존재하지 않고**(ⓑ) 원문도 **없다**(ⓒ). (b)는 *"1-1a apply가 09-06 창 전에 끝나야"* 하는데 **이틀 남아 불가능**하고, 못 대면 1-3이 **두 창** 뒤로 밀린다. 원문 대조는 **1-6 드릴이 승계**한다 |
| **Step 1 라이브(1-3 apply)** | ⛔ **2026-09-14 09:00 KST 전**(2026-09-07 기준 7일. **09-07 창 이동으로 하루 벌었다** — 아래 우발 계획. ⚠️ **절대일자가 기준이고 "남은 N일"은 시점 표기다**) | 09-06 창에서 **4차 장애가 실제로 났고**(회전 09-06 19:09, 13시간 10분), `NextRotationDate = 2026-09-14T08:59:59`로 **다음 창이 09-13 09:00 ~ 09-14 08:59:59**임이 확정됐다. 1-6 드릴을 하면 **드릴 +7일**로 이동 |
| **Step 2 착수** | **1-6 드릴 완료 후 7일 이내** | 1-7(자동 창 사후 검증)을 기다리지 않는다 |

✅ **승인 게이트 — 2026-09-04 닫힘.** [사람]이 ⓐ~ⓒ를 조회해 값이 들어왔고, 아래 결정표가 한 행으로
떨어졌다. **위 표의 세 기한은 그 값으로 확정된 절대일자다.**
(게이트를 세운 근거: r12 codex-cli#2 [높음] / codex-ide#4 [중간] / claude-ide#5 [중간], **3인 전원**.
r13·r14·r15에서도 codex-cli#1 / codex-ide#1이 **네 라운드 연속** 이 게이트를 이유로 `request-changes`를 유지했다.)

### 조회 결과 (2026-09-04, [사람] 실행)

| # | 값 | 판정 |
| --- | --- | --- |
| **ⓐ** | `LastRotatedDate = 2026-08-30T13:08:48+09:00` · `NextRotationDate = 2026-09-07T08:59:59+09:00` (`SecretStatus = active`) | **08-30 창에서 회전이 실제로 일어났다**(13:08 KST, 주간) → **3차 장애**. 당시 계산한 다음 창 09-06에서는 **4차 장애**가 났다. **현재 값은 기한 표를 본다**(2026-09-07 재조회: `Next = 2026-09-14T08:59:59+09:00`) |
| **ⓑ** | `list-rules` 결과가 **`linkpulse-prod-deploy-failed` 하나뿐**이고, 그 rule은 `ECS Deployment State Change` → SNS(plan 0005의 배포 실패 알림)다 | **1-1a 광역 관찰 rule은 존재하지 않는다** |
| **ⓒ** | `describe-log-groups --log-group-name-prefix /aws/events` → `[]` | **실이벤트 원문 없음**(ⓑ의 당연한 귀결) |
| **ⓓ** | ✅ **조회 완료(2026-09-04)** — `alb-target-5xx` **08-30 13:15:37 ALARM**, `canary_down` **13:17:04 ALARM**, 둘 다 **09-04 11:31~11:45 OK**. 전이 각 2회뿐 | ⛔ **3차 장애 확정: 4일 22시간 22분 54초.** 아래 참조 |

ℹ️ **부수 소득 — `NextRotationDate`의 해석이 실측으로 검증됐다.** revision 1은 그 값을 **회전 창의 끝**으로
읽고 창을 *"08-30 09:00 ~ 08-31 08:59:59"*로 계산했는데, **실제 회전이 08-30 13:08에 그 안에서 일어났다.**
이 문서가 창을 계산하는 방식이 맞다는 뜻이고, 09-06 창 계산도 같은 근거로 선다.

⛔ **ⓓ — 3차 장애가 있었고, 이 plan의 전제 하나를 반증한다.**
회전 08-30 13:08:48 → 복구 09-04 11:31:42, **4일 22시간 22분 54초**(1·2차를 합친 것보다 길다).
회고: [`docs/postmortems/2026-08-30-rds-rotation-outage.md`](../../postmortems/2026-08-30-rds-rotation-outage.md).

**이 plan이 가져가야 할 두 가지:**

1. ✅ **탐지 설계가 실측으로 확인됐다.** plan 0008의 목표(*"전면 실패 시 기대값 약 6분 · 상한 8분"*)에
   대해 **`alb-target-5xx` +6분 49초 · `canary_down` +8분 16초**, **플래핑 0회**(2차는 19회 왕복),
   118시간 내내 ALARM 유지. → **이 plan이 탐지를 범위 밖으로 둔 판단이 옳았다.**
   그리고 **2-6이 쓰려는 `/readyz` 503·`alb-target-5xx` 신호가 실장애에서 검증됐다**
   (2-6의 *"`T_fail` 미도래"* 독립 신호 판정이 그 신호에 기댄다).
2. ⛔ **"Step 1이 라이브가 아닌 창은 브릿지 사전 무장으로 막는다"는 전제가 세 번 연속 반증됐다.**
   이 plan은 08-30 창과 09-06 창을 그 전제로 넘긴다(*"회전 창과 Step 1 라이브 시점"* 절).
   **3차는 알람이 회전 +7분에 정확히 울렸는데도 118시간 갔다** — 브릿지는 *"사람이 알아챌 확률"*에
   걸려 있고 **세 번 다 졌다.** 이 전제를 *"막는다"*가 아니라 **"확률을 올릴 뿐이고 실패가 기본값이다"**로
   읽어야 한다. → **Step 1 라이브 기한**(기한 표 참조)**의 무게가 그만큼 크다.**
   런북 쪽 대응은 브릿지 런북 **(f)**(알림 도달 실증·창 당일 능동 확인)로 신설했다.

revision 12는 *"진행 전에 [사람]이 `describe-secret`으로 다시 읽어 표의 날짜를 갱신한다"*로 적었는데,
**그것이 곧 승인된 plan의 실행 단계에서 일정과 분기를 다시 설계하게 만드는 상태**다. codex-cli#2가
정확히 짚었다 — *"날짜 갱신을 승인 후 실행 단계의 plan 수정으로 넘기면 이 저장소의 plan 검토/승인 경계와
A-11의 절대 기한 원칙을 다시 우회한다."* → **날짜 확정을 병합 단계로 끌어온다.**

#### (기록 — 2026-09-04 닫힘. 아래는 당시 요구했던 것이고, 조회 결과는 위 표에 있다)

**[사람]이 제공해야 했던 read-only 상태 4개** (변경형 명령 없음, 가드레일 #1 무관):

| # | 무엇 | 명령 |
| --- | --- | --- |
| ⓐ | `LastRotatedDate` · `NextRotationDate` | `aws secretsmanager describe-secret --secret-id <RDS 관리 시크릿 ARN> --region ap-northeast-2` |
| ⓑ | 1-1a가 apply됐는지(rule·target 존재) | `aws events describe-rule` · `list-targets-by-rule` |
| ⓒ | 광역 로그 그룹에 실이벤트 원문이 있는지 | `aws logs filter-log-events`(해당 로그 그룹) |
| ⓓ | 08-30 창에 회전이 있었는데 장애가 났는지 | 알람 이력 / 브릿지 수행 여부 |

✅ **전부 확정됐다**(2026-09-04): 기한 표의 세 날짜 · 1-1a 폐지((a) 채택으로 **판정 시각·3분기 폴백은
산출물째 삭제**) · 1-7 대상 창. Step 1 라이브 기한은 *"다음 회전 창이 열리기 전"*이라는 의미를
유지한 절대일자로 박았고, **4차 장애 후 기한 표에서 다시 갱신**됐다.

⚠️ **그리고 1-1a의 존재 이유가 08-30 창과 함께 사라졌다 — 택일이 필요했다**(r12 claude-ide#5.
✅ **(a)로 확정됨**):
1-1a는 *"**1-3 apply 전에** 실이벤트 원문으로 전제를 검증한다"*는 **순서 불변식**을 사려고 신설됐다
(`:279-281` — revision 8까지는 원문을 처음 보는 시점이 1-6이라 **전제 검증이 라이브 전환 뒤**였다).
08-30 창이 지나가면서 **1-1a apply와 1-3 apply 사이에 있던 회전 창이 사라져** 둘이 같은 창 앞에 몰린다.
그대로 두면 1-1a 판정 시각이 1-3 apply보다 **뒤**가 되어 **1-1a가 고치려던 상태로 되돌아간다.**

| | 무엇 | 대가 |
| --- | --- | --- |
| **(a)** | **1-1a를 1-1에 흡수**한다. 다섯 리소스를 1-1과 함께 만들고, 원문 대조는 `:346` 그대로 **1-6 드릴이 승계**한다(드릴이 진짜 회전을 일으키므로 원문은 그때 확실히 온다) | 전제 검증이 라이브 전환 **뒤**로 돌아간다. 다만 codex-cli 개선대로 **AWS 공식 이벤트 계약이 1차 출처로 있으므로**(`AWSPENDING`·`AWSPREVIOUS`에는 이벤트를 발행하지 않고 `detail.versionId`는 라벨이 붙은 새 버전 ID) 원문이 유일한 근거는 아니다 |
| **(b)** | **1-1a를 유지하고 이유를 갱신**한다 — *"다음 창의 원문을 받기 위해"*. 그러면 **1-3은 그 창이 지난 뒤**로 간다 | **Step 1 라이브가 한 창(7일) 더 밀린다.** 그 창은 `:847`의 일반 규칙대로 브릿지 사전 무장으로 막는다 — **이 대가를 기한 표에 적지 않으면 회고 A-11이 경계한 "조용히 미뤄지는" 형태 그 자체다** |

**⚠️ 결론을 미리 적어 둔다 — ⓐ~ⓓ가 오면 판단이 기계적으로 끝난다**(r13 claude-ide 개선):

⚠️ **ⓑ를 두 상태로 나눈다 — "원문 없음"이 한 가지가 아니다**(r14 codex-ide#3).
revision 14의 표는 *"원문 없음 → 무조건 (b)"*였는데, **rule이 이미 apply된 상태**와 **아직 없는 상태**는
다르다. 전자는 다음 창의 원문을 **받을 수 있지만**, 후자는 **1-1a 작성·검토·apply가 그 창 전에 끝나야만**
가능하다 — 그리고 이 plan은 *"현재 라운드 속도로는 09-06 창에 못 댄다"*고 확정했다.
**rule도 없는 상태에서 (b)를 택하면 1-3이 한 창이 아니라 두 창 뒤로 밀린다.**

⚠️ **"원문 있음"도 두 상태다 — 배선 유무로 갈린다**(r15 codex-cli#2 [높음] / codex-ide#3 [중간]).
revision 15의 표는 원문만 있으면 **배선과 무관하게** 1-1a를 완료 처리했는데, **1-1은 다섯 리소스가
이미 apply돼 있다고 전제하고 그것이 `plan` 출력에 뜨면 중단하라고 한다.** 즉 *"과거 원문은 있으나
현재 배선은 없음"*(rule 삭제·drift)은 **1-1a를 건너뛰고 1-1도 즉시 중단해 실행 경로가 없다.**

| ⓒ 원문 | ⓑ rule·target 배선 | 결정 | 근거 |
| --- | --- | --- | --- |
| **있음** | **있음** | **1-1a 완료 처리.** (a)/(b) 택일 불필요 | 목적을 이미 달성했고 배선도 서 있다. 남은 것은 원문과 1-1 패턴의 **대조**뿐이고 그것은 1-1에서 한다 |
| **있음** | **없음** | **전제 검증은 완료 처리**하되 **다섯 리소스는 1-1에 흡수해 함께 작성·apply**한다 | 원문이 이미 전제를 검증했으므로 *"apply 후 창을 기다린다"*가 불필요하다. **다만 광역 관찰 배선은 최종 Step 1 구성의 일부**라 복구돼야 한다(rule 미매칭이 무증상이 되는 것을 막는 장치다 — 리스크표). ⚠️ **1-1의 `plan` 기대값을 이 경우로 갱신한다**: 다섯 리소스가 **신설로 뜨는 것이 정상**이고, *"신설 알람 6개"*는 **7개**가 된다 |
| 없음 | **이미 apply됨** | **(b)** — 이유를 *"다음 창의 원문을 받기 위해"*로 갱신 | 배선이 이미 서 있으므로 **다음 창이 원문을 준다.** 1-3이 그 창 뒤로 가는데 **어차피 그 창에는 못 댄다** → **추가 대가 0** |
| ✅ **없음** | ✅ **아직 없음** ← **이번 조회 결과** | ✅ **(a) 확정** — 1-1a를 1-1에 흡수, 원문 대조는 **1-6 드릴이 승계** | (b)를 택하면 *"1-1a apply가 다음 창 전에 끝나야"* 하는데 **라운드 속도상 불가능**하고, 못 대면 **1-3이 두 창 뒤로 밀린다.** ⚠️ **1-6 드릴은 진짜 회전을 일으키므로 원문이 확실히 온다** — 자연 창을 기다리지 않는다. AWS 공식 이벤트 계약도 1차 출처로 이미 있다(`:1-1a 산출물` 절) |

⚠️ **(a)를 택하면 전제 검증이 라이브 전환 뒤로 간다** — 1-1a가 원래 사려던 순서 불변식을 잃는 것이고,
그 대가는 *"이벤트 전제가 어긋나면 정상 회전마다 오탐 알람"*(리스크표)이다. **공식 문서가 그 전제를
뒷받침하므로 감수한다**고 여기 적어 둔다 — 조용히 넘어가지 않는다.

→ ✅ **(a)로 확정됐다.** 아래가 그 파급이고, 본문에 이미 반영했다:
- **단계 순서에서 1-1a를 뺀다** — `1-1 → 1-2 → 1-3(apply) → 1-4 → 1-5 → 1-6 → 1-7`.
- **광역 관찰 다섯 리소스**(rule + Logs target + log group + resource policy + `FailedInvocations` 알람)를
  **1-1이 함께 만든다.** → **1-1의 `plan` 기대값에서 신설 알람이 6개 → 7개**가 된다.
- **1-1a의 3분기 폴백(판정 시각·`TriggeredRules` 분기)은 삭제한다** — 대조할 원문이 apply 전에 오지 않으므로
  분기 자체가 성립하지 않는다. **원문 확인과 패턴 대조는 1-6 드릴이 승계**한다.
- **1-7 대상 창**: 1-6 이후 재계산한 `NextRotationDate`. **드릴을 건너뛰면 기한 표의 다음 창**이다
  (날짜를 여기 박지 않는다 — 회전이 한 번 더 일어날 때마다 낡는다).

⚠️ **(a)의 대가를 다시 적어 둔다** — 전제 검증이 **라이브 전환 뒤**로 간다. 완화는 둘이다:
**(1)** AWS 공식 이벤트 계약이 1차 출처로 있다(`AWSPENDING`·`AWSPREVIOUS`에는 이벤트를 발행하지 않고
`detail.versionId`는 라벨이 붙은 새 버전 ID). **(2)** **1-6 드릴이 진짜 회전을 일으키므로 원문이 확실히 온다** —
자연 창을 기다리지 않는다. 전제가 어긋나면 그때 **1-1 패턴과 handler 라벨 확인 계약을 고친다.**

✅ **이 절은 2026-09-04에 닫혔다 — 더 이상 블로커가 아니다**(r19 codex-cli#3 / codex-ide#4).
revision 19까지 *"사람의 응답을 기다리는 유일한 잔여 블로커"*라는 문장이 남아 있어
**위쪽 종료 선언과 정반대를 지시했다.**

작성 시점(revision 1) `NextRotationDate = 2026-08-31T08:59:59+09:00` → 창은 **08-30 09:00 ~ 08-31 08:59:59 KST**
였고 **그 창은 이미 지났다**. 그다음 창이 **09-06 09:00**이다. 매 라운드 `describe-secret`으로 재확인한다.
조건부 기한("Step 2 이후 판단" 등)을 쓰지 않는다 — 회고 [2026-08-23] A-11의 직접 반영이다.
2026-08-16 회고 액션 #10이 *"#2 적용 후 판단"* 이라는 조건부 판정이었고, 그 #2가 8일 지연되며
#10도 조용히 함께 멈춘 사이 2차 장애가 났다.

**이 plan의 범위가 아닌 것**: 탐지(plan 0008에서 완료·apply됨), 탐지 드릴(plan 0010),
IAM 최소권한 전반(별도), 회고 A-8의 `treat_missing_data` 전수 점검(별도).

## 배경/제약

### 왜 죽는가 (코드 기준)

`app/internal/db/db.go:20`이 `sql.Open("pgx", dsn)`으로 풀을 만든다. **`dsn` 문자열 안에 비밀번호가
박혀 있고, 그 문자열은 프로세스가 끝날 때까지 고정이다.** 비밀번호는 ECS가 컨테이너 기동 시
Secrets Manager에서 읽어 환경변수로 주입한 값이다(`infra/prod/ecs.tf:52-57`).

`pool.SetConnMaxLifetime(5 * time.Minute)`(`db.go:29`)이므로 커넥션은 5분마다 교체되는데,
새 커넥션도 **같은 DSN 문자열**을 쓴다. 따라서 로테이션 후:

- 기존 커넥션: Postgres가 비밀번호 변경으로 세션을 끊지 않아 잠시 살아 있다 (상한 5분, **하한 0**)
- 새 커넥션: 전부 `SQLSTATE 28P01`(인증 실패)

→ **로테이션 후 최대 5분 뒤 전면 장애.** 실측으로 2026-08-23 09:09:24 회전 → 09:13 첫 5xx(3분 36초).

**비밀번호를 다시 읽는 코드 경로가 존재하지 않는다.** 이것이 근본 원인이다.

### 실측 이력 (**4회 재발**)

| | 로테이션 | 지속 | 그때 있던 것 | 복구 소요 |
| --- | --- | --- | --- | --- |
| 1차 | 2026-08-16 22:08:46 | **66시간** | 런북 없음 | 수동 `force-new-deployment` |
| 2차 | 2026-08-23 09:09:24 | **24시간 23분** | 런북 · 알람 19회 플래핑 | **2분 12초** |
| **3차** | **2026-08-30 13:08:48**(일 주간) | **4일 22시간 22분 54초** | 위 + **plan 0008 탐지 apply**. Slack 카드 도착 | **5분 41초** |
| **4차** | **2026-09-06 19:09:00**(일 저녁) | **13시간 10분** | 위 + **SMS 경로 apply**. **문자 2통 도착** | **약 5분** |

| — | **2026-09-07 10:06:07** ✅ **통제된 회전**(f-4 창 이동, 장애 아님) | **12분 34초** | 위 전부 + **[사람] 입회 · 브릿지 즉시 실행** | 브릿지 (b) |

✅ **마지막 행이 "그때 있던 것"이 처음으로 작동한 사례다**(r21 claude-ide 개선) — 대책이 아니라
**사람이 그 자리에 있었다는 것**이 달랐다. 위 네 번은 전부 그 조건이 없었다.

**네 번 모두 복구 동작은 같고 2~6분이면 끝났다.** 실패한 것은 복구가 아니라
**"누가 언제 그 몇 분을 시작하는가"**다. Step 1은 그 시작을 기계에 넘기는 것이다.

⛔ **3·4차가 이 결론을 강화한다**(r19 claude-ide#2):
- **3차는 탐지가 완벽했는데 가장 길었다**(118시간) — 알람 +6분 49초, 플래핑 0회, Slack 카드 도착.
- **4차는 문자가 폰까지 도착했는데도 13시간**이었다.
→ **알림 경로를 강화하는 접근은 4차에서 한계에 닿았다.** 118h → 13h는 성과지만,
그 13시간은 *"다음 날 아침"*이라는 **사람의 리듬이 정한 값**이지 설계가 정한 값이 아니다.

- 회전 주기: `AutomaticallyAfterDays: 7`, `Duration` 미설정 → **UTC 하루짜리 창**
- **다음 창: 2026-09-14 09:00 ~ 09-15 08:59:59 KST** (`NextRotationDate = 2026-09-15T08:59:59+09:00`,
  **2026-09-07 10:06 창 이동 후 실측**). 창은 매번 `describe-secret`으로 재계산한다.
- ℹ️ **회전 스케줄은 "마지막 회전 + 7일"이다** — 창 이동으로 버는 시간은 *"항상 +7일"*이 아니라
  **직전 회전으로부터 얼마나 지났는지**에 달렸다(09-07 이동은 4차 회전 **다음 날**이라 **+1일**만 벌었다).

### 현재 IAM 배치 (Step 2에 영향)

- 시크릿 읽기 권한은 **execution role에만** 있다 (`infra/prod/iam.tf:34-38`, 해당 시크릿 ARN 한정).
  ECS가 컨테이너 기동 시 주입하기 위한 것이다.
- **task role은 권한 없는 빈 역할**이다 (`iam.tf:40-41` 주석: *"현재 앱은 AWS SDK를 쓰지 않으므로
  권한 없는 빈 역할이다"*). Step 2에서 앱이 런타임에 시크릿을 읽으려면 **task role에 권한을 붙여야 한다.**

### 가드레일 (AGENTS.md #1)

- 인프라 변경은 `terraform plan`까지만 에이전트가 수행하고, **`apply`는 사람이 한다.**
- 변경형 AWS CLI(`update-service`, `put-rule` 등)를 에이전트가 자율 실행하지 않는다.
- Step 1은 EventBridge·IAM 신설이므로 **plan 출력을 PR에 첨부**한다.
- 비밀값을 코드·커밋에 넣지 않는다. Step 2는 **시크릿 값이 아니라 ARN만** 코드/설정에 둔다.

### 아키텍처 확정 (revision 1의 미확정 전제를 리뷰가 1차 출처로 닫았다)

revision 1은 이 둘을 "Step 1-0에서 확인"으로 미뤘다. **그것이 잘못이었다** — 승인된 plan을
실행하다가 아키텍처가 바뀌면 08-30 기한 직전에 구현 범위가 흔들린다(r1 codex-cli#1·codex-ide#1).
두 전제 모두 지금 확정한다.

- **(P1) 참 — 다만 쓸 이벤트가 다르다.** Secrets Manager는 `AWSCURRENT` 라벨 이동을 잡는
  **네이티브 `Secret Label Updated` 이벤트**를 모든 시크릿에서 기본 발행하며, AWS가 회전 감지에
  이 이벤트를 권장한다. 이 이벤트는 **`resources[0]`에 시크릿 ARN**을 싣는다
  ([회전 모니터링](https://docs.aws.amazon.com/secretsmanager/latest/userguide/monitoring-eventbridge.html#monitoring-eventbridge-rotation),
  [이벤트 구조](https://docs.aws.amazon.com/secretsmanager/latest/userguide/event-detail-secret-label-updated-secretsmanager.html)).
  → **이것을 쓴다.**
  **CloudTrail `RotationSucceeded`는 쓰지 않는다.** 검토 중 2차 장애의 실제 이벤트
  (`EventId=0c0b51dc-c68a-4c9c-8205-c5e630875d23`)를 read-only 조회한 결과 **`Resources=[]`**이고
  대상 ARN은 `detail.additionalEventData.SecretId`에 있었다 — plan 0005의 top-level `resources`
  정확 매칭 선례가 **적용되지 않는다.** 게다가 CloudTrail 경유는 best-effort 전달이다.

- **(P2) 거짓 — Lambda가 필요하다.** EventBridge **event bus rule**의 ECS 타깃은 `ecs:RunTask`용
  "ECS task"뿐이고 `ecs:UpdateService` 범용 타깃은 없다
  ([rule targets](https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-targets.html#eb-targets-specifics-ecs-task)).
  `arn:aws:scheduler:::aws-sdk:...` universal target은 **EventBridge Scheduler**의 기능이지
  event rule의 기능이 아니다
  ([Scheduler universal targets](https://docs.aws.amazon.com/scheduler/latest/UserGuide/managing-targets-universal.html)).
  → **rule → Lambda → `UpdateService(forceNewDeployment=true)`** 로 확정한다.

**확정 경로:**

```
Secrets Manager (AWSCURRENT 이동)
  └─ EventBridge rule
       source      = aws.secretsmanager
       detail-type = "Secret Label Updated"
       resources   = [<대상 시크릿 ARN>]        ← 정확 매칭
       detail.labelUpdated = ["AWSCURRENT"]      ← 회전 완료 신호(패턴은 배열)
  └─ Lambda (VPC 미연결, AWS API만 호출)
       ecs:UpdateService --force-new-deployment
```

**IAM은 네 조각으로 분리한다**(r1 codex-cli 개선 / codex-ide#1, r2 codex-cli#1 / codex-ide#1):
1. Lambda **리소스 정책**: `lambda:InvokeFunction`을 **해당 rule ARN**으로 제한
2. Lambda **execution role**: **해당 ECS 서비스 ARN 한정** `ecs:UpdateService`
3. Lambda **execution role**: **대상 시크릿 ARN 한정 `secretsmanager:DescribeSecret`**
   — 전파 지연 확인(선행 조건)에 쓴다. `GetSecretValue`는 주지 않는다.
4. Lambda **execution role**: **멱등 테이블 ARN 한정 `dynamodb:GetItem` + `dynamodb:PutItem`**
   + CloudWatch Logs 쓰기 + on-failure SQS 전송 권한
   — **`GetItem`이 빠지면 첫 중복 이벤트가 `AccessDenied`로 끝난다**(r7 claude-ide#1). revision 7이
   *"fail-open이면 `PutItem` 하나로 충분"*으로 좁혔던 것은 **중복 억제를 포기할 때만 성립하는 결론**이었다.

⚠️ **`ecs:DescribeServices`는 주지 않는다**(r7 codex-cli#4 / codex-ide 개선 / claude-ide 개선). handler는
`DescribeServices`를 **호출하지 않는다** — 재배포 관측은 `UpdateService` **응답 자체**로 끝난다(아래 handler 계약).
revision 7까지 이 권한의 근거였던 *"멱등 판정에 `deployments`가 필요하다"*는 폐기된 시각 비교 설계의 잔재다.
(r6 codex-cli 개선 / codex-ide#5 / claude-ide#3 — revision 6은 이 목록이 "네 조각"이라 `DescribeSecret`과
DynamoDB가 빠져 있었고 handler 절은 "5분할", ADR은 다시 "4분할"이라 **셋이 서로 달랐다**.
구현자가 어느 목록을 따르든 첫 호출이 `AccessDenied`가 되지 않게 여기 하나로 통일한다.)

**Lambda 산출물 경계**(r2 claude-ide#5 — 저장소의 첫 Lambda다):
- **런타임: Python 3.13 + boto3.** 이유: boto3가 런타임에 내장돼 **의존성 패키징·빌드 단계가 불필요**하다.
  Go를 택하면 CI에 새 빌드 단계가 필요한데(현재 `.github/workflows/`는 앱 이미지 빌드만 한다)
  기한이 6일이라 그 작업량을 지지 않는다.
- **소스 경로: `infra/prod/lambda/rotation_redeploy/handler.py`** (AGENTS.md #3의 담당 경로 명시)
- **패키징: `data "archive_file"`로 인라인 zip** — S3 버킷·CI 아티팩트를 새로 만들지 않는다.
  - **`source_code_hash = data.archive_file.….output_base64sha256`을 반드시 건다**(r3 claude-ide#3).
    없으면 `handler.py`를 고쳐도 Terraform이 변경을 감지하지 못해 **`plan`에 아무것도 안 뜨고 옛 코드가 계속 돈다.**
    1-6 드릴에서 멱등 로직 결함을 발견해 고쳐야 할 때 조용히 막히는 함정이다.
  - **`hashicorp/archive` provider를 `versions.tf`의 `required_providers`에 추가**하고(`~>` 고정, 저장소 관례)
    `terraform init`으로 갱신된 `.terraform.lock.hcl`을 커밋한다 — 현재 lock에는 **aws 하나뿐**이다(확인함).
- **실행 파라미터 확정**(r4 codex-cli#1 / codex-ide#1 / claude-ide#1 — **3인 전원 [높음]**):
  - **`timeout = 120`(초).** `aws_lambda_function.timeout` 기본값은 **3초**다. 그대로 두면
    아래 bounded wait(60초)가 시작되자마자 매 호출이 강제 종료돼 **`UpdateService`에 한 번도 도달하지 못하고**
    재시도 2회 소진 후 on-failure 큐로만 쌓인다. **Step 1이 통째로 죽는 설정 누락이다.**
    근거는 **아래 "실행 예산" 표**다(5+60+20+5+15 = 105초 내부 deadline + 15초 여유).
    r8 claude-ide#5 — revision 8까지 이 자리에 있던 *"라벨 확인 60초 + deployment 관측 확인 + …"*은
    **폐기된 표현**(관측 확인 단계는 제거됐다)이었고 새로 붙은 `GetItem`·`PutItem`이 빠져 있었다.
  - **`memory_size = 256`(MB).** boto3 로딩 + 폴링에 충분하고 CPU 배분도 함께 오른다.
  - **⚠️ Lambda 환경변수로 대상을 주입한다 — 이것이 없으면 `UpdateService`를 구현할 수 없다**
    (r9 codex-cli#1 [높음]). **이벤트가 주는 값은 시크릿 ARN·라벨·`versionId`뿐이고 ECS 대상은 없다.**
    revision 9까지 plan 어디에도 handler가 `cluster`·`service`를 **어디서 얻는지**가 없었다 —
    IAM을 서비스 ARN으로 제한한 것은 요청 파라미터를 채워 주지 않는다. 그대로 구현하면 하드코딩
    여부가 실행자 판단으로 남거나, **`cluster`를 생략해 `default` 클러스터를 보고
    `ServiceNotFoundException`**이 난다(이 서비스는 `aws_ecs_cluster.main` 소속이다 — `ecs.tf:1`, `:71-74`).
    → **Terraform이 다음을 환경변수로 주입한다**(값은 전부 non-secret):

    | 환경변수 | 값 | 쓰이는 곳 |
    | --- | --- | --- |
    | `ECS_CLUSTER_ARN` | `aws_ecs_cluster.main.arn` | `UpdateService(cluster=…)` |
    | `ECS_SERVICE_NAME` | `aws_ecs_service.app.name` | `UpdateService(service=…)` — **필수 인자** |
    | `IDEMPOTENCY_TABLE_NAME` | 멱등 테이블 이름 | `GetItem`/`PutItem` |
    | `SECRET_ARN` | 대상 시크릿 ARN | `DescribeSecret` + 이벤트 `resources` 대조 |

    **handler는 기동 시 네 값이 비어 있으면 즉시 실패한다(fail-fast)** — 빈 값으로 AWS를 호출해
    엉뚱한 오류로 진단이 흐려지는 것을 막는다. 상수 하드코딩은 하지 않는다.
  - **boto3 connect/read timeout**을 명시해 SDK 호출 하나가 함수 예산을 다 먹지 않게 한다.
- **`archive_file`의 `output_path`는 `.gitignore` 대상 경로**로 둔다(r4 codex-cli 개선) —
  `terraform plan` 때 생성되는 zip이 작업 트리에 남거나 실수로 커밋되지 않게 한다.
  ⚠️ **같은 커밋에서 `.gitignore`에 `__pycache__/`·`*.pyc`도 추가한다**(r9 codex-cli 개선 — 확인 결과
  저장소 `.gitignore`에 Python 항목이 **하나도 없다**. 이 저장소의 첫 Python 코드이므로 단위 테스트가
  만드는 bytecode가 그대로 작업 트리에 남는다).
- **⚠️ handler 단위 테스트를 CI에 넣는다**(r9 codex-ide 개선). 현재 `.github/workflows/ci.yml`·`deploy.yml`은
  **Go 테스트만 실행**하므로, `handler.py`의 테스트는 최초 수동 실행 뒤 **회귀 방어가 되지 않는다.**
  표준 라이브러리 `unittest`로 끝내고(외부 의존성 없음) **PR CI에 짧은 Python 테스트 단계**를 추가한다.
  ⚠️ **fake 주입만으로는 부족하다 — `import boto3`가 module scope에 있으면 테스트 수집 단계에서
  바로 실패한다**(r10 codex-cli 개선 / codex-ide 개선). → **client factory를 handler 인자로 받고
  기본 factory 안에서만 boto3를 지연 import한다.** 테스트는 fake factory를 넘기므로 boto3를
  import하지 않고, `sys.modules` monkeypatch도 필요 없다.
  ⚠️ **러너 기본 Python에 기대지 말고 `actions/setup-python`으로 3.13을 고정한다**(r12 codex-cli 개선) —
  Lambda 런타임과 같은 버전이어야 수집·문법 드리프트가 CI에서 잡힌다.
- **로그 그룹을 Terraform으로 명시 생성한다**(r3 claude-ide 개선). `/aws/lambda/<fn>`은 첫 호출 때 자동 생성되고
  **retention이 무기한**이며 Terraform 관리 밖에 남는다. `logs.tf`가 `retention_in_days`를 지정하는 관례를 따른다.
  1-7의 사후 증거가 이 로그에 의존하므로 보존 기간이 명시돼야 한다.
- **단위 테스트**: `DynamoDB`/`UpdateService`/`DescribeSecret` fake로 handler 로직을 검증한다.
  **프로덕션 직접 invoke가 첫 로직 시험이 되지 않게 한다**(r2 codex-cli 개선).

### 회전 스케줄은 드릴로 이동한다 (r3 claude-ide#1 / codex-ide#1 — 실행 단계로 미루지 않고 확정)

revision 3은 *"온디맨드 회전이 다음 창을 미는가"*를 1-6 착수 전 확인으로 미뤘다. **지금 답이 있다.**

> `AutomaticallyAfterDays` — *"Secrets Manager **calculates the next rotation date based on the previous
> rotation**. ... In `DescribeSecret` and `ListSecrets`, this value is **calculated from the rotation
> schedule after every successful rotation**."*
> ([RotationRulesType](https://docs.aws.amazon.com/secretsmanager/latest/apireference/API_RotationRulesType.html))

현재 설정은 `AutomaticallyAfterDays: 7` + `Duration` 미설정이다. 따라서:

- **1-6 드릴을 수행하면 `LastRotatedDate`가 갱신되고 다음 창은 드릴 +7일로 이동한다.
  08-30 창은 오지 않는다.** 이것은 확인 사항이 아니라 **예상 동작**이다.
- 그러므로 **1-7의 대상은 "08-30"이 아니라 "1-6 이후 `describe-secret`으로 재계산한 창"**이다.
  드릴을 건너뛴 경우에는 **ⓐ의 `NextRotationDate`**다(원래 적혀 있던 *"08-30"*은 이미 지난 창이다).
- 절대 기한·우발 계획·리스크표·브릿지 런북 `(e)`의 창 날짜가 모두 여기에 연동된다.

**⚠️ 순서 강제 — Step 1이 라이브가 아닌 상태에서 1-6 드릴을 돌리면 장애를 하나 더 만드는 것이다.**
드릴은 진짜 회전을 일으킨다. 1-3(apply) 완료가 1-6의 선행 조건이다.

**회전 창 자체를 좁히는 선택지**(r3 claude-ide 개선): 브릿지 런북이 `ScheduleExpression`+`Duration`으로
창을 1시간대로 고정하는 안을 제안했고, 창이 1시간이면 1-7이 사후 포렌식이 아니라 직접 관측이 된다.
**다만 이 시크릿은 RDS가 관리하므로**(`manage_master_user_password = true`) 회전 규칙을 사용자가
직접 바꿀 수 있는지 확인이 필요하다. → **이 plan에서는 채택하지 않는다**(Step 1·2가 창 길이와 무관하게
동작하므로 의존성이 없다). 확인과 채택은 **plan 0011 이후**로 남긴다. 이후 라운드에서 재제기하지 않는다.

### plan 0008 승계 항목 전수 대조 (r2 claude-ide 지적 — 조용히 떨어뜨리지 않는다)

0008 `plan.md:469-477`이 "0009 작성 시 처음부터 반영"으로 넘긴 8건이다. **revision 2는 3건을 누락하고
1건을 정면으로 위반했다.** 이후 라운드에서도 이 표로 대조한다.

| 0008 | 내용 | revision 3 반영 위치 |
| --- | --- | --- |
| `:470` | 온디맨드 회전은 `aws rds modify-db-instance --rotate-master-user-password --apply-immediately` (1차 출처 확인됨) | **1-6 온디맨드 회전 드릴** (revision 2에서 누락) |
| `:471` | `OptionBeforeConnect`는 `28P01`을 관찰 못 함 → connector 래퍼 필요 | 2-0 |
| `:472` | 회전 중 최신 조회에도 **이전 자격증명이 반환될 수 있다** → 무중단 보장 안 됨 | **2-0 "회전 중 창"** (revision 2에서 누락) |
| `:473` | `SecretString`이 JSON → 파싱 단계 필요 | 2-1 |
| `:474` | 런타임 폴백 부재 + **`force-new-deployment`는 같은 taskdef라 롤백이 아니다** | 2-3 / **리스크표 정정** (revision 2가 완화 칸에서 위반) |
| `:475` | `ignore_changes` 때문에 CI 배포 1회 필요 | 2-2 · "배포 경계" |
| `:476` | 성공 기준이 무중단을 입증하지 못함 | **2-6 판별 수단** (revision 2에서 누락) |
| `:477` | DI 경계(`config.Config` 명시 필드) · singleflight/mutex | 2-1 · 2-2 |

### 배포 경계 (Step 2에 직접 영향 — ADR 0001)

`aws_ecs_service`에 `lifecycle { ignore_changes = [task_definition] }`가 걸려 있다(`ecs.tf:107-109`).
**Terraform으로 task definition을 바꿔도(예: env 추가) running 서비스에 곧장 반영되지 않는다** —
새 revision만 등록되고, **CI 배포가 1회 돌아야 라이브에 적용된다**
(`ecs.tf:105-106` 주석, [ADR 0001](../../adr/0001-cicd-terraform-ci-boundary.md) "결과/트레이드오프").
Step 2가 `DB_SECRET_ARN` env를 추가하므로 이 순서를 반드시 지킨다.

## 실행 단계

### Step 1 — 로테이션 완료 시 자동 재배포

**기한**: ✅ **2026-09-14 09:00 KST 전**(2026-09-07 창 이동 후. 1-6 드릴을 하면 드릴 +7일). 상단 기한 표와 같은 값이다 —
**08-30 창도 09-06 창도 Step 1으로 막지 않는다고 확정했다**(아래 "회전 창과 Step 1 라이브 시점").
⚠️ **09-06을 기한으로 적지 않는 이유**(r13 claude-ide#1): 같은 문서가 그 기한을 불가능하다고 확정하므로,
그대로 두면 **선언된 기한이 작성 시점에 이미 위반된 상태**가 된다 — `:32-33`이 r4 결함으로 인용한 형태다.
✅ **단계 순서 — 승인 게이트 (a) 확정 반영**(2026-09-04. 1-1a는 폐지되고 다섯 리소스는 1-1이 승계한다):
**1-1 → 1-2 → 1-3(apply) → 1-4 → 1-5(런북) → 1-6(드릴) → 1-7(자동 창).**
런북을 드릴보다 **앞에** 둔다 — 드릴 중 자동화가 실패하면 그 순간 필요한 것이 "어느 계층에서 실패했는지
구분하는 법"이다(r3 claude-ide 개선).

**1-1a. ⛔ (a) 채택으로 폐지 — 다섯 리소스는 1-1이 승계한다** (승인 게이트 2026-09-04 확정)

ⓑ·ⓒ 조회 결과 **광역 rule이 존재하지 않고 원문도 없었다.** (b)는 *"1-1a 작성·검토·apply가 09-06 창
전에 끝나야"* 하는데 **이틀 남아 불가능**하고, 못 대면 1-3이 **두 창** 뒤로 밀린다. → **(a) 확정.**

*이 단계가 사려던 것*(r8 claude-ide#3): 이 설계 전체가 **"회전 1회 = 매칭 이벤트 1개이고, 그 `detail.versionId`가
`AWSCURRENT`를 얻은 쪽"** 이라는 전제에 기댄다. 회전의 `finishSecret`은 새 버전에 `AWSCURRENT`를 붙이면서
**옛 버전에 `AWSPREVIOUS`를 옮기므로**, 매칭 이벤트가 하나만 나오는지·`versionId`가 항상 라벨을 얻은 쪽인지가
전제였다. 어긋나면 그 호출은 **60초 소진 → 오류 → 재시도 2회 → on-failure SQS**로 끝나 **회전이 성공했는데도
Lambda `Errors`·DLQ depth 알람이 뜬다.** 1-1a는 그 검증을 **1-3 apply 앞으로** 당기려던 장치였다.

⛔ **(a)를 택했으므로 전제 검증은 라이브 전환 뒤(1-6)로 돌아간다.** 근거 둘로 감수한다:
1. **AWS 공식 이벤트 계약이 1차 출처로 있다** — `AWSPENDING`·`AWSPREVIOUS`에는 `Secret Label Updated`를
   **발행하지 않고**, `detail.versionId`는 **라벨이 붙은 새 버전 ID**다
   ([이벤트 알림](https://docs.aws.amazon.com/secretsmanager/latest/userguide/secret-event-notifications.html),
   [이벤트 구조](https://docs.aws.amazon.com/secretsmanager/latest/userguide/event-detail-secret-label-updated-secretsmanager.html)).
   전제는 **문서상 이미 뒷받침된다.** 원문은 그것을 운영 증거로 확인하는 것이지 유일한 근거가 아니다
   (r12 codex-cli 개선).
2. **1-6 드릴이 진짜 회전을 일으키므로 원문이 확실히 온다** — 자연 창을 기다리지 않는다.
   전제가 어긋나면 그때 **1-1 패턴과 handler 라벨 확인 계약을 고친다**(재검토 라운드를 한 번 더 돈다).

**➡️ 다섯 리소스는 1-1이 함께 만든다**(r15 codex-ide#3 — *"광역 관찰 배선은 최종 Step 1 구성의 일부"*라
빠지면 **rule 미매칭이 무증상**이 된다. 리스크표의 그 행이 이 배선을 완화 수단으로 지목한다):
1. `aws_cloudwatch_event_rule` — 광역 관찰 rule(`source=aws.secretsmanager` + `resources=[대상 ARN]`, `detail` 필터 없음)
2. **`aws_cloudwatch_event_target`** — 타깃 = 아래 log group. ⚠️ Terraform에서 rule과 target은 **별도 리소스**이고
   이 저장소도 그 선례를 따른다(`eventbridge.tf:14`의 `aws_cloudwatch_event_rule.deploy_failed`와 `:74`의
   `aws_cloudwatch_event_target.deploy_failed_sns`). **빠뜨리면 `describe-rule`·resource policy 검사는
   전부 통과하는데 target이 0개라 원문을 한 건도 얻지 못한다**(r9 codex-ide#1 [높음]).
3. `aws_cloudwatch_log_group` — `/aws/events/` 접두사, `retention_in_days` 명시
4. `aws_cloudwatch_log_resource_policy` — principal **둘**(`events.amazonaws.com`·`delivery.logs.amazonaws.com`),
   action 둘, resource를 **stream 범위(`:*`)까지**(r9 codex-ide#2, AWS 공식 예제 형태)
5. **`aws_cloudwatch_metric_alarm` — 광역 rule의 `FailedInvocations` 1개.** `alarm_actions`는 기존
   `aws_sns_topic.alarms`를 쓴다(아래 알람 표와 같은 계약).

→ **1-1의 `plan` 기대값이 이만큼 커진다**: 위 다섯이 **신설로 뜨는 것이 정상**이고,
   **신설 알람은 6개가 아니라 7개**다(아래 1-1 검증에 반영돼 있다).
→ 검증도 1-1이 승계한다: `describe-rule`로 `State=ENABLED`, **`list-targets-by-rule`로 Logs target ARN**,
   `describe-resource-policies`로 **principal·action·resource 값 단언**(문서 존재 확인이 아니다),
   `describe-alarms`로 `FailedInvocations` 알람의 `ActionsEnabled`·`AlarmActions`.

ℹ️ **3분기 폴백(판정 시각 · `TriggeredRules` 분기)은 함께 삭제했다** — 대조할 원문이 1-3 apply **전에**
오지 않으므로 분기 자체가 성립하지 않는다. 원문 확인과 패턴 대조는 **1-6이 승계**하고,
1-6의 검증 항목에 *"광역 rule 로그 그룹에 실이벤트 원문이 적재됐는지"*가 이미 들어 있다.

**1-1. EventBridge rule + Lambda + IAM + 실패 보존을 Terraform으로 작성한다**
(✅ **(a) 확정으로 1-1이 전부 만든다** — 광역 관찰 **다섯 리소스**(위 1-1a 항목) + 좁은 rule·Lambda·IAM·
DynamoDB·SQS·**알람 7개**. revision 16까지는 *"다섯 리소스는 이미 apply됐으니 손대지 않는다"*였고,
그 전제는 **ⓑ 조회에서 rule이 존재하지 않는 것으로 반증됐다**.)

*좁은 rule (실제 동작용)*
- `source=aws.secretsmanager`, `detail-type="Secret Label Updated"`, `resources=[대상 시크릿 ARN]`,
  **`detail.labelUpdated = ["AWSCURRENT"]`**.
  ⚠️ **event pattern의 비교 값은 배열이다**(r3 codex-cli#1). revision 3이 *"문자열, 배열 아님"*이라고
  못박은 것은 **event pattern 문법과 반대**였다 — 이벤트 **본문**의 `labelUpdated`는 문자열이지만
  **패턴**은 배열이어야 하며 AWS 공식 예제도 `"labelUpdated": ["AWSCURRENT"]`를 쓴다.
  1-2 fixture에서는 **본문 `"AWSCURRENT"`(문자열) / 패턴 `["AWSCURRENT"]`(배열)** 로 구분해 적는다.

*광역 rule (관찰 전용)* — 미매칭은 무증상이라 실이벤트 원문을 남긴다.
- `source=aws.secretsmanager` + `resources=[대상 ARN]`만 매칭, `detail` 필터 없음. 타깃 = CloudWatch Logs.
- ⚠️ **EventBridge의 Logs 타깃은 IAM role을 쓰지 않는다**(r3 3인 전원 지적). 필요한 것은
  **log group의 resource-based policy**다
  ([resource-based policies](https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-use-resource-based.html)).
  → (a) `aws_cloudwatch_log_group`(`/aws/events/` 접두사 관례, `retention_in_days` 지정)
     (b) `aws_cloudwatch_log_resource_policy`(해당 log group ARN 한정)
     (c) **타깃에 `role_arn`을 주지 않는다.**
  ⚠️ **policy를 AWS 공식 예제 형태 그대로 쓴다**(r9 codex-ide#2 — revision 9는 principal이
  `events.amazonaws.com` 하나뿐이라 공식 계약보다 좁았다):
  - **principal 둘**: `events.amazonaws.com` **그리고 `delivery.logs.amazonaws.com`**
  - **action 둘**: `logs:CreateLogStream`·`logs:PutLogEvents`
  - **resource**: log group ARN의 **stream 범위까지**(`…:log-group:/aws/events/<이름>:*`)
  이게 빠지면 target을 제대로 붙여도 **첫 실이벤트가 `FailedInvocations`로 끝난다.**
  1-3의 `describe-resource-policies` 검증은 **principal·action·resource 값을 실제로 단언**한다.
  이게 빠지면 좁은 rule이 미매칭일 때 **원문을 남기려던 장치까지 함께 침묵**해 이 rule의 존재 이유가 사라진다.
- 광역 rule의 `FailedInvocations` 알람도 **1-1이 만든다**(✅ (a) 확정). 알람 표의 7개 전부가 1-1 소속이다.

*Lambda handler 계약*
- **멱등 키는 이벤트 `detail.versionId`, 저장소는 DynamoDB 조건부 쓰기다**
  (r5 codex-cli#1·#2 / codex-ide#1·#2 / claude-ide#1 — **결함 1·2를 한 장치로 닫는다**).

  revision 5는 같은 라운드에 두 처방을 넣었는데 **(A)가 (B)를 무효화했다**:
  (A) 멱등 비교 기준을 이벤트 `time` → `credentialsReadyAt`으로 교체,
  (B) 첫 호출이 새 deployment를 관측하고 반환해 순차 중복 억제.
  `credentialsReadyAt`은 **호출마다 새로 계산**되므로 지연 중복 호출은 더 늦은 값을 갖고,
  기존 deployment의 `createdAt`은 항상 그보다 과거라 **skip 조건을 영영 만족하지 못한다**
  → **중복마다 재배포를 보장**한다. 오류 반환 후 Lambda 재시도도 같은 경로를 탄다.
  **비교 기준이 호출마다 달라지면 어떤 중복 억제도 성립하지 않는다** — 이벤트 `time`이 dedup에
  작동했던 이유가 *모든 전달·재시도에서 같은 값*이기 때문이다.

  → **시각 비교로 멱등을 구현하지 않는다.** 안정적인 키에 조건부로 기록한다:
  - **테이블**: `linkpulse-prod-rotation-idempotency`, 파티션 키 `versionId`(S), 온디맨드 과금
    (회전 7일 1회, **강한 일관성 읽기 1회 + 조건부 쓰기 1회/회전**이라 사실상 $0. 아래 비용 절과 같은 값).
    **TTL 속성명 `expiresAt`**(epoch 초, Number)을 `ttl` 블록과 handler `PutItem` 항목에 **둘 다** 넣는다
    (r6 claude-ide 개선 — 속성명을 안 적으면 TTL이 설정만 되고 아무것도 만료되지 않는다).
    **보존 7일**(r7 claude-ide 개선 — revision 7의 30일은 회전 주기 7일 대비 과하게 길었다).
    멱등 창의 end-to-end 실질 상한은 **약 2시간**이다(r10 codex-cli 개선 — EventBridge target과
    Lambda async의 `maximum_event_age_in_seconds`는 **서로 다른 계층의 독립 상한**이라 합성된다.
    아래 "재시도 값" 절 참조). **7일이면 84배 여유**이고, 아래 잔여 위험(운영 롤백)의 노출 기간도 줄어든다.
    ⚠️ **TTL 삭제는 만료 즉시가 아니라 통상 수일 내**이므로(r6 codex-ide 개선) **TTL은 claim lease가 아니라
    보존 정리용으로만 쓴다.** 그리고 **만료됐지만 아직 삭제되지 않은 아이템은 `GetItem`에 계속 보인다**
    (r8 codex-ide 개선) — 억제가 유지되는 실제 기간은 *"TTL 만료 전"*이 아니라 **실제 삭제 전(7일 + 통상 수일)**이다.
  - ⚠️ **잔여 위험**(r6 codex-cli 개선): 파티션 키가 `versionId` 하나라 **같은 시크릿 버전에 `AWSCURRENT`가
    다시 부여되는 운영 롤백**은 **아이템이 실제로 삭제되기 전**(7일 + 통상 수일)에는 중복으로 간주돼 skip된다.
    이 plan의 범위에서는 그런 롤백이 없으므로 감수하고, 필요해지면 `event.id` 복합 키로 바꾼다.
  - **⚠️ 기록은 `UpdateService` 성공 관측 *뒤*다 — fail-open**(r6 codex-cli#1 / codex-ide#1 / claude-ide#1,
    **3인 전원 [높음]**). revision 6은 `PutItem`을 `UpdateService` **앞**에 뒀는데, 그 사이에서 실패하면:
    ```
    호출 1  라벨 확인 OK → PutItem 성공 → UpdateService 실패(스로틀/일시 오류/timeout)
            → 오류 반환
    재시도  라벨 확인 OK → PutItem → ConditionalCheckFailed(아이템 존재)
            → "처리됨" 판정 → skip + 성공 반환
    결과    재배포 0회. 오류가 아니므로 계층 2 on-failure로 가지 않고,
            알람 7개 중 무엇도 울지 않고, 같은 versionId는 앞으로도 전부 skip  ⇒ 영구 봉인
    ```
    **이 plan이 스스로 "자동 복구가 조용히 죽는 것이 최악 실패 모드"라고 규정한 그 실패를,
    중복을 막으려고 넣은 장치가 만들어낸다.** revision 6의 *"기록이 `UpdateService` 전이므로 재시도가
    안전하다"*는 **라벨 확인 실패 경로에만 맞고 기록 이후 경로에는 정반대**였다.
  - **⚠️ 부작용 *앞*에 읽는 지점이 하나 있어야 한다**(r7 codex-cli#1 / codex-ide#1 / claude-ide#1,
    **3인 전원 [높음]**). revision 7은 fail-closed를 뒤집으면서 **앞쪽 읽기를 함께 잃었고**, 그 결과
    조건부 쓰기가 *"부작용이 끝난 뒤의 skip"*만 남아 **아무것도 억제하지 못했다** — 중복 이벤트는
    `UpdateService`를 부른 **뒤에야** `ConditionalCheckFailed`를 받았다. skip이라는 이름만 남고
    재배포는 이미 일어난 상태였다.
  - **⚠️ marker의 의미를 좁혀 적는다 — "제출됨"이지 "복구됨"이 아니다**(r8 codex-cli#1 [높음] /
    claude-ide 개선). revision 8은 `UpdateService` 응답의 PRIMARY id를 *"관측 성공"*으로 불렀는데,
    **PRIMARY는 "가장 최근 배포"라는 지위일 뿐 rollout 결과가 아니다.** 새 배포는 `IN_PROGRESS`로
    시작해 나중에 `COMPLETED` 또는 (서킷브레이커 사용 시) `FAILED`가 된다. 즉 새 태스크가 시크릿
    주입·헬스체크에서 실패해 롤백돼도 marker는 이미 남고, 같은 `versionId`의 후속 전달은 전부
    skip되며, 최초 호출도 성공 반환했으므로 **on-failure 경로도 타지 않는다.**
    → **marker와 `outcome`을 `redeploy_submitted`로 명명한다**: *"이 `versionId`에 대해
    `UpdateService`가 200을 반환했다"* 까지만 뜻한다.
    **handler는 rollout을 추적하지 않는다** — 롤링은 수 분이고 함수 예산은 105초라 원리적으로 불가능하다.
    **rollout 실패는 이 Lambda의 책임이 아니라 다른 계층이 받는다**: ECS 배포 서킷브레이커의 자동 롤백
    (`ecs.tf:93-96`) + 기존 ALB/ECS 알람 + **plan 0008의 탐지(canary `/readyz`)** → **수동 브릿지**.
    리스크표에 이 잔여 위험을 한 행으로 명시한다.
  - **확정 흐름**:
    ```
    GetItem(versionId, ConsistentRead=true)
      ├─ 아이템 있음 → skip + 성공 반환                       ← 앞쪽 읽기 (중복 억제)
      └─ 아이템 없음 → 라벨 확인(선행 조건)
                     → UpdateService(forceNewDeployment)
                     → 응답에서 PRIMARY deployment id 확보
                     → PutItem(ConditionExpression: attribute_not_exists(versionId))
                     → 성공 반환                              ← 뒤쪽 쓰기 (fail-open 유지)
    ```
    **`ConsistentRead=true`가 필수다** — 기본 최종 일관성 읽기는 직전에 쓴 marker를 못 볼 수 있어
    억제가 확률적으로만 동작한다.
    **fail-open은 그대로다**: `PutItem` 전 어디서 실패해도 아이템이 없으므로 재시도가 재배포를 다시 건다.
    **완료 후 도착한 순차 중복은 실제로 억제된다** — `GetItem`이 부작용 전에 걸러낸다.
  - **계약은 at-least-once다.** 남는 중복 창은 **둘뿐이다**: ① 두 호출이 **동시에 `GetItem` miss**
    (선행 조회는 강한 일관성이어도 원자적이지 않다), ② `UpdateService` 성공 뒤 `PutItem` 전 실패 →
    재시도가 재배포를 한 번 더 건다.
    ECS `UpdateService`에는 `clientToken` 같은 멱등 파라미터가 없으므로(r6 codex-ide#1) API 수준의
    exactly-once는 불가능하다. **중복 재배포(fail-open)와 재배포 소실(fail-closed) 중 후자가 압도적으로 나쁘다** —
    전자는 무해한 롤링 한 번(서킷브레이커도 있다), 후자는 **서비스가 내려간 채 알람도 없다.**
    상태 기계(`CLAIMED`/`COMPLETED` + lease + 조건부 takeover)로 정확도를 더 올릴 수 있으나
    (r6 codex-cli#1 / codex-ide#1 제안), 회전이 7일 1회이고 중복의 대가가 롤링 한 번이라
    **상태 기계를 늘리지 않는다**(AGENTS.md 운영 책임 원칙 — 사람이 이해하고 운영할 수 있어야 한다).
  - **반환값 규칙을 한 줄로 못박는다**(r7 claude-ide 개선): `GetItem` 적중 또는 `PutItem`
    **`ConditionalCheckFailedException` = 성공 반환**, **그 외 모든 실패 = 오류 반환**.
    ⚠️ 두 성공은 **서로 다른 일이 일어난 것**이므로 `outcome`으로 구분한다(r8 codex-cli#5) —
    전자는 재배포를 **부르지 않았고**, 후자는 재배포를 **이미 제출했다.** 아래 `outcome` 표를 따른다.
    구현자가 `ConditionalCheckFailedException`만 따로 잡아야 한다는 것을 놓치지 않게 한다
    — 스로틀·5xx를 조건 실패와 함께 삼키면 fail-closed가 되살아난다.
  - **조건부 쓰기가 원자화하는 것과 하지 않는 것을 구분한다**(r7 3인 전원). 조건부 `PutItem`은
    **"누가 완료 marker를 쓰는가"를 원자화한다** — 계정 동시성 한도와 무관하다. 그러나
    **"누가 `UpdateService`를 부르는가"는 원자화하지 않는다.** 순차 중복은 선행 `GetItem`이 막고,
    동시 중복은 위 at-least-once 계약이 받는다. revision 7까지 리스크표·테스트가 조건부 쓰기에
    기대했던 *"동시 재배포 방지"*는 **이 장치가 줄 수 없는 보장이었다.**
  - `reserved_concurrent_executions = 1`은 **defense-in-depth로 유지하되 필수 게이트에서 내린다.**
    **1-1 HCL을 쓰기 전에**(r9 claude-ide 개선 — 예약 포함 여부는 리소스 인자라 HCL 작성 시점에
    결정된다. *"착수 전"*은 모호해 HCL을 썼다가 되돌리게 한다) `aws lambda get-account-settings`로
    예약 가능 여부를 확인하고
    (*"Unreserved account concurrency **minus 100**"* 하한), **부족하면 예약 없이 진행한다** —
    예약이 있으면 동시 창 ①이 실질적으로 닫히고, 없으면 at-least-once 잔여 위험으로 남는다
    (대가는 롤링 한 번이다).
  - **IAM**: execution role에 해당 테이블 ARN 한정 **`dynamodb:GetItem` + `dynamodb:PutItem`** 두 action.
    선행 조회를 되살렸으므로 `GetItem`이 반드시 필요하다(r7 claude-ide#1). `UpdateItem`은 여전히 불필요하다.

- **라벨 확인은 비교 기준이 아니라 `UpdateService`의 선행 조건이다**(r5 claude-ide#1).
  r4 codex-cli#2가 지적한 *"자격증명 가용 확인 전에 시작된 배포가 skip된다"*는
  **라벨 확인을 선행 조건으로 두는 것만으로 닫힌다** — 확인 전에는 `UpdateService`를 호출하지 않으므로
  이 Lambda가 만든 deployment는 **항상 자격증명 가용 이후**다. 비교 기준까지 바꿀 필요가 없었다.

- **전파 지연 대기 — `DescribeSecret` 선행 조건**(r3 codex-cli#2, r4 3인).
  이벤트 수신 직후 곧바로 재배포하면 새 태스크가 **옛 `AWSCURRENT`를 주입받아 그대로 실패**할 수 있다.
  - **API는 `secretsmanager:DescribeSecret`으로 확정한다**(대상 ARN 한정). `VersionIdsToStages`만으로
    *"이벤트 `detail.versionId`에 `AWSCURRENT`가 붙었는가"*를 판정할 수 있고 **암호화된 값을 반환하지 않는다.**
    **`GetSecretValue`는 쓰지 않는다** — 비밀번호 원문을 함수 메모리로 끌어오는데 이 함수는 이벤트 필드를
    로그로 찍으므로 불필요한 blast radius다(가드레일 #2).
  - **bounded wait: 최대 60초**(값 고정 — 함수 `timeout`이 여기서 파생된다). 상한 내 미확인이면
    **오류를 반환**해 재시도 경로를 타게 한다. **DynamoDB 기록 전에 실패하므로 재시도가 안전하다.**
  - **⚠️ 잔여 위험 — bounded wait는 창을 좁히는 heuristic이지 폐쇄가 아니다.**
    `DescribeSecret`이 본 것과 **ECS execution role이 태스크 기동 시 하는 읽기**는 다른 호출이다.
    `GetSecretValue`로 바꿔도 그 역시 제3의 호출이라 완전한 증명이 아니다 —
    **최소권한을 택하고 잔여 위험은 "탐지 → 수동 브릿지"가 받는다**(리스크표).

- **실행 예산을 수치로 확정한다**(r5 codex-cli#3 / codex-ide#3 / claude-ide#5 — `timeout = 120`의 근거 완결).

  **확정 흐름의 호출 순서 그대로 합산한다**(r7 3인 전원 — revision 7의 표는 이동 전 잔재 행이 남아
  DynamoDB 행이 둘이었고, 합계가 선언값 105초가 아니라 **120초**여서 *"합계가 `timeout`과 같으면
  안전 여유가 아니다"*라고 적은 바로 그 상태였다):

  ⚠️ **구간 예산은 SDK 재시도까지 포함해 산정한다**(r8 codex-cli#2 / codex-ide#1, **2인 동일**).
  revision 8은 *"각 구간의 boto3 connect/read timeout을 이 표의 값으로 설정한다"*로 끝냈는데,
  **`connect_timeout`·`read_timeout`은 한 번의 연결/읽기 시도 제한이지 API 호출 전체의 제한이 아니다.**
  botocore는 timeout·5xx·스로틀을 **별도 횟수·지수 backoff로 재시도**하므로 `read_timeout = 20초`만
  정해서는 그 구간이 20초에 끝난다는 보장이 없다. 그러면 `remaining < 15초` 검사를 호출 **전에** 해도
  이미 시작한 재시도를 끊지 못해 **120초에 강제 종료되고 약속한 `failed_deadline` 로그조차 안 남는다.**
  → **`botocore.config.Config`에 `retries = {"mode": "standard", "total_max_attempts": N}`을 고정하고,
  `시도 수 × (connect + read) + backoff`가 구간 상한 안에 들도록 값을 정한다:**

  ⚠️ **backoff 상한은 모든 행에 같은 값(2초)을 쓴다**(r9 claude-ide#3 — revision 9의 표는 같은
  `mode: standard`·같은 `attempts: 2`인데 행 1·4는 backoff 1초, 행 3은 2초로 잡아 **둘 중 하나가 틀린**
  상태였다. 1초가 맞으면 행 3의 20초 예산에 근거가 없고, **2초가 맞으면 행 1·4의 최악이 6초라 예산 5초를
  넘어** — 그러면 단계 deadline이 **정상적인 스로틀 재시도 한 번을 잘라낸다.** `PutItem` 쪽이 특히 나쁘다:
  `UpdateService`는 이미 200을 받았으므로 (f) 경로를 타 **중복 재배포 + `Errors` 알람**으로 승격된다).
  → ~~2초로 통일하고 행 1·4의 예산을 6초로 올린 뒤, 여유를 15 → 13초로 줄여 합계 105초를 유지한다.~~
  **⬆️ r9의 결론이고 revision 23에서 대체됐다 — 아래 ⛔ 참조(예산 6은 게이트가 항상 닫히는 값이다).**

  ⛔ **revision 22까지 이 표의 행 1·4가 `예산 = 최악`(둘 다 6초)이었고, 그것이 구현에서
  [높음] 결함이 됐다**(코드 교차 검토 round-1 codex-cli#1 / claude-ide#1, 2인 독립 재현).
  **단발 구간은 진입 게이트가 `deadline = monotonic() + 예산`을 잡은 **바로 다음 문장**에서
  시계를 다시 읽으므로, `단계 잔여 = 예산 - ε`가 되어 `예산 == 최악`이면 `잔여 < 최악`이
  **항상 참**이다.** 정상 이벤트도 AWS 호출 전에 `failed_deadline`으로 끝나 **재배포가 0회**가 된다.
  실시계 시행에서 **10회 중 10회 차단**됐고, 정지 시계 fake가 단위 테스트 19개 전부에서 이를 가렸다.
  → **행 1·4의 예산을 8초로 올리고 여유를 13 → 9초로 줄여 합계 105초를 유지한다.**
  **8은 새 숫자가 아니라 행 3(`UpdateService`)이 이미 쓰던 `예산 = 최악 + 2` 패턴으로 되돌린 값**이고,
  그러면 단발 세 구간의 여유가 2초로 균일해진다. 수정 후 실시계 시행 **2,000회 차단 0회**로 확인했다.

  ⚠️ **불변식은 `예산 > 최악`이다.** 이 표의 어떤 행도 둘을 같게 두지 않는다.

  | # | 구간 | boto3 `Config` | 최악 실행시간 | 예산 |
  | --- | --- | --- | --- | --- |
  | 1 | `GetItem`(`ConsistentRead=true`) — 앞쪽 읽기 | connect 1 / read 1 / attempts 2 | 1+1 **+backoff 2** +1+1 = 6 | **≤ 8초** |
  | 2 | 라벨 확인 bounded wait (`DescribeSecret` 폴링) | connect 1 / read 3 / attempts 1, 호출 간 sleep 5초 | **단계 monotonic deadline으로 강제** | **≤ 60초** |
  | 3 | `UpdateService` | connect 2 / read 6 / attempts 2 | 2+6 **+backoff 2** +2+6 = 18 | **≤ 20초** |
  | 4 | `PutItem` 조건부 쓰기 — 뒤쪽 쓰기 | connect 1 / read 1 / attempts 2 | 6 | **≤ 8초** |
  | 5 | SDK 지터·최종 로그/반환 여유 | — | — | ≤ 9초 |
  | | **합계 = handler 내부 deadline** | | | **105초** |
  | | **함수 `timeout`** | | | **120초** (내부 deadline보다 15초 크게) |

  ℹ️ **합계 105는 장부 숫자다** — 각 단계가 `_monotonic() + BUDGET`으로 deadline을 새로 잡으므로
  105초짜리 전역 deadline은 코드에 존재하지 않는다. 실제 강제 장치는 `RESERVE_SECONDS`(함수 잔여)와
  단계별 deadline 둘이다. 그리고 **단발 구간에서 `stage` 항은 `예산 > 최악`인 한 바인딩되지 않는다** —
  그 셋을 지키는 것은 `function` 항 하나이고, `stage` 항이 실제로 도는 곳은 라벨 확인 폴링 루프다.

  ⚠️ **botocore의 backoff 상한은 `Config`로 직접 지정할 수 없다** — 위 2초는 *"`total_max_attempts = 2`이면
  재시도가 1회뿐이고 그 지연이 지수 backoff의 가장 작은 구간"*이라는 **가정**이다. 따라서
  **실제 강제 장치는 아래 단계별 monotonic deadline이지 이 수치가 아니다.** 1-2 (h)②가 그것을 고정한다.

  **상한 합계가 함수 `timeout`과 같으면 안전 여유가 아니다**(r6 codex-ide#3) — 모든 구간이 상한에 닿으면
  반환·직렬화 전에 런타임이 종료된다. **합계 105초 = 내부 deadline, 함수 `timeout` 120초**로 15초를 벌린다.

  **강제 방식 — 단계별 monotonic deadline**(r8 codex-cli#2):
  - 각 구간 진입 시 `time.monotonic()` 기준 **단계 deadline**을 잡는다. 라벨 확인 폴링의 sleep과
    재시도는 **전부 그 안에서** 돈다(초과하면 루프를 깨고 `failed_label_wait`).
  - **SDK 호출 진입 전마다** `가용 = min(단계 잔여, context.get_remaining_time_in_millis()/1000 - 15)`를
    계산하고, **가용 < 그 호출의 최악 실행시간**이면 호출하지 않고 즉시 중단한다.
    revision 8의 *"남은 시간 15초 미만이면 중단"* 단독 검사로는 **16초 남았을 때 최악 18초짜리
    `UpdateService`에 진입**하는 것을 막지 못했다.
  - 중단 시 구조화 로그(`failed_deadline`)를 남기고 오류를 반환한다(강제 종료되면 어떤 실패인지 기록조차 안 남는다).
- **재배포 제출 판정은 `UpdateService` 응답으로 끝낸다**(r7 codex-cli#4 / codex-ide 개선 / claude-ide 개선).
  **판정 기준은 `UpdateService`가 200을 반환했다는 사실 하나다.** 응답 `service.deployments[]`에서
  `status == "PRIMARY"`인 항목의 `id`는 **사후 추적용 로그 필드**로만 남긴다 — handler에는 그 id가
  방금 만들어진 것인지 직전 CI 배포의 것인지 **비교할 기준이 없다**(r8 claude-ide 개선. 실질 위험은
  낮다 — `forceNewDeployment`는 항상 새 배포를 만든다 — 하지만 revision 8의 *"자기 성공으로 오인할 수
  없다"*는 이 설계가 주지 않는 보장이었다).
  **별도 `DescribeServices` 조회를 하지 않는다** — ① ECS 최종 일관성 때문에 **정상 재배포를 관측 실패로
  오판해 중복 재배포를 만드는 경로**가 사라지고, ② IAM에서 `ecs:DescribeServices`가 빠진다.
  응답에 PRIMARY deployment가 없으면 **오류를 반환**한다(서비스 부재·권한 오류는 `UpdateService` 자체가 예외로 던진다).
- **수신 이벤트의 `id`·`time`·`detail.versionId`를 구조화 로그로 남긴다.** 1-7의 사후 증거가 여기 의존한다.
- **판정 결과를 구조화 로그의 `outcome` 필드로 구분한다**(r6 claude-ide 개선, **r8 codex-cli#1·#5로 재정의**).
  **네 값이 서로 다른 사실을 뜻한다** — 1-7의 사후 증거가 이 필드에 의존하므로 이름과 사실이 어긋나면
  회전 횟수와 중복 창을 잘못 읽는다:

  | `outcome` | 뜻 | `UpdateService` | marker |
  | --- | --- | --- | --- |
  | `redeploy_submitted` | 이 호출이 재배포를 제출하고 marker를 썼다 | 호출함(200) | 이 호출이 씀 |
  | `redeploy_submitted_raced_marker` | 재배포는 제출했으나 marker 경합에서 졌다 — **허용된 중복 재배포**(동시 miss) | 호출함(200) | 다른 호출이 씀 |
  | `skipped_duplicate` | 선행 `GetItem` 적중 — **재배포를 부르지 않았다** | 호출 안 함 | 이미 있음 |
  | `failed_<지점>` | 실패 — `failed_getitem` / `failed_label_wait` / `failed_update_service` / `failed_putitem` / `failed_deadline` | 지점에 따라 다름 | 없음 |
  | `failed_validation` | **환경변수 fail-fast 또는 이벤트 대조 실패** — 모든 AWS 호출보다 앞에서 끝난다 | 호출 안 함 | 없음 |

  ⚠️ **`failed_validation`은 revision 23에서 추가됐다**(구현 중 발견 — 이 plan이 환경변수 fail-fast와
  이벤트 `resources` 대조를 둘 다 요구하면서 그 실패의 이름을 주지 않아, 구현자가 임의로 붙이면
  1-7의 사후 증거가 표와 어긋난다). 이 값이 나오면 **회전과 무관한 설정·이벤트 문제**이고
  재배포는 시도되지 않았다. `error` 필드가 환경변수 누락인지 다른 시크릿 이벤트인지 구분한다.

  ⚠️ `redeploy_submitted*`는 **rollout 성공을 뜻하지 않는다**(위 marker 의미 절). *"재배포가 실제로
  성공했는가"*는 이 로그가 아니라 ECS deployment 상태·기존 알람·canary가 답한다.
- **⚠️ 이번 범위는 구조화 로그까지다 — 커스텀 지표·알람을 만들지 않는다**
  (r7 codex-cli#3 / codex-ide#3 / claude-ide#4). revision 7은 *"지표로도 내보내 알람으로 잡는다"*고
  적었으나 **발행 방식(EMF / metric filter / `PutMetricData`)·namespace·dimension·임계값·`treat_missing_data`가
  전부 없었고, `PutMetricData`라면 IAM 목록에도 없어 `AccessDenied`**가 된다. 약속만 남고 설계가 없는 상태다.
  → **약속을 제거한다.** 근거: ① 선행 `GetItem`을 되살려 *"skip만 계속 나온다 = 봉인"* 위험 자체가
  크게 낮아졌다(봉인은 fail-closed에서 나왔고 지금은 fail-open이다), ② 자동 복구가 죽는 경우의 신호는
  이미 **알람 7개 + on-failure SQS + 1-7 사후 검증 + plan 0008의 탐지**가 받는다, ③ 기한이 6일이라
  IAM·Terraform·비용·테스트를 새로 늘리지 않는다. 지표가 필요해지면 **EMF(추가 IAM 불요)**를 1순위로
  별도 라운드에서 다룬다.
- 로그에 SecretString·비밀번호가 절대 찍히지 않는다.

*실패 보존 — 두 계층*
- **(계층 1) EventBridge target DLQ + 재시도 정책** — EventBridge가 Lambda 비동기 큐에 **전달하지 못한** 경우.
  이 큐에는 `events.amazonaws.com`에 `sqs:SendMessage`를 주는 **queue policy**가 필요하다.
  ⚠️ **source를 제한한다 — confused deputy 경계를 여기도 적용한다**(r12 codex-cli#3).
  **실측 확인했다**: `:143`이 Lambda 리소스 정책을 *"`lambda:InvokeFunction`을 **해당 rule ARN**으로 제한"*
  하는데, **DLQ queue policy에는 그 경계가 없어 계정 밖 EventBridge까지 이 큐에 쓸 수 있다.**
  AWS 공식 예제도 `aws:SourceArn`을 해당 rule ARN으로 제한한다
  ([EventBridge DLQ 권한](https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-rule-dlq.html#eb-rule-dlq-permissions)).
  → queue ARN 한정 `Resource`에 더해 **`ArnEquals aws:SourceArn = <좁은 rule ARN>`**(+ `aws:SourceAccount`)을
  계약에 넣고, **1-3 검증에서 `get-queue-attributes`의 `Policy` 값을 단언**한다.
- **(계층 2) Lambda asynchronous invocation on-failure destination(SQS)** — handler의 `AccessDenied`·
  타임아웃·예외는 계층 1로 가지 않는다. **이 큐에는 queue policy가 불필요하다**(r3 claude-ide#5) —
  메시지를 넣는 주체가 **Lambda 서비스가 함수의 execution role로** 수행하므로, 필요한 것은
  execution role의 `sqs:SendMessage`뿐이다. 쓰이지 않는 신뢰 관계를 만들지 않는다.
- **재시도 값을 확정한다 — 두 계층 모두**(r3 codex-cli 개선, **r8 codex-ide#2**):
  - **계층 2 (Lambda async)**: `maximum_retry_attempts = 2`, `maximum_event_age_in_seconds = 3600`.
  - **계층 1 (EventBridge target)**: `aws_cloudwatch_event_target`의 **`retry_policy`에 같은 값**
    (`maximum_retry_attempts = 2`, `maximum_event_age_in_seconds = 3600`)을 **명시한다.**
    ⚠️ **명시하지 않으면 기본이 최대 24시간·185회 재시도**라(r8 codex-ide#2) — revision 8이 TTL 근거로
    쓴 *"멱등 창의 실질 상한은 1시간"*이 **성립하지 않는다.** 24시간 뒤 도착한 전달도 marker에 걸려
    억제되긴 하지만, 그때는 이미 다음 회전이 지나 **엉뚱한 세대의 이벤트로 재배포**할 수 있다.
  - 근거 — 회전은 7일 1회라 빈도가 낮고, 1시간이면 일시적 스로틀·전파 지연을 넘기기 충분하며,
    그보다 오래된 이벤트로 재배포하는 것은 오히려 위험하다.
  - ⚠️ **두 값은 서로 다른 계층의 독립 상한이라 단순히 "1시간"이 아니다**(r10 codex-cli 개선).
    EventBridge의 값은 **타깃 전달 재시도**의 상한이고, Lambda의 값은 **Lambda가 비동기 큐에 받은 뒤
    보관하는 시간**이다. 최악은 **합성**된다 — EventBridge가 마지막에 전달한 이벤트가 Lambda 큐에서
    다시 최대 1시간 대기할 수 있으므로 **end-to-end 상한은 약 2시간**으로 본다.
    → **TTL 7일이 충분하다는 결론은 바뀌지 않는다**(2시간 대비 84배). *"1시간/168배"* 표현만 정정한다.

*관측 — 알람 7개* (✅ **(a) 확정으로 7개 전부 1-1 소속**. revision 16까지는 *"1-1a에서 1개 + 1-1에서 6개"*였다) (r4 codex-cli#5 / codex-ide 개선 — revision 4는 "6개"라고 썼는데 세면 7개다)

⚠️ **설정을 표로 확정한다 — 특히 `alarm_actions`**(r9 codex-ide#3). revision 9까지는 지표 이름과
`treat_missing_data`만 있고 `dimensions`·`statistic`·`period`·`evaluation_periods`·`threshold`·
**`alarm_actions`가 없었다.** CloudWatch 알람은 action을 연결하지 않으면 **상태만 바뀌고 Slack 카드도
수동 브릿지도 시작하지 않는다** — 즉 *"알람 7개가 자동화 실패를 받는다"*는 이 plan의 완화가
**성립하지 않는 상태**였다. `infra/prod`의 기존 알람은 전부 `alarm_actions`/`ok_actions =
[aws_sns_topic.alarms.arn]`으로 SNS → Chatbot → Slack 경로를 명시한다(`monitoring.tf:62-63` 등).

| # | 지표 (namespace) | dimension | stat / period / eval | threshold | `treat_missing_data` | 작성 단계 |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | `FailedInvocations` (`AWS/Events`) — **광역 rule** | `RuleName` | Sum / 60초 / 1 | `> 0` | `notBreaching` | 1-1 |
| 2 | `FailedInvocations` (`AWS/Events`) — 좁은 rule | `RuleName` | Sum / 60초 / 1 | `> 0` | `notBreaching` | 1-1 |
| 3 | `Errors` (`AWS/Lambda`) | `FunctionName` | Sum / 60초 / 1 | `> 0` | `notBreaching` | 1-1 |
| 4 | `InvocationsFailedToBeSentToDlq` (`AWS/Events`) | `RuleName` | Sum / 60초 / 1 | `> 0` | `notBreaching` | 1-1 |
| 5 | `DestinationDeliveryFailures` (`AWS/Lambda`) | `FunctionName` | Sum / 60초 / 1 | `> 0` | `notBreaching` | 1-1 |
| 6 | `ApproximateNumberOfMessagesVisible` (`AWS/SQS`) — EventBridge DLQ | `QueueName` | Maximum / 300초 / 1 | `> 0` | `notBreaching` | 1-1 |
| 7 | `ApproximateNumberOfMessagesVisible` (`AWS/SQS`) — Lambda on-failure | `QueueName` | Maximum / 300초 / 1 | `> 0` | `notBreaching` | 1-1 |

**7개 전부** `alarm_actions = [aws_sns_topic.alarms.arn]`, `ok_actions = [aws_sns_topic.alarms.arn]`
(기존 저장소 관례 그대로 — 새 SNS 토픽을 만들지 않는다).
- 임계값이 전부 `> 0`인 이유: 이 지표들은 **정상 시 값이 없거나 0**이고, 한 건이라도 나오면 그 자체가
  *"자동 복구 경로에 문제가 생겼다"*는 신호다. 회전이 7일 1회라 노이즈가 없다.
- DLQ depth 2개만 `period = 300`인 이유: SQS 지표는 **활성화 시 최대 15분 지연**이 있어(아래) 1분 주기가
  의미 없고, 어차피 **확인용**이다.
- **`DLQ depth만으로는 "DLQ로 보내는 동작 자체가 실패한 경우"를 못 본다** — 그때 depth는 계속 0이다.
  권한 오류가 바로 이 실패를 만든다(#4·#5가 그것을 본다).
- **`treat_missing_data` 근거**(r4 claude-ide#6). `infra/prod`의 기존 알람은 **13개 전부**
  이 값을 의도적으로 나눠 쓴다. 신규 7개 중 `FailedInvocations`(2)·`Errors`·`InvocationsFailedToBeSentToDlq`·
  `DestinationDeliveryFailures`는 **정상 시 데이터가 아예 없는 희소 지표**라 기본값이면 INSUFFICIENT_DATA에
  앉는다 → **`notBreaching`**. **DLQ depth 2개도 `notBreaching`**이다(r5 codex-cli#4 / claude-ide#2 — revision 5의 근거가 1차 출처와 반대였다).
  SQS는 *"메시지가 있거나 어떤 작업이 접근하면 최대 6시간 활성"*으로 보고 **6시간 넘게 비활성이면 지표 전송을 중단**한다.
  회전은 7일 1회라 두 DLQ는 정상 시 메시지도 API 접근도 없으므로 `missing`이면 **영구 INSUFFICIENT_DATA에 상주**한다.
  ⚠️ 같은 문서: **비활성 큐가 활성화될 때 지표에 최대 15분 지연**이 있다 → **DLQ depth 알람은 최대 15분 늦게 뜬다.**
  빠른 신호는 `FailedInvocations`·Lambda `Errors`·`DestinationDeliveryFailures`(1분 지표)이고
  **DLQ depth는 확인용**이라는 순서를 런북(1-5)에 적어 *"DLQ가 조용하니 괜찮다"* 오판을 막는다.
→ 검증: `terraform fmt -check` · `validate` · `plan` 통과. plan 출력이 **신설 리소스만** 포함하고
   (**신설 알람은 7개다** — ✅ **(a) 확정으로 광역 rule `FailedInvocations`가 1-1로 돌아왔다**(2026-09-04).
   revision 16까지는 *"6개(광역 rule 알람은 1-1a 소속)"*였다. r10 claude-ide 개선:
   숫자를 여기 적어야 `plan` 출력 대조가 기계적으로 된다)
   **기존 ECS/ALB/RDS/알람 변경 0**, **`0 destroy`**. ✅ **광역 관찰 다섯 리소스는 이번에 신설로 뜬다**((a) 확정).
   AWS 이름·description은 ASCII만([[aws-descriptions-ascii-only]]).

**1-2. handler 단위 테스트 + 이벤트 패턴 fixture**
*가드레일 범위*: **변경형 AWS 명령을 실행하지 않는다.** 저장소 파일 작성(handler·테스트·fixture)은
구현 범위이며 금지 대상이 아니다(r3 codex-cli 개선 — revision 3의 "read-only" 표현이 작성까지
금지하는 것처럼 읽혔다).

*handler 단위 테스트* — fake `DynamoDB`/`UpdateService`/`DescribeSecret`으로.
**기대값은 확정 흐름(선행 `GetItem` → 라벨 확인 → `UpdateService` → `PutItem`)과 at-least-once 계약을
그대로 반영한다**(r7 3인 전원 — revision 7의 (b)(b-2)(b-3)(e)는 *"조건부 쓰기가 `UpdateService`를 막는다"*를
전제해 **작성한 대로 통과할 수 없었다**):
- (a) marker 없음(정상 첫 호출) → 라벨 확인 후 **`UpdateService` 1회 + `PutItem` 1회**
- (b) **완료 후 도착한 순차 중복**: `GetItem`이 marker를 반환 → **`UpdateService`를 부르지 않고**
  `outcome=skipped_duplicate`로 성공 반환. **이 테스트가 중복 억제의 유일한 증거다.**
  - (b-2) `GetItem`이 `ConsistentRead=true`로 호출되는지 확인한다(최종 일관성 읽기면 억제가 확률적이 된다)
  - (b-3) `PutItem`이 `ConditionalCheckFailedException`을 던진 경우(동시 miss 이후 늦게 진 쪽) →
    **성공 반환**하되 **`outcome=redeploy_submitted_raced_marker`**를 로그로 확인한다
    (r8 codex-cli#5 — 이 호출은 재배포를 **이미 제출했으므로** `skipped_duplicate`가 아니다.
    구분하지 않으면 1-7의 사후 증거가 실제 재배포 횟수를 잘못 센다). 재배포 중복은 계약 안이다
- (c) `versionId`가 아직 `AWSCURRENT`에 없음 → bounded wait → 상한 초과 시 **오류 반환**
  (marker를 쓰지 않았으므로 재시도가 안전하다)
- (d) `UpdateService` 응답에 **PRIMARY deployment가 없음** → **오류 반환**
- (e) **동시 호출**: 둘 다 `GetItem` miss → **둘 다 `UpdateService`를 부르고**, `PutItem`은
  **하나만 성공**한다. 성공 기준은 *"재배포 1회"*가 아니라 **"marker를 쓴 쪽이 정확히 하나이고,
  진 쪽도 오류가 아닌 성공으로 끝난다"**이다(r7 claude-ide#1 — 선행 조회는 원자적이지 않으므로
  *"둘 중 하나만 `UpdateService`"*는 이 설계가 줄 수 없는 보장이다). 중복 재배포는 계약상 허용된다.
  두 호출의 `outcome`이 각각 `redeploy_submitted` / `redeploy_submitted_raced_marker`인지 확인한다.
  ⚠️ **이 경로는 1-3의 예약 동시성 분기에 따라 실전 발생 빈도가 달라진다**(r8 claude-ide 개선) —
  예약이 잡히면 동시 호출 자체가 스로틀돼 사실상 발생하지 않고, 생략되면 잔여 위험으로 남는다.
  **테스트는 어느 쪽이든 유지한다**(계약 자체를 고정하는 값이 있다).
- (f) **`UpdateService` 성공 뒤 `PutItem`이 스로틀·5xx로 실패 → 오류 반환 → 재시도** →
  marker가 없으므로 **재배포가 한 번 더**(at-least-once 계약).
- (g) **`UpdateService` 실패 → 재시도** → 기록이 없으므로 **재배포를 다시 시도한다**(fail-open 검증).
- (h) **deadline 강제** — 둘 다 본다(r8 codex-cli#2 / codex-ide#1): ①진입 시 남은 시간이 부족해
  SDK 호출을 **아예 시작하지 않고** `outcome=failed_deadline`으로 끝난다. ②**느린 fake SDK**(재시도·sleep
  포함)가 단계 monotonic deadline을 넘길 때 루프가 깨지고 `failed_label_wait`/`failed_deadline`으로 끝난다
  — 진입 시 잔여 시간만 보는 구현은 ②에서 통과하지 못한다.
- (i) **`UpdateService` 응답의 PRIMARY가 `rolloutState=IN_PROGRESS`여도 `redeploy_submitted`로 marker를 쓴다**
  (r8 codex-cli#1의 의도를 **관측 가능한 형태로** 다시 씀 — r9 claude-ide 개선. revision 9의
  *"rollout이 `FAILED`가 되는 시나리오"*는 **handler가 rollout을 관측하지 않으므로 fake에 줄 입력이 없어**
  (a)의 복사본이 됐다. `rolloutState`는 응답에 실제로 들어 있으므로 fake로 줄 수 있고, 같은 것을 증명한다).
  같은 `versionId`의 후속 이벤트가 skip되는 것도 함께 확인한다. **이 잔여 위험을 리스크표가 받는다**
  (자동 재시도 없음 → 서킷브레이커·기존 알람·0008 탐지 → 수동 브릿지).
- (j) **`UpdateService` 요청 파라미터 계약**(r9 codex-cli#1): 환경변수에서 읽은
  **`cluster = $ECS_CLUSTER_ARN`·`service = $ECS_SERVICE_NAME`·`forceNewDeployment = True`**가
  정확히 그 값으로 전달되는지 fake로 단언한다. 그리고 **네 환경변수 중 하나라도 비면 기동 시 즉시 실패**한다.
- fake 목록에 **DynamoDB client**를 포함한다(r6 codex-cli#2 / codex-ide#2 — 새 핵심 의존성이 빠져 있었다).

*fixture — 용도가 정반대인 둘을 디렉터리로 나눈다*(r8 claude-ide 개선).
`docs/plans/0009-p4d2-rotation-resilience/fixtures/` 아래:
- **`pattern/`** — `aws events test-event-pattern`용. **`versionId` 값은 의미가 없다**(손으로 쓴 값이면 충분).
  **패턴은 배열, 샘플 이벤트 본문은 문자열**로 적는다.
  반례: (a) 다른 시크릿 ARN (b) 커스텀 스테이징 라벨 (c) 다른 `detail-type`(CloudTrail 유래).
- **`invoke/`** — 1-4의 `lambda invoke`용. **`detail.versionId`가 실제 값이어야 한다**(1-4 참조) —
  라벨 확인이 하드 선행 조건이라 임의 값이면 60초 소진 후 반드시 실패한다.
  파일명으로 구분해 두어 실행자가 잘못 집을 여지를 없앤다.
→ 검증: 단위 테스트 전부 통과. `pattern/`으로 대상 ARN + `AWSCURRENT`만 `true`, 반례는 전부 `false`.

**1-3. [사람] apply**
→ 검증: `list-rules`·`list-targets-by-rule`로 두 rule·타깃·DLQ 배선
   **그리고 좁은 rule 타깃의 `RetryPolicy`가 `MaximumRetryAttempts=2`·`MaximumEventAgeInSeconds=3600`인지**
   (r8 codex-ide#2 — 명시 안 하면 기본 24시간·185회라 "멱등 창 1시간" 근거가 무너진다.
   revision 8은 계층 2만 `get-function-event-invoke-config`로 확인했다),
   `get-function-event-invoke-config`로 on-failure destination·retry 값,
   `lambda get-policy`로 rule ARN 한정 `InvokeFunction`,
   **`get-function-configuration`으로 `Timeout=120`·`MemorySize=256`**,
   `get-function-concurrency` — **예약 가능했으면 1, 불가능해 생략했으면 미설정**(분기형.
   ⚠️ 근거 문구를 계약에 맞춘다(r8 codex-ide#3 / codex-cli 개선): **조건부 쓰기가 보장하는 것은
   marker 유일성뿐이고**, 예약을 생략하면 **동시 중복 재배포는 at-least-once 잔여 위험으로 수용**한다.
   *"원자성을 DynamoDB가 담보한다"*는 end-to-end 재배포까지 포함하는 것처럼 읽혀 폐기한다),
   **`describe-alarms`로 신규 알람 7개의 `ActionsEnabled=true`·
   `AlarmActions`=`alarms` SNS ARN·`Dimensions`·`Statistic`·`Period`·`Threshold`·
   `treat_missing_data`(**7개 전부 `notBreaching`**)를 위 표와 대조**(r9 codex-ide#3 —
   action이 없으면 상태만 바뀌고 Slack 카드도 수동 브릿지도 시작하지 않는다),
   **`get-function-configuration`의 `Environment.Variables`에 `ECS_CLUSTER_ARN`·`ECS_SERVICE_NAME`·
   `IDEMPOTENCY_TABLE_NAME`·`SECRET_ARN`이 실제 값으로 들어 있는지**(r9 codex-cli#1),
   `dynamodb describe-table`로 키 스키마·billing mode·`ACTIVE`, **`describe-time-to-live`로
   `TimeToLiveStatus=ENABLED`와 속성명 `expiresAt`**(r6 codex-cli#4 — `describe-table`은 TTL 상태를 반환하지 않는다),
   **execution role 정책에 `dynamodb:GetItem`이 들어 있는지**(r7 claude-ide#1 — 빠지면 첫 중복
   이벤트가 `AccessDenied`로 끝나는데, 그때까지 아무 증상도 없다) **그리고 `ecs:DescribeServices`가
   들어 있지 않은지**(handler가 호출하지 않으므로 최소권한),
   `describe-log-groups`·`describe-resource-policies`로 광역 rule의 Logs 권한 확인 —
   **principal 둘·action 둘·resource(stream 범위 `:*`)에 더해 condition 2개
   (`StringEquals aws:SourceAccount`·`ArnEquals aws:SourceArn` = 광역 rule ARN)까지 값을 단언한다**
   (revision 23 추가. 코드 교차 검토 round-1 codex-ide#2 — AWS의 ECS lifecycle events 예제가
   **같은 두 principal에 두 조건을 함께** 걸므로, 조건 없이 두면 로그 그룹 ARN 한정만으로는
   *"누구를 대신해 호출하는가"*가 묶이지 않는다. 그 근거로 코드에 조건을 넣었으므로 검증도 따라간다).

**1-4. [사람] Lambda 직접 호출로 동작을 검증한다**
`put-events`로는 운영 rule을 종단 검증할 수 없다 — 고객 이벤트의 `source`는 `aws.`로 시작할 수 없다.
변경형이므로 **[사람]이 실행한다**(AGENTS.md #1).
⚠️ **①은 프로덕션 롤링 재배포를 실제로 일으킨다**(r8 claude-ide 개선 — revision 8은 검증 항목의
*"`deployments[].createdAt` 갱신"*에만 암시했다). 1-6과 같이 **통제된 짧은 롤링을 감수하는 단계**이므로
저트래픽 시간대에 수행하고 서킷브레이커가 켜져 있는지 먼저 확인한다.

⚠️ **fixture 제약 — 이것이 없으면 ①이 반드시 실패한다**(r8 claude-ide#2):
확정 흐름에서 `UpdateService` 앞의 라벨 확인은 **하드 선행 조건**이라, 임의의 `versionId`를 넣으면
`GetItem` miss → 60초 bounded wait 소진 → **오류 반환**으로 끝나고 ①-2(중복 skip)도 marker가 없어 함께 무너진다.
실행자가 *"자동화가 깨졌다"*로 오진하기 쉽다.
- **① fixture(`fixtures/invoke/`)의 `detail.versionId`는 `aws secretsmanager describe-secret`의
  `VersionIdsToStages`에서 현재 `AWSCURRENT`인 버전 id를 그대로 넣는다.**
  (1-2의 `fixtures/pattern/`은 패턴 매칭 검증용이라 `versionId` 값이 의미가 없다 — 재사용하면 안 된다.)
- **② 실패 fixture는 "존재하지 않는 `versionId`"로 만든다** — 라벨 확인이 상한까지 소진되고 오류를 반환하는
  경로가 목표다. 시도마다 60초를 태우므로 아래 대기 시간 계산에 **60초 × 3회(최초+재시도 2회)**가 들어간다.
ℹ️ **①이 남기는 marker는 현재 `AWSCURRENT` 버전의 `versionId` 것이다**(r9 claude-ide 개선).
실해는 없다 — 그 버전의 라벨 이동 이벤트는 이미 지나갔고 이후 회전은 새 `versionId`를 만든다. 다만
**멱등 테이블 잔여 위험**(같은 버전에 `AWSCURRENT` 재부여)과 겹치는 유일한 실사례이므로,
1-6·1-7에서 로그를 읽을 때 *"이 marker는 1-4가 남긴 것"*임을 알 수 있게 기록해 둔다.
→ 검증: ①정상 이벤트 → `deployments[].createdAt` 갱신 + `rolloutState=COMPLETED`, `/readyz` 200, 왕복 302.
   **①-2 같은 fixture를 한 번 더 invoke**해 `outcome=skipped_duplicate`로 끝나고 `deployments[].createdAt`이
   **갱신되지 않는지** 확인한다(r7 claude-ide 개선 — `GetItem` 경로와 그 IAM은 이렇게 하지 않으면
   **첫 중복 이벤트가 올 때까지 드러나지 않는다**).
   ②**실패 fixture를 `--invocation-type Event`(비동기)로 invoke**해 on-failure SQS에 실제 메시지가
   적재되고 `DestinationDeliveryFailures=0`인지 확인한다. **기본 동기 호출로는 on-failure destination을
   전혀 시험하지 못한다**(r4 codex-cli#4 / codex-ide 개선). 비동기 재시도 2회가 끝날 때까지의
   **대기 시간을 수치로 확정한다**: 최초 실행 + 재시도 2회 × (라벨 확인 60초 소진 + 백오프)
   + destination 전달 여유 → **최대 10분**(각 시도가 함수 `timeout` 120초까지 가지 않고 60초 상한에서
   오류를 반환하므로 여유가 충분하다. r8 claude-ide#2). 확인은 둘로 나눈다 — **`receive-message`로 실제 메시지 본문 확인(즉시)** /
   **DLQ depth 알람 전이 확인(최대 15분 + SQS 활성화 지연)**. 확인 후 **[사람]이 테스트 메시지를 삭제해 알람을 정상화**한다
   (안 하면 알람이 계속 ALARM에 남는다). 의도적으로 발생한 Lambda `Errors` 알람도 함께 정리한다.
   **rule→Lambda 배선은 여기서 증명되지 않는다** — 1-6이 그 역할이다.

**1-5. 런북 갱신 (드릴보다 먼저)**
- `secret-rotation-bridge.md`: (b) 수동 재배포가 자동화됐음을 명시하되 **자동화 실패 시 수동 절차로 남긴다**.
  **`(e)`의 회전 창 날짜도 함께 고친다** — 드릴로 스케줄이 이동한다(r3 claude-ide#1).
- `alarm-response.md` §13 분기 2: 로테이션 직후 알람이면 **자동 재배포가 돌았는지 먼저 확인**하는 순서와,
  **어느 계층에서 실패했는지 구분하는 법**(계층 1 DLQ / 계층 2 on-failure / 전달 실패 지표)을 적는다.
- **⚠️ 이 시점에는 "회전 5xx = 전부 브릿지 대상"이다 — 시점 가드를 문장 안에 넣는다**
  (r11 claude-ide#3. revision 11은 *"Step 2 라이브 이후:"*라는 **접두**만 붙였는데, 런북을 읽는 사람은
  접두보다 판별 기준을 먼저 본다).
  1-5는 **1-3 apply 직후**이고 Step 2 착수는 *"1-6 드릴 완료 후 7일 이내"*라 **그 사이에 몇 주가 있다.**
  그 기간에 회전이 나면 Step 2가 없으므로 5xx는 짧지 않고 **Step 1의 재배포(수 분)까지 지속된다.**
  → 런북에 적는 문장은 이것이다: *"**Step 2가 라이브가 아닌 동안 회전 직후의 `alb-target-5xx`는 전부
  브릿지 대상이다.** 자동 재배포(Step 1)가 돌았는지 먼저 확인하고, `/readyz`가 1분 안에 200으로
  돌아오지 않으면 브릿지."*
  ⚠️ **"짧은 5xx는 정상"은 Step 2가 라이브가 된 뒤에야 참이다** — 그 문장은 **2-5의 런북 갱신 항목**으로
  옮겼다(2-4 실측값이 나온 뒤에 적어야 추정이 아니라 실측이 된다).
→ 검증: 사람이 런북만 보고 "자동화가 돌았는지, 어디서 실패했는지"를 판정할 수 있는가.

**1-6. [사람] 온디맨드 회전 드릴 — 종단 증명** (0008 `:470` 승계)
```bash
aws rds modify-db-instance --db-instance-identifier linkpulse-prod-pg \
  --rotate-master-user-password --apply-immediately --region ap-northeast-2
```
- **⛔ 선행 조건: 1-3(apply)이 완료돼 Step 1이 라이브여야 한다.** 드릴은 진짜 회전을 일으키므로
  Step 1 없이 돌리면 장애를 하나 더 만드는 것이다.
- 수동 브릿지를 옆에 띄우고 (b-0) 관측 루프를 먼저 켠다. 통제된 수 분의 다운을 감수한다.
- **드릴 직후 `describe-secret`으로 `NextRotationDate`를 다시 읽어 1-7의 대상 창을 재계산하고,
  그 날짜를 브릿지 런북 `(e)`에 다시 기록한다**(r4 codex-ide#2 — 1-5에서 고친 날짜는 드릴 전 값이라
  드릴 후 다시 갱신해야 한다).
→ **컷오프: 10분**(1-7과 같은 기준). 10분 안에 회복되지 않으면 **드릴 실패로 보고 수동 브릿지로 전환**한다.
→ 검증: 사람 개입 없이 `/readyz` 회복. **광역 rule 로그 그룹에 실이벤트 원문이 적재**됐는지 확인.
   Lambda 로그에 수신 이벤트 `id`·`time`·`versionId` 존재. 회복 시간 실측.
   **이것이 Step 1의 실질 완료 조건이다.**

**1-7. [사람] 다음 자동 회전 창 — 사후 증거로 확인한다**
대상 창은 **1-6 이후 재계산한 `NextRotationDate`**다.
✅ **드릴을 건너뛰면 기한 표의 다음 창**이다. ⚠️ **여기에 날짜를 박지 않는다**(r21 claude-ide 개선) —
회전이 한 번 더 일어날 때마다 낡는다(실제로 09-06·09-07 두 번 갱신되며 두 번 낡았다).
**1-6이 이미 *"드릴 직후 `describe-secret`으로 다시 읽어 1-7 대상 창을 재계산한다"*를 갖고 있으므로
그 절차를 따른다.**
창이 24시간이라 폴링으로 지킬 수 없으므로 **사후 증거를 조합한다**:
`LastRotatedDate`(T0) + Lambda 로그의 수신 이벤트 `time`/`id` **와 `outcome` 필드**(r8 codex-cli#5 —
`redeploy_submitted` / `redeploy_submitted_raced_marker` / `skipped_duplicate`를 세어야 **실제 재배포 횟수와
중복 창**을 맞게 읽는다) + `deployments[].createdAt`·`rolloutState` +
`alb-target-5xx`·`canary_down` 알람 이력 + 광역 rule 로그.
⚠️ **`redeploy_submitted`는 제출까지만 뜻하므로**, 회복 판정은 반드시 `rolloutState`와 알람 이력으로 확인한다.
⚠️ **`deployments[]` 증거는 사라질 수 있다**(r9 codex-cli 개선): 회전 뒤 **CI 배포가 한 번 더 일어나면**
그 deployment가 `DescribeServices` 응답에서 밀려나 판정이 불가능해진다. → **검증 창 동안 배포를 동결**하거나
(런북 1-5에 체크 항목으로 넣는다), 동결이 어려우면 **Lambda 로그가 남긴 PRIMARY deployment id를
ECS 서비스 이벤트 이력·CloudTrail과 대조**하는 대안을 런북에 함께 적는다.
→ 검증: T0 → 회복까지 **10분 이내**. 초과·미발화면 회고에 기록하고 Step 2 우선순위를 올린다.

**회전 창과 Step 1 라이브 시점 — 판단 규칙과 그 적용 기록**

⛔ **먼저 규칙이고, 창별 판단은 그 아래 기록이다**(r19 claude-ide 개선 — revision 19까지 이 절은
*"특정 창(08-30·09-06)의 판단"*이 본문이라 **창이 지날 때마다 통째로 낡았다**):

> **판단 규칙**: **남은 작업(리뷰 라운드 + 구현 + 코드 교차 검토 3인 + PR + [사람] apply) >
> 창까지 남은 기간**이면 그 창에는 Step 1을 못 댄다. **이 판단을 조건부로 두지 않고 사실로 확정한다** —
> 조건부 항목이 조용히 떨어지는 것이 이 프로젝트가 **네 번** 당한 패턴이다(회고 A-11).
>
> ⚠️ **"라운드 간격 N일" 같은 상수를 쓰지 않는다.** 그 값은 작업 공백을 포함한 실측이라 라운드마다 바뀐다
> (r13에서 *"오늘이 09-03이고 남은 3일"*로 적었다가 **하루 만에 낡았고**, r19까지도 *"9일 뒤"*가 남아
> **또 낡았다** — r19 claude-ide#1. **처방을 적은 자리에서 처방을 어긴 형태다**).
> **못 대는 창에는 아래 "우발 계획"이 적용된다.**

*적용 기록*

| 창 | 판단 | 실제로 무슨 일이 있었나 |
| --- | --- | --- |
| **08-30 09:00 ~ 08-31** | 못 댄다 → 브릿지 사전 무장으로 막는다 | ⛔ **3차 장애 4일 22시간 22분 54초.** 브릿지 미실행 |
| **09-06 09:00 ~ 09-07** | 못 댄다 → 브릿지 사전 무장으로 막는다 | ⛔ **4차 장애 13시간 10분.** SMS까지 도착했는데 미실행 |
| **09-13 09:00 ~ 09-14** | 못 댄다 → **(ii) 창 이동을 택했다** | ✅ **2026-09-07 10:06 온디맨드 회전으로 이 창을 없앴다.** 통제된 다운 12분 34초. **판단이 실패하지 않은 첫 회차다** |
| **09-14 09:00 ~ 09-15** | **기한 표의 Step 1 라이브 기한이 이 창 직전이다** | 아래 "우발 계획" — 결정 기한 **2026-09-11 09:00** |

⛔ **두 창에서 같은 판단을 했고 두 번 다 장애가 났다. 그 판단의 후반부**(*"브릿지가 막는다"*)**가
두 번 반증됐다** — 기한 표 첫 행이 지금 *"실패가 기본값"*이라고 적는 이유다.

*08-30 창에 대해 당시 적어 둔 것 — 이제 반증된 부분을 표시한다*

**08-30을 막는 것은 다음 둘이라고 적었다**(r4 claude-ide#4):

1. **탐지 경로는 바뀌었으나 실측되지 않았다** — canary가 `/readyz`를 프로빙하므로 DB 장애 시
   **분당 약 33건**의 503이 발생해 `alb-target-5xx`가 ALARM에 고정되고 `canary_down`이 백스톱으로
   붙는 **설계**다(2026-08-16 장애 증거 시간당 약 1,976건 ÷ 60 ≈ 32.9 — ADR 0004 실측).
   ✅ **이 유보는 해소됐다 — 3·4차에서 두 번 실측됐다**(r19 claude-ide#1):

   | | 3차(08-30) | 4차(09-06) |
   | --- | --- | --- |
   | `alb-target-5xx` | 회전 **+6분 49초** | **+5분 37초** |
   | `canary_down` | **+8분 16초** | **+7분 41초** |
   | 플래핑 | **0회** | **0회** |
   | 알림 도달 | Slack 카드 | Slack + **SMS 2통** |

   **plan 0008의 목표(약 6분·상한 8분)에 두 번 부합했고, 두 런북의 *"실측은 아직 없다"* 유보도
   실측값으로 교체됐다**(`alarm-response.md` §13).
2. **[사람] 브릿지 사전 무장** — ⛔ **네 번 연속 실패했다.** 4차에는 **문자가 폰까지 도착했는데도
   13시간**이었다. 브릿지 런북 (f)가 그 실패를 다룬다.

**구간별 목표 — 3·4차로 갱신한다**(revision 4의 *"24시간 → 1시간 이내"*는 야간 구간에 근거가 없었고,
revision 19까지 남아 있던 표는 *"알람 자체도 미실측"*·*"깨어 있으면 복구 2분"*이라 **둘 다 반증됐다**):

| 구간 | 기대했던 것 | 실측 |
| --- | --- | --- |
| 깨어 있는 구간 | 폴링 간격 + 복구 2분 | ⛔ **3차는 주간 13:08 회전인데 118시간** — *"깨어 있는 구간"*이 안전하지 않다 |
| 야간 구간 | 보장 없음 | ⛔ **4차는 일요일 저녁 19:09 시작 → 밤을 넘겨 13시간.** 프레이밍이 틀렸다 — **가장 긴 장애가 주간 회전**이었다 |
| 알람 | 미실측 | ✅ **두 번 실측**(위 표). **탐지는 문제가 아니다** |

⛔ **결론: 다운 시간을 정하는 것은 시간대도 탐지도 아니라 "사람이 브릿지를 시작하기까지"다.**
3차는 그것이 **4일 22시간**, 4차는 **13시간**이었다. **Step 1만이 그 구간을 없앤다.**

ℹ️ **plan 0010(탐지 드릴)에 대한 당시 판단은 대체됐다** — *"08-30 전에 돌려 설계를 실측으로 만들 것인가"*를
논의했고 *"실측 없이 간다"*로 결론냈는데, **3·4차 실장애가 그 실측을 두 번 줬다.**
0010의 범위 재검토는 4차 회고 액션에 있다.

**우발 계획 — 일반 규칙**(r4 claude-ide#8):
> **Step 1이 라이브가 아닌 모든 회전 창에 대해, 창이 열리기 전 [사람]이 브릿지를 사전 무장한다.**
> 대상 창은 매번 `describe-secret`의 `NextRotationDate`로 재계산한다.

⛔ **그런데 이 규칙 하나만 남겨 두는 것이 회고 A-11이 경계한 형태다**(**r19 claude-ide#3 [중간]**).
기한 표 첫 행이 같은 수단을 *"네 번 연속 실패 — 실패가 기본값"*이라고 적는데,
**그 창의 대비책이 그것 하나뿐**이면 *"조건부로 조용히 미뤄지는"* 상태와 다르지 않다.

✅ **09-13 창에 대해서는 (ii)를 수행했다(2026-09-07 10:06). 아래는 그 판단 기록이고,
다음 창(09-14 09:00)에 같은 표를 다시 적용한다 — 결정 기한 `2026-09-11 09:00 KST`.**

**셋 중 하나를 고른다:**

| | 수단 | 대가 | 전제 |
| --- | --- | --- | --- |
| **(i)** | **Step 1 라이브** — **기한 표의 절대일자 전**(날짜를 여기 박지 않는다 — r21 claude-ide 개선) | 없음(이것이 목표다) | 병합 → 구현 → **코드 교차 검토 3인** → PR → apply가 **창까지 남은 기간 안에** 들어가야 한다 |
| **(ii)** | **창을 옮긴다** — [사람]이 지켜보는 시각에 온디맨드 회전을 일으켜 무방비 창을 뒤로 민다(브릿지 런북 **(f-4)**) | **통제된 다운 약 12분**(09-07 실측 12분 34초). ⚠️ **미는 폭은 "마지막 회전 + 7일" 기준**이라 직전 회전으로부터 지난 기간에 달렸다 | **[사람] 입회 + 브릿지 즉시 실행 + 의도적 다운 수용**(r21 codex-ide#4 — *"없음"*이 아니다) |
| **(iii)** | **브릿지 무장만** — 창 당일 능동 확인(런북 f-3)까지 포함 | **네 번 연속 실패한 수단.** 4차엔 SMS가 도착했는데도 13시간 | — |

⚠️ **(ii)는 이 plan이 r14에서 뺀 통제 창이 아니다** — rule을 끄지 않는다. **회전 시각만 옮긴다.**

⛔ **그러나 1-6의 선행 조건과는 화해가 필요하다**(**r21 claude-ide#3 / codex-ide#4**).
1-6은 *"⛔ Step 1이 라이브여야 한다 — **Step 1 없이 돌리면 장애를 하나 더 만드는 것**"*이라고 금지하는데,
**(ii)는 정확히 그것을 한다.** revision 21이 붙인 구분(*"통제 창이 아니다"*)은 **rule을 끄는지에 대한 것**이라
이 충돌을 다루지 않는다. → **런북에 이미 답이 있다**(`secret-rotation-bridge.md` f-4):

| | 1-6 드릴 | **(ii) = f-4 창 이동** |
| --- | --- | --- |
| 목적 | **Step 1 검증**(자동 복구가 도는가) | **무방비 창을 사람이 지켜보는 시각으로 옮긴다** |
| 전제 | ⛔ **Step 1 라이브 필수** | **[사람] 입회 + 브릿지 즉시 실행 + 의도적 다운 수용** |
| 복구 주체 | Step 1(자동) | **[사람] 브릿지 (b)** |
| 컷오프 | **10분**(자동 복구가 그 안에 끝나야 한다) | **적용되지 않는다** — 구간이 다르다(회전 명령 → 완료 대기가 포함된다. 09-07 실측 12분 34초) |

→ **(ii)는 1-6이 아니다.** 그리고 **2-6의 회전 트리거 (가)는 1-6과 같은 조건**(Step 1 라이브 후)이므로
**(ii)와 (가)는 명령만 같고 전제가 다르다** — revision 21이 *"같은 명령"*이라고만 적어 둘을 섞이게 했다.
⚠️ **(i)과 (ii)는 배타적이지 않다** — (ii)로 창을 밀어 두고 (i)을 계속 진행하는 것이 가장 안전하다.
**(iii)만 남기는 선택은 하지 않는다** — 그것이 네 번의 장애가 말하는 전부다.

→ **결정 기한까지 결정하고 이 표에 기록한다.** 결정하지 않으면 자동으로 (iii)이 되고,
그것이 회고 A-11이 규정한 *"조건부로 조용히 미뤄지는"* 형태다.

*(ii) 2026-09-07 수행 기록 — f-4의 첫 실행*

| | 값 |
| --- | --- |
| 회전 | **2026-09-07 10:06:07** (온디맨드) |
| ⚠️ **Step 1 상태** | **미배포.** 광역·좁은 rule 모두 아직 없다(게이트 ⓑ 조회 결과 그대로). **RDS 회전 스케줄만 enabled였다** — r21 codex-ide#4로 정정. revision 21은 *"Step 1 rule은 켠 채"*라고 적었는데 **켤 rule 자체가 없었다** |
| 복구 | **[사람]이 브릿지 (b) 즉시 실행** — 자동 재배포가 아니다 |
| `canary_down` ALARM | 10:14:41 (회전 **+8분 34초**) |
| `canary_down` OK | 10:18:41 (**통제된 다운 약 12분 34초**) |
| 알림 | ✅ **Slack 카드·SMS 모두 ALARM·OK 양방향 도착** |
| 결과 | 다음 창 **09-13 09:00 → 09-14 09:00** |

⛔ **두 가지를 정정한다 — 이 표를 처음 쓸 때 내가 틀린 값을 적었다:**
1. **"창이 +7일 밀린다"는 틀렸다.** 스케줄은 **"마지막 회전 + 7일"**이라, 직전 회전(4차, 09-06 19:09)이
   **바로 전날**이면 **+1일**만 번다. → **창 이동으로 버는 시간은 직전 회전으로부터 지난 기간에 달렸다.**
   장애 직후에 쓰면 효과가 가장 작고, 창 직전에 쓰면 가장 크다.
   → **다음 결정(09-11 기한)에 이렇게 쓴다**(r21 claude-ide 개선): **09-14 창에 (ii)를 다시 쓰려면
   창 직전(09-13)에 걸어야 +7일에 가깝다.** 09-11에 걸면 +4일밖에 못 번다.
2. **"통제된 다운 약 6분"은 틀렸다 — 실측 약 12분 34초.** 6분은 *"재배포 → 알람 OK"*만 센 값이고,
   **회전 명령 → 회전 실제 완료(`SecretStatus=active`)까지 기다리는 시간**이 빠져 있었다.
   ✅ **그래도 4차의 13시간 10분과 비교하면 63분의 1이다** — 판단 자체는 유지된다.

✅ **부수 수확: 알림 경로 전체가 실증됐다.** ALARM·OK 양방향으로 **Slack 푸시와 SMS가 둘 다 폰에 도착**했다.
→ **4차 회고 B-4의 답이 나왔다**: 4차의 13시간은 *"알림을 못 봤다"*가 아니라 **"알림은 왔는데 그때
대응하지 않았다"**이다. **알림 쪽으로 더 할 것이 없다는 뜻이고, 남은 것은 Step 1뿐이다.**

### Step 2 — 앱이 회전을 견딘다 (착수 기한 = **1-6 드릴 완료 후 7일 이내**)

⚠️ **1-7(다음 자동 창 사후 검증)을 기다리지 않는다**(r4 codex-ide#3). 1-6 드릴이 Step 1의 실질 완료
조건이고, 드릴이 다음 자동 창을 +7일 미므로 1-7을 게이트로 두면 **근본 대응이 1~2주 밀린다** —
회고 A-11이 지적한 "조건부 게이트로 미루기"를 그대로 재현하는 것이다.
1-7은 Step 2와 **병행되는 운영 추적 항목**이다.

**2-0. 인증 실패 관찰 위치와 "회전 중 창"을 확정한다 — Step 2의 근간**

*(가) 훅으로는 구현 불가* — pgx v5.10.0 `stdlib/sql.go:266-273`을 직접 확인했다:
```go
if err = c.BeforeConnect(ctx, &connConfig); err != nil { return nil, err }
if conn, err = pgx.ConnectConfig(ctx, &connConfig); err != nil { return nil, err }
```
훅은 연결 **전에** `ConnConfig` 사본을 고칠 뿐이고 인증 실패는 다음 줄이 곧장 반환한다.
**`stdlib.GetConnector`로 얻은 connector를 `driver.Connector`로 감싸** `Connect` 반환 오류를 관찰한다.
- `errors.As`로 `*pgconn.PgError`를 꺼내 **`Code == "28P01"`일 때만** 무효화한다. 네트워크·타임아웃은 제외.
- **세대(generation)를 호출별 로컬 상태로 묶는다**(r2 codex-cli 개선). 각 `Connect` 시도가 사용한
  `(password, generation)`을 그 호출의 지역 변수로 들고 있어야 한다. 공유 콜백의 가변 필드에 두면
  25개 동시 연결이 서로 덮어쓴다.
- **⚠️ 새 비밀번호가 실제로 dial에 들어가는 경로를 명시한다**(r12 codex-ide 개선). revision 12까지는
  *"오류를 바깥 wrapper에서 관찰한다"*는 계약만 강했고, **갱신된 값이 `stdlib.GetConnector`의 고정
  `ConnConfig`를 어떻게 대체하는지가 구현자 추론에 남아 있었다.**
  → **계약**: `Connect`는 매 시도마다 provider에서 현재 `(password, generation)`을 읽어
  **`pgx.ConnConfig`의 사본을 만들고 그 사본의 `Password`에 주입한 뒤** 그 사본으로 connector를 부른다.
  **원본 config는 절대 변경하지 않는다**(동시 연결이 서로 덮어쓴다). 검증 ⑯이 이것을 고정한다.
- **singleflight**로 동시 실패 다수의 재조회를 1회로 합친다. **`Do`가 아니라 `DoChan`(또는 동등한
  비동기 결과 채널)을 쓴다**(r10 codex-cli#2) — 동기 `Do`로 기다리면 **호출자가 refresh가 끝날 때까지
  묶여** *"호출자는 즉시 반환하지만 작업은 계속됨"*이 성립하지 않는다. 호출자는
  `select { case r := <-ch: … ; case <-ctx.Done(): return err }`로 둘 중 먼저 오는 것을 취한다.
- **⚠️ context 소유권을 하나의 계약으로 못박는다**(r10 codex-cli#2 / codex-ide#2, **2인 [높음]/[중간] 동일**).
  revision 10은 여기서 *"backoff와 SDK 호출은 전달받은 `context.Context` 취소를 따른다"*고 적고
  아래 (나)에서는 *"SDK 호출은 detached context에서 완료돼야 한다"*고 적어 **정면으로 충돌**했다.
  구현자가 앞 문장을 따르면 `/readyz`의 1초 취소가 SDK를 끊어 **revision 9의 무한 실패가 그대로 재발**한다.
  → **아래 표가 단일 계약이다. 다른 곳의 문장이 이와 다르면 이 표가 이긴다:**

  | 대상 | 어느 context | 생성원 / 취소 주체 | deadline |
  | --- | --- | --- | --- |
  | dial #1, refresh 결과 대기, dial #2 | **`ctx`(caller)** | `database/sql`이 준다 / 호출자가 취소 | `min(ctx 잔여, 각 단계 상한)` |
  | **Secrets Manager `GetSecretValue`** | **`refreshCtx`(provider 소유)** | **provider가 `context.Background()`에서 만든다** / provider만 취소(+ 앱 종료 시 `provider.Close()`) | **10초 고정**(아래 시도당 예산 표) |
  | provider backoff 게이트 | (context 없음 — 시각 비교) | provider 전역 상태 | — |

  - **`refreshCtx`는 요청 값(request-scoped values)을 담지 않는다** — `database/sql`이 요청과 무관하게
    풀 보충용 `Connect`를 부르는 경우가 있으므로(r10 codex-ide#2) 요청 스코프를 상속하면 안 된다.
  - **`Connect`는 자기 `ctx`가 죽으면 즉시 반환한다**(`driver.Connector` 취소 계약). 그때도
    `refreshCtx`의 작업은 계속되고 **결과는 waiter가 하나도 없어도 provider 상태에 반영**된다.
  - **⚠️ 종료 경계를 둔다**(r11 codex-ide 개선): provider에 `Close()`를 두고 **앱 종료 시 `sql.DB.Close`
    직후 호출**해 `refreshCtx`를 명시적으로 취소한다. 상한이 있어 누수는 아니지만, 이것이 없으면
    **종료 뒤 최대 10초 동안 detached 작업이 provider 상태를 갱신**하고 그 구간을 테스트할 방법도 없다.

*(나) 회전 중 `AWSCURRENT`가 아직 옛 값인 창* (0008 `:472` 승계, r2 claude-ide#2 — **revision 2 누락분**)
단일 사용자 회전은 `setSecret`(DB 비밀번호 실제 변경) → `testSecret` → `finishSecret`(`AWSCURRENT` 이동)
순서다. 즉 **DB는 이미 새 비밀번호인데 `GetSecretValue(AWSCURRENT)`는 아직 옛 값을 반환하는 구간**이 있다.
이때 `28P01` → 재조회 → **같은 값** → `28P01`이 반복된다. 세 규칙을 계약으로 못박는다:

- **`Connect` 한 번의 예산은 유한하다. 포기하지 않는 주체는 provider다.**
  revision 3은 *"terminal cap을 두면 영구 실패로 남으니 무한 backoff"*라고 썼는데 **전제가 틀렸다**(r3 codex-ide#2).
  `driver.Connector.Connect`가 오류를 반환해도 `sql.DB`가 영구 실패가 되는 것이 아니라 **다음 연결 요청에서
  다시 호출된다.** 반대로 `database/sql`은 풀 보충을 위해 요청과 무관하게 `Connect`를 호출하고 현재 구현은
  단일 `connectionOpener`가 이를 **직렬 호출**하므로, 그 안에서 무한 루프를 돌면 **opener 자체를 점유**해
  장기 장애 때 연결 생성 경로가 막힌다. 공식 계약도 **dial 기본 timeout을 별도로 두라**고 명시한다
  ([`driver.Connector`](https://pkg.go.dev/database/sql/driver#Connector)).
  → **호출별 SDK/dial timeout을 두고 `Connect` 1회는 유한하게 실패한다.** backoff·rate-limit 상태는
  **provider의 전역 상태**로 들고 있어, **다음 `Connect`가 복구를 이끈다.**
  **"포기 없음"은 호출 수준이 아니라 provider 수준의 계약이다.**
  - **상시 백그라운드 refresher goroutine을 두지 않는다**(r4 claude-ide#7). 근거: **canary가 `/readyz`를
    상시 프로빙하므로**(plan 0008, apply 완료) 무트래픽 상태에서도 DB 핑 경로가 계속 호출돼
    `Connect` 기회가 끊기지 않는다.
    ⚠️ **probe 간격은 "30초"가 아니라 앱이 보는 실측 기준 평균 약 2초다**(r10 claude-ide#1 —
    revision 10은 같은 문서 안에서 이 값을 두 가지로 썼고 **둘이 각각 다른 결론을 떠받치고 있었다**).
    `regions` 미지정이라 **기본 전체 리전 체커**가 각각 30초 주기로 돌고 서로 조정되지 않으므로,
    앱이 보는 것은 **분당 약 33건 = 평균 약 1.8초 간격**이다(2026-08-16 장애 증거의 canary 트래픽
    시간당 약 1,976건 ÷ 60 ≈ 32.9 — ADR 0004 실측. 이 plan의 "08-30 탐지 근거" 절이 쓰는 바로 그 수치다).
    *"다음 probe 최대 30초"*를 유지하면 그 탐지 근거가 무너지고, 33건/분을 유지하면 회복 시간이
    내려간다 — **후자가 실측이므로 후자로 통일한다.**
    ⚠️ 다만 **체커들이 조정되지 않으므로 이것은 평균이고 엄밀한 상한이 아니다**(r10 codex-cli#1 /
    codex-ide#1 / r9 codex-ide#4 — Route53 공식 문서도 여러 요청 뒤 공백이 생길 수 있다고 적는다).
    아래 회복 시간은 **평균 cadence 기준의 기대값**이지 보장이 아니다.
  - **⚠️ 그런데 그 `/readyz` 요청 context는 1초다 — refresh를 요청 context에서 분리한다**
    (r9 codex-cli#2 [높음]). `health.go:19`가 `readinessTimeout = 1 * time.Second`이고
    `:42-43`이 `context.WithTimeout(r.Context(), readinessTimeout)`로 `PingContext`에 넘긴다.
    무트래픽에서 `Connect`를 일으키는 **유일한 주체가 이 canary**인데, 계획한 예산은
    **Secrets Manager 최악 8초**(아래 시도당 예산 표) + dial 5초라 **1초 안에 끝날 수 없다.** revision 9까지의 설계는
    *"매 호출이 1초에 취소되고 30초 뒤 같은 1초 context로 다시 취소되는"* 무한 루프였다 —
    **"포기 없음"이 provider 수준에서도 성립하지 않았다.**
    `/readyz`의 1초를 늘리는 것은 선택지가 아니다: 그 값은 **Route53이 2초 안에 2xx를 요구하기 때문에
    의도적으로 1초**이고(`health.go:10-18` 주석, ADR 0004 §1-a), 0008이 방금 그 경로로 apply됐다.
    → **refresh 결과를 다음 호출로 승계하는 구조를 택한다**(codex-cli가 제시한 세 선택지 중 셋째):

    ```
    Connect(ctx)                                   ← ctx는 호출자 것 (/readyz면 1초)
      1) 현재 세대 비밀번호로 dial   timeout = min(ctx 잔여, 5초)
      2) 성공 → 반환
      3) 28P01 관찰 → refresh 요청 (단일 비행)
           refresh는 ctx와 분리된 detached context에서 돈다 (자체 상한 10초)
           ctx가 취소돼도 refresh는 완료되고 결과가 provider 전역 상태에 남는다
      4) 현재 Connect는 min(ctx 잔여, 3초)까지만 결과를 기다린다
           값이 오면 → 새 값으로 dial 재시도  timeout = min(ctx 잔여, 5초) → 반환
           안 오면   → 오류 반환 (다음 Connect가 승계한다)
    ```

    ⚠️ **`/readyz` 경로에서는 4)의 대기가 실질 0이다**(r10 claude-ide 개선). 1초 ctx에서 dial #1이
    끝나면 남는 시간이 수백 ms라 **"3초 대기"가 실행되는 일이 없다** — 사실상 항상 *"안 오면 → 오류 반환"*
    가지를 탄다. 구현자가 *"1초 ctx에서도 3초를 기다린다"*로 오해하면 `/readyz`가 Route53의 2초를 넘겨
    **`health.go`의 1초가 막으려던 상황을 그대로 만든다.**

    - **detached refresh는 상시 goroutine이 아니다** — 28P01을 볼 때만 뜨고, 단일 비행이라 동시 최대 1개,
      10초 상한이 지나면 반드시 끝난다. 2-1 ⑦(goroutine 누수 0)과 충돌하지 않는다.
    - **`driver.Connector`의 취소 계약을 지킨다** — `Connect`는 자기 `ctx`가 죽으면 즉시 반환한다.
      ⚠️ **"opener 점유가 없다"가 아니라 "무한 점유가 없다"이다**(r11 claude-ide 개선). `database/sql`이
      풀 보충으로 부르는 `Connect`는 **DB가 닫힐 때까지 사는 `db.ctx`**를 받으므로 `min(ctx 잔여, …)`가
      실질 제한이 되지 않고, 그 경로에서는 `Connect` 한 번이 **최악 13초 동안 opener를 점유**한다.
      유한하므로 설계 의도(장기 장애에도 연결 생성 경로가 막히지 않음)는 지켜지지만,
      **이 설계가 주는 보장은 "유한"까지다.**
    - **`Connect` 1회 최악(넉넉한 ctx) = dial 5 + 대기 3 + dial 5 = ≤ 13초**
      (r9 codex-cli#3 / codex-ide#4 — revision 9의 *"SDK 6 + dial 5 = 11초"*는 **28P01을 관찰한 첫 dial과
      새 값으로 재시도하는 둘째 dial 중 하나를 세지 않았고**, SDK backoff·provider backoff도 빠져 있었다).
    - **provider backoff는 `Connect` 안에서 기다리지 않는다**(r9 codex-ide#4의 질문에 대한 답).
      backoff는 *"다음 refresh를 언제 허용할지"*의 게이트다 — backoff 창 안이면 **refresh를 시작하지 않고
      즉시 오류를 반환**하고 다음 `Connect`가 다시 시도한다. 그래서 위 13초에 backoff가 더해지지 않는다.
  - **⚠️ 회복 시간은 시계를 고정한 뒤에만 의미가 있다**(r10 codex-cli#1 / codex-ide#1 → **r11 codex-cli#1 /
    codex-ide#1로 한 번 더 갈라졌다. 두 라운드 연속 [높음] 2인 동일**).
    revision 10의 *"무트래픽 최대 약 40초"*는 세 가지를 빠뜨렸고(① **회전 중 창**에서 첫 refresh가
    *같은 값*을 얻고 끝나는 경우 ② **refresh를 시작시킬** probe 한 번 ③ provider backoff 대기),
    revision 11의 **T0 삼분할은 두 쌍을 등치시켜 다시 틀렸다**:
    **`setSecret` = 첫 `28P01`**(r11 codex-cli#1)과 **`AWSCURRENT` 이동 = 새 값이 조회 가능**(r11 codex-ide#1).
    둘 다 **이 plan 자신의 다른 문장과 충돌한다** — 기존 세션은 비밀번호 변경 뒤에도 최대 5분 살아 있고
    (배경 "왜 죽는가"), Secrets Manager는 변경이 모든 endpoint에 즉시 보인다고 보장하지 않는다
    ([문제 해결](https://docs.aws.amazon.com/secretsmanager/latest/userguide/troubleshoot.html)).
    ⚠️ **revision 12의 다섯 시각도 같은 형태로 다시 틀렸다 — 3인 전원이 같은 곳을 지적했다**
    (r12 codex-cli#1 [높음] / codex-ide#3 [중간] / claude-ide#1 [높음]).
    `T_visible`을 *"provider가 처음 새 값을 **반환받은** 시각(refresh 로그)"*으로 정의해 놓고,
    바로 아래 24초 산술이 **그 시각 뒤에 backoff 게이트·probe·refresh를 다시 셌다.** 그 작업들은
    정의상 `T_visible` **앞**에 끝난다. **r11에서 "라벨 이동 vs 가시화"를 가른 것과 같은 형태가
    한 칸 뒤로 밀린 것이다 — 이번엔 "가시화(fetchable) vs 취득(fetched)"이 합쳐졌다.**
    → **시각을 여섯으로 나눈다. 아래가 이 문서의 단일 시계이고, 다른 절은 여기의 이름만 쓴다:**

    ⚠️ **아래 넷째 열은 "상한"이 아니라 "기대 예산"이다**(r13 codex-ide#4 / codex-cli 개선).
    이 문서는 `:975-984`에서 **Route53 체커가 비조정이라 probe 간격 1.8초는 평균이지 상한이 아니다**라고
    명시했는데, 그 값을 더한 17초·24초를 *"최악"*·*"상한"*으로 불렀다 — **probe 공백에 상한이 없으므로
    그 합도 상한이 아니다.** 진짜 상한은 **이 plan이 통제하는 항**(게이트 5초 · refresh 10초 · dial 5초)뿐이고,
    probe가 끼는 순간 그 구간은 기대값이 된다. → **표기를 "기대 예산"으로 통일한다.**

    | 시각 | 무엇 | 관측 수단 | 앞 구간의 예산 |
    | --- | --- | --- | --- |
    | **T_set** | `setSecret` — DB 비밀번호가 실제로 바뀐 시각 | **관측 불가**(회전 lambda 내부) | — |
    | **T_fail** | 앱이 **첫 `28P01`을 관찰**한 시각 = 신규 연결이 처음 필요해진 시각 | provider 로그 | **T_set→T_fail 상한 없음.** 기존 세션만 재사용되면 최대 커넥션 수명(5분) 뒤까지 오지 않고, **Step 1이 먼저 재배포하면 아예 관측되지 않는다** |
    | **T_label** | `finishSecret` — `AWSCURRENT` 라벨 이동 | EventBridge 이벤트 `time`(광역 관찰 rule의 로그 그룹. 1-1이 만들고 1-3에서 apply된다). **`LastRotatedDate`는 근사치**다 — 공식 계약은 *"마지막으로 회전한 시각"*이지 라벨 이동 시각이 아니다([`DescribeSecret`](https://docs.aws.amazon.com/secretsmanager/latest/apireference/API_DescribeSecret.html)) | **T_set→T_label 상한 없음**(회전 lambda 내부) |
    | **T_avail** | 새 값이 **조회 가능**해진 시각 — *"물어보면 새 값이 온다"*. **아직 아무도 안 읽었다** | ⚠️ **관측 불가**(물어보기 전에는 알 수 없고, 아는 순간이 곧 `T_visible`이다). 2-4 (b)의 fixture만 이 시각을 안다 | **T_label→T_avail 상한 없음** — 전파·캐시. Secrets Manager는 변경이 모든 endpoint에 즉시 보인다고 보장하지 않는다 |
    | **T_visible** | provider가 **처음 새 값을 받은** 시각 — *"물어봤고 새 값이 왔다"* | refresh 성공 로그(세대 N→N+1) | **T_avail→T_visible = 게이트 잔여 ≤5초 + probe ~2초(평균) + refresh ≤10초 = 기대 예산 약 17초** — ⚠️ **상한이 아니다**(probe 공백에 상한이 없다). `T_avail` 전의 refresh는 성공해도 **값이 같아 세대를 올리지 않으므로 로그도 나오지 않는다** |
    | **T_ready** | **새 세대로 dial/ping이 처음 성공**한 시각 | **`credential_recovered` 로그**(2-1에서 구현) | **T_visible→T_ready = 약 2~7초**(다음 probe + dial) |

    - **다운타임은 `T_fail → T_ready`다.** `T_fail`은 `T_label`보다 **앞일 수도 뒤일 수도 있다.**
      기존 풀만 재사용되는 동안은 `28P01`도 503도 없다(**다운타임 0**) — 그 경우 refresh 경로 자체가
      실행되지 않으며, 이는 2-6이 *"Step 2 경로가 한 번도 실행되지 않을 수 있다"*고 적은 것과 같은 사실이다.
    - **회전 내부 창 = `T_set → T_label`. 전파 = `T_label → T_avail`. 자격증명 채택 = `T_avail → T_ready`.**
      **주체가 다르므로 세 구간의 숫자를 합치거나 비교하지 않는다.**
    - **⚠️ "약 24초"는 `T_avail` 기준이고, "약 2~7초"는 `T_visible` 기준이다** — **둘은 같은 구간이 아니다**
      (r12 3인 전원). 하나는 *"새 값이 준비된 뒤 앱이 그것을 집어 쓰기까지"*(24초, **관측 불가한 시작점**),
      다른 하나는 *"앱이 새 값을 받은 뒤 연결이 살아나기까지"*(2~7초, **실측 가능**)다.

    | 기준 | 무트래픽(canary만) | 트래픽 있음 | 실측 |
    | --- | --- | --- | --- |
    | **`T_fail` → `T_ready`**(= 다운타임) | `T_avail`이 이미 지났으면 **약 24초**, 아니면 **상한 없음**(전파 대기) | `T_avail` 이후 **수 초** | `T_fail`·`T_ready` 둘 다 로그라 **실측 가능** |
    | **`T_avail` → `T_ready`**(관측 목표) | **약 24초** | 수 초 | ⚠️ **프로덕션 실측 불가**(`T_avail`이 관측 불가). **2-4 (b)에서만 잰다** |
    | **`T_visible` → `T_ready`** | **약 2~7초**(다음 probe + dial) | 수 초 | 두 로그의 차라 **실측 가능. 2-6이 재는 값** |

    - **`T_avail` 이후 약 24초의 내역**(r11 claude-ide#1로 다이어그램이 되고, **r12에서 시작점 이름이
      정정됐다**):

      ```
      T_avail ─ backoff 게이트 잔여 ≤5초 ─ [probe #1 ~2초] ─ refresh ≤10초 ─┬─ T_visible ─ [probe #2 ~2초] ─ dial ≤5초 ─ T_ready
                                          └ 이 probe가 refresh를 시작시킨다  │              └ 이 probe가 새 값을 소비한다
                                                                            └ refresh 로그가 여기서 난다 (약 17초)
      ```
      게이트가 닫혀 있는 동안 도착한 `Connect`는 아래 계약대로 **refresh를 시작하지 않고 즉시 반환**하므로,
      게이트가 열린 뒤 **새로 도착한 probe 한 번**이 있어야 refresh가 시작된다.
      합 = 5+2+10 (= `T_avail`→`T_visible`, 약 17초) + 2+5 (= `T_visible`→`T_ready`, 약 2~7초)
      = **약 24초**(refresh 상한 10초는 r11 claude-ide#2로 재산정한 값이다).
      ⚠️ **두 `probe` 항이 평균이므로 17초도 24초도 상한이 아니다**(r13 codex-ide#4). 상한이 있는 항만
      더하면 5+10+5 = **20초 + probe 2회**이고, **probe 2회에는 상한이 없다.**
    - **⚠️ 24초는 관측 목표이지 실측값이 될 수 없다**(r12 claude-ide#1) — 시작점 `T_avail`을 프로덕션에서
      알 방법이 없기 때문이다. **실측하는 자리는 2-4 (b) 하나뿐**이고(fixture가 전환 시각을 안다),
      **2-6이 재는 것은 `T_visible`→`T_ready`(2~7초)와 `T_fail`→`T_ready`(다운타임)다.**
    - **T_set→T_label 구간에 상한이 없는 이유**: `setSecret` 뒤 `finishSecret`까지는 **Secrets Manager 회전
      lambda 내부**라 이 plan이 제어하지도 관측하지도 못한다. 그동안 DB는 새 비밀번호인데
      `GetSecretValue(AWSCURRENT)`는 옛 값을 준다 — provider는 backoff(≤5초)로 **계속 재시도**하지만
      새 값이 없으므로 회복할 수 없다. **Step 1도 이 구간을 못 받는다**: Step 1의 라벨 확인이
      `UpdateService`의 선행 조건이라 **똑같이 `AWSCURRENT` 이동을 기다린다.**
      **`T_label`→`T_avail`(전파) 구간도 같다** — 라벨은 옮겨졌지만 provider가 받는 값이 아직 옛 값이면
      provider 입장에서는 회전 중 창과 구분되지 않는다.
      → **두 구간 모두 "회전 중 창"의 대가로 감수한다**(0008 `:472` 승계 항목이 이미 *"무중단 보장 안 됨"*이라
      적었다). 리스크표에 잔여 위험으로 명시한다.
    - **⚠️ 이 값들은 평균 cadence 기준의 기대값이지 보장이 아니다** — Route53 체커가 서로 조정되지 않는다.
      **"관측 목표"로 쓰고 SLA로 쓰지 않는다.**

    어느 쪽이든 **`T_avail` 기준으로 Step 1의 "수 분"보다 한 자릿수 짧다** — 다만 2-6의 판별은
    **refresh 로그가 아니라 `credential_recovered` 로그**로 하고, ⚠️ **시각 경합으로 판정하지 않는다**
    (r12 codex-ide#2 / claude-ide#2, 아래 2-6). **2-4는 시나리오별 시계**(그중 `T_set` 기준은 (a)뿐)**를,
    2-6은 `T_visible`→`T_ready`와 `T_fail`→`T_ready`를 잰다** — 같은 구간이 아니라는 것을
    각 단계에 적어 둔다(r10 codex-cli#1).
- **값이 바뀌지 않은 refresh는 세대를 올리지 않는다.** 세대만 올리면 이어지는 `28P01`이 전부
  "새 세대의 실패"로 집계돼 재조회가 급증한다. 값 동일이면 backoff 간격만 늘린다.
- **⚠️ backoff와 호출별 제한을 수치로 확정한다**(r8 codex-cli#3 — revision 8은 *"backoff를 늘린다"*로만
  적어 **재조회 폭주를 막는 구현도, "수 초 내 회복"하는 구현도 둘 다 plan을 만족한다**고 읽혔다.
  목표 복구 시간이 "수 초"인데 상한이 없으면 검증 기준이 성립하지 않는다):

  | 파라미터 | 값 | 근거 |
  | --- | --- | --- |
  | 초기 간격 | **200ms** | 전파 지연 직후의 짧은 창을 놓치지 않는다 |
  | 배수 / jitter | **2배 / full jitter** | 동시 다수 실패가 같은 시각에 몰리지 않게 한다 |
  | **최대 간격** | **5초** | **목표가 "수 초 회복"이다.** 새 `AWSCURRENT`가 보인 뒤 재시도까지 최대 5초 |
  | reset 조건 | **값이 바뀐 refresh 성공 시 즉시 초기값으로** | 값 동일 성공은 reset하지 않는다(회전 중 창) |
  | Secrets Manager **operation 전체** | **`refreshCtx = context.WithTimeout(context.Background(), 10초)`** + **시도당 상한 3초** + retryer는 **`retry.AddWithMaxBackoffDelay(retry.NewStandard(func(o *retry.StandardOptions) { o.MaxAttempts = 2 }), 2*time.Second)`** | ⚠️ r9 codex-ide#4 — 시도당 timeout만으로는 부족하다. **standard retryer의 기본 최대 backoff는 20초**라 시도 수만 2로 고정해도 호출 전체의 상한이 서지 않는다. **operation context가 최종 강제 장치다.** ⚠️ **API 이름 정정**(r10 codex-ide#4): `retry.StandardOptions`의 필드는 `MaxAttempts`·**`MaxBackoff`**이고 **`MaxBackoffDelay`라는 필드는 없다** — 그 이름은 `retry.AddWithMaxBackoffDelay` **래퍼 함수**다. revision 10 문구를 그대로 옮기면 **컴파일이 깨진다.** `config.WithRetryer`에 넘기는 팩토리는 **호출마다 새 retryer를 반환**해야 한다. ⚠️ **시도당 상한은 r11 claude-ide#2로 되살렸다**(아래 예산 표) |
  | pgx dial timeout | **5초** (`min(ctx 잔여, 5초)`) | `Connect` 1회 예산 = dial 5 + refresh 대기 3 + dial 5 = **≤ 13초** |
  | canary probe 간격(앱이 보는 값) | **평균 약 1.8초**(분당 약 33건, ADR 0004 실측) | 전체 리전 체커가 각 30초 주기로 **비조정** 프로빙한 결과. **평균이지 상한이 아니다** |
  | detached refresh 상한 | **10초** | SDK operation timeout과 같다. 이 시간 뒤 goroutine은 반드시 종료된다 |
  | `Connect`의 refresh 대기 | **min(ctx 잔여, 3초)** | 초과해도 refresh는 detached로 계속되고 **다음 `Connect`가 승계**한다. **`/readyz` 경로에서는 실질 0에 가깝다**(위) |

  ⚠️ **시도당 상한이 없으면 `MaxAttempts = 2`가 아무 의미도 갖지 않는다**(r11 claude-ide#2).
  revision 10의 표에는 *"connect 1초 / read 2초 / 시도 2회 → 최악 ≤ 6초"*가 있었는데 revision 11이
  **operation context 하나로 교체하면서 시도당 값을 지웠다.** 그러면 **시도 1이 operation 상한을 다 쓰고
  재시도가 한 번도 일어나지 않는 것**도 계약을 만족하고, 위 (나)의 *"Secrets Manager 최악 6초"*는
  근거를 잃는다. **Step 1이 이미 쓰는 예산 표 형식**(`시도 수 × 시도당 상한 + backoff ≤ 구간 상한`)을
  Step 2에도 그대로 적용한다:

  | # | 항 | 값 | 최악 |
  | --- | --- | --- | --- |
  | 1 | **시도당 상한**(HTTP client `Timeout` — dial+TLS+응답. 시도마다 새 요청이라 시도당으로 적용된다) | **3초** | 3 |
  | 2 | 재시도 backoff (`retry.AddWithMaxBackoffDelay`, 재시도 1회) | **≤ 2초** | 2 |
  | 3 | 재시도 1회 | 3초 | 3 |
  | | **호출 전체 최악** | | **8초** |
  | | **`refreshCtx` operation 상한** | | **10초**(마진 2초) |

  ⚠️ **마진 0을 두지 않는다** — 8초 ctx에 최악 8초를 넣으면 정상적인 스로틀 재시도 한 번이 잘려
  refresh가 실패한다. 이 plan이 r6·r9에서 같은 형태를 세 번 고쳤고, Step 1도 *"합계 105초 = 내부 deadline,
  함수 `timeout` 120초"*로 15초를 벌려 뒀다. **그래서 operation 상한을 8초 → 10초로 올린다**
  (회복 산술의 *"refresh ≤10초"*가 이 값이다).

  → 2-1 검증에 **fake clock**으로 *옛 값 반복 → 최대 간격 5초 도달 → 새 값 노출 → 회복*,
  SDK operation timeout, context 취소, **성공 후 backoff reset**,
  그리고 **"호출자 ctx가 1초에 취소돼도 detached refresh가 완료돼 다음 `Connect`가 성공한다"**를 고정한다.
  ⚠️ **취소 시점을 테스트에 명시한다**(r10 codex-cli#2): caller `ctx`를 **첫 dial이 `28P01`을 반환한 뒤**
  취소해야 이 경로가 검증된다. 그 전에 취소하면 refresh가 시작되지도 않아 **다른 경로를 우연히 통과**한다.
- **goroutine 누수·재조회 폭주가 없어야 한다** — 장시간 동일 값·SDK 장애 상황에서 테스트로 고정한다.

→ 검증: **코드 작성 전에 이 설계를 문서로 리뷰받는다.** DB 연결 경로라 blast radius가 크다.

**2-1. 비밀번호 provider 구현**
- `GetSecretValue`는 **`AWSCURRENT`만** 사용한다. `AWSPENDING`은 연결에 쓰지 않는다.
- `SecretString` JSON에서 `password` 필드를 파싱한다(0008 `:473` 승계).
- **로그에 SecretString·비밀번호·DSN이 절대 포함되지 않는다**(테스트로 고정).
- **⚠️ 2-6이 판정 근거로 쓰는 로그 필드를 여기서 구현한다**(r8 claude-ide#1 — revision 8은 이 둘을
  **2-6에서 처음 요구**했는데 2-1·2-2 어느 구현 항목에도 없었다. 2-5가 **순서를 강제하는 게이트 배포**라,
  2-6에서 부재를 발견하면 코드 수정 → CI 배포를 처음부터 다시 돌아야 하고, 그동안 **회전 창 하나를
  통째로 버린다**(회전은 7일 주기다)):
  - **⚠️ `auth_failed_observed` 로그**(= 시계의 `T_fail`) — **r12 codex-cli#4 / claude-ide#3 [중간 2인]**.
    시계 표가 `T_fail`의 관측 수단을 *"provider 로그"*라 적고 **다운타임을 `T_fail`→`T_ready`로 정의**하는데,
    revision 12의 2-1에는 그 로그를 남기라는 요구가 **없었다** — 즉 **plan이 정의한 다운타임을 계산할 수 없었다.**
    r8 claude-ide#1(*"2-6이 쓰는 로그 필드를 2-1로 올려라"*)·r11 codex-ide#2와 **같은 형태의 재발**이다.
    → 첫 `28P01` 관찰에서 **세대당 1회** *"auth_failed_observed, 세대 N"*을 남긴다(`task_id` 포함).
    발행 규칙을 세대당 1회로 두는 이유는 **폭주 방지**다 — 회전 중 창에서는 실패가 계속 반복된다.
    ⚠️ **2-6이 두 실패 원인을 가르는 데 이 로그가 필요하다**: *"기존 세션만 재사용돼 `28P01`이 오지 않음"*과
    *"Step 2 코드가 깨져서 안 남"*은 **이 로그 없이는 완전히 동일하게 보인다**(claude-ide#3).
  - **refresh 성공 로그**(= 시계의 `T_visible`): *"비밀번호 refresh 성공, 세대 N→N+1"* — 세대 전이를
    필드로 남긴다. 값 동일 refresh는 세대를 올리지 않으므로 이 로그도 나오지 않는다(위 계약과 일치).
  - **⚠️ `credential_recovered` 로그**(= 시계의 `T_ready`) — **r11 codex-ide#2 [높음]**.
    refresh 성공 로그는 *"`GetSecretValue`가 다른 값을 반환했고 provider 상태가 바뀌었다"*는 증거일 뿐,
    **그 세대로 pgx dial/인증이 성공했다는 증거가 아니다.** 시계 표도 `T_visible` 뒤에 *"다음 probe + dial"*이
    더 있어야 회복이라고 적는다. 그런데도 2-6이 refresh 로그를 복구 증거로 쓰면,
    **wrapper의 비밀번호 주입이나 둘째 dial이 깨져도** old task가 refresh 로그를 먼저 남기고
    잠시 뒤 **Step 1의 재배포가 `/readyz`를 200으로 되돌려** 판정이 통과한다 — 실제 복구 주체는 Step 1인데
    Step 2가 복구했다고 기록되는 **false positive**다.
    → **새 세대를 사용한 첫 dial/ping 성공에서 딱 한 번** *"credential_recovered, 세대 N+1"*를 남긴다
    (세대당 1회. `sync.Once`류가 아니라 **세대 전이에 묶인 1회**여야 다음 회전에서도 다시 난다).
    **2-6의 성공 조건과 두 `task_id` 대조는 이 로그로 한다.**
  - **⚠️ 기동 시 provider 모드 로그** — **r11 claude-ide#4**. 기동 직후 구조화 로그 한 줄로
    `secret_provider_mode=secret|static`(+ `secret` 모드면 ARN의 시크릿 이름)을 남긴다.
    2-5 smoke가 이 줄로 **"Step 2 코드가 실제로 도는 이미지인지"**를 확인한다(아래 2-5 참조).
  - **`task_id` 필드**: 기동 시 **`${ECS_CONTAINER_METADATA_URI_V4}/task`**를 조회해
    (r8 codex-cli 개선 — `TaskARN`은 컨테이너 기본 응답이 아니라 **`/task` 응답의 계약**이다)
    `TaskARN`의 마지막 세그먼트를 **모든 구조화 로그에 고정 첨부**한다.
    **짧은 HTTP timeout(2초)·응답 크기 제한·JSON 오류 처리**를 두고, 실패하면 `task_id=unknown`으로 남긴다.
    ⚠️ `unknown`이 상시 발생하면 2-6이 판정 불가가 되므로 **정상 경로를 테스트로 고정한다.**
→ 검증: 단위 테스트 — ①정상 시 캐시 히트로 SDK 호출 0회 ②`28P01` 후 1회만 재조회
   ③동시 실패 다수에도 재조회 1회 ④네트워크 오류에는 재조회 0회 ⑤늦은 옛 `28P01`이 새 세대를 지우지 않음
   ⑥**재조회 결과가 옛 값과 동일한 경우**(회전 중 창) — 세대 불변 + backoff 증가 + **provider 수준 포기 없음**
   ⑦장시간 동일 값·SDK 장애에서 **goroutine 누수 0, 재조회 폭주 없음**
   ⑧`Connect` 1회가 **유한 시간 안에**(≤ 13초, 위 표) 오류를 반환하고 **호출자 ctx가 죽으면 즉시 반환**한다
   (opener 점유 금지)
   ⑨**backoff 계약**(fake clock): 옛 값 반복 → 최대 간격 5초 도달 → 새 값 노출 → 회복, 성공 후 reset
   ⑩**로그 필드**: refresh 성공 로그에 세대 전이가 있고, 모든 로그에 `task_id`가 붙는다
   (metadata endpoint fake 정상/실패 두 경우 — 실패 시 `unknown`)
   ⑪**detached refresh 승계**(r9 codex-cli#2): 1초 context로 `Connect`를 부르면 그 호출은 취소되지만
   **refresh는 완료되고**, 이어지는 `Connect`가 새 값으로 성공한다. detached goroutine이 10초 안에
   반드시 종료되는 것도 함께 본다. **caller 취소는 첫 dial이 `28P01`을 반환한 뒤에 한다**
   (r10 codex-cli#2 — 그 전에 취소하면 refresh가 시작되지도 않아 다른 경로를 우연히 통과한다).
   ⑫**`credential_recovered`가 refresh 성공과 분리된다**(r11 codex-ide#2): **refresh는 성공했는데
   새 세대 dial이 실패**하는 fake를 넣어 *"refresh 성공 로그는 나오고 `credential_recovered`는 안 나온다"*를
   고정한다. 이 테스트가 없으면 2-6의 false positive 경로가 그대로 남는다. 세대당 1회만 나오는 것,
   다음 세대 전이에서 다시 나오는 것도 함께 본다.
   ⑬**시도당 상한**(r11 claude-ide#2): SDK 시도 하나가 3초를 넘기면 그 시도가 잘리고 **재시도가 실제로
   일어나는지**, 그리고 호출 전체가 `refreshCtx` 10초 안에 끝나는지를 fake transport로 고정한다.
   ⑭**`Close()` 계약**(r11 codex-ide 개선): `provider.Close()` 뒤에는 진행 중 refresh가 취소되고
   새 refresh가 시작되지 않는다.
   ⑮**`auth_failed_observed`**(r12 codex-cli#4 / claude-ide#3): 첫 `28P01`에서 한 번 나고, 같은 세대의
   반복 실패에서는 **다시 나지 않으며**(폭주 방지), 세대가 바뀌면 다시 난다. `task_id`가 붙는다.
   ⑯**두 dial이 서로 다른 비밀번호를 받는다**(r12 codex-ide 개선): refresh 전후의 `Connect`가 실제로
   **다른 `Password` 값**으로 dial하는지 fake connector로 고정한다 — 아래 데이터 흐름 계약의 회귀 방어다.
→ ⚠️ **이 패키지 테스트는 `go test -race`로도 돌린다**(r10 codex-ide 개선). password·generation·backoff
   상태와 detached goroutine을 여러 `Connect`가 동시에 읽고 쓰므로, mutex·singleflight 경계의 회귀는
   레이스 검출기가 가장 잘 잡는다. CI 명령에 명시한다.

**2-2. 시크릿 식별자 전달 경로를 만든다 — 코드·HCL 작성과 `terraform plan`까지만**
⚠️ **HCL 작성 자체를 두 단계로 나눈다**(r8 codex-cli#4). revision 8은 2-2에서 `skip_destroy`·task role
정책·`DB_SECRET_ARN`을 **한꺼번에** 작성한 뒤 2-5의 apply ①이 *"`skip_destroy`만, replacement 없음"*을
요구했는데, **모든 HCL 변경이 이미 작업 트리에 있으면 일반 `terraform apply`가 env replacement와 정책까지
함께 적용**하므로 그 게이트를 만족할 수 없다(임시 revert나 `-target`에 암묵적으로 의존하게 된다).
- **2-2a**: `aws_ecs_task_definition.app`에 **`skip_destroy = true`만** 추가한다.
  ⚠️ **"Terraform/HCL diff에 `skip_destroy`만"이라는 뜻이다**(r9 codex-ide 개선) — 2-1에서 이미 쓴
  **앱 코드는 그대로 둔다.** 앱 변경을 임시 revert하라는 뜻이 아니다(앱은 CI 배포 전까지 라이브에 없다).
  → `terraform plan` 기대값: **task definition in-place(`false/null → true`), replacement(`-/+`) 없음, `0 destroy`.**
  → 여기서 멈추고 **2-5의 apply ①**(사람)로 간다.
- **2-2b**: apply ① 이후에 **task role 정책 + `DB_SECRET_ARN` env**를 작성한다.
  → `terraform plan` 기대값: task role 정책 **신설** + task definition **`-/+`**(새 revision 등록).
  → **여기서 apply하지 않는다** — 2-3·2-4 통과가 배포 게이트다(r2 codex-cli#4).
현재 컨테이너에는 `DB_PASSWORD` **값만** 들어가고 ARN이 없다(`ecs.tf:52-57`). 앱 설정도 비밀번호가
포함된 `DatabaseURL` 문자열만 만든다(`config.go:33-34`). task role 권한만으로는 무엇을 읽을지 모른다.
- `DB_SECRET_ARN`(비밀 아님)을 task definition **환경변수**로 추가한다.
- **⛔ SDK 리전의 source of truth는 `DB_SECRET_ARN` 파싱이다. `AWS_REGION`은 배포 시점 교차검사다**
  (**r14 codex-cli#4로 계약을 하나로 정리했다**). revision 12~14는 *"env가 없으면 SDK가 리전을 정하지 못해
  전량 실패"*라고 설명해 놓고 **같은 절에서 ARN 파싱으로 `WithRegion`을 쓴다**고 적어 **서로 모순**이었다 —
  ARN이 리전을 주므로 env가 없어도 **기능 자체는 멀쩡하다.** 아래가 확정 계약이다:
  **문제의 출발점은 실측이었다**(r12 codex-ide#1): `infra/prod/ecs.tf`의 `environment`에
  `AWS_REGION`·`AWS_DEFAULT_REGION`이 **없고**, 저장소 전체에 `WithRegion` 호출도 **하나도 없다.**
  **Lambda는 런타임이 자동 주입하지만 ECS Fargate는 하지 않고**, AWS SDK for Go v2에는 **기본 리전이 없다**
  ([리전 지정](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html#specifying-the-aws-region)).
  **둘 중 하나는 반드시 있어야 하고, ARN 파싱을 택한다** — ARN은 리전을 항상 포함하고
  **어차피 시크릿을 지목하는 값이라 리전이 어긋날 수 없다.**
  - **`{ name = "AWS_REGION", value = var.region }`을 2-2b의 env에 추가한다.**
  - **SDK 설정**: `DB_SECRET_ARN`을 파싱해 얻은 리전을 **`config.WithRegion`에 명시**한다.
    **이것이 SDK를 동작시키는 값이다.** ARN 형식이 아니거나 리전 필드가 비면 **기동 오류.**
  - **`AWS_REGION` env의 목적은 다르다** — *"task definition이 의도한 리전으로 만들어졌는지"*를 보는
    **배포 시점 교차검사**다. 잘못된 리전의 시크릿 ARN이 들어가면 IAM(시크릿 ARN 한정 정책)이 먼저
    막지만, **배포 전에 걸러 내는 편이 싸다**(2-5 게이트가 그 자리다).
    ARN의 리전과 `AWS_REGION`이 **다르면 기동 오류**로 처리한다.
  - ⚠️ **빈 값의 계약을 명시한다 — "다름"인가 "검사 생략"인가**(r13 claude-ide#4 [낮음]).
    ⚠️ **결과는 "SDK 실패"가 아니라 의도적인 설정 fail-fast다**(r14 codex-cli#4) — SDK는 ARN으로 돌아간다.
    revision 13은 *"다르면 기동 오류"*만 적어 **두 읽기가 다 가능했고 결과가 정반대였다**:
    전자면 env 누락으로 production이 기동 실패하고(그런데 `config.WithRegion`은 ARN에서 얻으므로
    **기능 자체는 멀쩡하다**), 후자면 이 env는 **실질적으로 아무 일도 하지 않는다**(ARN 파싱이 이미 준다).
    *"이중 방어"*라는 목적을 살리려면 전자여야 하므로 **그렇게 적는다** — 바로 아래 `DB_SECRET_ARN`이
    같은 문제를 이미 명시적으로 처리한 형태를 그대로 쓴다:
    - **`APP_ENV == production` 이고 `AWS_REGION`이 비면 → `Config` 로드 실패**(기동 오류).
    - `APP_ENV`가 development/빈 값이면 → 검사 생략(로컬은 정적 모드라 SDK를 쓰지 않는다).
  - **production 설정 단위 테스트로 두 분기를 고정한다** — 미설정 / ARN 리전 불일치.
- `Config`에 명시 필드를 추가하고 `db.Open`이 구조화된 연결 설정을 받도록 경계를 바꾼다(0008 `:477` 승계).
- task role에 해당 시크릿 ARN 한정 `secretsmanager:GetSecretValue`를 부여한다(현재 빈 역할, `iam.tf:40-45`).
  **execution role의 기존 권한은 그대로 둔다** — 기동 시 주입은 2-3(a)의 seed로 계속 쓴다.
- **⚠️ production에서는 `DB_SECRET_ARN` 누락을 기동 오류로 처리한다**(r10 codex-ide#3).
  아래 로컬 계약만 두면 **apply 순서 실수·잘못된 task definition base·env 유실**이 있어도 새 앱이
  **구 비밀번호로 정상 기동**하고 `/readyz` 200·왕복 302 smoke까지 통과한다 — 즉 **Step 2가 배포되지
  않았는데 배포된 것처럼 보이고, 다음 회전 때까지 드러나지 않는다.**
  저장소에는 이미 같은 형태의 경계가 있다: `APP_ENV=production`이 task definition에 주입되고
  (`ecs.tf:40`), `config.go`가 *"운영 모드에서 DB가 없으면 조용히 인메모리로 떠 데이터가 증발하는 사고를
  막는다(fail-fast)"*로 `APP_ENV=production && dsn == ""`를 거부한다. **같은 자리에 같은 형태로 추가한다.**
  - `APP_ENV == production` 이고 `DB_SECRET_ARN`이 비면 → **`Config` 로드 실패**(기동 오류).
  - `APP_ENV`가 development/빈 값이면 → 아래 정적 모드 허용.
  - **설정 단위 테스트로 두 분기를 고정한다.**
- **⚠️ `DB_CONN_MAX_LIFETIME`(검증용 손잡이)을 함께 추가한다**(r15 claude-ide#1 (가)).
  `db.go:29`가 `pool.SetConnMaxLifetime(5 * time.Minute)`로 **상수를 박고 있어**, 2-6이 `T_fail`을
  설계로 만들 수단이 없다(그 결과가 r15에서 3인 전원 [높음]으로 지적된 *"관측 창 < 필요 시간"*이다).
  - **환경변수로 읽게 바꾸고 기본값은 현행 `5m` 그대로** 둔다 — **미설정 시 동작은 지금과 동일**하다.
  - `time.ParseDuration`으로 파싱하고, 파싱 실패나 0 이하면 **기본값으로 떨어뜨리고 경고 로그**를 남긴다
    (기동을 막지 않는다 — 이 값은 안전 관련이 아니라 검증 편의다).
  - **task definition env로는 2-6 검증 기간에만 `30s`를 넣는다.** 평시에는 넣지 않는다.
  - **설정 단위 테스트**: 미설정 → `5m`, `30s` → `30s`, 잘못된 값 → `5m` + 경고.
  - ⚠️ **실효값을 기동 구조화 로그에 남긴다**(r19 codex-ide 개선) — 잘못된 값이 **기동 실패가 아니라
    폴백 + 경고**라 **task definition 문자열 확인만으로는 실효값을 증명하지 못한다.**
    `secret_provider_mode`와 같은 줄에 `conn_max_lifetime`을 넣고, 2-5의 6·7번이 그것으로 확인한다.
  ℹ️ `SetConnMaxIdleTime`은 그대로 둔다 — 신규 연결을 유도하는 것은 **lifetime** 쪽이다.
- **로컬/개발 계약**(r2 claude-ide 개선): `DB_SECRET_ARN`이 비어 있으면(docker-compose의 `DATABASE_URL`
  경로, `config.go:126-128`) provider는 **정적 비밀번호로 동작**한다. P0의 로컬 완결성을 깨지 않는다.
→ 검증: 위 2-2a·2-2b 각각의 `terraform plan` 기대값이 **실제 resource action 기준**으로 맞는가.
   revision 2의 "정책 1건만, 다른 변경 0"은 **틀렸다** — 2-2b는 task definition `-/+`를 포함한다.

**2-3. 폴백의 계약을 두 경우로 나눈다** (0008 `:474` 승계)
- **(a) 기동 시 SDK 장애**: ECS가 주입한 값이 그 시점의 `AWSCURRENT`라 **유효하다.** seed로 쓰고 정상 기동.
- **(b) 회전 후 refresh 장애**: 실행 중 태스크의 env는 이미 **옛 비밀번호**다. **폴백으로 복구되지 않는다.**
  옛 값을 영구 캐시하지 않고 backoff로 `AWSCURRENT` 조회를 계속하며, 그동안 **장애가 지속된다.**
  이 구간의 복구는 **Step 1의 자동 재배포**가 맡는다.
→ 검증: (a) SDK 강제 실패 테스트에서 정상 기동 + `/readyz` 200.
   (b) 회전 후 SDK 실패 시뮬레이션에서 `/readyz` 503 유지 + **그 사실이 구조화 로그로 드러난다**
   (r8 codex-ide#4 / claude-ide#4 / codex-cli 개선 — Step 1에서 지운 *"지표"* 약속이 여기 남아 있었다.
   2-2가 task role에 주는 권한은 `secretsmanager:GetSecretValue` 하나뿐이라 `cloudwatch:PutMetricData`가
   없고, 발행 방식·namespace·비용도 없다. **Step 2도 구조화 로그까지로 통일한다**).

**2-4. [사람] 통합 테스트 — 배포 게이트**
단위 fake로는 `database/sql`의 오류 전달·재시도, pgx 오류 래핑 판별, 기존 풀과 신규 연결이 섞일 때의
회복을 입증하지 못한다. 로컬 disposable Postgres(docker-compose — **실행은 사용자**)로:
- 풀 예열 → **비밀번호 실제 변경** → 신규 연결 강제 → 첫 `28P01` → 단일 재조회 → 쿼리 성공까지,
  **프로세스 재시작 없이**.
- **⚠️ 시나리오마다 시계와 합격값을 따로 고정한다**(r11 codex-ide#3). revision 11은 *"T0a→쿼리 성공
  15초"* 하나만 두고 같은 단계에서 **회전 중 창**과 **SDK 실패**도 통과하라고 했는데, 이 둘의 길이는
  **fixture가 주입하는 값**이다. 구값 창을 3초만 둬도 `Connect` 최악 13초와 합쳐 15초를 넘고,
  반대로 아주 짧게 두면 *"포기 없음"*을 시험하지 않고도 통과한다 — **합격 여부가 fixture 값에 따라
  임의로 달라져 배포 게이트가 성립하지 않는다.**

  | # | 시나리오 | fixture | 시계 시작 | 합격 |
  | --- | --- | --- | --- | --- |
  | (a) | **새 값 즉시 가시화** | provider가 곧바로 새 값을 반환 | `T_set`(비밀번호 변경) | **≤ 15초** (`Connect` 최악 13초 + 여유. 로컬 Postgres라 전파 지연 없음) |
  | (b) | **구값 창 D초**(회전 중 창 재현) | provider가 **D = 20초** 동안 옛 값을 반환한 뒤 새 값 | **`T_avail`**(= fixture가 새 값으로 전환한 시각) — ⚠️ **이 문서에서 `T_avail`을 실제로 잴 수 있는 유일한 자리**다(r12 3인 전원) | **≤ 30초**(= `T_avail`→`T_ready` 관측 목표 24초 + 여유). ⚠️ **이 합격선이 성립하는 이유는 테스트가 probe 주기를 결정적으로 통제하기 때문이다**(r13 codex-cli 개선) — 프로덕션의 비조정 probe에는 상한이 없어 같은 값을 SLA로 쓸 수 없다. 그리고 D 동안 **포기 없이 재시도**하고 재조회 폭주·goroutine 누수가 없다 |
  | (c) | **SDK K회 실패** | `GetSecretValue`가 **K = 3회** 연속 실패한 뒤 성공 | **마지막 주입 실패가 끝난 시각** | **≤ 15초** |

  D·K는 **고정값으로 코드에 박고** 바꿀 때는 합격값도 함께 다시 계산한다. **`T_set`을 기준으로 재는 것은
  (a)뿐이다** — (b)·(c)의 지연은 plan이 통제하는 구간이 아니므로 `T_set` 기준으로 재면 의미가 없다.
- **⚠️ 실제 `readinessHandler`의 1초 context를 통과하는 경로를 게이트에 포함한다**(r9 codex-cli#2):
  회전 후 **`/readyz`만 반복 호출**하는 시나리오에서 *"옛 값 → detached refresh → 다음 호출에서 200"*이
  성립하는지 본다. **이것이 무트래픽 회복의 유일한 경로**이므로 통합 테스트에서 실증하지 않으면
  2-6 실전에서 처음 확인하게 된다.
- **⚠️ 회복 창 동안 나는 503 건수를 실측한다**(r10 claude-ide 개선). 계산으로는 refresh 창 **상한** 10초 ×
  분당 33건 ≈ 5~6건이지만 **10초는 상한이고 33건/분은 평균이라 실제로는 더 적을 수 있다** —
  **2-4가 회복 창의 실제 길이를 잴 수 있는 유일한 자리**다. 여기서 재 두면 런북에 적을 문장이
  **추정이 아니라 실측**이 된다. ⚠️ **이 실측을 런북에 반영하는 단계는 2-5의 런북 갱신 항목이다**
  (r11 claude-ide 개선 — 1-5는 2-4보다 몇 주 앞이라 그때는 실측값이 없다).
- ⚠️ **이 단계가 재는 구간은 위 표의 시나리오별 시계다** — **2-6**(`T_fail`→`T_ready` · `T_visible`→`T_ready`)·
  **목표 절**(`T_avail`→`T_ready`)과 **다른 구간**이므로 숫자를 직접 비교하지 않는다(r10 codex-cli#1).
  ⚠️ **revision 13은 이 문장에 옛 이름 둘을 그대로 뒀다**(r13 claude-ide#2 / codex-cli#3 / codex-ide 개선) —
  `T_avail` 도입이 **문서 전체의 이름 교체**였는데 *"어느 절이 어느 시계를 쓰는지"*만을 위해 존재하는
  이 문장이 폐기된 이름을 가리켰다. **이름을 바꾼 라운드에는 `grep`으로 전수를 훑는다.**
- 동시 요청에서 Secrets Manager mock 호출이 1회로 합쳐지는지
- **`credential_recovered` 로그가 실제 쿼리 성공과 같은 시각에 난다**(r11 codex-ide#2) — 2-6이 이 로그를
  복구 증거로 쓰므로, **로그와 실제 회복이 어긋나지 않는다**를 여기서 한 번 실증한다
→ 검증: 위 전부 통과해야 2-5로 간다.

**2-5. [사람] 배포 — 순서를 강제한다**
main push가 자동 배포를 시작하므로 **앱 merge가 인프라보다 먼저 가면 안 된다**(r2 codex-ide#5).
**작업 트리 상태를 단계마다 명시한다**(r8 codex-cli#4): apply ① 시점의 트리에는 **2-2a만** 반영돼 있고,
apply ② 시점에는 **2-2b까지** 반영돼 있다. 임시 revert나 `-target`을 쓰지 않는다.

> ⚠️ **1·2번은 Step 2 코드 검토 round-4에서 대체됐다.** Terraform은 state의 revision 하나만 deregister하고
> (provider 6.52.0 `task_definition.go`의 Read·Delete가 state의 `arn`만 다룬다) 서비스가 도는 revision은 CI가
> 등록한 것이라, ①을 먼저 하지 않아도 롤백 대상이 사라지지 않는다. 인프라를 내려 둔 상태였으므로
> **이 브랜치에서 `full-apply-prod.sh`로 재기동한 것이 곧 ①②**였다(2026-10-07 실행, 위 "실행 결과").
> 일회성 절차 문서 `apply-gate-2-5.md`는 실행 후 지웠다. 아래 1·2번은 기록으로 남긴다.

1. **[사람] apply ① — 작업 트리에 `skip_destroy = true`만 있는 상태(2-2a)에서 apply.**
   **성공 기준은 "변경 0"이 아니다**(r4 codex-ide#5 — revision 4의 표현이 틀렸다). 현재 미설정 값의 기본이
   `false`이므로 **`false/null → true` in-place 반영**이 뜬다. 기준은 **task definition replacement가
   없을 것** + state에서 `skip_destroy = true` 확인. **① plan에 replacement(`-/+`)가 뜨면 중단**하고 롤백 전략을 재검토한다(게이트).
2. **[사람] apply ② — 2-2b를 작성한 뒤 apply**(task role 정책 신설 + **`DB_SECRET_ARN`·`AWS_REGION` env** →
   task definition 새 revision 등록. **아래 3번 게이트와 같은 두 env다** — r14 codex-ide 개선).
   여기서 이전 revision이 **ACTIVE로 남아 있는지 확인**한다.
3. **[사람] CI 배포 전 게이트**(r10 codex-ide#3): `aws ecs describe-task-definition`으로 **최신 ACTIVE
   revision의 `containerDefinitions[].environment`에서 **두 env**를 확인한다
   (r13 codex-ide#5 / claude-ide#4 — revision 13은 `AWS_REGION`을 필수 계약으로 추가해 놓고
   **같은 게이트의 검사 대상에는 넣지 않았다.** *"없으면 다음 회전 때까지 드러나지 않는다"*고
   방금 규정한 env가 배포 전 검사에서 빠져 있었다):
   - **`DB_SECRET_ARN`** — 실제 값이 있는가
   - **`AWS_REGION`** — 실제 값이 있고 **`DB_SECRET_ARN`의 리전과 일치**하는가
   하나라도 어긋나면 여기서 멈춘다 — production fail-fast가 잡아 주긴 하지만, **배포를 시작한 뒤 태스크가
   기동 실패하는 것보다 배포 전에 멈추는 것이 싸다.**
4. **CI 배포 1회**(`workflow_dispatch` 또는 main push) — `ignore_changes` 때문에 apply만으로는
   라이브에 반영되지 않는다(ADR 0001). **앱 merge가 ①②보다 먼저 가면 안 된다.**
5. smoke 검증: `/readyz` 200, 링크 왕복 302, **그리고 기동 로그의 `secret_provider_mode=secret` 확인**.
   ⚠️ **smoke만으로는 Step 2 배포를 증명하지 못한다** — 다만 **이유가 revision 11에서 바뀌었다**
   (r11 claude-ide#4). production fail-fast(2-2)가 생겨 **env 누락 경로에서는 이제 정적 모드로 뜨지 못한다.**
   남는 구멍은 다른 것이다: **CI가 provider 코드가 없는 옛 이미지를 배포하면** 앱은 `DB_SECRET_ARN`을
   그냥 무시하고 `DB_PASSWORD`로 정상 기동해 smoke를 통과한다. 그리고 **위 3번 게이트는 task definition의
   `environment`만 보므로 이미지 내용을 확인하지 않는다.**
   → **값싼 확인 하나로 닫는다**: 2-1이 남기는 **기동 시 `secret_provider_mode` 로그 한 줄**을 여기서 본다.
   `static`이거나 줄 자체가 없으면 **옛 이미지가 배포된 것**이므로 멈춘다.
6. **[사람] 2-6 검증용 env 투입 — `DB_CONN_MAX_LIFETIME=30s`**(r15 claude-ide#1 (가)).
   ⛔ **위 1~5와 같은 apply 경로를 탄다 — "CI 배포만"으로는 들어가지 않는다**
   (**r19 codex-cli#1 / codex-ide#1, [높음] 2인**). revision 19는 *"env를 추가하고 CI 배포"*라고만 적었는데,
   **배포 workflow는 `describe-task-definition`으로 최신 ACTIVE를 받아 이미지 필드만 교체**하고
   저장소의 미적용 HCL은 읽지 않는다(`.github/workflows/deploy.yml:116-135`).
   ADR 0001도 *"Terraform에서 task definition을 바꾸면 새 revision은 등록되지만 running 서비스는
   옛 revision을 계속 쓴다 — 반드시 CI 배포를 1회 트리거해야 라이브에 적용된다"*로 확정한다.
   → **그대로 두면 30초 값이 라이브에 들어가지 않아 2-6이 다시 5분 경합을 타거나,
   실행자가 승인된 plan 밖에서 `register-task-definition`을 직접 설계해 drift를 만든다.**
   1. **HCL 수정** — `ecs.tf`의 `environment`에 `DB_CONN_MAX_LIFETIME = "30s"` 추가.
   2. `terraform fmt` · `validate` · **`plan` 기대값: task definition `-/+`(새 revision 등록) — 요약은
      `1 to add, 0 to change, 1 to destroy`이고 destroy 1건이 그 교체분이다(`skip_destroy`라 revision은 ACTIVE로 남는다) ·
      다른 리소스 변경 0.**
   3. **[사람] apply.**
   4. `aws ecs describe-task-definition`으로 **최신 ACTIVE의 `environment`에 `DB_CONN_MAX_LIFETIME=30s`** 확인.
   5. **CI 배포 1회**(`workflow_dispatch` 또는 main push) → 라이브 반영.
   6. **기동 로그로 실효값 확인** — 문자열 확인만으로는 부족하다(r19 codex-ide 개선):
      잘못된 값은 **기동 실패가 아니라 `5m` 폴백 + 경고**이므로, 2-1이 남기는
      `conn_max_lifetime` 구조화 로그가 **`30s`**인지 본다.
   ⚠️ **평시 값이 아니다.** 되돌림(아래 7)을 **2-6의 완료 조건**으로 못박는다 —
   잊으면 *"임시 설정이 영구가 되는"* 형태로 남는다.
7. **[2-6 이후] `DB_CONN_MAX_LIFETIME` 회수 — 같은 경로를 반대로 탄다.**
   HCL에서 env 제거 → `plan`(task definition `-/+` — 요약 `1 to add, 1 to destroy`) → **[사람] apply** →
   최신 ACTIVE에 env가 **없는지** 확인 → **CI 배포 1회** → 기동 로그가 **`5m`**인지 확인.
   ⚠️ **2-6을 재시도하려면 6번을 다시 수행한다** — 회수한 뒤에는 30초가 없다.
   ⚠️ **중간에 apply나 CI가 실패하면**: task definition은 새 revision이 등록됐지만 라이브는 옛 revision이다.
   `describe-services`의 `taskDefinition`과 최신 ACTIVE revision을 대조해 **어느 쪽이 라이브인지 먼저 확정**한 뒤,
   CI 배포를 재트리거한다(ADR 0001의 `ignore_changes` 경계 때문에 `terraform apply` 재실행으로는 안 옮겨진다).
8. **런북 갱신 — 2-4 실측을 여기서 반영한다**(r11 claude-ide#3 / 개선3. 1-5는 2-4보다 몇 주 앞이라
   그때는 실측값이 없었다). Step 2가 라이브가 된 지금 `alarm-response.md`의 회전 5xx 항목을 고친다:
   *"회전 직후 **2-4에서 실측한 길이**의 5xx는 Step 2가 동작 중이라는 신호다. 브릿지를 시작하는 기준은
   건수가 아니라 **지속 시간과 `/readyz` 회복 여부**다 — `/readyz`가 1분 안에 200으로 돌아오지 않으면
   그때 브릿지."* 1-5가 적어 둔 *"Step 2 미배포 중에는 전부 브릿지 대상"* 문장을 이 문장으로 **교체**한다.
   ⚠️ **이 갱신이 없으면 Step 2 배포 후 첫 회전에서 운영자가 오탐 카드를 받는다** — 광역 관찰 rule을 넣을 때 스스로
   경계한 것과 같은 종류이고, 여기서는 **기존 알람**이라 신뢰 손상이 더 크다.
   `alb-target-5xx`의 임계·평가 기간 조정이 필요한지는 **2-4 실측값을 근거로 여기서 판단**한다.
- **롤백 경계 — 이 revision에서 확정한다**(r3 codex-cli#5 / codex-ide#6 / claude-ide#4.
  revision 3이 "2-5 시점에 택일"로 미룬 것은 r1이 막았던 "실행 중 구성이 흔들린다"와 같은 종류였다):
  **`skip_destroy = true`를 채택한다.**
  - 근거: `aws_ecs_task_definition.app`에 현재 `skip_destroy`가 없어(코드 확인) env 변경 replacement 때
    이전 revision이 **deregister(INACTIVE)** 되고, INACTIVE로는 서비스를 되돌릴 수 없다. 장애 중에
    "직전 revision을 다시 가리키는" 빠른 롤백이 가능해야 한다. 누적되는 ACTIVE revision은 이 규모에서
    실질 비용이 없다.
  - **적용 순서를 두 apply로 분리한다**(r3 claude-ide#4): **① `skip_destroy = true`만 apply**
    → **② env 변경 apply**. 한 번의 apply에서 replacement가 어느 쪽 설정값을 보는지 자명하지 않으므로
    보존을 확실히 하려면 선행 apply가 필요하다. **그래서 HCL 작성도 2-2a/2-2b로 나눈다**(r8 codex-cli#4). ①의 성공 기준은 위 1번과 같다 — **replacement 없이 in-place 반영**이며 *"변경 0"이 아니다*
    (r5 codex-ide#5 / claude-ide#3 — revision 5가 numbered step은 고쳤는데 이 문장에 옛 표현이 남았다).
  - **앱 이미지 롤백은 이것과 별개로 이미 가능하다** — CI가 최신 ACTIVE를 base로 옛 이미지 태그를
    새 revision으로 등록하므로(`workflow_dispatch`, ADR 0001), `skip_destroy`는 **env/HCL 롤백용**이다.
  → 2-2a·2-2b의 `terraform plan` 기대치가 각각 ①②에 대응한다.

**2-6. [사람] 실전 검증 — Step 2가 복구했음을 판별 가능하게** (0008 `:476` 승계, r2 codex-ide#4 / claude-ide#6)
**`/readyz` 200만으로는 Step 2의 회복을 입증할 수 없다.** 기존 DB 세션이 최대 5분 살아 있고 Step 1이 수 분 내
재배포하므로, Step 2 경로가 **한 번도 실행되지 않을 수 있다.** 그 상태의 `/readyz` 200은 Step 1이 최신 env를
다시 주입했다는 증거일 뿐이다.
ℹ️ **그러나 Step 1을 끌 필요는 없다**(r14에서 확정) — 아래 판정식은 **회전 전에 기록한 task ID**로
범위를 좁히므로 Step 1이 띄운 새 태스크의 로그가 **애초에 대조 대상이 아니기** 때문이다
(r15 codex-cli 개선으로 근거를 정정했다 — 아래 2번).
→ **판별 수단은 ① 하나다.** revision 14까지 있던 ②(통제 창)는 **범위에서 뺐다** — 사유는 아래 2번.

1. **구조화 로그 대조 (기본).** **2-1에서 구현한** 로그를 쓴다(r8 claude-ide#1 — 2-6은 이 필드를
   **소비**하는 단계지 요구를 처음 꺼내는 자리가 아니다. r8 codex-ide#4 / claude-ide#4 — *"·지표"*는
   Step 1과 같은 판정으로 삭제했다).
   ⚠️ **성공 증거는 refresh 성공 로그가 아니라 `credential_recovered` 로그다**(**r11 codex-ide#2 [높음]**).
   refresh 성공 로그는 *"`GetSecretValue`가 다른 값을 반환했고 provider 캐시가 갱신됐다"*까지만 말한다 —
   **그 세대로 dial/인증이 성공했다는 증거가 아니다.** 2-0의 시계 표도 `T_visible` 뒤에 *"다음 probe + dial"*이
   더 있어야 회복이라고 적는다. refresh 로그를 성공 판정에 쓰면 **wrapper의 비밀번호 주입이나 둘째 dial이
   깨져도** old task가 refresh 로그를 먼저 남기고, 잠시 뒤 **Step 1의 재배포가 `/readyz`를 200으로 되돌려**
   *"refresh 로그 < `deployments[].createdAt`"* 조건과 최종 smoke가 **둘 다 통과**한다 —
   실제 복구 주체는 Step 1인데 Step 2가 복구했다고 기록되는 **false positive**다.
   ⛔ **시각 경합(`T1` vs `deployments[].createdAt`)으로 판정하지 않는다**
   (**r12 codex-ide#2 / claude-ide#2, [높음] 2인 동일**). revision 12는 *"T1 < `deployments[].createdAt`이면
   Step 2가 먼저 복구한 것"*이라고 적으면서 근거를 *"Step 1 재배포는 수 분"*으로 들었는데,
   **`createdAt`은 `UpdateService`가 호출된 순간 찍히는 제출 시각이지 롤링이 끝난 시각이 아니다.**
   ⚠️ **이 plan이 Step 1에 대해 세 라운드에 걸쳐 세운 규율을 2-6이 그대로 어겼다** —
   `:400-410`은 *"PRIMARY는 '가장 최근 배포'라는 지위일 뿐 rollout 결과가 아니다"*, `:838`은
   *"`redeploy_submitted`는 제출까지만 뜻하므로 회복 판정은 `rolloutState`와 알람 이력으로 확인한다"*
   (**1-7의 문장이다. 2-6도 같은 문장을 쓴다** — r12 claude-ide 개선).
   실제 두 시각은 **둘 다 `T_label` 기준 수십 초 단위**이고 그 사이에 **상한 없는 전파 구간(`T_label`→`T_avail`)**이
   끼므로, 전파가 10초만 걸려도 *"Step 1이 먼저"*로 뒤집힌다 — 그때 Step 1은 **제출만** 했고 새 태스크는
   몇 분 뒤에야 트래픽을 받는데, **실제로 서비스를 되살린 것은 old task에서 돈 Step 2**다.
   **판정 방향이 반대로 나온다(false negative).**

   → **시각 비교 없이 성립하는 더 강한 증거가 이미 이 문서 안에 있다**(r12 claude-ide#2):
   ℹ️ **`credential_recovered`는 세대 전이에 묶여 있다** — Step 1이 띄운 새 태스크는 보통 env에서
   새 비밀번호를 seed로 받아 세대 전이가 없다(r12 claude-ide 개선).
   ⚠️ **다만 "원리적으로 불가능"은 아니다**(r15 codex-cli 개선): 이 문서는 리스크표에서
   *"전파 지연으로 새 태스크가 옛 `AWSCURRENT`를 주입받을 수 있다"*를 잔여 위험으로 인정하고,
   **그런 새 태스크는 이후 refresh에서 새 값을 얻어 세대 전이를 만든다.**
   → **false positive를 막는 실제 경계는 "회전 전에 기록한 task ID"다.** 세대 결합은 그 위의 보강일 뿐,
   판정 근거로 단독으로 쓰지 않는다.
   → **판정: "(0)에서 기록한 회전 *전* task_id에서 `credential_recovered`가 났다" = Step 2 복구의 증거다.**
   `deployments[].createdAt`은 **"Step 1이 개입했는지"를 보는 보조 지표로만** 남기고, 판정식에서 뺀다.

   ⛔ **단, "났다"를 이번 회전으로 한정한다 — watermark가 없으면 이전 회전의 로그가 통과한다**
   (**r13 codex-cli#2 [높음]**). revision 13의 판정식은 *"회전 전 task ID 집합에 `credential_recovered`가
   **존재하는지**"*만 요구하는데, **ECS 태스크가 두 회전 창 이상 살아 있으면 이전 세대 전이 로그도 같은
   `task_id`를 가진다** — 회전 창이 7일이고 태스크가 그보다 오래 사는 것은 정상이므로 **현실적인 경로다.**
   r12에서 시각 경합을 없앤 것은 옳았지만(`createdAt`과 겨루는 것) **시간 범위까지 함께 없앤 것은 과했다** —
   *"어느 로그를 볼 것인가"*(범위)와 *"누가 먼저인가"*(경합)는 다른 문제다.
   → **(0)에서 각 태스크의 로그 watermark(`timestamp` + `eventId` 쌍, 아래 (0) 참조)를 함께 기록**하고,
   **그 watermark 이후**의 로그만 본다. 그리고 **순서까지 성립해야** 성공으로 친다:

   ```
   auth_failed_observed(generation = N)  →  refresh 성공(N → N+1)  →  credential_recovered(generation = N+1)
   ```
   같은 `task_id`에서 **이 세 줄이 이 순서로** 나야 한다.
   ⛔ **세대가 이어지지 않거나 순서가 어긋나면 "판정 불가"가 아니라 "확정된 Step 2 결함"이다**
   (**r21 codex-cli#1 / codex-ide#1, [높음] 2인**) — 아래 종료 경로 표가 단일 기준이다.
   **로그가 아예 생성되지 않은 경우만 판정 불가다.**
   ⚠️ **watermark를 `T_label` 이후로 자르지 않는다**(codex-cli#2) — 시계 표대로 **`T_fail`은 `T_label`보다
   앞설 수 있어서**(기존 세션이 먼저 끊기는 경우) `T_label` 기준으로 자르면 첫 줄을 잘라 버린다.
   **기준은 회전 명령/창 이전에 잡은 watermark다.**

   ⛔ **어느 회전을 관측할지 먼저 정한다 — (0)은 "회전 전"에 실행돼야 하는데 자연 창은 24시간이다**
   (**r19 codex-ide#3 [중간]**). revision 19는 (0)의 기준점 기록과 회전 직후 반복 호출을 요구하면서
   **그 2~3분을 24시간 창에서 어떻게 포착하는지**를 정하지 않았다. 둘 중 하나로 확정한다:

   | | 방식 | 판정 |
   | --- | --- | --- |
   | **(가)** | **[사람]이 지켜보는 시각에 온디맨드 회전을 일으킨다** — `aws rds modify-db-instance --rotate-master-user-password --apply-immediately`. **Step 1은 켠 채로 둔다**(rule을 끄지 않는다 — r14에서 통제 창을 뺀 이유가 그대로 유효하다) | ✅ **채택.** (0)의 기준점을 확실히 잡을 수 있고, 관측 창 2~3분을 놓치지 않는다. **회전 창도 +7일 밀려** 다음 무방비 창이 멀어진다 |
   | (나) | 자연 창(24시간)을 기다리며 기준점·호출·증거 수집을 자동화 | ❌ 미채택. **새 상시 코드·스케줄이 필요**하고, 이 plan의 목적(Step 1·Step 2)에서 벗어난다 |

   ⚠️ **(가)는 진짜 회전을 일으키므로 짧은 다운을 만든다.** 다만 **Step 1이 라이브인 뒤**에 하는 것이라
   자동 재배포가 받는다 — 그것이 2-6이 검증하려는 대상이기도 하다.
   **1-6 드릴과 같은 명령이고, 2-6은 그 드릴의 Step 2 버전이다.**

   **(0) ⚠️ 회전 *전에* task_id 2개를 기록한다 — 이것이 없으면 ①은 구조적으로 항상 판정 불가다**
   (**r12 claude-ide#4 [중간]**). 수단 ①은 **Step 1을 켠 채로** 도는 시나리오인데, Step 1이 정상이면
   `T_label` 직후 재배포가 제출돼 **10분 상한 안에 running task 집합이 통째로 새 태스크로 바뀐다.**
   **새 태스크의 `task_id`는 (0)의 집합에 없어 애초에 판정 대상이 아니므로**(r21 codex-cli#3 —
   *"새 태스크는 남기지 않는다"*가 **아니다**. r15에서 지운 거짓 전제가 여기서 세 번째로 살아났다),
   `list-tasks`를 **판정 시점에** 실행하면
   **교집합이 항상 공집합**이다 — *"기본 수단"*인 ①이 **Step 1이 도는 한 성공할 수 없다.**
   그러면 **매 회전마다 판정 불가**가 되어 Step 2 검증이 영영 끝나지 않는다.
   → **회전 전에 셋을 기록한다**: **① task ID 2개**(`aws ecs list-tasks`) — 대조 대상,
   **② 각 태스크의 ENI IP**(`describe-tasks`) — 관측 창 종료 판정용(r14 codex-ide#4),
   **③ 각 태스크의 로그 watermark** — 이번 회전의 로그만 보기 위해(r14 codex-cli#2).
   ⚠️ **watermark 형식**(r14 codex-cli 개선 / codex-ide 개선 — 위 ③의 구체 계약이다). revision 14는
   *"마지막 이벤트 시각 또는 `nextToken`"*으로 **둘 중 하나를 실행 시점에 고르게** 남겼는데,
   `FilterLogEvents`의 `nextToken`은 **페이지 진행 토큰이라 24시간 뒤 만료되고 응답을 다 읽으면
   아예 없을 수도 있다** — 드릴 당일 해석이 갈린다.
   → **태스크별 log stream에서 `(마지막 이벤트의 timestamp, 그 이벤트의 eventId)` 쌍을 기록**하고,
   판정 때는 **`FilterLogEvents`를 `startTime = 기록한 timestamp`로 호출한 뒤 그 `eventId`까지를 버린다**
   (동률 timestamp를 `eventId`로 가른다). 경계는 **기록한 이벤트 자신을 제외**한다.

   **재는 값**: **T0 = `T_label`**(광역 관찰 rule 로그의 **이벤트 `time`**. `LastRotatedDate`는
   **근사치로만** 쓴다 — 공식 계약이 라벨 이동 시각을 보장하지 않는다, r11 codex-ide#1) /
   **`T_fail`→`T_ready`(다운타임)** / **`T_visible`→`T_ready`(약 2~7초, 실측 목표)**.
   ⚠️ **`T_avail`→`T_ready`(24초)는 여기서 못 잰다** — `T_avail`이 관측 불가라 **2-4 (b)의 몫**이다.
   ⚠️ **2-4·2-6·목표 절이 재는 구간은 서로 다르다**(r10 codex-cli#1). **숫자를 비교하기 전에 시계부터 맞춘다.**

   **⚠️ `auth_failed_observed`로 원인을 좁힌다 — 다만 부재를 확정 판정으로 쓰지 않는다**
   (r12 claude-ide#3 → **r13 codex-ide#3으로 강도를 내렸다**):
   - **있는데 `credential_recovered`가 없다** → **Step 2 결함**이다(재조회·주입·둘째 dial 중 하나).
     이건 확정할 수 있다 — 실패를 관찰하고 로그까지 남긴 것이 증명됐기 때문이다.
   - **둘 다 없다** → ⛔ **판정 불가**다. revision 13은 이것을 *"`T_fail` 미도래 = Step 2 결함 아님"*으로
     **단정했는데 성립하지 않는다**(codex-ide#3): **`28P01` 분류·래핑이 깨졌거나 로그 발행 코드가 깨진
     경우에도 똑같이 아무 로그가 없다.** 그 둘은 명백히 Step 2 결함이다.
     ⚠️ *"이 로그를 두 원인을 구분하려고 도입했다"*는 설명 자체와 모순이었다 — **부재는 아무것도 구분하지
     않는다.** (2-1 검증 ⑮가 로그 코드를 단위 테스트로 고정하므로 확률은 낮지만, **낮은 것과 증명된 것은 다르다.**)
   - → **부재를 "Step 2 무결"로 읽으려면 독립 신호가 필요하다**: 관측 창 동안
     **`/readyz` 503이 한 건도 없었고 `alb-target-5xx`도 조용했다**면 *"신규 연결에서 인증 실패가
     일어나지 않았다"*가 앱 로그와 **독립적으로** 뒷받침된다. 그 증거가 있을 때만
     *"`T_fail` 미도래"*로 기록하고, 없으면 **판정 불가로 남긴다.**

   **⚠️ ①의 관측 창은 10분이 아니라 "회전 전 태스크가 트래픽을 받는 동안"이다**(**r13 claude-ide#3 [중간]**).
   ①은 **Step 1을 켠 채로** 도는 시나리오라 `T_label` 직후 재배포가 제출되고, 롤링이 진행되면
   **ALB가 (0)의 태스크로 라우팅을 멈추고 드레인이 시작**된다 — 그 시점부터 `/readyz` 반복 호출은
   그 태스크에 닿지 않으므로 `auth_failed_observed`도 `credential_recovered`도 **더는 생기지 않는다.**
   즉 아래 *"두 `task_id`가 모두 관측될 때까지 10분"* 절차는 **태스크가 바뀌지 않는다는 전제**로
   쓰여 있고 ①에는 맞지 않는다.
   → **①에서는 관측 창이 (0)의 task ID가 `list-tasks`에서 사라지는 순간 끝난다.**
   ⛔ **종료 조건은 `list-tasks`의 태스크 소멸이 아니라 ALB 대상 등록 해제다**(**r14 codex-ide#4**).
   revision 14는 *"(0)의 task ID가 `list-tasks`에서 사라지는 순간"*으로 잡았는데 **그것이 실제 증거 생성
   중단보다 늦다** — ALB는 **대상 deregistration 즉시 새 요청 라우팅을 멈추고** 대상은 그 뒤
   `draining`에 머무르므로, **태스크가 아직 `RUNNING`이어도 관측 창은 이미 닫혀 있다**
   ([대상 등록 해제](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/target-group-register-targets.html)).
   → **(0)에서 회전 전 태스크의 ENI IP도 함께 기록**하고, `aws elbv2 describe-target-health`에서
   **그 대상이 `draining`이 되거나 목록에서 빠지는 시점**을 관측 창의 끝으로 쓴다.
   두 로그가 다 나오기 전에 그 시점이 오면 **10분을 기다리지 말고 즉시 종료**한다 —
   **원인은 Step 2가 아니라 Step 1의 롤링이므로 "자극이 닿지 못했다"는 세 번째 원인**이고,
   위 두 분기 어디에도 넣지 않는다. **다음 온디맨드 시도에서 ①을 다시 돌린다**(아래 회전 트리거 (가)).
2. ⛔ **통제 창(Step 1 rule 일시 비활성화 + 온디맨드 회전)은 이 plan의 범위에서 뺀다 — 확정**
   (**r14 codex-cli#2·#3 / codex-ide#2 / claude-ide#3·#4, 3인 전원 5건**).

   **왜 지웠는가 — 두 이유이고, 첫째가 더 중요하다.**

   **(1) ①의 증거가 r12~r13에서 결정적이 되어 ②의 존재 이유가 사라졌다.**
   ②는 *"Step 1을 켠 채로는 Step 2의 회복을 입증할 수 없다"*는 전제로 신설됐는데, 그 전제는
   **판정이 시각 경합이던 시절의 것**이다. 지금 ①이 요구하는 것은
   **(0)의 회전 전 `task_id` + watermark 이후 + `auth_failed_observed(N) → refresh N→N+1 →
   credential_recovered(N+1)` 순서**이고, 이 증거는 **Step 1이 켜져 있어도 오염되지 않는다.**
   ⚠️ **오염을 막는 경계는 "회전 전에 기록한 task ID"다 — "새 태스크는 세대 전이를 만들 수 없다"가 아니다**
   (r15 codex-cli 개선). revision 15는 후자를 근거로 들었는데 **이 문서의 다른 문장과 충돌한다**:
   리스크표가 *"전파 지연으로 새 태스크가 옛 `AWSCURRENT`를 주입받을 수 있다"*를 잔여 위험으로 인정하고,
   **그런 새 태스크는 이후 refresh에서 새 값을 얻어 세대 전이 로그를 남긴다.**
   → 근거를 **task ID 경계 하나로** 좁힌다. 결론(*"Step 1을 끄지 않아도 누가 복구했는지가 갈린다"*)은
   그대로 성립한다 — 새 태스크의 `task_id`는 (0)의 집합에 **없기 때문**이다.
   ②는 더 이상 **증거 오염 방지**를 위해 필요한 백스톱이 아니다.
   ⛔ **그런데 이것은 ②가 하던 두 일 중 하나(증거 오염 방지)일 뿐이다 — 나머지 하나(관측 창 확보)는
   별도로 닫아야 한다**(**r15 codex-cli#3 / codex-ide#2 / claude-ide#1, 3인 전원 [높음]**).
   revision 15는 *"관측 창도 충분하다 — 창은 수 분이고 기대 회복은 24초"*라고 적었는데 **비교 대상이 틀렸다.**
   창 안에 들어가야 하는 것은 24초가 아니라 **`T_fail` 도래 + (`T_fail`→`T_ready`)**이고,
   **`T_fail`은 최대 커넥션 수명만큼 늦을 수 있다**(시계 표·2-6 도입부가 두 번 적은 사실이다).
   실측으로 확인했다:

   | | 값 | 근거 |
   | --- | --- | --- |
   | **관측 창** | **약 2~3분** | ECS가 옛 태스크를 deregister하는 시점 = **새 태스크가 ALB 헬시**. 그 헬스체크는 **`/healthz` liveness**이지 DB 준비 상태가 아니다(`alb.tf:27-35`, `interval 30 × healthy_threshold 3`) — 즉 **빨리 헬시해지고 옛 태스크도 빨리 빠진다**(r15 codex-cli#3) |
   | **필요 시간** | **최대 5분 + 24초** | `T_fail`은 신규 연결이 필요해질 때 온다. `db.go:29` `SetConnMaxLifetime(5 * time.Minute)` — 게다가 Go 계약상 만료 커넥션은 **재사용 전에 지연 폐쇄**될 수 있어 더 늦을 수 있다 |

   **필요 쪽이 더 길다.** ⚠️ 그리고 이 문서는 그 사실을 이미 두 곳에 적어 두었다 —
   *"최대 대기 10분"*(아래)과 *"창은 10분을 넘지 못한다"*를 나란히 놓으면 결론은 하나다:
   **10분이 실제로 필요한 회차에서 ①은 구조적으로 증거를 만들지 못한다.**
   자극을 늘려도 풀리지 않는다 — `/readyz` 반복은 **idle 커넥션을 재사용**할 뿐 신규 연결을 강제하지 않고
   ([`DB.PingContext`](https://pkg.go.dev/database/sql#DB.PingContext) 계약, r15 codex-ide#2),
   병렬 부하로 idle 25개를 넘기는 방법은 이 plan이 이미 기각했다(*"동시 점유가 보장되지 않는다"*).

   → **`T_fail`을 우연이 아니라 설계로 만든다**(r15 claude-ide#1 (가), **권장안 채택**):
   **검증 기간 동안만 `SetConnMaxLifetime`을 짧게 한다.**
   - `db.go:29`의 `5 * time.Minute`를 **환경변수로 읽게 하고**(기본값은 현행 5분 유지),
     2-5의 6번 경로로 **`DB_CONN_MAX_LIFETIME=30s`**를 투입한다.
     ⚠️ **이것은 보장이 아니라 확률 개선이다**(r19 codex-ide#3 / codex-cli 개선).
     `SetConnMaxLifetime`은 **연결의 최대 재사용 수명만 정하고 요청이나 새 연결을 발생시키지 않는다**
     ([Go 계약](https://pkg.go.dev/database/sql#DB.SetConnMaxLifetime)). 실제로 `T_fail`이 오려면
     그 뒤에 **probe가 도착해야 하는데 probe 간격은 평균이지 상한이 아니고**, ALB가 두 태스크에
     각각 보낸다는 보장도 없다. 게다가 **`T_label`→`T_avail`(전파)에 상한이 없어** 새 값이 조회
     가능해지기 전에 옛 태스크가 draining될 수 있다.
     → **30초는 "5분 경합"을 "30초 경합"으로 줄일 뿐이다.** 2-6이 **비차단 추적 항목**인 이유가 이것이고,
     판정 불가가 나오면 다음 회차에서 다시 시도한다.
   - **앱 파라미터라 인프라가 번지지 않는다** — ②를 뺀 이유(인프라 범위가 Step 1로 번짐)와 충돌하지 않는다.
   - 2-6이 끝나면 **원값으로 되돌리는 배포 한 번**이 대가다. 그 되돌림을 2-6의 완료 조건에 넣는다.
   - 근거: **2-4가 이미 *"신규 연결 강제"*를 fixture로 한다** — 프로덕션에서 같은 일을 하는 유일한 손잡이다.
   - ⚠️ 비용: 커넥션이 30초마다 교체된다. canary 트래픽이 분당 약 33건이므로 **연결 생성 부하는 무시할 수준**이고,
     기간도 한 회전 창이다.

   **(2) 안전하게 만들려면 인프라 범위가 Step 1로 번진다.** r13이 넣은 dead-man은
   **계약이 절반만 적힌 상태**였고, 3인이 그 구멍을 전수로 짚었다:
   - **role이 어디에도 없다**(claude-ide#3 / codex-cli#2) — *"1-1/1-3에서 함께 만든다"*고 **배정만** 했고
     IAM 목록(네 조각)·1-1 리소스 범위·**1-1의 `plan` 게이트 숫자**(*"신설 알람은 6개"* 같은 기계적 대조)·
     1-3 검증 어디에도 없다. **이 plan이 r6에서 이미 고친 형태**(*"IAM 목록이 절마다 달라 첫 호출이
     `AccessDenied`가 된다"*)인데, 이번에는 목록이 다른 게 아니라 **아예 없다.**
   - **필요한 것이 role 하나가 아니다** — `scheduler.amazonaws.com` trust(+ confused-deputy 제한),
     좁은 rule ARN 한정 `events:EnableRule`, 실행자의 `scheduler:CreateSchedule/GetSchedule/DeleteSchedule`,
     그 role을 target에 붙일 **`iam:PassRole`**.
   - **`get-schedule` 성공이 `EnableRule` 성공을 뜻하지 않는다**(codex-cli#3 / codex-ide#2) —
     assume/권한 오류, target 전달 실패, universal target 입력 오류로 **조용히 실패**할 수 있다.
     `FlexibleTimeWindow`, target ARN(`arn:aws:scheduler:::aws-sdk:eventbridge:enableRule`),
     `Input`의 `Name`/`EventBusName`, `RoleArn`, `RetryPolicy`, `DeadLetterConfig`가 전부 미확정이었다
     ([`create-schedule` 계약](https://docs.aws.amazon.com/cli/latest/reference/scheduler/create-schedule.html),
     [universal target](https://docs.aws.amazon.com/scheduler/latest/UserGuide/managing-targets-universal.html)).
     → **`trap`이 실패하는 바로 그 상황에서 dead-man도 조용히 실패**할 수 있으므로 *"30분 안에 반드시"*는
     **성립하지 않는 보장이었다.** 게다가 Scheduler는 **60초 정밀도**라 정확한 상한도 아니다
     ([시간 정밀도](https://docs.aws.amazon.com/scheduler/latest/UserGuide/schedule-types.html)).
   - **T+30분 예산 자체가 상한 없는 구간을 덮어야 했다**(claude-ide#4) — ②의 타임라인은
     *"안정화 5분 + 회전 + 관측 10분"*인데 **회전 구간은 이 plan이 "상한 없음"이라 확정한 값**이다.

   ⛔ **종료 경로를 둘로 가른다 — "판정 불가"와 "확정된 결함"을 같은 통에 넣지 않는다**
   (**r19 codex-ide#2 [높음]**). revision 19는 불확정 사유에 *"Step 2 결함"*까지 넣어 **셋 다 다음 자연
   회전으로 미뤘는데**, 그러면 **깨진 구현을 한 창(7일) 동안 운영에 남긴다.**
   이 문서 자신이 *"Step 2의 코드 결함은 Step 1이 못 받는다 — 이전 이미지 태그로 CI 재배포가 유일한
   백스톱"*이라고 적어 놓은 것과 정면으로 충돌한다.

   | 관측 | 뜻 | 종료 경로 |
   | --- | --- | --- |
   | `auth_failed_observed` **없음** + `credential_recovered` **없음** | **판정 불가** — `T_fail` 미도래인지 로그 코드가 깨진 것인지 구분 불가 | **비차단.** 사유 기록 → 다음 온디맨드 시도에서 재시도(6번 재투입 포함) |
   | (0)의 대상이 ALB에서 빠짐 | **자극 미도달** — Step 2와 무관 | **비차단.** 동상 |
   | `auth_failed_observed` **있음** + `credential_recovered` **없음** | ⛔ **확정된 Step 2 결함** — 실패를 관찰하고 로그까지 남겼는데 회복하지 못했다 | ⛔ **다음 회전을 기다리지 않는다.** 즉시 **이전 이미지 태그로 CI 재배포**(`workflow_dispatch`)해 Step 2를 라이브에서 내리고, 코드를 고쳐 **2-4부터 다시** 검증한다 |
   | 세대·순서가 어긋남(예: `refresh N→N+1` 뒤 `credential_recovered(N)`) | ⛔ **확정된 Step 2 결함** | 동상 |

   ℹ️ **"비차단"은 증거가 생성되지 않은 회차에만 적용된다.** 증거가 생성됐고 그것이 결함을 가리키면
   그때는 차단이다 — Step 2가 라이브인 채로 다음 회전을 맞으면 **그 회전이 실장애가 된다.**

   → **판정 불가인 경우에만** 다음 시도에서 ①을 다시 돌린다. 사유를 기록해 자극 절차를 조정한다.
   ⛔ **재시도는 "다음 자연 회전"이 아니라 "다음 [사람] 입회 온디맨드 회전"이다**
   (**r21 codex-ide#2 [높음]**). revision 21은 회전 트리거를 (가) 온디맨드로 확정해 놓고
   **재시도 경로는 전부 "다음 자연 회전"을 지시**했는데, **이 문서 자신이 자연 창에서는
   (0) 기준점과 2~3분 관측 창을 포착할 수 없다고 판단**해 (가)를 채택한 것이다 —
   **plan이 불가능하다고 한 방식을 재시도 경로가 지시하고 있었다.**
   → 재시도도 **6번 재투입 → 온디맨드 회전 → (0) → 관측**의 같은 절차다.
   **"2창 연속"이 아니라 "2회 연속 시도"**로 센다.

   ⛔ **끝을 정한다 — "대가는 최대 한 창"은 다음 회차가 성공한다는 가정이었다**(**r15 claude-ide#3 [중간]**).
   불확정 사유 셋 중 *"`T_fail` 미도래"*와 *"자극 미도달"*은 **창 길이와 커넥션 수명의 관계**라
   **회차가 바뀌어도 그대로**다 — 위 (가)를 적용하지 않으면 같은 이유로 반복 실패할 근거가 있는데
   revision 15에는 **상한도, 기한도, "몇 번 실패하면 무엇을 한다"도 없었다.**
   ⚠️ **이 plan은 같은 문제를 1-7에서 이미 풀었다** — *"1-7을 게이트로 두면 근본 대응이 1~2주 밀린다.
   회고 A-11이 지적한 '조건부 게이트로 미루기'를 그대로 재현하는 것이다. **1-7은 Step 2와 병행되는
   운영 추적 항목이다**"*(`:948-951`). **2-6에도 같은 규율을 적용한다:**

   | | 무엇이 | 성격 |
   | --- | --- | --- |
   | **2-4 통합 테스트** | **Step 2의 차단 게이트** | 이미 그렇게 정의돼 있다(*"[사람] 통합 테스트 — 배포 게이트"*). 배포 승인의 근거는 여기다 |
   | **2-6 실전 검증** | **병행 운영 추적 항목** — **Step 2 완료의 차단 게이트가 아니다** | 프로덕션 실증은 값이 크지만, **반복 가능한 실패에 Step 2 완료를 묶지 않는다**(회고 A-11) |

   - **2회 연속 시도가 판정 불가면** 사유를 회고에 기록하고 **별도 라운드에서** 위 (가)의 강도를 올리거나
     ②(아래 요구사항 목록)를 다룬다. **2-6이 Step 2 착수·완료를 막지 않는다.**
   - 그래도 **(가)를 먼저 적용한다** — 한 줄짜리 앱 파라미터로 첫 회차의 성공 확률이 크게 오른다.

   ℹ️ **통제 창이 다시 필요해지면 별도 라운드에서 다룬다** — 위 (2)의 목록이 그때의 요구사항이다.
   ⚠️ **그때 두 길을 나란히 본다**(r15 claude-ide 개선): **인프라를 늘리는 길**(②+dead-man, 위 목록) /
   **앱 파라미터로 푸는 길**(위 (가)의 커넥션 수명 — 더 짧게, 또는 검증용 강제 재연결 신호).
   **후자가 이 서비스 규모에 맞고 인프라를 늘리지 않는다.**
   이 plan의 목적은 **Step 1 라이브와 Step 2 근본 대응**이고, 그 목적에 ②는 필요하지 않다.

**⚠️ 신규 연결을 확실히 만들어야 판정이 성립한다**(r3 codex-ide#7). 풀은 idle을 최대 25개 보유하고
커넥션 수명이 5분이라, 회전 직후 `/readyz`와 smoke 요청이 **기존 세션만 재사용하면 `28P01`이 관측되지 않아
refresh도 `credential_recovered`도 나오지 않는다**(= 시계 표의 `T_fail`이 오지 않는 경우).
그러면 판정 불가다 — ⚠️ **관측 창은 "회전 전 태스크가 ALB에서 트래픽을 받는 동안"이고 10분보다 짧다**
(r13 claude-ide#3 / r14 codex-ide#4). Step 1이 켜져 있으므로 재배포가 제출되고, **새 태스크가 헬시해지면
옛 태스크가 deregister되어 자극이 끊긴다.** 상한 10분은 그 창을 넘지 못한다.
→ **확정한다**(r4 codex-ide#6 — revision 4의 *"…등 방법과 최대 대기 시간을 정하고"*는 선택을 실행 시점에
   미룬 것이다):
   - **새 진단 표면을 만들지 않는다 — 대신 `/readyz`를 명시적으로 반복 호출한다**(r5 codex-ide#6 /
     codex-cli 개선, **r9 codex-cli#4로 정정**). revision 5의 두 방법은 동등하지 않았다: k6로 짧은 쿼리를
     보내도 idle 25개 동시 점유가 보장되지 않고, `pg_sleep` 진단 엔드포인트는 **프로덕션에 지연 유발
     표면**을 새로 만든다. 그래서 둘 다 채택하지 않는다.
     ⚠️ **그러나 "자극 불요"라는 결론은 과했다**(r9 codex-cli#4).
     → **[사람]이 `/readyz`를 반복 호출한다. 절차·상한·실패 처리는 아래 "두 태스크 관측" 항목 하나에만
     적는다**(r10 claude-ide#3 — revision 10은 병합 과정에서 같은 절차를 두 블록에 썼고, 체크리스트로
     쓰는 문서에서 *"두 번 호출하라는 것인가"*로 읽힐 여지가 있었다).
     **프로덕션에 새 코드·부하를 넣지 않는다.**
   - **최대 대기: 10분** ⚠️ **그런데 이 값이 실제로 필요해지는 회차는 판정할 수 없다**(r15 claude-ide 개선) —
     관측 창은 **약 2~3분**(새 태스크가 ALB 헬시해지면 옛 태스크가 빠진다)이라 **10분을 넘지 못한다.**
     그래서 위 (가)의 `DB_CONN_MAX_LIFETIME=30s`가 **10분을 필요 없게 만드는 장치**다.
     아래 근거는 **(가)를 적용하지 않은 경우의 상한**이다.
     (r6 codex-cli#3 / codex-ide#4 / claude-ide#4 — revision 6의 5분은
     **풀 교체 보장 5분과 마진 0**이었다). Go 계약상 만료 커넥션은 **재사용 전에 지연 폐쇄될 수 있고**,
     여기에 마지막 커넥션의 잔여 수명 + 다음 probe + ALB 라우팅 여유가 붙는다.
     0008이 같은 형태를 이미 고쳤다(*"성공 기준 6분이 실측 5분47초 대비 마진 13초 → 10분으로 완화"*, `:466`).
     1-6·1-7과 같은 값이라 기준도 통일된다.
   - **성공 조건 — 판정식 전체를 여기 다시 적는다**(**r15 codex-cli#4 [중간]**. revision 15는
     권위 있는 판정식을 위쪽 판정 문단에만 두고 **최종 체크리스트에는 *"`credential_recovered`가 있는지"*만**
     남겨, **운영자가 마지막 체크리스트만 따르면 r14에서 고친 stale-log false positive가 재발**한다):
     > **(0)에서 회전 전에 기록한 task ID 2개 각각에서**, **그 태스크의 watermark `(timestamp, eventId)` 이후에**,
     > **`auth_failed_observed(세대 N)` → `refresh 성공(N → N+1)` → `credential_recovered(세대 N+1)`**
     > **세 줄이 이 순서로** 나야 한다.
     > ⛔ **세대가 이어지지 않거나 순서가 어긋나면 "확정된 Step 2 결함"이다 — 판정 불가가 아니다.**
     > 종료 경로 표를 따라 **즉시 이전 이미지로 CI 재배포**한 뒤 2-4부터 재검증한다(r21 2인 [높음]).

     (r6 codex-ide#4 + r11 codex-ide#2 + r13 claude-ide 개선 — *"두 running task"*라는 옛 표현이
     **판정 시점의 running으로 읽히던** 오독 경로도 여기서 닫혔다).
     태스크가 2개이고 canary가 ALB를 거치므로 한 태스크에만 도달할 수 있다 → **구조화 로그에 task 식별자를
     넣어** 둘 다 확인한다. 한쪽만 나오면 판정 불가로 종료한다.
     ⚠️ **refresh 성공 로그만 있고 `credential_recovered`가 없으면 "성공"이 아니라 "Step 2 결함"이다** —
     캐시는 갱신됐는데 그 값으로 연결하지 못한 상태이고, 그때 `/readyz`가 200으로 돌아왔다면
     복구 주체는 Step 1이다.
   - **판정 방법**: **(0)에서 회전 전에 기록한** task ID 2개 각각에 대해, **그 태스크의 watermark 이후
     로그에서 위 세 줄의 순서와 세대 연속성을 확인**한다 — `credential_recovered`의 존재만 보지 않는다
     (r15 codex-cli#4). `FilterLogEvents`를 `startTime = 기록한 timestamp`로 호출하고
     **그 `eventId`까지 버린다**(경계는 자기 자신을 제외).
     (r12 claude-ide#4 — **판정 시점에 `list-tasks`를 실행하면 안 된다.**
     Step 1이 돌았으면 그 목록은 이미 새 태스크로 바뀌어 있어 **(0)의 집합과 교집합이 공집합**이 된다).
     `task_id`는 **2-1에서 구현한 필드**를 쓴다 — 구현 지시는 2-1에 있다(r9 claude-ide#4 —
     revision 9는 refresh 로그만 2-1로 올리고 `task_id`는 2-6에 구현 지시를 남겼고, 게다가
     **`/task` 경로가 빠진 정정 전 문구**여서 구현자가 base URI를 조회하면 `TaskARN`이 없어
     `task_id=unknown`이 상시 발생한다 — 이 plan이 *"판정 불가"*로 규정한 바로 그 상태다).
     `unknown`이면 **판정 불가로 처리한다**(증거 없이 성공으로 넘기지 않는다).
   - **⚠️ 두 태스크 관측을 우연에 맡기지 않는다 — [사람]이 `/readyz`를 반복 호출한다**(r9 codex-cli#4).
     revision 9의 *"풀이 스스로 교체되고 canary가 자극한다"*는 **사용하는 API가 주지 않는 보장**이다:
     Go의 `SetConnMaxLifetime`은 **최대 재사용 시간**을 정할 뿐 사용 중 커넥션까지 정확히 5분 안에
     닫는다는 계약이 아니고(cleaner는 idle 목록만 닫는다), Route53 헬스체커들은 서로 조정되지 않아
     ALB를 거친 요청이 두 태스크에 각각 도달한다는 타깃별 보장도 없다.
     → **절차**(이 문서에서 유일한 절차 기술이다): 회전 후 **`/readyz`를 1초 간격으로 반복 호출**한다
     (읽기 전용·비파괴, canary가 이미 상시로 하는 것과 같은 요청이라 새 표면을 만들지 않는다).
     **두 `task_id`가 모두 관측될 때까지** 계속한다.
     **상한은 10분이거나 (0)의 대상이 ALB에서 `draining`/미등록이 되는 시점 중 빠른 쪽**이다
     (r13 claude-ide#3 / r14 codex-ide#4). 등록이 빠진 뒤의 대기는 **아무 증거도 만들지 않는다** —
     즉시 판정 불가로 종료하고 **사유를 기록한 뒤 다음 온디맨드 시도에서 다시 돌린다.**
   - **⚠️ 이 반복 호출은 `alb-target-5xx`를 울릴 수 있다 — 끝나면 정리한다**(r10 claude-ide#2,
     **r11 codex-cli 개선 / codex-ide 개선으로 "반드시"를 내렸다**).
     detached refresh가 도는 동안 도착한 `/readyz`는 전부 **503**이고(대기 초과 시 오류 반환),
     1초 간격이면 refresh 창 **상한** 10초에 10건 안팎이 쌓인다. 기존 알람은
     `HTTPCode_Target_5XX_Count` **Sum ≥ 5 / 5분**(`monitoring.tf:56-60`)이라 **임계를 넘을 수 있다.**
     ⚠️ **다만 10초는 상한이다** — refresh가 빨리 끝나면 5건에 못 미쳐 발화하지 않을 수도 있다.
     **발화 여부를 성공 판정에 쓰지 않는다.** 정리 절차는 발화한 경우에만 수행하는 **조건부**다.
     → 1-4가 *"의도적으로 발생한 알람을 [사람]이 정리한다"*를 적은 것과 같이, **2-6 체크리스트에도
     알람 정리 단계를 넣는다**: 검증 종료 후 `alb-target-5xx`가 OK로 복귀했는지 확인하고,
     Slack 카드에 *"2-6 검증 중 의도적 발생"*을 남긴다.
     ℹ️ **ALB 타깃 헬스에는 영향이 없다** — ALB health check path는 `/healthz`다(`alb.tf:30`).
   - ⛔ **완료 조건에 `DB_CONN_MAX_LIFETIME` 되돌림 배포(2-5의 7번)가 포함된다** —
     **판정 성공이든 판정 불가든, 이 단계를 떠날 때 그 env가 남아 있으면 안 된다.**
   - ⚠️ **cleanup 대상은 둘이다**(r21 codex-cli#4 — revision 21은 여기서 *"되돌릴 인프라 상태가 없다"*라고
     적어 실행자가 Terraform 원복을 건너뛸 수 있었다. 그 문장은 **r14에서 통제 창을 뺄 때 쓴 것**이고
     **r15에서 env가 추가된 뒤에도 갱신되지 않았다**):
     1. ⛔ **task definition env `DB_CONN_MAX_LIFETIME`** → **2-5의 7번 경로로 되돌린다. 이건 있다.**
     2. **의도적으로 발생한 알람** → 아래 항목.
   - ℹ️ **되돌릴 *rule* 상태는 없다**(r14에서 통제 창을 뺀 결과 — rule을 끄지 않는다). `/readyz` 반복 호출은
     읽기 전용이고 rule을 끄지 않으므로 **cleanup 대상은 의도적으로 발생한 알람뿐**이다(위 항목).
     revision 13·14가 여기 두었던 `trap`·dead-man 계약은 **통제 창과 함께 삭제**했다 —
     *"`trap`이 모든 경로를 덮는다"*는 **셸이 주지 않는 보장**이었고, 그것을 고치려고 넣은 Scheduler
     dead-man은 **인프라 범위를 Step 1로 번지게 했다**(2번 참조).

### 비용 (r1 codex-cli 개선)

- **Step 1**: Lambda 호출 7일 1회 + **DynamoDB 온디맨드**(회전당 **강한 일관성 읽기 1회 + 조건부 쓰기 1회**,
  중복 이벤트가 오면 읽기와 조건 실패 쓰기가 그만큼 추가된다 → 그래도 사실상 $0. r7 codex-cli 개선) +
  SQS 2개(EventBridge DLQ · Lambda on-failure) + Logs +
  **알람 7개**(`FailedInvocations` 좁은·광역 rule 2개 · Lambda `Errors` · DLQ depth 2개 ·
  전달 실패 지표 `InvocationsFailedToBeSentToDlq`/`DestinationDeliveryFailures`) + 광역 rule의 CloudWatch Logs.
  **"월 $0.2 미만"은 부정확하다**(r2 codex-cli / codex-ide 개선) — `infra/prod`에 이미 표준 알람이 13개 있어
  CloudWatch 무료 10 alarm-metric 한도를 넘겼을 가능성이 높다. ap-northeast-2 표준 알람 **7개**로
  **약 $0.70/월**이고 여기에 SQS·Lambda·Logs(광역 rule 적재분 포함) 소액이 붙는다
  → **약 $0.8/월 수준, 현재 Free Tier 사용량에 따라 변동.**
- **Step 2**: Secrets Manager `GetSecretValue`는 캐시 + 실패 시에만 호출하므로 회전당 수 회.
  시크릿 보관료는 이미 RDS 관리형으로 발생 중이라 증분 없음. AWS SDK 의존성이 이미지 크기를 늘린다.
- 둘 다 **인스턴스 크기 변경 불요.**

### ADR

Step 1 완료 시 `/docs/adr/`에 남긴다: 왜 `Secret Label Updated`인가(vs CloudTrail `RotationSucceeded`),
왜 Lambda가 필요한가(event rule에 `UpdateService` 타깃 없음), **IAM 4분할 경계**, DLQ·멱등 방침.
**revision 3~11의 새 결정도 함께 남긴다**(r3 claude-ide#6): Python 3.13 런타임 선택 이유와 그 트레이드오프
(관리형 런타임의 **boto3 버전은 AWS가 고정해 주지 않는다** — handler가 쓰는 API는
**`update_service`·`describe_secret`·`get_item`·`put_item` 넷**이고 전부 오래된 안정 API라 실질 위험은 낮다.
r8 codex-ide 개선 / claude-ide#5 — revision 8까지 이 목록이 *"`describe_services`/`update_service`"*여서
**제거된 API를 세고 DynamoDB·Secrets Manager 클라이언트를 빼먹은 상태**였다. 목록이 위험 평가의 유일한
근거라 목록이 틀리면 평가도 틀린다),
**botocore `retries` 고정과 단계별 monotonic deadline**, **ECS 대상을 환경변수로 주입한 이유**,
**Step 2에서 refresh를 요청 context에서 분리한 이유**(`/readyz`의 1초가 Route53 계약이라 늘릴 수 없다),
**context 소유권 표**(caller vs `refreshCtx`), **여섯 시각 시계**(`T_set`/`T_fail`/`T_label`/`T_avail`/
`T_visible`/`T_ready` — 특히 **`T_avail`(조회 가능)과 `T_visible`(취득)의 분리**)**와
"회전 중 창·전파 창은 어느 쪽도 못 받는다"는 사실**,
**시각 비교 없이 Step 2 복구를 판정하는 근거 = "회전 전에 기록한 task ID 집합"**
(⚠️ *"새 태스크는 세대 전이 로그를 못 남긴다"*가 **아니다** — 전파 지연으로 옛 값을 seed한 새 태스크는
이후 refresh에서 세대 전이를 만들 수 있다. r15 codex-cli 개선으로 근거를 task ID 경계로 좁혔고,
r19 codex-cli#2로 이 ADR 항목까지 전파했다), 2계층 실패 보존, **멱등을 DynamoDB 조건부 쓰기(키=`versionId`, `UpdateService` 성공 뒤 기록)로 한 근거**
(시각 비교가 왜 성립하지 않는지 + **fail-open vs fail-closed 선택과 at-least-once 계약**,
**선행 `GetItem`(순차 중복 억제)과 조건부 `PutItem`(marker 원자성)의 역할 분리**,
**재배포 제출 판정을 `UpdateService` 응답으로 끝내 `ecs:DescribeServices`를 뺀 근거**,
**marker가 "제출됨"이지 "복구됨"이 아니라는 것과 그 잔여 위험** 포함),
광역 관찰 rule의 존재 이유, `skip_destroy` 택일 근거.

## 리스크/롤백

| 리스크 | 완화 | 롤백 |
| --- | --- | --- |
| **함수 `timeout` 기본 3초로 bounded wait가 매번 강제 종료** | `timeout = 120`·`memory_size = 256` 확정. 1-3에서 `get-function-configuration`으로 확인 | HCL 수정 후 재apply |
| **계정 동시성 한도 부족으로 `reserved_concurrent_executions` apply 실패** | **marker 유일성은 조건부 쓰기가 보장하고**, 예약 미설정 시 **동시 중복 재배포는 at-least-once 잔여 위험으로 수용**한다(r8 codex-ide#3 — *"원자성을 DynamoDB가 담보"*는 end-to-end 재배포까지 포함하는 것처럼 읽혔다). 착수 전 `get-account-settings` 확인, 부족하면 **예약 없이 진행** | 예약 제거 후 재apply |
| **bounded wait 통과 후에도 ECS가 옛 값을 읽음** | **heuristic이지 폐쇄가 아니다.** `DescribeSecret`이 본 것과 ECS execution role의 읽기는 다른 호출이다. 잔여 구간은 **탐지 → 수동 브릿지**가 받는다 | 수동 브릿지 |
| **재배포는 제출됐지만 rollout이 `FAILED`로 롤백 → marker가 남아 같은 `versionId`의 자동 재시도가 없다** | ⚠️ **완화하지 않고 감수한다**(r8 codex-cli#1). handler는 rollout을 추적하지 않는다 — 롤링은 수 분이고 함수 예산은 105초라 원리적으로 불가능하다. 이 실패는 **ECS 배포 서킷브레이커 자동 롤백(`ecs.tf:93-96`) + 기존 ALB/ECS 알람 + plan 0008 탐지(canary `/readyz`)**가 받는다. `outcome`을 **`redeploy_submitted`**로 명명해 *"제출됨 ≠ 복구됨"*을 로그에서도 구분한다. 1-2 (i)로 고정 | **수동 브릿지**(자동 재시도 없음) |
| **EventBridge target retry가 기본 24시간·185회라 다음 회전 세대에 옛 이벤트가 도착** | target `retry_policy`에 **2회/3600초 명시**(r8 codex-ide#2). 두 계층 상한이 합성돼 멱등 창 end-to-end 상한이 약 2시간으로 확정된다(r10 codex-cli 개선). 1-3 `list-targets-by-rule`로 확인 | 값 수정 후 재apply |
| **SDK 재시도가 구간 예산을 넘겨 handler가 120초에 강제 종료(로그 없음)** | botocore `retries.total_max_attempts` 고정 + **단계별 monotonic deadline** + SDK 호출 진입 전 `min(단계 잔여, 함수 잔여-15초)` 검사(r8 codex-cli#2 / codex-ide#1). 1-2 (h)②로 검증 | `timeout`·시도 수 조정 후 재apply |
| **2-6에서야 앱 로그 필드 부재가 드러나 게이트 배포를 다시 돈다** | **로그 다섯 개를 전부 2-1 구현 항목으로** 올리고 검증 ⑩⑫⑮로 고정한다: `task_id`·refresh 성공(r8 claude-ide#1) · **`credential_recovered`**(최종 성공 증거, r11 codex-ide#2) · **`auth_failed_observed`**(`T_fail`, r12 codex-cli#4/claude-ide#3) · `secret_provider_mode`(r11 claude-ide#4). **2-6은 소비만 한다** | 코드 수정 후 CI 재배포(창 1회 손실) |
| **이벤트 전제(회전 1회 = 매칭 1개, `versionId`=라벨 획득 버전)가 어긋나 정상 회전마다 오탐 알람** | ⛔ **완화가 약해졌다 — (a) 확정으로 전제 검증이 라이브 전환 뒤로 갔다**(승인 게이트 2026-09-04). 남은 근거는 **AWS 공식 이벤트 계약**(`AWSPENDING`·`AWSPREVIOUS`에는 발행 안 함, `detail.versionId`는 라벨 획득 버전)이고, **1-6 드릴이 진짜 회전을 일으켜 원문을 확실히 준다.** 어긋나면 그때 1-1 패턴과 handler 라벨 확인 계약을 고친다 | 패턴·계약 수정 후 재apply(재검토 라운드 1회) |
| **순차 중복 이벤트가 재배포를 두 번 건다** | **부작용 전 `GetItem`(`ConsistentRead=true`)이 marker를 보면 `UpdateService`를 부르지 않는다.** 조건부 `PutItem`은 marker 원자성만 담당한다. 계약은 **at-least-once** — 기록 전 실패 시 롤링이 한 번 더 도는 것을 감수한다. 1-2 (b)로 검증 | 서킷브레이커 + 수동 브릿지 |
| **event pattern 문법 오류로 rule이 영구 미매칭** | 패턴은 `labelUpdated = ["AWSCURRENT"]`(**배열**), 이벤트 본문은 문자열. 1-2 fixture로 양쪽을 구분해 검증 | 광역 rule 원문으로 패턴 수정 후 재apply |
| **rule 미매칭이 무증상** | 광역 관찰 rule(Logs 타깃) 병설 + **log group resource policy**(role_arn 아님) + 광역 rule `FailedInvocations` 알람 | 원문으로 패턴 수정 |
| **전파 지연으로 새 태스크가 옛 `AWSCURRENT`를 주입받아 실패** | `UpdateService` 전에 이벤트 `versionId`가 `AWSCURRENT`에 붙었는지 **bounded wait**(≤60초). 상한 초과 시 오류 반환 → 재시도 | 수동 브릿지 |
| **동시 Lambda 실행이 각각 재배포(동시 `GetItem` miss)** | ⚠️ **완화하지 않고 감수한다.** 선행 조회는 강한 일관성이어도 원자적이지 않고 `UpdateService`에 `clientToken`이 없어 어떤 설계로도 이 창은 남는다. 조건부 `PutItem`이 **marker를 하나로 만들고**, `reserved_concurrent_executions = 1`이 defense-in-depth로 좁힌다. 대가는 **롤링 한 번**이다. 1-2 (e)로 검증 | 서킷브레이커 + 수동 브릿지 |
| **`UpdateService` 실패 후 재시도가 skip돼 자동 복구가 영구 봉인** | **기록을 `UpdateService` 성공 관측 뒤로**(fail-open). 기록 전 실패는 재시도가 재배포를 다시 건다. 1-2 (g)로 검증 | 수동 브릿지 |
| **Lambda 실행 실패가 조용히 사라짐** | 2계층 분리(EventBridge target DLQ / Lambda on-failure destination) + DLQ depth 2개 + **전달 실패 지표 2개** 알람. depth만으로는 "DLQ 전송 자체 실패"를 못 본다 | 알람 카드 → 수동 브릿지 |
| **회전 전 시작된 배포가 진행 중이라 skip → 옛 비밀번호 잔존** | 라벨 확인을 **선행 조건**으로 둬 확인 전에는 `UpdateService`를 호출하지 않는다 → 이 Lambda가 만든 deployment는 항상 자격증명 가용 이후다 | 수동 브릿지 |
| **`handler.py` 수정이 배포되지 않음** | `source_code_hash = archive_file.output_base64sha256` 필수. `hashicorp/archive` provider 선언 + lock 갱신 | — |
| **Step 1 없이 드릴을 돌려 장애를 하나 더 만듦** | **1-3(apply) 완료를 1-6의 선행 조건으로 못박음.** 드릴은 진짜 회전을 일으킨다 | 수동 브릿지(사전 무장 상태) |
| **드릴이 회전 스케줄을 밀어 일정 전제가 깨짐** | **예상 동작으로 확정 기재.** 1-7 대상 창을 `NextRotationDate` 재계산으로 정의하고 기한·우발 계획·런북 `(e)`를 연동 | — |
| **Step 2가 DB 연결 경로를 깨 전면 장애** | 2-0 문서 리뷰 → 2-4 통합 테스트를 **배포 게이트**로. **Step 1은 이 경우의 백스톱이 아니다** — 같은 이미지를 다시 띄울 뿐이고 서킷브레이커 `rollback`도 같은 revision이면 무동작(`ecs.tf:93-96`) | **이전 이미지 태그로 CI 재배포**(`workflow_dispatch`) — 유일한 백스톱 |
| **`Connect` 무한 루프가 connectionOpener를 점유** | 호출별 dial/SDK timeout으로 **`Connect` 1회는 유한하게 실패**. backoff는 provider 전역 상태로 들고 다음 호출이 재시도 | provider를 환경변수 고정으로 되돌림 |
| **회전 중 옛 `AWSCURRENT` 창에서 재조회 폭주** | 값 동일 refresh는 세대를 올리지 않고 backoff만 증가. 2-1 ⑥⑦로 goroutine 누수·폭주 없음을 테스트 | 위와 동일 |
| **task definition replacement로 이전 revision이 INACTIVE → 롤백 불가** | **`skip_destroy = true` 채택 확정.** ① `skip_destroy`만 apply(성공 기준 = **replacement 없이 `false/null → true` in-place**, `-/+`가 뜨면 **중단**) → ② env 변경 apply | 직전 ACTIVE revision을 다시 가리킴 |
| **Step 2 회복 판정 시 신규 연결이 안 생기거나 한 태스크만 관측돼 판정 불가** | **[사람]이 `/readyz`를 1초 간격으로 반복 호출**해 두 `task_id`가 모두 나올 때까지 확인(r9 codex-cli#4 — 풀 교체·canary 도달은 API가 주는 보장이 아니다). **대기 상한 10분**(5분 마진 0 금지). ⚠️ **그런데 10분이 실제로 필요한 회차는 판정할 수 없다** — 관측 창(약 2~3분)이 그보다 짧다(r15 3인 전원). → **검증 기간에만 `DB_CONN_MAX_LIFETIME=30s`**로 `T_fail`을 설계로 만든다 | 사유 기록 후 다음 온디맨드 시도에서 재시도. **2창 연속 판정 불가면 별도 라운드** |
| **검증 절차가 Step 1 rule을 꺼서, 이 plan이 막으려는 장애를 스스로 만든다** | ⛔ **통제 창을 범위에서 뺐다**(r14 3인 전원 5건). `trap`은 `SIGKILL`·호스트 소실을 못 덮고, 그것을 고치려던 Scheduler dead-man은 **`get-schedule` 성공이 `EnableRule` 성공을 뜻하지 않아** 같은 상황에서 조용히 실패할 수 있으며 **인프라 범위를 Step 1로 번지게 했다.** → **rule을 끄지 않는다.** ①의 세대 순서 증거가 Step 1이 켜져 있어도 오염되지 않으므로 필요하지 않다 | — (되돌릴 상태 없음) |
| **Lambda가 `cluster`·`service`를 몰라 `ServiceNotFoundException`(또는 `default` 클러스터 호출)** | **Terraform이 `ECS_CLUSTER_ARN`·`ECS_SERVICE_NAME`·`IDEMPOTENCY_TABLE_NAME`·`SECRET_ARN`을 환경변수로 주입**하고 handler가 기동 시 **fail-fast**(r9 codex-cli#1). 1-2 (j)로 요청 파라미터 검증, 1-3에서 `Environment.Variables` 확인 | 환경변수 수정 후 재apply |
| **광역 관찰 rule이 target 없이 apply돼 원문을 한 건도 못 받음** | 광역 관찰 범위를 **다섯 리소스**(rule + **Logs target** + log group + resource policy + `FailedInvocations` 알람)로 확정(r9 codex-ide#1). ✅ **(a) 확정으로 1-1이 함께 만든다.** apply 후 **`list-targets-by-rule`로 target ARN 단언** | target 추가 후 재apply |
| **광역 Logs resource policy가 좁아 첫 실이벤트가 `FailedInvocations`로 끝남** | principal **둘**(`events.amazonaws.com`·`delivery.logs.amazonaws.com`) + action 둘 + resource를 **stream 범위(`:*`)까지**(r9 codex-ide#2, AWS 공식 예제 형태). 검증에서 값을 단언 | policy 수정 후 재apply |
| **알람 7개에 `alarm_actions`가 없어 상태만 바뀌고 사람에게 안 감** | 7개 전부 `alarm_actions`/`ok_actions = [aws_sns_topic.alarms.arn]`(기존 관례)(r9 codex-ide#3). 1-3에서 `describe-alarms`의 `ActionsEnabled`·`AlarmActions`·dimension까지 확인 | action 추가 후 재apply |
| **원문이 안 와서 1-3이 멈추고 Step 1 라이브 기한이 조건부가 됨** | ✅ **(a) 확정으로 이 경로가 사라졌다** — 1-3 앞에 원문을 기다리는 단계가 없다. 원문 확인과 패턴 대조는 **1-6이 승계**하고, 기한은 **표의 절대일자**를 유지한다(날짜를 여기 박지 않는다 — 회전마다 낡는다) | — |
| **회전 중 창(`T_set`→`T_label`)과 전파 창(`T_label`→`T_avail`)에는 Step 1도 Step 2도 회복시키지 못한다** | ⚠️ **완화하지 않고 감수한다**(r10 codex-cli#1 / codex-ide#1 + **r11 codex-ide#1로 전파 구간을 추가**). 두 구간 모두 provider가 받는 값이 옛 값이라 **재조회로 얻을 새 값이 없다.** Step 1의 라벨 확인도 같은 이벤트를 기다린다. Secrets Manager는 변경이 모든 endpoint에 즉시 보인다고 보장하지 않으므로 **`T_label` 직후에도 옛 값이 올 수 있다.** provider는 backoff(≤5초)로 계속 재시도해 **`T_avail` 이후 자력으로 통과한다**(`T_avail`→`T_visible` 기대 예산
약 17초 → `T_ready`). ⚠️ **`T_avail`→`T_visible`은 감수 구간이 아니다**(r13 claude-ide#2) — Step 2가
게이트+probe+refresh로 **스스로 지나가는** 구간이라 여기 포함하면 감수 범위를 그만큼 과대하게 적는다. 0008 `:472`가 이미 *"무중단 보장 안 됨"*으로 승계한 항목이다 | 수동 브릿지 |
| **Step 2가 정상인데도 회전마다 `alb-target-5xx`가 떠 오탐으로 읽힘** | **런북 문장을 시점별로 나눈다**(r10 claude-ide#2 + **r11 claude-ide#3**): **1-5**(Step 2 미배포 기간)에는 *"회전 5xx는 전부 브릿지 대상"*, **2-5**(Step 2 라이브 이후)에 *"짧은 5xx는 정상"* + 판별 기준(지속 시간·`/readyz` 회복 여부)으로 **교체**한다. 2-6 체크리스트에 **조건부 알람 정리 단계**. ⚠️ **발화는 "반드시"가 아니라 "할 수 있다"**(r11 codex-cli 개선 / codex-ide 개선 — 10초는 상한, 33건/분은 평균). 임계 조정은 2-4 실측을 근거로 **2-5에서 판단**. ALB 타깃 헬스는 `/healthz`라 영향 없음(`alb.tf:30`) | 임계·평가 기간 조정 |
| **production에 `DB_SECRET_ARN`이 없어 정적 모드로 조용히 퇴행 — smoke는 통과** | `APP_ENV=production && DB_SECRET_ARN==""` → **기동 오류**(r10 codex-ide#3, `config.go`의 기존 DSN fail-fast와 같은 자리·같은 형태). 2-5에 **CI 배포 전 task definition env 확인 게이트** | env 추가 후 재apply → CI 재배포 |
| **`/readyz` 1초 context 안에서 refresh가 끝나지 않아 무트래픽 회복이 영영 안 됨** | **refresh를 detached context(10초 상한)에서 단일 비행으로 돌리고 결과를 다음 `Connect`가 승계**(r9 codex-cli#2). `/readyz`의 1초는 Route53 계약이라 늘리지 않는다. 2-1 ⑪·2-4 게이트로 검증 | 상시 refresher 도입 재검토 |
| **2-6이 refresh 로그를 복구 증거로 써 Step 1의 재배포를 Step 2 성공으로 오판(false positive)** | **`credential_recovered` 로그**(새 세대로 dial/ping 성공, 세대당 1회)를 2-1에서 구현하고 **2-6의 T1·두 `task_id` 조건을 그 로그로** 한다(r11 codex-ide#2). 2-1 검증 ⑫가 *"refresh는 성공, 새 세대 dial은 실패"* 경로를 고정한다 | 판정 불가로 종료 |
| **CI가 provider 코드 없는 옛 이미지를 배포해도 smoke가 통과** | 2-5의 task definition env 게이트는 **이미지 내용을 보지 않는다**(r11 claude-ide#4). **기동 시 `secret_provider_mode=secret|static` 구조화 로그 한 줄**을 2-1에서 남기고 **2-5 smoke에서 확인**한다. `static`이거나 줄이 없으면 중단 | 올바른 이미지 태그로 CI 재배포 |
| **SDK 리전이 어디서도 오지 않아 프로덕션 `GetSecretValue`가 전량 실패** | **실측 확인**: `ecs.tf`에 리전 env가 없고 저장소에 `WithRegion`도 없다. **ECS Fargate는 Lambda와 달리 자동 주입하지 않고 SDK v2에는 기본 리전이 없다**(r12 codex-ide#1). → **`DB_SECRET_ARN` 파싱 리전을 `config.WithRegion`에 명시하는 것이 SDK를 동작시키는 값**이다(r14 codex-cli#4로 계약 확정). `AWS_REGION` env는 **SDK용이 아니라 배포 시점 교차검사**이고, 빈 값·불일치는 **의도적 설정 fail-fast**다 | ARN 형식 검증 + 기동 오류 |
| **EventBridge DLQ queue policy에 source 제한이 없어 계정 밖 EventBridge가 쓸 수 있다(confused deputy)** | Lambda 리소스 정책은 rule ARN으로 제한하는데(`:143`) **DLQ만 빠져 있었다**(r12 codex-cli#3). queue ARN 한정 `Resource` + **`ArnEquals aws:SourceArn = <좁은 rule ARN>`**(+ `aws:SourceAccount`). 1-3에서 `get-queue-attributes`의 `Policy` 단언 | policy 수정 후 재apply |
| **2-6이 `deployments[].createdAt`(제출 시각)을 Step 1의 복구 시각으로 써 판정이 반대로 뒤집힘** | **시각 경합을 판정식에서 뺀다**(r12 codex-ide#2 / claude-ide#2). **"회전 전에 기록한 task ID 집합"**을 경계로 삼아 *"그 집합의 task_id에서 그 로그가 났다"* 자체를 증거로 한다(⚠️ *"새 태스크는 못 남긴다"*가 아니다 — 옛 값을 seed한 새 태스크는 남길 수 있다. r19 codex-cli#2). `createdAt`은 **Step 1 개입 여부의 보조 지표**로만. 1-7의 *"제출됨 ≠ 복구됨"* 문장과 통일 | 판정 불가로 종료 |
| **2-6이 판정 시점에 `list-tasks`를 실행해 교집합이 항상 공집합 → 구조적 판정 불가** | **회전 *전*에 기준점 셋(task ID·ENI IP·watermark)을 기록하는 (0) 단계**(r12 claude-ide#4) | 사유 기록 후 다음 온디맨드 시도에서 재시도 |
| **관측 창(약 2~3분)이 `T_fail` 도래(최대 커넥션 수명 5분)보다 짧아 증거가 아예 생기지 않는다** | ⛔ **②를 뺀 뒤 남은 유일한 실전 검증이 수렴하지 않을 수 있다**(r15 3인 전원 [높음]). ALB 헬스체크가 **`/healthz` liveness**라 새 태스크가 빨리 헬시해지고 옛 태스크도 빨리 빠진다(`alb.tf:27-35`). `/readyz` 반복은 **idle 커넥션을 재사용**할 뿐 신규 연결을 강제하지 않는다. → **검증 기간에만 `DB_CONN_MAX_LIFETIME=30s`**(앱 파라미터라 인프라가 번지지 않는다). 끝나면 원값으로 되돌리는 배포가 2-6 완료 조건 | **2-6은 차단 게이트가 아니다**(2-4가 게이트). 2창 연속 실패면 별도 라운드 |
| **`T_fail`을 남기는 로그가 없어 다운타임을 계산할 수 없다** | **`auth_failed_observed`(세대당 1회, `task_id` 포함)를 2-1에 추가**(r12 codex-cli#4 / claude-ide#3) | — |
| **로그 부재를 "Step 2 무결"로 오독** | ⛔ **부재는 아무것도 구분하지 않는다**(r13 codex-ide#3) — `28P01` 분류·래핑이나 로그 발행 코드가 깨져도 똑같이 로그가 없다. **둘 다 없으면 판정 불가**로 두고, *"`T_fail` 미도래"*로 기록하려면 **`/readyz` 503·`alb-target-5xx`가 조용했다는 독립 신호**가 있어야 한다 | 판정 불가로 종료 |
| **이전 회전이 남긴 `credential_recovered`가 이번 회복 증거로 재사용됨(false positive)** | 태스크가 회전 창(7일)보다 오래 사는 것은 정상이라 `task_id`만으로는 갈리지 않는다(r13 codex-cli#2). **(0)에서 태스크별 로그 watermark를 함께 기록**하고 그 이후의 로그만 보며, `auth_failed_observed(N) → refresh N→N+1 → credential_recovered(N+1)` **순서와 세대 연속성**까지 성립해야 성공. ⚠️ **`T_label` 기준으로 자르지 않는다** — `T_fail`이 그보다 앞설 수 있다 | ⛔ **순서·세대가 어긋나면 확정 결함 → 즉시 이전 이미지로 CI 재배포**(r21 2인). 로그 미생성만 판정 불가 |
| **관측 창이 Step 1 롤링의 드레인으로 조기에 닫혀 Step 2와 무관하게 판정 불가** | 창은 **(0)의 대상이 ALB에서 `draining`/미등록이 되는 순간** 끝난다 — **`list-tasks` 소멸보다 이르다**(r13 claude-ide#3 → **r14 codex-ide#4로 정정**. ALB는 deregistration 즉시 라우팅을 멈추므로 태스크가 `RUNNING`이어도 자극이 안 닿는다). (0)에 **ENI IP 기록** 추가, `describe-target-health`로 판정. 원인을 *"자극이 닿지 못했다"*는 **세 번째 분기**로 기록 | 다음 온디맨드 시도에서 ① 재시도 |
| **Step 1만 하고 Step 2를 무기한 미룸** | **이것이 이번 재발의 실제 원인이다.** 착수 기한을 **1-6 드릴 완료 후 7일 이내**로 절대일자화(회고 A-11). 1-7을 게이트로 두지 않는다 | — |

## 검토 반영 로그

<!-- /plan-merge가 라운드별로 기록. 형식: [rN] 리뷰어#번호 지적요약 → 반영|기각 — 사유 -->

### revision 23 (2026-09-09) — 구현·실측이 plan을 정정한다

⚠️ **이 revision은 plan 리뷰 라운드가 아니라 `code-review/round-1·2`와 1-3·1-6 실측에서 왔다.**
plan이 코드를 고치는 방향이 아니라 **코드가 plan의 사실 오류를 드러낸 방향**이다.

| # | 정정 | 출처 | 파급 대상(sweep 목록) |
| --- | --- | --- | --- |
| 1 | **실행 예산표 행 1·4를 `예산 6 → 8`, 여유 `13 → 9`**(합계 105 유지) | code-review r1 codex-cli#1 / claude-ide#1 [높음] | 예산표 · `timeout = 120` 근거 문장 · 리스크표의 SDK 재시도 행 |
| 2 | **`outcome` 표에 `failed_validation` 추가** | 구현 중 발견(r1 claude-ide 개선으로 지적) | `outcome` 표 · 1-7 사후 증거 절 |
| 3 | **1-3 검증의 `describe-resource-policies`에 condition 2개 단언 추가** | code-review r1 codex-ide#2 [중간] | 1-3 검증 목록 · 1-1 광역 rule 계약 |

⛔ **#1이 가장 중요하다 — plan 쪽 숫자가 먼저 깨져 있었다.**
`예산 = 최악`(둘 다 6초)은 단발 구간에서 **진입 게이트가 구조적으로 항상 닫히는** 값이다.
`deadline = monotonic() + 예산`을 잡은 다음 문장에서 시계를 다시 읽으면 `잔여 = 예산 - ε`이므로
`잔여 < 최악`이 항상 참이 된다. 구현자가 이 표를 충실히 옮긴 결과 **정상 이벤트도 AWS 호출 전에
`failed_deadline`으로 끝나 재배포가 0회**가 됐고, 정지 시계 fake가 단위 테스트 19개 전부에서 이를 가렸다.
실시계 재현 10/10 차단 → 수정 후 2,000회 차단 0회.
**행 3이 이미 `예산 = 최악 + 2`였다** — 행 1·4만 그 패턴에서 이탈해 있었다.
→ **불변식 `예산 > 최악`을 표 위에 명시했다.**

ℹ️ **함께 적어 둔 것**: 합계 105는 **장부 숫자**이고 코드가 강제하는 전역 deadline은 없다
(강제 장치는 `RESERVE_SECONDS`와 단계별 deadline). 단발 구간에서 `stage` 항은 `예산 > 최악`인 한
바인딩되지 않는다 — 다음 사람이 여유 행을 더 깎거나 *"단계 게이트가 GetItem/PutItem을 지켜 준다"*고
읽는 것을 막는다.

✅ **Step 1은 이 revision 시점에 1-1~1-6 완료·라이브다**(1-6 드릴 2026-09-09: 회전 → 재배포 제출
1분 15초, 롤아웃 완료 **4분 10초**, 사람 개입 0). 이벤트 계약도 실이벤트로 확인됐다 —
`detail.labelUpdated`는 **본문 문자열**(패턴은 배열), `detail.versionId`는 라벨을 얻은 새 버전,
**회전 1회 = 매칭 이벤트 1건**. 광역 rule 로그와 Lambda 로그의 `event_id`가 일치해
**rule → Lambda 배선까지 증명**됐다. → `1-1a` 절이 (a) 채택의 대가로 남겨 둔
*"전제 검증이 라이브 전환 뒤로 간다"*는 **해소됐다.**

**[r21] revision 21 → 22** (3인 전원 제출, 전부 `reviewed-revision: 21`, 전부 `request-changes`.
**반영 17 · 기각 0**. ⚠️ **결함 11건 중 8건이 "09-07 (ii) 수행"이라는 사실 하나의 파급**이고,
**그 편집에 로그를 남기지 않은 것이 sweep이 돌지 않은 직접 원인**이다 — claude-ide의 진단이 정확하다:
*"**로그를 쓰는 행위가 sweep의 트리거였는데 그것을 건너뛰었다.** 결함 2를 먼저 닫으면 1이 따라온다."*
Step 1 본체(handler 계약·IAM 4분할·예산 표·알람 표)는 **아홉 라운드 연속 무수정**):

*먼저 — 누락된 병합 기록을 소급해 남긴다*

**[창 이동] revision 20 → 21** (2026-09-07. 리뷰 라운드가 아니다. **r19 claude-ide#3으로 신설한
3선택지 중 (ii)를 실제로 수행**했고 그 결과를 반영했다):
- **온디맨드 회전 2026-09-07 10:06:07** — `canary_down` ALARM 10:14:41 → OK 10:18:41,
  **통제된 다운 12분 34초.** `NextRotationDate` **09-14 → 09-15 08:59:59**,
  다음 창 **09-13 → 09-14 09:00**. Step 1 라이브 기한도 같이 이동.
- ✅ **부수 수확**: Slack 푸시·SMS가 **ALARM·OK 양방향으로 전부 폰 도착** → **4차 회고 B-4 종결**
  (*"알림 미도달"*이 아니라 *"도달 후 미대응"*). **알림 경로로는 더 닫을 것이 없다.**
- ⛔ **자기 값 둘을 실측으로 정정**: *"+7일 밀린다"* → **"마지막 회전 + 7일"이라 직전 회전으로부터
  지난 기간에 달렸다**(이번은 +1일) / *"약 6분"* → **12분 34초**(회전 완료 대기가 빠져 있었다).
- ⚠️ **이 항목을 그때 쓰지 않은 것이 r21 결함 8건을 만들었다.**

*결함*

- [r21] claude-ide#1 [높음] / codex-ide#3 [높음] / codex-cli#2 [중간] [3인 전원] — **09-13 → 09-14 변경이 여섯 곳에 도달하지 않았고, 그중 하나는 "상단 기한 표와 같은 값"이라고 단언하면서 다른 값을 적는다**(`:390`). 나머지: `:83`·`:148`·`:897`(인용한 `NextRotationDate`가 **두 번 낡았다**)·`:931`(적용 기록 표가 **오지 않는 창**을 대상으로 두고 **09-07 수행 자체가 표에 없다**)·`:1936`·3선택지 (i) 행 → **반영** — **r13#1·r14#1·r19#1과 같은 형태이고 이번 대상은 이 문서에서 하중이 가장 큰 날짜다.** 리뷰어 처방대로 **날짜를 박지 않고 "기한 표 참조"**로 바꿨다(1-7은 *"1-6 직후 `describe-secret` 재계산"* 절차를 참조하게 했다 — claude-ide 개선). 적용 기록 표에 **09-13 행을 "(ii)로 없앤 창"**으로 고치고 **09-14 행을 신설**했다.
- [r21] claude-ide#2 [중간] / codex-cli 개선 / codex-ide 개선 [3인 전원] — **`revision: 21`인데 검토 로그의 마지막이 `[r19] 19 → 20`이고 20 → 21 항목이 없다.** 이 plan은 비-리뷰 병합도 전부 기록해 왔는데(`[게이트]`·`[ⓓ]`·`[4차 장애]`) **이번만 빠졌다.** 서식 문제가 아니라 **프로토콜의 부품**이다 — `reviewed-revision` 대조가 라운드 동기화 키인데 번호가 로그 없이 움직였다 → **반영** — ⚠️ **이것이 이번 라운드의 근본 원인이다.** claude-ide가 짚은 대로 **로그를 쓰는 과정이 파급 sweep의 트리거**인데, 그것을 건너뛰어 **sweep이 돌 계기 자체가 없었다.** 위에 `[창 이동] revision 20 → 21`을 **다른 비-리뷰 병합과 같은 형식으로 소급 작성**했고, 쓰면서 실제로 여섯 곳이 드러났다.
- [r21] codex-cli#1 [높음] / codex-ide#1 [높음] — **2-6에서 세대·순서 불일치의 종료 경로가 서로 반대다.** r19에서 신설한 종료 표는 *"확정된 Step 2 결함 → 즉시 이전 이미지로 롤백"*인데, **앞쪽 판정 설명·최종 성공 체크리스트·리스크표는 같은 경우를 "판정 불가"**라고 한다. 2-6이 비차단이므로 **후자를 따르면 깨진 이미지가 다음 회전까지 라이브에 남고**, *"Step 1은 Step 2 코드 결함의 백스톱이 아니다"*라는 계약도 다시 위반된다 → **반영** — **r19에서 종료 표를 신설하면서 기존 판정식 문장들을 갱신하지 않았다**(위 결함 2와 같은 뿌리). **종료 표를 단일 기준으로** 선언하고 세 곳(판정 문단·최종 체크리스트·리스크표)을 **"순서·세대가 어긋나면 확정 결함 → 즉시 롤백. 로그 미생성만 판정 불가"**로 통일했다.
- [r21] codex-ide#2 [높음] — **회전 트리거를 (가) 사람 입회 온디맨드로 확정했는데, 모든 판정 불가 재시도 경로는 다시 "다음 자연 회전"을 지시해 실행 불가능한 절차가 됐다.** plan 자신이 *"자연 창에서는 (0) 기준점과 2~3분 관측 창을 포착할 수 없다"*고 판단해 (가)를 채택한 것이다 → **반영** — **(가) 채택의 파급이 재시도 경로 일곱 곳에 가지 않았다.** 본문 4곳·리스크표 5곳을 **"다음 온디맨드 시도"**로 바꾸고, **"2창 연속" → "2회 연속 시도"**로 세는 단위도 고쳤다. *"재시도도 6번 재투입 → 온디맨드 회전 → (0) → 관측의 같은 절차"*를 명시했다.
- [r21] codex-ide#4 [중간] / claude-ide#3 [중간] — **09-07 수행 기록이 "Step 1 rule은 켠 채"라고 적는데 그 rule은 존재하지 않는다.** 게이트 ⓑ 조회가 *"광역·좁은 rule 모두 없음"*이었고 이번 재조회도 같다. **실제로는 Step 1 없이 f-4 수동 브릿지로 12분 34초를 감수한 창 이동**이다. 그리고 **1-6이 *"Step 1 없이 돌리면 장애를 하나 더 만드는 것"*이라고 금지하는 바로 그 행동**인데 plan이 화해시키지 않는다 — 붙어 있는 구분(*"통제 창이 아니다"*)은 **rule을 끄는지에 대한 것**이라 이 충돌을 다루지 않는다. 게다가 **1-6 컷오프 10분 vs (ii) 실측 12분 34초**라 기준을 적용하면 *"실패"*로 읽힌다 → **반영** — **사실 오류였다.** *"Step 1 rule은 켠 채"*는 2-6 트리거 (가)에 대해 쓴 문장인데 **09-07 기록에 그대로 붙였다.** 수행 기록에 **⚠️ Step 1 상태 = 미배포** 행과 **복구 = [사람] 브릿지 (b)** 행을 넣었다. 그리고 **런북 f-4에 이미 있던 구분**(*"f-4는 그 드릴이 아니다"*)을 **1-6 vs (ii) 4행 대조표**로 plan에 옮겼다 — 목적·전제·복구 주체·컷오프. **"(ii)와 (가)는 명령만 같고 전제가 다르다"**도 명시했다((가)는 Step 1 라이브 후). 3선택지 (ii) 행의 전제도 *"없음"* → **"[사람] 입회 + 브릿지 즉시 실행 + 의도적 다운 수용"**으로 고쳤다.
- [r21] codex-cli#3 [중간] — **"새 태스크는 `credential_recovered`를 남기지 않는다"는 거짓 전제가 (0) 기준점 설명에 또 있다.** 판정 문단·ADR·리스크표는 r15·r19에서 *"경계는 회전 전 task ID"*로 고쳤는데 여기만 남았다 → **반영** — ⚠️ **같은 문장이 세 번째로 살아났다**(r15 지적 → r19에서 ADR·리스크표만 → r21에서 (0)). **경계를 "새 태스크의 `task_id`는 (0)의 집합에 없어 애초에 판정 대상이 아니다"**로 바꾸고, 세대 결합은 **"보조 성질, 보장 아님"**으로 격하했다.
- [r21] codex-cli#4 [중간] — **2-6의 `DB_CONN_MAX_LIFETIME` 회수 지시가 바로 다음 문장의 "되돌릴 인프라 상태가 없다"와 모순된다.** 실행자가 Terraform 원복을 건너뛸 수 있다 → **반영** — 그 문장은 **r14에서 통제 창을 뺄 때 쓴 것**이고 **r15에서 env가 추가된 뒤에도 갱신되지 않았다**(또 같은 뿌리). **cleanup 대상을 둘로 명시**했다: ⛔ **task definition env(2-5의 7번 경로)** · 의도적 알람. *"되돌릴 **rule** 상태는 없다"*로 범위를 좁혔다.

*개선 제안*

- [r21] claude-ide 개선 — **실측 이력 표에 09-07 통제 회전을 넣어라. 지금은 네 번의 실패만 있고 한 번의 성공이 없다** → **반영** — 회차 번호 없이 한 행 추가하고, *"**'그때 있던 것' 열이 처음으로 작동한 사례** — 대책이 아니라 **사람이 그 자리에 있었다는 것**이 달랐다"*를 적었다.
- [r21] claude-ide 개선 — **f-4 재사용 조건을 다음 결정에 어떻게 쓰는지까지 적어라** → **반영** — *"09-14 창에 (ii)를 다시 쓰려면 **창 직전(09-13)에 걸어야 +7일에 가깝다.** 09-11에 걸면 +4일밖에 못 번다"*를 넣어 결정이 기계적이 되게 했다.
- [r21] claude-ide 개선 — **1-7의 유도를 값이 아니라 절차로 바꿔라** → **반영** — *"ⓐ 확정: `NextRotationDate = …` → 그다음이 …창"*은 **회전마다 낡는다**(실제로 두 번 낡았다). **1-6이 이미 가진 *"드릴 직후 `describe-secret`으로 재계산"*을 참조**하게 했다.

**[r21] 병합 자기점검**:
- ⛔ **일곱 라운드 연속 같은 형태이고, 이번엔 원인이 특정됐다 — 로그를 안 썼다.** r19 자기점검이 *"'사실만 바꿨다'는 편집도 그 사실을 인용하는 절을 전수로 훑어야 한다"*고 처방했는데, **09-07 편집은 그 처방의 첫 적용 대상이었고 sweep을 돌리지 않았다.** claude-ide가 그 이유를 짚었다 — **로그를 쓰는 행위 자체가 sweep의 트리거**였는데(무엇이 바뀌었는지 적으려면 파급을 세야 한다) **로그를 건너뛰니 트리거가 없었다.** → **처방을 하나로 합친다: `revision`을 올리는 모든 편집은 검토 로그 항목을 먼저 쓰고, 그 항목의 "파급 대상" 목록을 sweep 목록으로 쓴다. 로그 없는 revision 증가를 금지한다.**
- **결함 3·6·7이 전부 "r19에서 새로 만든 것이 기존 문장과 충돌하는데 기존 쪽을 안 고친 것"이다** — 종료 표 vs 판정식, (가) 트리거 vs 재시도 경로, env 회수 vs cleanup 문장. **새 것을 넣을 때 그것이 대체하는 옛 문장을 같이 찾는 습관이 없다.** → **"추가"가 아니라 "교체"로 생각한다: 무엇을 대체하는가를 먼저 적는다.**
- **codex-ide#4는 사실 오류였다** — 존재하지 않는 rule을 *"켠 채"*라고 적었다. 2-6 트리거 (가)에 대해 쓴 문장을 09-07 기록에 복사하면서 **전제가 다른 것을 확인하지 않았다.** 그리고 그 오류가 **1-6의 금지와의 충돌을 가렸다**(claude-ide#3) — *"Step 1이 켜져 있었다"*면 충돌이 없어 보인다. **한 줄의 사실 오류가 규칙 충돌을 숨겼다.**
- **revision 22에서 내가 새로 쓴 것**: `[창 이동] 20 → 21` 소급 로그, 09-13→09-14 여섯 곳(날짜 대신 참조), 종료 경로 3곳 통일, 재시도 경로 9곳, (ii) 수행 기록의 Step 1 상태·복구 주체, 1-6 vs (ii) 4행 대조표, (0) 경계 정정, cleanup 2항목, 실측 이력 09-07 행. **이 아홉이 다음 라운드의 1차 확인 대상이다.** sweep은 `09-13`·`자연 회전`·`남기지 않는다`·`되돌릴 인프라`·`판정 불가`를 **본문·리스크표 분리 전수 출력**으로 확인했다.

**[r19] revision 19 → 20** (3인 전원 제출, 전부 `reviewed-revision: 19`, 전부 `request-changes`.
**반영 17 · 기각 0**. r15 이후 **리뷰 없이 병합이 세 번**(게이트 종료 16→17, ⓓ 17→18, 4차 장애 18→19)
있었고, **이번 결함 10건 중 7건이 그 세 병합의 파급 누락**이다. claude-ide의 정리가 정확하다 —
*"3·4차 장애라는 새 사실이 **기한 표와 검토 로그까지만 도착하고, 그 사실을 근거로 쓰는 절에는 닿지 않았다**."*
Step 1의 handler 계약·IAM 4분할·예산 표·알람 표는 **여덟 라운드 연속 무수정**):

*결함*

- [r19] codex-cli#1 [높음] / codex-ide#1 [높음] — **`DB_CONN_MAX_LIFETIME=30s`의 투입·회수에 Terraform apply 경로가 없다.** 2-5의 6·7번은 *"task definition env를 추가/제거하고 CI 배포"*라고만 적었는데, **배포 workflow는 `describe-task-definition`으로 최신 ACTIVE를 받아 이미지 필드만 교체**하고 저장소의 미적용 HCL을 읽지 않는다 → **30초가 라이브에 안 들어가거나, 실행자가 승인된 plan 밖에서 `register-task-definition`을 설계해 drift를 만든다** → **반영** — **실측 확인했다**(`.github/workflows/deploy.yml:116-135`의 `describe-task-definition` → `render-task-definition`(이미지만) → `deploy-task-definition`, ADR 0001 §결정 1·2와 *"Terraform에서 task definition을 바꾸면 즉시 라이브에 반영되지 않는다 — 반드시 CI 배포를 1회 트리거"*). ⚠️ **같은 문서의 2-2·2-5 1~5번이 이미 `plan → [사람] apply → 최신 ACTIVE 확인 → CI 배포` 규율을 쓰고 있는데 내가 6·7번에만 적용하지 않았다.** 두 단계를 그 형식으로 다시 썼고(HCL 수정 → `fmt`/`validate`/`plan`(`-/+`·`0 destroy`) → [사람] apply → 최신 ACTIVE env 확인 → CI 배포 → **기동 로그 실효값 확인**), **중간 실패 시 복구 절차**(`describe-services`의 `taskDefinition`과 최신 ACTIVE를 대조해 라이브를 먼저 확정 — `ignore_changes` 때문에 apply 재실행으로는 안 옮겨진다)와 **다음 회차 재시도 시 재투입**도 넣었다.
- [r19] codex-ide#2 [높음] — **2-6이 "판정 불가"와 "확정된 Step 2 결함"을 다시 합쳐, 결함을 발견해도 다음 자연 회전까지 깨진 구현을 운영에 남긴다.** 판정 절은 *"`auth_failed_observed` 있는데 `credential_recovered` 없음 → Step 2 결함"*으로 확정하는데, 종료 절은 불확정 사유에 **그것까지 포함**해 셋 다 다음 회전으로 미룬다. 이 문서 자신의 계약(*"Step 2의 코드 결함은 Step 1이 못 받는다 — 이전 이미지가 유일한 백스톱"*)과 충돌한다 → **반영** — **비차단화(r15 claude-ide#3)를 적용하면서 그것이 어디까지 적용되는지를 나누지 않았다.** 종료 경로를 4행 표로 갈랐다: **`T_fail` 미도래·자극 미도달 → 비차단, 다음 회차 재시도** / **세 로그가 시작됐는데 recovery가 없거나 세대·순서가 어긋남 → 확정 결함, 즉시 이전 이미지로 CI 재배포 후 2-4부터 재검증.** *"비차단은 증거가 생성되지 않은 회차에만 적용된다 — Step 2가 라이브인 채 다음 회전을 맞으면 그 회전이 실장애가 된다"*를 명시했다.
- [r19] claude-ide#1 [높음] — **"회전 창과 Step 1 라이브 시점" 절 전체가 2026-09-04 시점 그대로다.** `:931-934`가 *"09-06 창은 브릿지 사전 무장으로 막는다 — 이것이 기한 표의 첫 행이다"*라고 가리키는데 **그 창은 지났고 4차 장애가 났으며, 기한 표 첫 행은 지금 정반대**(*"네 번 연속 실패 — 실패가 기본값"*)를 적는다. **포인터와 대상이 서로 반대말을 한다.** `:947`은 *"다음 창은 9일 뒤"*(오늘 6일)이고, `:915-922`는 *"알람 자체도 미실측"*(두 번 실측됨)·*"깨어 있으면 복구 2분"*(3차는 주간 회전인데 118시간) → **반영** — ⚠️ **가장 아픈 지적은 위치다**: `:947`은 *"판정 기준은 날짜가 아니라 **관계**로 적는다(r13에서 '오늘이 09-03이고 남은 3일'로 적었다가 하루 만에 낡았다)"*고 선언한 **두 줄 아래**인데 **거기서 다시 절대 일수를 박고 또 낡았다** — **처방을 적은 자리에서 처방을 어겼다.** → 절을 **"판단 규칙 → 적용 기록"** 구조로 재작성했다(claude-ide 개선). 규칙을 맨 앞에 올리고(*"남은 작업 > 창까지 남은 기간이면 못 댄다. **라운드 간격 같은 상수를 쓰지 않는다**"*), 08-30·09-06·09-13을 **창별 기록 표**로 내렸다. 탐지 유보는 **3·4차 실측 표로 교체**했고, 구간별 목표 표는 *"기대했던 것 vs 실측"* 2열로 바꿔 **야간 프레이밍이 틀렸다**(가장 긴 장애가 주간 회전)를 적었다. plan 0010 블록도 *"실측 두 건이 대체했다"*로 정리했다.
- [r19] claude-ide#2 [중간] — **실측 이력 절이 "2회 재발"에 멈춰 있다.** 배경/제약에서 **문제의 크기를 규정하는 자리**인데 네 건 중 둘만 있고, 빠진 둘이 가장 중요하다 — 3차는 **1·2차를 합친 것보다 길고**, 4차는 **SMS가 도착한 상태에서도 13시간**이라 *"실패한 것은 복구가 아니라 시작 시점"*이라는 결론을 한 단계 더 강화한다 → **반영** — 표를 **4회로 갱신**하고 *"그때 있던 것"* 열을 추가해 **대책이 쌓이는데도 다운이 줄지 않은 것**이 보이게 했다. *"두 번 모두 2분 12초"* → *"네 번 모두 2~6분"*. 목표 절의 *"약 24시간 → 수 분"*도 **"13시간~118시간(4회 실측 범위) → 수 분"**으로 바꿨다(claude-ide 개선) — **그 폭이 Step 1의 가치를 더 크게 말한다.**
- [r19] claude-ide#3 [중간] — **09-13 창을 못 대면 남는 수단이 "실패가 기본값"이라고 스스로 규정한 절차 하나뿐이고, 저장소에 있는 다른 선택지를 가리키지 않는다.** 우발 계획(*"브릿지를 사전 무장한다"*)과 기한 표 첫 행(*"네 번 연속 실패"*)을 나란히 놓으면 **09-13의 대비책 = 기본값이 실패인 절차 하나**다. 그리고 **09-13까지 6일인데 plan은 아직 `in-review`**이고, `:941`의 관계식을 **오늘 값으로 적용한 문장이 없다** → **반영** — **이번 라운드의 실질이다.** 리뷰어가 짚은 대로 **브릿지 런북 (f-4)(창 이동)가 이미 저장소에 있는데 plan이 가리키지 않았다.** → **09-13 창 대비를 3선택지 표로 신설**하고 **결정 기한을 `2026-09-10 09:00 KST`로 절대일자화**했다: **(i) Step 1 라이브 / (ii) f-4로 창 이동(통제된 다운 약 6분, 오늘도 가능) / (iii) 브릿지 무장만(네 번 진 수단)**. *"(i)과 (ii)는 배타적이지 않다 — (ii)로 창을 밀어 두고 (i)을 진행하는 것이 가장 안전하다"*와 *"결정하지 않으면 자동으로 (iii)이 되고 그것이 A-11이 규정한 형태"*를 적었다.
- [r19] codex-ide#3 [중간] / codex-cli 개선 — **30초는 `T_fail`을 "회전 후 30초 안"으로 결정적으로 만들지 않는다.** `SetConnMaxLifetime`은 **연결의 최대 재사용 수명만 정하고 요청이나 새 연결을 발생시키지 않고**, probe 간격은 평균이며 `T_label→T_avail`에 상한이 없어 새 값이 조회 가능해지기 전에 옛 태스크가 draining될 수 있다. **그리고 2-6이 어떤 회전을 어떻게 관측할지 정하지 않았다** — (0)은 *"회전 전"*에 실행돼야 하는데 자연 창은 24시간이다 → **반영** — **두 지적이 같은 답을 가리킨다.** *"넉넉히 덮는다"*를 **"5분 경합을 30초 경합으로 줄일 뿐"**으로 내리고, **회전 트리거를 (가) [사람]이 지켜보는 시각에 온디맨드 회전으로 확정**했다(Step 1은 **켠 채로** — r14에서 통제 창을 뺀 이유가 유효하다). (나) 자연 창 자동화는 **새 상시 코드·스케줄이 필요해 미채택.** ⚠️ **(가)는 위 claude-ide#3의 (ii)와 같은 명령**이라 두 결함이 한 수단으로 닫힌다.
- [r19] codex-cli#3 [중간] / codex-ide#4 [중간] / claude-ide 개선 — **2026-09-04에 닫힌 승인 게이트가 본문에서는 여전히 현재형이고 "유일한 잔여 블로커"로 남아 있다.** 실행자가 착수 가능 여부에 대해 **정반대 지시**를 받는다. 확정 대상으로 **이미 삭제된 산출물**(1-1a 판정 시각·3분기 폴백)을 지목하기도 한다 → **반영** — 절에 **`#### (기록 — 2026-09-04 닫힘)`** 표제를 붙여 격리하고, *"제공해야 할"* → *"제공해야 했던"*, *"이 값이 오면 확정할 것"* → *"✅ 전부 확정됐다"*(삭제된 산출물은 그렇게 표시), *"택일이 필요하다"* → *"필요했다 — (a)로 확정"*, 그리고 **잔여 블로커 문장을 종료 선언으로 교체**했다. ⓐ 행에도 *"→ 3차 장애 / 09-06에서 4차 / 현재 값은 기한 표"*를 붙여 기록으로 정확해지게 했다(claude-ide 개선).
- [r19] codex-cli#2 [중간] — **r15의 정정("새 태스크도 stale seed를 받으면 세대 전이를 만들 수 있다 → 실제 경계는 회전 전 task ID")이 활성 절 전체에 전파되지 않았다.** 판정 문단은 정확한데 **ADR 산출물과 리스크표는 그 거짓 성질을 판정 근거로 기록하라고 한다** → **반영** — **r15에서 판정 문단만 고치고 ADR·리스크표는 안 봤다**(이번 라운드 결함 7건이 공유하는 형태다). 두 곳 모두 *"새 태스크는 못 남긴다"* → **"경계는 회전 전에 기록한 task ID 집합"**으로 교체하고, ADR 항목에는 **왜 그 표현이 틀렸는지**(전파 지연 seed)까지 적어 다시 되돌아가지 않게 했다.

*개선 제안*

- [r19] codex-ide 개선 — **`DB_CONN_MAX_LIFETIME`의 실효값을 기동 로그에 남기고 2-5에서 확인하라** — 잘못된 값은 **기동 실패가 아니라 `5m` 폴백 + 경고**라 task definition 문자열 확인만으로는 실효값을 증명하지 못한다 → **반영** — `secret_provider_mode`와 같은 줄에 `conn_max_lifetime`을 넣고 2-5의 6·7번이 **`30s`/`5m`**을 각각 확인하게 했다. **"설정했다"와 "적용됐다"를 가르는 같은 규율**이다.
- [r19] codex-ide 개선 — **"남은 6일"은 검토 다음 날부터 낡는다** → **반영** — *"2026-09-13 09:00 KST 전(2026-09-07 기준 6일)"*으로 시점을 붙이고 **절대일자가 기준임을 명시**했다.
- [r19] claude-ide 개선 — **절 제목은 일반화됐는데 내용이 특정 창의 판단 기록이다. 관계식을 절 맨 앞으로 올리고 창별 판단을 아래 기록으로 내려라** → **반영** — claude-ide#1의 처방과 함께 적용했다. **다음 창에서 또 낡지 않는 구조**가 됐다.

**[r19] 병합 자기점검**:
- ⛔ **리뷰 없이 세 번 병합한 것의 대가가 이번 라운드다.** 게이트 종료·ⓓ·4차 장애는 각각 *"사실 기록"*이라 설계 변경이 아니라고 판단했는데, **사실이 바뀌면 그 사실을 근거로 쓰는 절이 전부 낡는다.** 결함 10건 중 7건이 그 형태였고, 그중 **claude-ide#1은 plan의 핵심 정당화 절**이었다. → **"사실만 바꿨다"는 편집도 그 사실을 인용하는 절을 전수로 훑어야 한다.** 설계 변경보다 오히려 파급이 넓다 — 사실은 여러 절이 공유하기 때문이다.
- ⛔ **여섯 라운드 연속 같은 형태이고, 이번엔 처방을 적은 자리에서 어겼다.** `:947`의 *"9일 뒤"*는 **두 줄 위에서 *"날짜가 아니라 관계로 적는다"*고 선언한 바로 그 자리**에 있었다(claude-ide#1). r13에서 같은 실수를 하고 처방을 적었는데, **그 처방 문단 자체를 갱신 대상으로 보지 않았다.** → **처방을 적은 문단은 그 처방의 첫 번째 적용 대상이다.**
- **r15 정정의 전파 누락(codex-cli#2)도 같은 뿌리다** — 판정 문단만 고치고 ADR·리스크표를 안 봤다. **r15 자기점검이 *"sweep은 개수가 아니라 전수 출력으로, 리스크표는 본문과 분리해"*라고 적었는데, 그건 r15에서 바꾼 값에만 적용하고 r15가 **정정한 사실**에는 적용하지 않았다.**
- **잘한 것 하나**: codex-ide#3(회전 트리거)과 claude-ide#3(09-13 대비)이 **같은 수단**(사람이 지켜보는 시각의 온디맨드 회전)을 가리킨다는 것을 병합에서 발견해 하나로 묶었다. 두 리뷰어가 다른 문제를 보고 같은 답에 도달한 것이고, 그것이 **f-4가 이미 저장소에 있는 이유**이기도 하다.
- **revision 20에서 내가 새로 쓴 것**: 2-5 6·7번의 apply 경로, 2-6 종료 경로 4행 표, 회전 트리거 (가)/(나), *"회전 창과 Step 1 라이브 시점"* 절의 규칙/기록 재구성, 실측 이력 4회 표, 09-13 3선택지와 결정 기한(09-10), 게이트 절 격리, 증거 경계 정정 2곳. **이 여덟이 다음 라운드의 1차 확인 대상이다.** sweep은 **본문·리스크표 분리**로 `남기지 않는다`·`못 남긴다`·`2회 재발`·`잔여 블로커`·`9일`·`30초`를 전수 출력으로 확인했다.

**[4차 장애] revision 18 → 19** (2026-09-07. 리뷰 라운드가 아니다 — **09-06 창에서 4차 장애가 실제로
발생해** 기한과 전제를 갱신했다):

- **4차: 회전 2026-09-06 19:09:00 → 복구 09-07 08:19, 13시간 10분.**
  회고: `docs/postmortems/2026-09-06-rds-rotation-outage.md`.
- ✅ **탐지가 두 번 연속 설계값에 부합했다** — `alb-target-5xx` **+5분 37초**, `canary_down` **+7분 41초**.
- ✅ **3차 회고 A-1b(SMS 경로)가 작동했다** — **문자 2통이 회전 +5분에 도착**했고 복구 후 OK 문자까지 왔다.
  → **2-6의 독립 신호 판정이 기대는 알람 경로가 두 번째로 검증됐다.**
- ⛔ **그런데도 13시간이었다.** 이 plan의 전제(*"Step 1이 라이브가 아닌 창은 브릿지 사전 무장으로 막는다"*)가
  **네 번째로 반증**됐고, 이번에는 **알림이 폰까지 도달한 상태에서** 반증됐다.
  3차는 *"Slack 도착 ≠ 사람 도달"*이었고 **4차는 *"SMS 도착 = 사람 도달, 그러나 ≠ 사람 행동"***이다.
  → 기한 표의 브릿지 행을 *"막는다"*가 아니라 **"실패가 기본값"**으로 다시 적었다.
- ⛔ **기한이 확정됐고 여유가 없다** — `NextRotationDate = 2026-09-14T08:59:59` →
  **다음 창 09-13 09:00 ~ 09-14 08:59:59.** **Step 1 라이브까지 남은 6일**이고,
  이 plan은 아직 `in-review`이며 리뷰 라운드가 남아 있다.
- ℹ️ **부수 발견**: `set-alarm-state`로 알림 경로를 테스트하려 했는데 아무 알림도 오지 않았고,
  원인이 **두 알람이 이미 `ALARM`이었기 때문**(CloudWatch는 상태 **전이**에만 알림)이었다.
  **그 침묵이 4차 장애를 발견하게 했다.** 브릿지 런북 f-2c에 기록했다.

**[ⓓ] revision 17 → 18** (2026-09-04. 승인 게이트의 마지막 미조회 항목 ⓓ를 실측으로 채웠다):

- **3차 장애 확정** — 회전 2026-08-30 13:08:48 → 복구 09-04 11:31:42, **4일 22시간 22분 54초.**
  1차(66시간)·2차(24시간 23분)를 **합친 것보다 길다.** 회고: `docs/postmortems/2026-08-30-rds-rotation-outage.md`.
- ✅ **탐지 설계가 실측으로 확인됐다** — `alb-target-5xx` 회전 **+6분 49초**, `canary_down` **+8분 16초**,
  **플래핑 0회**(2차는 19회 ALARM↔OK 왕복), 118시간 내내 ALARM 유지. plan 0008의 목표에 부합한다.
  → 이 plan이 **탐지를 범위 밖으로 둔 판단이 옳았다**는 근거이고, **2-6의 독립 신호 판정**
  (*"`/readyz` 503·`alb-target-5xx`가 조용했다면 `T_fail` 미도래"*)이 기대는 신호가 실장애에서 검증됐다.
- ⛔ **반면 이 plan의 전제 하나가 세 번째로 반증됐다** — *"Step 1이 라이브가 아닌 창은 브릿지 사전
  무장으로 막는다"*. **알람이 정확히 울렸는데도 118시간 갔다.** 브릿지는 *"사람이 알아챌 확률"*에 걸려
  있고 세 번 다 졌다. → *"막는다"*가 아니라 **"확률을 올릴 뿐이고 실패가 기본값"**으로 읽어야 한다.
  **Step 1 라이브 기한(2026-09-13)의 무게가 그만큼 크다.**
- 런북 대응은 이 plan 밖에서 했다 — 브릿지 런북에 **(f)** 신설(알림 도달 실증 f-2 · 창 당일 능동 확인 f-3 ·
  창 이동 선택지 f-4), `alarm-response.md` §13의 *"실측 없음"* 유보를 실측값으로 교체.

⚠️ **이것은 리뷰 라운드가 아니다.** revision 18은 r15 리뷰 이후 **게이트 종료(revision 17)와 ⓓ 기록**만
반영했다. 리뷰어 3인의 다음 검토 대상은 **revision 18**이다.

**[게이트] revision 16 → 17** (2026-09-04. **리뷰 라운드가 아니라 승인 게이트를 닫은 병합**이다.
r12~r15 네 라운드 동안 codex-cli#1 / codex-ide#1이 *"운영 상태 미확정"*을 이유로 `request-changes`를
유지했고, [사람]이 ⓐ~ⓒ를 조회해 값이 들어왔다):

- **ⓐ** `LastRotatedDate = 2026-08-30T13:08:48+09:00` · `NextRotationDate = 2026-09-07T08:59:59+09:00` →
  **08-30 창에서 회전이 실제로 일어났고**(13:08 KST, 주간), 다음 창은 **09-06 09:00 ~ 09-07 08:59:59 KST**다.
  ℹ️ **부수 소득**: revision 1이 `NextRotationDate`를 **창의 끝**으로 읽고 창을 계산한 방식이
  **실측으로 검증됐다**(창 08-30 09:00~08-31 08:59:59 안에서 08-30 13:08에 회전). 09-06 창 계산도 같은 근거다.
- **ⓑ** `list-rules` 결과가 `linkpulse-prod-deploy-failed`(plan 0005의 ECS 배포 실패 → SNS) 하나뿐 →
  **광역 관찰 rule은 존재하지 않는다.**
- **ⓒ** `/aws/events` 접두 로그 그룹 `[]` → **실이벤트 원문 없음.**
- → 결정표가 **`원문 없음 × 배선 없음` 한 행**으로 떨어졌다: ✅ **(a) 확정.**

*확정 사항*

- **기한 3개**: 브릿지 사전 무장 **2026-09-06 09:00 KST 전** / 1-1a **해당 없음(폐지)** /
  Step 1 라이브(1-3 apply) **2026-09-13 09:00 KST 전**. 1-7 대상 창은 드릴을 건너뛰면 **09-13 09:00 창**.
- **1-1a 폐지 — 다섯 리소스는 1-1이 승계한다.** 단계 순서에서 1-1a를 빼고
  (`1-1 → 1-2 → 1-3 → 1-4 → 1-5 → 1-6 → 1-7`), **1-1의 `plan` 기대값에서 신설 알람 6개 → 7개**로 고쳤다.
  **3분기 폴백(판정 시각·`TriggeredRules` 분기)은 삭제** — 대조할 원문이 1-3 apply 전에 오지 않으므로
  분기 자체가 성립하지 않는다. 원문 확인과 패턴 대조는 **1-6이 승계**한다.
- **(a)의 대가를 리스크표에 정직하게 내렸다**: *"이벤트 전제가 어긋나 정상 회전마다 오탐 알람"* 행의
  완화가 **약해졌다**(전제 검증이 라이브 전환 뒤로 감). 남은 근거는 **AWS 공식 이벤트 계약**과
  **1-6 드릴이 진짜 회전을 일으킨다**는 것이고, 어긋나면 그때 패턴·계약을 고치고 재검토 라운드를 한 번 더 돈다.
- **09-06 창을 못 대는 이유를 다시 적었다** — revision 16까지의 *"라운드 간격 8일 ≥ 남은 기간"*은
  **r11→r12의 작업 공백을 포함한 실측**이었고, **09-03~09-04에는 하루에 네 라운드가 돌았다.**
  실제 이유는 *"apply까지 사람의 손이 여러 번 필요한데 그 사이가 이틀"*이다. **09-13은 9일 뒤이고
  현재 cadence면 닿는다.**

*⚠️ ⓓ — 조회하지 않았고 결정에도 쓰이지 않지만, 회고에 남겨야 한다*

**08-30 13:08에 회전이 실제로 일어났다.** Step 1도 Step 2도 없는 상태이므로 **앱은 반드시 `28P01`로 죽었다.**
브릿지를 수행해 복구했는지·얼마나 걸렸는지가 **브릿지 무장 절차의 첫 실전 실측**이다.
`docs/postmortems/`에 **3차 사건**으로 기록해야 한다(2026-08-16·08-23 회고 2건이 이미 `상태: 초안`이다).
이 값은 plan 진행을 막지 않는다.

*[게이트] 자기점검*

- **네 라운드를 같은 이유로 막힌 끝에 닫혔다.** r12에서 게이트를 세운 판단(*"날짜 확정을 실행 단계가 아니라
  병합 단계로 끌어온다"*)은 옳았지만, **그 값을 얻는 데 필요한 것이 "사람의 AWS 로그인"이라는 것**을
  r12에 명시하지 않아 세 라운드가 더 돌았다. 리뷰어 둘은 매 라운드 직접 조회를 시도했고 세션 만료로 실패했다.
  → **사람만 할 수 있는 입력이 게이트에 있으면, 그 입력을 "무엇을·어떻게·누가"까지 첫 라운드에 적는다.**
  r13에서 명령 표를 넣고 r14에서 결정표를 넣어 **남은 것을 ⓐ 하나로 줄인 뒤에야** 실제로 값이 왔다.
- **결정표를 미리 채워 둔 것이 값을 했다.** 값이 도착한 순간 판단이 **한 행 조회**로 끝났다 —
  r13·r14·r15 세 라운드에 걸쳐 리뷰어들이 표를 ⓒ×ⓑ 2축 4행으로 다듬어 둔 결과다.
- **revision 17에서 내가 새로 쓴 것**: 게이트 조회 결과 표, (a) 확정과 그 파급 6곳(단계 순서·1-1 전제·
  알람 6→7·1-1a 절 폐지·리스크표 3행·1-7 대상 창), 09-06 창 판정 근거의 재작성, 기한 3개의 절대일자.
  **다음 라운드의 1차 확인 대상이다.** `1-1a`·`09-06`·`08-30`·`신설 알람`을 **본문과 리스크표를 분리해
  전수 출력**으로 확인했다.

**[r15] revision 15 → 16** (3인 전원 제출, 전부 `reviewed-revision: 15`, 전부 `request-changes`.
**반영 16 · 기각 0 · 사람 확인 대기 2**. ⚠️ **3인 전원 [높음]이 같은 곳을 짚었다 — r14에서 ②를 지운 근거가
절반만 검토됐다.** claude-ide의 정리가 정확하다: ②는 **증거 오염 방지**와 **관측 창 확보** 두 일을 했는데,
**첫째만 반박하고 둘째는 한 줄짜리 추정으로 대체**했으며 **그 추정의 비교 대상이 틀렸다.**
Step 1의 handler 계약·IAM 4분할·예산 표·알람 표는 **일곱 라운드 연속 무수정**이고 이번엔 Step 1 지적이 0건이다):

*결함*

- [r15] codex-cli#1 [높음] / codex-ide#1 [높음] — **승인 게이트가 네 라운드 연속 열려 있다.** codex-ide는 이번에도 ⓐ 조회를 시도했으나 **AWS 세션 만료**로 실패했다. codex-cli는 *"결정표가 ⓑ·ⓒ에도 의존하므로 '즉 ⓐ만 남는다'는 결론이 문서만으로 재현되지 않는다"*고 덧붙였다 → **반영 불가 · 사람 확인 대기** — 값이 없으면 해소되지 않는다. codex-cli의 덧붙임은 맞고, 이번 라운드에 결정표를 ⓒ×ⓑ 2축에서 **3행**으로 다시 나눠(아래 codex-cli#2) 그 의존을 표에 드러냈다.
- [r15] codex-cli#3 [높음] / codex-ide#2 [높음] / claude-ide#1 [높음] [3인 전원] — **②를 뺀 뒤 남은 유일한 프로덕션 검증이 수렴하지 않을 수 있다.** revision 15는 *"관측 창도 충분하다 — 창은 수 분, 기대 회복은 24초"*로 닫았는데 **비교 대상이 틀렸다.** 창 안에 들어가야 하는 것은 24초가 아니라 **`T_fail` 도래 + (`T_fail`→`T_ready`)**이고, **`T_fail`은 최대 커넥션 수명만큼 늦을 수 있다**(이 plan이 시계 표와 2-6 도입부에 두 번 적은 사실이다) → **반영** — ⚠️ **claude-ide가 가장 아픈 증거를 댔다: revision 15 자신이 같은 절에서 *"최대 대기 10분"*과 *"창은 10분을 넘지 못한다"*를 나란히 적었다.** 두 문장을 붙이면 결론은 하나다 — **10분이 필요한 회차에서는 구조적으로 증거를 만들지 못한다.** 실측으로 확인했다: **관측 창 ≈ 2~3분**(codex-cli#3이 짚은 대로 ALB 헬스체크가 **`/healthz` liveness**라 새 태스크가 빨리 헬시해지고 옛 태스크도 빨리 빠진다, `alb.tf:27-35`) vs **필요 ≈ 최대 5분 + 24초**(`db.go:29` `SetConnMaxLifetime(5 * time.Minute)`). **필요 쪽이 더 길다.** 자극을 늘려도 안 된다 — codex-ide#2가 1차 출처로 짚은 대로 `/readyz` 반복은 **idle 커넥션을 재사용**할 뿐 신규 연결을 강제하지 않고, 병렬 부하는 이 plan이 이미 기각했다. → **claude-ide의 (가)를 채택한다: `T_fail`을 우연이 아니라 설계로 만든다.** `db.go:29`의 상수를 **`DB_CONN_MAX_LIFETIME` 환경변수로 바꾸고 기본값은 현행 `5m` 유지**(미설정 시 동작 불변), **2-6 검증 기간에만 `30s`**를 얹는다 → `T_fail`이 회전 후 30초 안에 오고 창 2~3분이 넉넉히 덮는다. **앱 파라미터라 인프라가 번지지 않아 ②를 뺀 이유와 충돌하지 않고**, 2-4가 이미 *"신규 연결 강제"*를 fixture로 하는 것과 같은 발상이다. 2-5에 **투입(6번)과 되돌림(7번)** 배포를 넣고 **되돌림을 2-6의 완료 조건**으로 못박았다.
- [r15] claude-ide#3 [중간] (+ codex-cli#3·codex-ide#2의 대안 제시) — **①이 판정 불가일 때의 종료 조건이 없다 — *"대가는 최대 한 창"*은 다음 회차가 성공한다는 가정이다.** 불확정 사유 셋 중 *"`T_fail` 미도래"*·*"자극 미도달"*은 **창 길이와 커넥션 수명의 관계**라 **회차가 바뀌어도 그대로**이므로 같은 이유로 반복 실패할 근거가 있는데 **상한도 기한도 없었다** → **반영** — ⚠️ **이 plan은 같은 문제를 1-7에서 이미 풀어 놓았다**(*"1-7을 게이트로 두면 근본 대응이 1~2주 밀린다 … 1-7은 Step 2와 병행되는 운영 추적 항목이다"*). **2-6에도 같은 규율을 적용해 표로 확정했다**: **차단 게이트는 2-4 통합 테스트**(이미 그렇게 정의돼 있다), **2-6은 병행 운영 추적 항목 — Step 2 완료를 막지 않는다.** **2창 연속 판정 불가면 회고에 기록하고 별도 라운드**에서 (가)의 강도를 올리거나 ②를 다룬다. codex-ide#2가 제시한 대안(*"2-4를 배포 승인 증거로, 2-6을 best-effort로"*)과 같은 결론이다.
- [r15] codex-cli#2 [높음] / codex-ide#3 [중간] — **결정표의 "원문 있음 / 배선 무관" 행이 1-1의 시작 전제와 충돌해 실행 경로가 없다.** 표는 원문만 있으면 배선이 없어도 1-1a를 완료 처리하는데, **1-1은 다섯 리소스가 이미 apply됐다고 전제하고 그것이 `plan` 출력에 뜨면 중단하라고 한다** → *"과거 원문은 있으나 현재 배선은 없음"*(rule 삭제·drift)이면 **1-1a를 건너뛰고 1-1도 즉시 중단한다** → **반영** — r14에서 표를 ⓒ×ⓑ 2축으로 만들면서 **"원문 있음" 행만 ⓑ를 "무관"으로 뭉갰다.** 3행으로 다시 나눴다: **원문 있음+배선 있음 → 완료 처리** / **원문 있음+배선 없음 → 전제 검증은 완료 처리하되 다섯 리소스를 1-1에 흡수해 함께 작성·apply** / **원문 없음 → (b) 또는 (a)**. codex-ide#3이 짚은 *"광역 관찰 배선은 최종 Step 1 구성의 일부라 복구돼야 한다"*(rule 미매칭이 무증상이 되는 것을 막는 장치)도 근거로 적었고, 그 경우 **1-1의 `plan` 기대값이 바뀐다**(신설 알람 6개 → 7개)는 것까지 명시했다.
- [r15] codex-ide#4 [중간] / claude-ide#2 [중간] — **② 제거의 파급이 리스크표 두 행에 도달하지 않았다 — 한 행은 삭제된 수단을 롤백 경로로 지정한다.** *"판정 불가 시 rule 즉시 재활성화(`trap`)"* / 롤백 *"rule `ENABLED` 복귀"*, 그리고 *"②의 기준점 기록과 같은 성격"* / 롤백 *"②(통제 창)로 전환"* → **반영** — ⚠️ **네 라운드 연속 같은 형태이고 이번에는 성격이 한 단계 나쁘다.** claude-ide의 진단이 정확하다: r14 자기점검이 sweep 목록에 **`②`·`통제 창`·`dead-man`·`trap`을 명시적으로 넣었다고 적었는데 두 행이 남았다** — **대상 선정이 아니라 실행/완료의 문제**다. 실제로 내가 한 것은 **개수를 세는 것**(`② : 25`, `통제 창 : 8`)이었지 **전수를 열어 보는 것**이 아니었고, 리스크표는 문서 끝의 큰 표라 본문 sweep에서 눈에 안 들어왔다. → 이번에는 **리스크표를 본문과 분리해 별도로 훑었다.** 두 행을 현재 계약(*"사유 기록 후 다음 온디맨드 시도에서 재시도"*)으로 맞추고, claude-ide 개선대로 **완화 칸의 *"②의 기준점 기록과 같은 성격"*도** 함께 고쳤다. 관측 창 문제 행도 신설했다.
- [r15] codex-cli#4 [중간] — **2-6의 최종 체크리스트가 앞에서 확정한 판정식을 누락해 stale-log false positive가 재발한다.** 권위 있는 판정식(watermark 이후 + 세 로그의 세대·순서)은 위쪽 판정 문단에만 있고, 실제 *"성공 조건"*·*"판정 방법"*은 **`credential_recovered`가 있는지만** 대조하라고 적는다 — **운영자가 마지막 체크리스트만 따르면 r14에서 고친 것이 되돌아간다** → **반영** — r14에서 판정식을 고칠 때 **판정 문단만 고치고 체크리스트는 손대지 않았다**(위 codex-ide#4와 같은 형태의 파급 누락이다). 성공 조건에 **판정식 전체를 인용 블록으로 다시 적고**, 판정 방법에도 **watermark 경계와 `FilterLogEvents` 호출 방식**을 넣었다.

*개선 제안*

- [r15] codex-cli 개선 — **"Step 1의 새 태스크는 세대 전이를 원리적으로 만들 수 없다"를 판정 근거에서 빼라.** plan은 다른 곳에서 **전파 지연으로 새 태스크가 옛 `AWSCURRENT`를 주입받을 수 있음**을 잔여 위험으로 인정하고, **그런 새 태스크는 이후 refresh에서 새 값을 얻어 세대 전이를 만든다** → **반영** — 지적이 정확하고, **이 문서가 반복 경계해 온 *"이름이 사실보다 많은 것을 약속한다"*의 또 다른 사례**다(r9 *"풀이 스스로 교체된다"*, r11 *"opener 점유가 없다"*, r12 *"반드시 발화한다"*, r14 *"trap이 모든 경로를"*에 이어 다섯 번째). → **false positive를 막는 실제 경계는 "회전 전에 기록한 task ID"** 하나로 좁히고, 세대 결합은 **그 위의 보강**으로 격하했다. ⚠️ **②를 뺀 결론은 그대로 성립한다** — 새 태스크의 `task_id`가 (0)의 집합에 **없기 때문**이다. 같은 표현이 남아 있던 세 곳(도입부·판정 문단·판정 방법)을 함께 고쳤다.
- [r15] codex-ide 개선 / claude-ide 개선 [2인 동일] — **(0)의 세 기록 목록 직후에 watermark 요구가 중복된다** → **반영** — r14에서 목록화하면서 r13 문장을 지우지 않았다. 목록만 남기고 뒤 문장은 *"위 ③의 구체 계약"*으로 역할을 바꿨다.
- [r15] claude-ide 개선 — **"최대 대기 10분"을 결함 1과 연결해 두면 오해가 준다** — 지금은 근거와 *"창이 10분을 넘지 못한다"*가 떨어져 있어 실행자가 **그것이 곧 증거를 못 만드는 조건**이라는 것을 놓치기 쉽다 → **반영** — 10분 옆에 *"이 값이 실제로 필요해지는 회차는 판정할 수 없다"*와 **(가)가 10분을 필요 없게 만드는 장치**라는 것을 붙였다.
- [r15] claude-ide 개선 — **②를 되살릴 때의 요구사항 목록에 (가)(커넥션 수명)도 대안으로 적어 두라** → **반영** — *"인프라를 늘리는 길 / 앱 파라미터로 푸는 길"*을 나란히 두고 **후자가 이 규모에 맞다**고 적었다.
- [r15] codex-ide 개선 — **1-1a 분기가 확정되면 Step 1 단계 순서와 1-1의 "이미 apply됨" 전제도 함께 갱신하라** → **반영** — 단계 순서 앞에 ⛔ *"이 순서는 분기가 (b)일 때의 것"*과 **게이트에서 함께 갱신할 항목**을 적었다.

**[r15] 병합 자기점검**:
- ⛔ **"지우는 결정"을 할 때 그 절차가 하던 일을 전수로 세지 않았다.** r14 자기점검은 *"판정·증거 구조를 바꿀 때는 그 구조가 있었기 때문에 필요했던 절차를 함께 재평가한다"*고 적었는데, **이번에 필요했던 것은 그 역방향** — claude-ide의 표현대로 *"절차를 지울 때는 그 절차가 하던 일을 전수로 세는 것"*이다. ②는 두 일을 했고 나는 **하나만 반박한 뒤 나머지를 한 줄 추정으로 덮었다.** 그리고 그 추정은 **24초만 세고 `T_fail`을 빼먹었다** — r10~r12에서 세 라운드에 걸쳐 고친 *"상태 기계 전체를 세지 않는"* 실수와 **같은 형태**다.
- ⛔ **sweep을 "세기"로 했지 "열어 보기"로 하지 않았다.** r14 자기점검에 목록을 적었고 실제로 돌렸는데, 출력은 `② : 25`·`통제 창 : 8` 같은 **개수**였고 나는 그것을 확인으로 취급했다. **리스크표 두 행은 그 안에 있었다.** → **처방: sweep은 개수가 아니라 전수 출력으로 받고, 리스크표처럼 문서 끝의 큰 표는 본문과 분리해 따로 훑는다.** 이번 라운드에는 그렇게 했다.
- **다섯 번째 "보장하지 않는 것을 보장한다고 쓴" 문장을 지적받았다**(codex-cli 개선의 *"원리적으로 만들 수 없다"*). r9·r11·r12·r14에 이어서다. 앞의 넷은 전부 **외부 계약**(Go·AWS·셸)에 대한 것이었는데 **이번에는 내 문서 안의 다른 문장과 충돌**했다 — 리스크표가 *"새 태스크가 옛 값을 받을 수 있다"*를 이미 인정하고 있었다. → **"원리적으로/반드시/절대"를 쓸 때 외부 계약뿐 아니라 이 문서의 리스크표와도 대조한다.**
- **revision 16에서 내가 새로 쓴 것**: 관측 창 vs 필요 시간 실측 표, `DB_CONN_MAX_LIFETIME` 계약(2-2b·2-5 6·7번·2-6 완료 조건), 2-4/2-6의 게이트·추적 항목 구분, 결정표 3행, 판정식의 체크리스트 복원, 증거 경계를 task ID로 좁힌 것, 리스크표 두 행 + 신규 행, 단계 순서의 분기 의존 표시. **이 여덟이 다음 라운드의 1차 확인 대상이다.** sweep은 **본문과 리스크표를 분리해** `②`·`통제 창`·`trap`·`dead-man`·`원리적으로`·`DB_CONN_MAX_LIFETIME`·`10분`으로 **전수 출력**을 받아 확인했다.

**[r14] revision 14 → 15** (3인 전원 제출, 전부 `reviewed-revision: 14`, 전부 `request-changes`.
**반영 18 · 기각 0 · 사람 확인 대기 2**. ⚠️ **결함 12건 중 5건이 r13에서 내가 넣은 dead-man 하나에 몰렸고**,
3인 전원이 그것을 짚었다. **그 5건의 결론은 "dead-man을 제대로 만들라"가 아니라 "통제 창 자체를 빼라"였다** —
자세한 사유는 2번. Step 1의 handler 계약·IAM 4분할·예산 표·알람 표는 **여섯 라운드 연속 무수정**):

*결함*

- [r14] codex-cli#1 [높음] / codex-ide#1 [높음] — **승인 게이트가 이번에도 닫히지 않았다(세 라운드 연속).** codex-ide는 이번에도 ⓐ 조회를 시도했으나 **AWS 세션 만료**로 실패했다 → **반영 불가 · 사람 확인 대기** — 값이 없으면 해소되지 않는다. 이번 라운드에 한 것은 **게이트가 덮지 못하는 항목을 게이트 밖으로 꺼낸 것**이다: claude-ide#1·#2가 짚은 09-06 파급과 브릿지 기한은 **ⓐ와 무관하게 지금 고칠 수 있는 것**이라 전부 고쳤다.
- [r14] codex-cli#2 [높음] / codex-cli#3 [높음] / codex-ide#2 [높음] / claude-ide#3 [중간] / claude-ide#4 [낮음] — **r13이 넣은 dead-man이 계약의 절반만 적힌 상태다.** (ㄱ) role을 *"1-1/1-3에서 함께 만든다"*고 **배정만** 하고 IAM 목록·1-1 리소스 범위·**1-1의 `plan` 게이트 숫자**·1-3 검증 어디에도 넣지 않았다 — 게다가 **필요한 것이 role 하나가 아니다**(`scheduler.amazonaws.com` trust, 좁은 rule ARN 한정 `events:EnableRule`, 실행자의 `scheduler:*Schedule`과 **`iam:PassRole`**). (ㄴ) **`get-schedule` 성공이 `EnableRule` 성공을 뜻하지 않는다** — `FlexibleTimeWindow`·universal target ARN·`Input`·`RoleArn`·`RetryPolicy`·`DeadLetterConfig`가 전부 미확정이라 **`trap`이 실패하는 바로 그 상황에서 dead-man도 조용히 실패**할 수 있고, Scheduler는 **60초 정밀도**라 *"30분 안에 반드시"*는 성립하지 않는 보장이었다. (ㄷ) T+30분 예산이 **이 plan이 "상한 없음"이라 확정한 회전 구간**을 덮어야 한다 → **반영 — 다만 dead-man을 완성하는 대신 ②(통제 창) 자체를 범위에서 뺐다.** 세 리뷰어가 모두 *"완성하든지 ②를 빼든지"*를 명시했고, **빼는 쪽의 근거가 더 강하다**: ⚠️ **②의 존재 이유가 r12~r13에서 이미 사라졌다.** ②는 *"Step 1을 켠 채로는 입증할 수 없다"*는 전제로 신설됐는데 그 전제는 **판정이 시각 경합이던 시절의 것**이고, 지금 ①이 요구하는 **`auth_failed_observed(N) → refresh N→N+1 → credential_recovered(N+1)` 순서**는 **Step 1이 켜져 있어도 오염되지 않는다** — Step 1이 띄운 새 태스크는 env seed라 세대 전이가 없어 그 순서를 **원리적으로 만들 수 없다.** 즉 **rule을 끌 이유가 없다.** 관측 창도 충분하다(ECS는 새 태스크가 헬시해진 뒤 옛 태스크를 deregister하므로 수 분이고, Step 2 기대 회복은 약 24초다). ①이 불확정이면 **사유를 기록하고 다음 자연 회전 창에서 ①을 다시 돌린다** — 대가는 최대 한 창이고 **백스톱을 스스로 끄지 않는다.** 통제 창·`trap`·dead-man·관련 리스크 행을 전부 삭제했고, 다시 필요해지면 위 (2)의 목록이 그때의 요구사항이라고 남겼다.
- [r14] claude-ide#1 [중간] — **09-06 → "그다음 창" 교체의 파급이 세 곳에 도달하지 않았고, 그중 하나는 판정 규칙이다.** `:390`은 **1-1a 3분기 폴백 전체의 결론 문장**(*"세 경로 어디에서도 09-06 09:00 기한은 유지한다"*)인데, 헤드라인이 09-13으로 옮겨간 뒤에도 **이미 폐기된 목표를 향해 몰아붙인다.** `:369`·`:1680`도 같다 → **반영** — ⚠️ **r13 결함 #2(`T_avail` 파급 3곳)의 정확한 재판이다.** 더 아픈 것은 리뷰어가 짚은 지점이다: **r13 자기점검이 sweep 목록을 적었는데 거기에 `09-06`이 없었다** — 그런데 *"기한 표의 다음 창 형태"*는 revision 14가 새로 쓴 여덟의 **첫 항목**이다. **sweep 대상을 고르는 기준 자체가 "이번에 바꾼 값 전부"가 아니었다.** 세 곳을 리뷰어 처방대로 **날짜 대신 "기한 표 참조"**로 바꿨다 — ⓐ가 오면 표 한 곳만 고치면 된다.
- [r14] claude-ide#2 [중간] — **브릿지 사전 무장 기한이 지나간 창(08-30)에 고정돼 있는데, 그것이 09-06 창의 유일한 보호 수단이다.** revision 14는 같은 표의 Step 1 라이브 행만 *"그다음 창"* 형태로 고치고 **이 행은 손대지 않았다.** `:39`가 인용한 r4 결함(*"선언된 기한이 이미 위반돼 있었다"*)이 **Step 1 행에서는 고쳐지고 브릿지 행에는 남았다** → **반영** — **운영 시급성은 이쪽이 더 높다**(Step 1 라이브는 09-13 목표지만 브릿지는 이틀 뒤다)는 지적이 정확하다. ⚠️ **이 행도 ⓐ를 기다릴 필요가 없었다** — `:924`의 일반 규칙(*"Step 1이 라이브가 아닌 모든 회전 창에 대해 창이 열리기 전 무장한다"*)이 이미 만료되지 않는 형태를 준다. 그 형태로 바꿨다(*"매 회전 창이 열리기 전 — 다음 대상은 09-06 09:00 KST(ⓐ로 확인)"*).
- [r14] codex-ide#3 [중간] — **"원문 없음 → (b)" 결정표가 ⓑ의 두 상태를 합쳐 일정 결론을 잘못 낼 수 있다.** 1-1a rule이 **이미 apply됨**과 **아직 없음**은 다르다 — 전자는 다음 창 원문을 받지만, 후자는 **1-1a 작성·검토·apply가 그 창 전에 끝나야만** 가능한데 이 plan은 *"현재 라운드 속도로는 그 창에 못 댄다"*고 확정했다. **rule도 없는데 (b)를 택하면 1-3이 한 창이 아니라 두 창 뒤로 밀린다** → **반영** — r13에서 결정표를 만들 때 *"원문 유무"* 하나로만 갈랐는데 **ⓑ와 ⓒ는 독립 변수**였다. 표를 **ⓒ 원문 × ⓑ 배선** 2축으로 다시 짜고 **"원문 없음 + 배선 없음 → (a)"**를 추가했다. (a)의 대가(전제 검증이 라이브 전환 뒤로 감)와 그 완화(**1-6 드릴이 진짜 회전을 일으켜 원문을 확실히 준다** + AWS 공식 이벤트 계약이 1차 출처)도 함께 적어 **조용히 넘어가지 않게** 했다.
- [r14] codex-ide#4 [중간] — **①의 관측 창 종료를 `list-tasks`의 태스크 소멸로 잡으면 실제 증거 생성 중단보다 늦다.** ALB는 **대상 deregistration 즉시 새 요청 라우팅을 멈추고** 대상은 그 뒤 `draining`에 머무르므로, **태스크가 아직 `RUNNING`이어도 관측 창은 이미 닫혀 있다** → **반영** — r13에서 이 종료 조건을 만들 때 *"태스크가 사라진다"*를 자극 중단의 대리 지표로 썼는데, **plan 자신이 "ALB가 라우팅을 멈추는 순간부터 닿지 않는다"고 설명해 놓고** 판정은 한 단계 뒤 사건으로 잡았다. → **(0)에 회전 전 태스크의 ENI IP 기록**을 추가하고, `aws elbv2 describe-target-health`에서 **그 대상이 `draining`/미등록이 되는 시점**을 창의 끝으로 쓴다.
- [r14] codex-cli#4 [중간] — **`AWS_REGION`의 필요성 설명과 최종 코드 계약이 모순된다.** *"env가 없으면 SDK가 리전을 정하지 못해 전량 실패"*라고 설명하면서 **같은 절에서 ARN 파싱으로 `WithRegion`을 쓴다**고 적어, **env가 없어도 기능은 멀쩡하다**고 스스로 인정한다. 리스크표도 폐기된 근거를 유지한다 → **반영** — 지적이 정확하다. r12에서 결함(SDK에 기본 리전 없음)을 받고 r13에서 이중 방어를 넣으면서 **"둘 중 무엇이 SDK를 동작시키는 값인가"를 정하지 않았다.** → **ARN 파싱을 source of truth로 확정**하고(ARN은 리전을 항상 포함하고 시크릿을 지목하는 값이라 어긋날 수 없다), **`AWS_REGION`의 목적을 "배포 시점 교차검사"로 정정**했다. 그 결과 **빈 값의 결과는 "SDK 실패"가 아니라 의도적인 설정 fail-fast**이고, 헤드라인·리스크표·테스트 근거를 그 계약으로 통일했다. 2-5 게이트는 그대로 유효하다 — **배포 전에 거르는 것이 목적**이기 때문이다.

*개선 제안*

- [r14] codex-cli 개선 / codex-ide 개선 [2인 동일] — **watermark를 "마지막 이벤트 시각 또는 `nextToken`" 중 하나로 두지 말고 한 방식으로 고정하라.** `FilterLogEvents`의 `nextToken`은 **페이지 진행 토큰이라 24시간 뒤 만료되고 응답을 다 읽으면 없을 수도 있다** → **반영** — r13에서 watermark를 넣을 때 *"또는"*으로 남긴 것이 **드릴 당일 해석이 갈리는 형태**였다. **`(timestamp, eventId)` 쌍**으로 고정하고, 판정 시 `startTime = 기록한 timestamp`로 호출한 뒤 **그 `eventId`까지 버리고 경계는 자기 자신을 제외**한다고 못박았다(동률 timestamp를 `eventId`로 가른다).
- [r14] codex-ide 개선 — **2-5 apply ② 설명에도 env를 `DB_SECRET_ARN` 및 `AWS_REGION`으로 적어 바로 아래 CI 게이트와 표현을 맞춰라** → **반영** — 같은 형태의 파급 누락이라 함께 고쳤다.
- [r14] claude-ide 개선 — **`:129`의 "다음 창: 2026-08-30…"에 시점 표시가 없다**(`:91`은 *"작성 시점(revision 1) … 이미 지났다"*로 정직하게 적었는데 배경/제약의 같은 값은 현재형 사실처럼 남아 있다) → **반영** — *"revision 1 실측이고 그 창은 이미 지났다. 현재 값은 ⓐ로 확인한다"*로 고쳤다.
- [r14] claude-ide 개선 — **`:271`·`:857`의 "드릴을 건너뛴 경우에만 2026-08-30"은 이제 불가능한 분기다** → **반영** — 규칙(*"1-6 이후 재계산한 `NextRotationDate`"*)은 그대로 두고 폴백 날짜만 **ⓐ의 `NextRotationDate`**로 바꿨다.
- [r14] claude-ide 개선 — **`:372`의 1-1a 판정 시각(08-31 09:00)에 ⛔ 게이트 참조를 붙여라** → **반영** — 헤더(`:320-323`)와 같은 형태로 맞췄다. 지난 날짜를 기준으로 폴백을 타지 않는다.
- [r14] claude-ide 개선 — **`:390`을 고칠 때 (b) 분기와의 관계도 한 줄 적어라** → **반영** — *"(b)는 **한 창**만 미루는 것이고, 그 창 안에서도 3분기 폴백이 그대로 적용된다"*를 넣었다. 두 절을 나란히 읽는 사람이 멈추지 않는다.
- [r14] codex-ide 개선 — **비용·ADR에 Scheduler 실행 role과 일회성 schedule을 추가하라** → **반영 불요** — ②를 뺐으므로 Scheduler가 이 plan에 남지 않는다. 비용·ADR에 추가할 것이 없다.

**[r14] 병합 자기점검**:
- ⛔ **세 라운드 연속 같은 형태로 틀렸고, 이번에는 처방을 실행했는데도 틀렸다.** r13에서 *"이름·기준·단계를 바꾸는 편집은 `grep`으로 옛 값 전수를 먼저 뽑는다"*고 처방하고 **실제로 sweep을 돌렸는데**, 목록에 **이번 라운드가 바꾼 가장 중요한 값(`09-06`)이 없었고** 새로 **배정한** 산출물(Scheduler role)은 애초에 대상이 아니었다. claude-ide의 진단이 정확하다 — **처방의 문제가 아니라 대상 선정의 문제**다. → **sweep 목록은 "내가 고친 문장에 나오는 단어"가 아니라 "내가 바꾼 값 + 새로 배정한 산출물" 전부여야 한다.** 이번 라운드에는 `②`·`통제 창`·`dead-man`·`09-06`·`08-30`·`08-31`·`nextToken`·`AWS_REGION`을 전수로 훑었고, 그 과정에서 **리뷰가 짚지 않은 잔여 3곳**(`:1183` 승계 문장, cleanup 블록, `:1467` watermark 계약)도 함께 잡았다.
- **가장 값이 큰 배움은 "고치라"는 지적을 "지우라"로 읽은 것이다.** dead-man 5건은 전부 *"계약을 완성하라"*는 형태였지만, 완성하면 **인프라 범위가 Step 1로 번지고** 이 plan의 목적(Step 1 라이브 + Step 2 근본 대응)에서 멀어진다. 그런데 **②가 필요했던 이유가 r12~r13의 판정식 개선으로 이미 사라져 있었다** — 세 라운드 동안 ②를 *"아껴 두는 백스톱"*으로 유지하면서 **그것이 아직 필요한지는 다시 묻지 않았다.** → **판정·증거 구조를 바꿀 때는 "그 구조가 있었기 때문에 필요했던 절차"를 함께 재평가한다.**
- **r13이 dead-man을 넣은 것 자체가 과잉이었다.** codex-ide#2(r13)의 지적(*"`trap`이 모든 경로를 덮지 못한다"*)은 옳았지만, 내가 고른 처방(Scheduler)은 **한 문장으로 배정할 수 있는 크기가 아니었다.** 그때 이미 대안(②를 안 함)을 적어 두고도 **기본으로 삼지 않았다** — *"검증 수단을 줄이는 것"*보다 *"수단을 유지하며 안전장치를 더하는 것"*을 반사적으로 골랐다. **새 인프라를 요구하는 처방은 "이 절차가 정말 필요한가"를 먼저 묻는다.**
- **revision 15에서 내가 새로 쓴 것**: ② 제거와 그 사유, ①의 관측 창 종료 조건(ALB draining), (0)의 세 기록(task ID·ENI IP·watermark), watermark `(timestamp, eventId)` 계약, 결정표의 ⓑ×ⓒ 2축, `AWS_REGION`의 목적 정정, 브릿지 기한의 비만료 형태, 기한 참조로의 교체 3곳. **이 여덟이 다음 라운드의 1차 확인 대상이다.** sweep은 **바꾼 값 전부**(`②`·`통제 창`·`dead-man`·`09-06`·`08-30`·`08-31`·`nextToken`·`AWS_REGION`·`list-tasks`)로 돌렸다.

**[r13] revision 13 → 14** (3인 전원 제출, 전부 `reviewed-revision: 13`, 전부 `request-changes`.
**반영 17 · 기각 0 · 사람 확인 대기 2**. **결함 12건 중 10건이 revision 13에서 내가 새로 쓴 여덟 항목 안**이다.
⚠️ **성격이 r12와 다르다** — claude-ide의 진단이 정확하다: *"r12는 **새로 쓴 설계가 틀린 것**이었고,
r13은 **고친 설계가 다른 절에 도달하지 않은 것**이다."* 이름 교체의 파급 누락(#2), (0) 단계만 넣고 그것이
사는 절차는 옛 전제 그대로(#3), 새 env의 빈 값 계약과 게이트 누락(#4) — **셋 다 "방금 고친 것을
참조하는 곳"을 훑으면 나온다.** Step 1의 handler 계약·IAM·예산 표·알람 표는 **다섯 라운드 연속 무수정**):

*결함*

- [r13] codex-cli#1 [높음] / codex-ide#1 [높음] — **승인 게이트가 아직 닫히지 않아 이 revision 자체를 실행 승인할 수 없다.** ⓐ~ⓓ가 들어오기 전에는 (a)/(b)를 고를 수 없고, 실행 절에는 지난 08-29/08-30 기한과 08-30 원문 전제의 1-1a·3분기가 그대로 남아 있다. codex-ide는 이번 검토에서 ⓐ를 직접 조회하려 했으나 **AWS 세션 만료**로 확인하지 못했다 → **반영 불가 · 사람 확인 대기** — 두 리뷰어의 판정이 옳다. revision 13이 이 항목을 *"유일한 잔여 블로커"*로 인정한 것과 별개로, **값이 들어오기 전에는 `request-changes`가 해소되지 않는다.** 내가 이번 라운드에 할 수 있는 것은 게이트를 **더 기계적으로** 만드는 것뿐이라 그렇게 했다(아래 claude-ide 개선 — ⓑ·ⓒ 상태별 결정을 표로 미리 적어 **남는 것이 ⓐ의 날짜 하나**가 되게 했다). ⚠️ **두 라운드 연속 같은 이유로 막혔다.**
- [r13] claude-ide#1 [중간] — **09-06 기한이 "선언과 동시에 불가능하다고 확정"된 상태다 — r4 결함의 정확한 재현이고, 이번엔 이 plan이 스스로 그 계산을 적어 넣었다.** 기한 표(`:39`)와 Step 1 헤더(`:300`)가 09-06을 기한으로 선언하는데, 같은 문서 600줄 아래(`:898`)가 *"09-06 창도 Step 1으로 막지 않는다 — 사실로 확정한다"*고 적는다. **`:32-33`이 r4 결함으로 인용해 둔 문장과 날짜만 08-30 → 09-06으로 바뀌었다.** ⚠️ **승인 게이트가 이것을 덮지 못한다** — 09-06 불가 판정의 근거는 *"라운드 간격 8일"*이라는 **plan 내부의 사실**이라 ⓐ~ⓓ와 무관하게 이미 끝났다 → **반영** — **ⓐ~ⓓ 없이도 고칠 수 있는 유일한 기한 항목이고, 지적이 정확하다.** 리뷰어가 제시한 형태 그대로 기한을 **"09-06 창 다음 창이 열리기 전"**(09-06에 회전이 일어나면 2026-09-13 09:00 KST)으로 바꾸고, 표 행과 Step 1 헤더 둘 다 **⛔ "09-06 창에는 못 댄다"**를 먼저 적게 했다. 절 제목도 *"2026-08-30 창은…"* → *"회전 창과 Step 1 라이브 시점"*으로 일반화했다.
- [r13] codex-cli#2 [높음] — **2-6이 `task_id`만 대조해 이전 회전이 남긴 `credential_recovered`를 이번 회복 증거로 재사용할 수 있다(false positive).** ECS 태스크가 두 회전 창 이상 살아 있으면 이전 세대 전이 로그도 같은 `task_id`를 가진다 → **반영** — **회전 창이 7일이고 태스크가 그보다 오래 사는 것은 정상이라 현실적인 경로다.** ⚠️ **r12에서 시각 경합(`createdAt`과 겨루는 것)을 없앤 것은 옳았지만 시간 *범위*까지 함께 없앤 것은 과했다** — *"어느 로그를 볼 것인가"*(범위)와 *"누가 먼저인가"*(경합)는 다른 문제인데 하나로 묶어 지웠다. → **(0)에서 태스크별 로그 watermark를 함께 기록**하고 그 이후만 보며, **`auth_failed_observed(N) → refresh N→N+1 → credential_recovered(N+1)`의 순서와 세대 연속성**까지 성립해야 성공으로 친다. 리뷰어가 짚은 대로 **`T_label` 기준으로 자르지 않는다** — 시계 표대로 **`T_fail`은 `T_label`보다 앞설 수 있어** 첫 줄이 잘린다. 리스크표에 행을 신설했다.
- [r13] codex-ide#2 [높음] — **2-6 통제 창의 cleanup은 셸 `trap`만으로 "모든 경로"를 보장하지 못한다.** `SIGKILL`·실행 호스트/터미널 소실·프로세스 중단·`enable-rule` 자체 실패에는 trap이 실행되거나 성공한다는 보장이 없고, 그러면 **Step 1 rule이 다음 회전까지 비활성으로 남는다** → **반영** — **이 plan이 막으려는 24시간 장애를 이 plan의 검증 절차가 만드는 형태**라 강도가 맞다. revision 12·13이 적은 *"모든 경로에서"*는 **셸이 주지 않는 보장**이었다(이 문서가 반복해 경계해 온 *"이름이 사실보다 많은 것을 약속한다"*의 또 다른 사례). → **0단계로 독립 dead-man을 신설**했다: rule을 끄기 **전에** `aws scheduler create-schedule`로 **T+30분 1회성 `events:EnableRule`**(`ActionAfterCompletion=DELETE`)을 걸고 **`get-schedule` 확인을 회전 시작의 게이트**로 둔다. 정상 종료 시 삭제하며, `trap`은 *"대체가 아니라 빠른 경로"*로 격하했다. 필요한 것은 **`events:EnableRule` 한 action짜리 role 하나**다. ⚠️ **대가를 낮추는 선택지도 함께 적었다**: 이 리소스가 과하다고 판단되면 **②를 수행하지 않고 다음 자연 회전 창을 기다린다** — ②는 원래 백스톱이고 *"dead-man 없이 백스톱을 스스로 끄는 것보다 한 창 기다리는 편이 싸다."* **[사람]이 택한다.**
- [r13] codex-ide#3 [중간] — **`auth_failed_observed` 부재만으로 "신규 연결 유도 실패이며 Step 2 결함 아님"을 확정할 수 없다.** `28P01` 분류·래핑이나 로그 발행 코드가 깨진 경우에도 똑같이 로그가 없고, 그 둘은 **명백히 Step 2 결함**이다 → **반영** — 지적이 정확하고, 리뷰어가 덧붙인 *"이 로그를 두 원인을 구분하려고 도입했다는 설명 자체와 모순"*이 특히 맞다. **부재는 아무것도 구분하지 않는다.** r12에서 이 로그를 넣으면서 *"있음"*의 의미만 검토하고 *"없음"*의 의미는 검토하지 않았다. → **"둘 다 없음"을 판정 불가로 내렸다.** *"`T_fail` 미도래"*로 기록하려면 **앱 로그와 독립된 신호**(관측 창 동안 `/readyz` 503이 0건이고 `alb-target-5xx`도 조용했다)가 있어야 한다고 명시했다. 2-1 검증 ⑮가 확률을 낮추지만 **낮은 것과 증명된 것은 다르다**는 것도 적었다.
- [r13] claude-ide#3 [중간] — **2-6 ①의 관측 창이 Step 1의 롤링으로 조기에 닫히는데 절차는 그것을 모르고 10분을 기다린다.** ①은 Step 1을 **켠 채로** 도는데, 롤링이 시작되면 **ALB가 (0)의 태스크로 라우팅을 멈추고 드레인**되므로 `/readyz` 반복 호출이 그 태스크에 닿지 않는다 — 자극이 끊기면 두 로그 다 더는 생기지 않는다. 게다가 절차 블록이 **②의 목소리로** 쓰여 있다(*"Step 1은 계속 꺼진 상태로 남는다"* — ①에서는 켜져 있다) → **반영** — **(0) 단계를 넣으면서 대조 *대상*만 고치고 증거를 *만드는* 절차는 ② 전제 그대로 뒀다.** r12 결함 #4의 귀결인데 혼자서는 안 보이는 종류다. → **10분 상한을 ①/②로 갈랐다**: ②는 태스크 불변이라 10분, **①은 "10분 또는 (0)의 태스크가 `list-tasks`에서 사라지는 시점 중 빠른 쪽"**. 사라진 뒤의 대기는 **아무 증거도 만들지 않으므로** 즉시 종료하고 ②로 간다. 원인을 *"자극이 닿지 못했다"*는 **세 번째 분기**로 따로 두어 위 두 분기(`T_fail` 미도래 / Step 2 결함)에 섞이지 않게 했다. ②의 목소리로 쓰인 문장도 ①/② 공통으로 고쳤다.
- [r13] claude-ide#2 [중간] / codex-cli#3 [중간] / codex-ide 개선 [3인 전원] — **`T_avail` 신설의 파급 갱신이 세 곳에서 빠졌다.** (ㄱ) `:1298`(2-4)이 *"2-6(`T_label`→`T_ready`)·목표 절(`T_visible`→`T_ready`)"*로 **둘 다 옛 이름**을 쓴다 — *"어느 절이 어느 시계를 쓰는지"* 하나를 위해 존재하는 문장이다. (ㄴ) `:1566`(리스크표)이 전파 창을 `T_label`→`T_visible`로 적는다 — **`T_avail`→`T_visible`은 Step 2가 스스로 통과하는 구간**이라 감수 범위를 17초만큼 과대하게 적는다. (ㄷ) `:18`(목표 절)이 `T_avail`→`T_ready`(관측 목표)와 다운타임(`T_fail`→`T_ready`)을 *"다운 시간"* 한 단어로 부른다 → **반영** — 3인이 같은 곳을 짚었다. claude-ide가 *"`grep`으로 전수를 훑는 것이 처방(내가 그렇게 확인했다)"*이라고 적은 대로 **전수 sweep**을 돌려 셋 다 고쳤다. ⚠️ (ㄴ)는 이름뿐 아니라 **값이 틀린 것**이라 성격이 다르다 — 감수 구간과 자력 통과 구간을 섞었다.
- [r13] codex-ide#4 [중간] / codex-cli 개선 — **평균 probe cadence를 더해 놓고 17초·24초를 "최악"·"상한"이라 부른다.** 이 문서는 `:975-984`에서 **Route53 체커가 비조정이라 1.8초는 평균이지 상한이 아니다**라고 스스로 명시했다 → **반영** — **내가 r10에 확립한 사실을 r12·r13의 산술에 적용하지 않았다.** 시계 표의 넷째 열을 *"앞 구간의 상한"* → **"앞 구간의 예산"**으로 바꾸고, 17초·24초를 **"기대 예산"·"관측 목표"**로 통일했다. **상한이 있는 항만 더하면 5+10+5 = 20초 + probe 2회이고 probe 2회에는 상한이 없다**는 것도 적었다. codex-cli 개선대로 **2-4의 30초 합격선은 테스트가 probe 주기를 결정적으로 통제하기 때문에 성립하며 SLA로 쓸 수 없다**는 전제도 명시했다.
- [r13] codex-ide#5 [낮음] / claude-ide#4 [낮음] — **2-5의 CI 배포 전 게이트가 revision 13에서 추가한 `AWS_REGION`을 검사하지 않고, 그 env의 "빈 값" 계약도 정해지지 않았다.** *"없으면 Step 2가 통째로 Step 1 폴백에 의존하고 다음 회전 때까지 드러나지 않는다"*고 방금 규정한 env가 **같은 게이트에서 빠져 있다.** 그리고 *"ARN 리전과 다르면 기동 오류"*만 적어 **빈 값이 "다름"인지 "검사 생략"인지가 없다** — 두 읽기의 결과가 정반대다 → **반영** — claude-ide가 짚은 대로 *"이중 방어"*라는 목적을 살리려면 **빈 값 = 기동 오류**여야 하고, **바로 아래 `DB_SECRET_ARN`이 같은 문제를 이미 명시적으로 처리한 형태**가 있다. 그 형태를 그대로 써서 `APP_ENV == production && AWS_REGION == ""` → `Config` 로드 실패를 못박고, 개발 환경은 정적 모드라 SDK를 쓰지 않으므로 검사 생략으로 갈랐다. 2-5 게이트는 **두 env + 리전 일치**까지 확인하도록 고쳤다.

*개선 제안*

- [r13] claude-ide 개선 — **(a)/(b) 택일의 조건부 결론을 표에 미리 적어 두면 ⓐ~ⓓ 도착 시 판단이 기계적이 된다** → **반영** — ⓑ·ⓒ 상태별 결정 표를 넣었다: **원문 있음 → 1-1a 완료 처리(택일 자체가 불필요)** / **원문 없음 → (b), 추가 대가 0**. 그 결과 **남는 것은 ⓐ의 날짜 확인 하나**가 되고, ⓓ는 결정에 쓰이지 않고 **회고에 들어갈 사실**임을 구분해 적었다(그래도 조회하는 이유 = 브릿지 무장 절차의 실효 평가).
- [r13] claude-ide 개선 — **`:894`의 "오늘이 2026-09-03"은 라운드마다 낡는다. 날짜 대신 "라운드 간격 ≥ 남은 기간" 형태로 적어라** → **반영** — **실제로 하루 만에 틀렸다**(오늘 2026-09-04, *"남은 3일"*은 2일이었다). 판정 기준을 *"검토 라운드 간격 실측(r11→r12·r12→r13 모두 약 8일) ≥ 창까지 남은 기간이면 그 창에는 못 댄다"*는 **관계**로 바꿨다. 리뷰어 지적대로 이 관계는 **라운드가 돌수록 더 강해지기만 하므로** 다시 계산할 필요가 없다.
- [r13] claude-ide 개선 — **`:1458`의 성공 조건이 *"두 running task"*라는 옛 표현으로 남아 판정 시점의 running으로 읽힌다** → **반영** — *"(0)에서 회전 전에 기록한 task ID 2개"*로 판정 방법과 표현을 통일했다. **r12 claude-ide#4가 지적한 오독 경로가 한 문장에 남아 있었다.**
- [r13] claude-ide 개선 — **1-1a의 제목과 기한이 지나간 창(08-30)을 근거로 남아 있다. 제목 옆에 게이트 참조 한 줄을 붙여라** → **반영** — 제목을 *"다음 회전의 실이벤트 원문"*으로 고치고 **⛔ 게이트 참조 + "08-30 창은 이미 지났다"**를 헤더에 붙였다. 실행자가 *"이미 지난 창의 원문을 받으러"* 이 단계를 시작하지 않는다.

**[r13] 병합 자기점검**:
- **r12 자기점검이 예측한 여덟 항목에서 결함 10건이 나왔다 — 두 라운드 연속 예측 적중이다.** 그런데 **예측이 맞는데도 못 막았다**는 것이 핵심이다. r12에서 *"다시 읽는 것으로는 부족하다"*고 쓰고 **관측 수단 열**이라는 장치를 만든 것은 유효했다(claude-ide가 *"개별 결함 수정이 아니라 원인에 대한 장치"*라고 평가했고 실제로 `T_avail`을 잡아냈다). **그런데 그 장치를 만든 라운드에서 이름 교체의 파급은 `grep`으로 훑지 않았다.** 장치는 *"새로 쓰는 것"*에만 걸었고 *"이미 쓴 것"*에는 안 걸었다.
  → **처방: 이름·기준·단계를 바꾸는 편집은 반드시 `grep`으로 옛 값의 전수 목록을 먼저 뽑고, 그 목록을 다 지운 뒤에 병합을 끝낸다.** claude-ide가 *"내가 그렇게 확인했다"*고 적은 것이 리뷰어 쪽 표준이면 병합 쪽도 같아야 한다.
- **r13 결함의 지배적 형태는 "고친 것이 그것을 참조하는 절에 도달하지 않음"이다.** #2(이름 교체 3곳), #3((0) 단계만 넣고 절차는 옛 전제), #5(새 env가 게이트에 없음), claude-ide 개선(성공 조건의 옛 표현) — **네 건이 같은 형태**다. r11·r12는 *"새로 쓴 것이 틀림"*이었는데 라운드가 돌수록 **설계 결함 → 전파 결함**으로 옮겨 가고 있다. 좋은 신호이면서 동시에 **전파는 기계적으로 막을 수 있는 종류**라 남겨 둘 이유가 없다.
- **codex-ide#2(dead-man)는 이 문서가 반복 경계해 온 형태의 또 다른 사례다** — *"모든 경로에서"*라는 문장이 **셸이 주지 않는 보장**을 약속했다. r9의 *"풀이 스스로 교체된다"*, r11의 *"opener 점유가 없다"*, r12의 *"반드시 발화한다"*와 같은 계열이고, **이번에는 대가가 가장 크다**(검증 절차가 24시간 장애를 만든다). → **"모든/반드시/항상"이 들어간 문장은 그 보장을 주는 API·계약을 옆에 적는다. 못 적으면 문장을 내린다.**
- **codex-cli#2(watermark)는 내가 r12에 한 수정이 과했던 것이다** — 시각 경합을 없애면서 시간 범위까지 지웠다. **"틀린 것을 지울 때 그것과 붙어 있던 맞는 것도 같이 지우지 않았는지"**를 확인한다.
- **revision 14에서 내가 새로 쓴 것**: 기한 표의 "다음 창" 형태, dead-man 0단계, watermark + 세대 순서 판정식, ①/② 관측 창 분리, 로그 부재의 판정 불가 처리, "예산 vs 상한" 표기 통일, `AWS_REGION` 빈 값 계약, (a)/(b) 결정 표. **이 여덟이 다음 라운드의 1차 확인 대상이다.** 이번에는 **각 항목이 참조되는 곳을 `grep`으로 전수 확인**했다(`T_label`·`T_visible`·`running task`·`모든 경로`·`AWS_REGION`·`10분`).

**[r12] revision 12 → 13** (3인 전원 제출, 전부 `reviewed-revision: 12`, 전부 `request-changes`.
**반영 21 · 기각 0 · 사람 확인 대기 1**. **결함 13건 중 11건이 revision 12에서 내가 새로 쓴 여섯 항목 안에서
나왔다** — r11 병합 자기점검이 *"다음 라운드의 1차 확인 대상은 이 여섯"*이라고 예측한 그대로다.
claude-ide는 그 예측이 **다섯 건 전부 적중**했다고 적었다. Step 1(handler 계약·IAM·예산 표·알람 표)은
**네 라운드 연속 손댈 것이 없다**는 평가가 3인 모두에게서 유지됐고, 이번 Step 1 지적 2건은
**revision 12가 새로 건드린 자리가 아니라 처음부터 있던 구멍**이다(AWS_REGION·DLQ policy)):

*결함*

- [r12] codex-cli#1 [높음] / codex-ide#3 [중간] / claude-ide#1 [높음] — **`T_visible`이 두 뜻으로 쓰여 같은 구간에 24초와 2~7초가 세 줄 간격으로 있다.** 시계 표는 `T_visible`을 *"provider가 처음 새 값을 **반환받은** 시각(refresh 로그)"*으로 정의하는데, 바로 아래 24초 산술이 **그 시각 뒤에 backoff 게이트·probe·refresh를 다시 센다** — 정의상 `T_visible` **앞**에 끝나는 작업들이다. 목표 절(`:18-19`)·기준 표(`:994`)·2-4 (b)(`:1183`) 세 곳이 이 잘못된 등치에 얹혀 있었고, *"아래가 이 문서의 **단일 시계**"*라고 선언한 표를 **배포 게이트가 다른 뜻으로 쓰고 있었다** → **반영** — **3인 전원이 같은 곳을 짚었고 반박할 여지가 없다.** claude-ide의 진단이 특히 정확하다: *"r11에서 라벨 이동과 가시화를 가른 것과 **같은 형태가 한 칸 뒤로 밀렸다** — 이번엔 **가시화(fetchable)와 취득(fetched)**이 합쳐졌다."* → **`T_avail`을 신설해 시각을 여섯으로 늘렸다**: `T_avail`(새 값이 **조회 가능**, *"물어보면 온다"*, **관측 불가**) / `T_visible`(provider가 **처음 받음**, *"물어봤고 왔다"*, refresh 로그). **24초 = `T_avail`→`T_ready`**(관측 목표), **2~7초 = `T_visible`→`T_ready`**(실측 가능), 그 사이 `T_avail`→`T_visible` = 게이트+probe+refresh **최악 17초**로 갈랐다. 다이어그램에 `T_visible` 지점을 표시해 두 구간이 눈으로 보이게 했다. ⚠️ **24초는 시작점이 관측 불가라 프로덕션에서 실측할 수 없다**(claude-ide) — *"관측 목표"*로만 쓰고 **실측하는 자리는 2-4 (b) 하나뿐**(fixture가 전환 시각을 안다)임을 명시했다. codex-ide#3이 대안으로 제시한 *"현 정의를 유지하고 24초를 `T_fail` 기준으로"*는 **채택하지 않았다** — `T_fail`은 `T_avail`보다 앞일 수도 뒤일 수도 있어(기존 풀 재사용) 그 기준으로는 24초가 일반적으로 성립하지 않는다.
- [r12] codex-ide#2 / claude-ide#2 [높음, 2인 동일] — **2-6의 `T1 < deployments[].createdAt` 판정이 성립하지 않는다.** `createdAt`은 **`UpdateService`가 호출된 순간** 찍히는 **제출 시각**이지 새 태스크가 복구한 시각이 아니다. 두 시각은 **둘 다 `T_label` 기준 수십 초 단위**이고 그 사이에 **상한 없는 전파 구간**이 끼므로, 전파가 10초만 걸려도 *"Step 1이 먼저"*로 뒤집히는데 그때 Step 1은 **제출만** 했고 실제로 되살린 것은 old task의 Step 2다 — **판정 방향이 반대로 나온다(false negative)** → **반영** — claude-ide의 지적이 가장 아프다: **이 plan이 Step 1에 대해 세 라운드에 걸쳐 세운 *"제출됨 ≠ 복구됨"*(`:400-410`·`:838`의 `redeploy_submitted`)을 2-6이 그대로 어겼다.** r11 자기점검이 *"판정 기준을 쓸 때 '이 신호가 증명하는 것'과 '판정하려는 것'을 나란히 적는다"*고 처방해 놓고 **`createdAt`에는 적용하지 않았다.** → **시각 경합을 판정식에서 완전히 뺐다.** 대신 claude-ide가 찾아낸 **이 문서 안의 재료**를 쓴다: `credential_recovered`는 **세대 전이에 묶여 있어** 회전 전부터 살아 있던 태스크에서만 나고, **Step 1이 띄운 새 태스크는 env seed로 시작해 세대 전이가 없어 이 로그를 원리적으로 남길 수 없다.** 따라서 *"회전 **전** task_id에서 `credential_recovered`가 났다"* 자체가 Step 2 복구의 증거다. `createdAt`은 **Step 1 개입 여부의 보조 지표**로 격하했고, 1-7과 **같은 문장**을 쓰도록 맞췄다(claude-ide 개선3). codex-ide가 요구한 *"회전 전 running task 기준선 기록"*은 아래 claude-ide#4의 (0) 단계로 함께 닫혔다.
- [r12] claude-ide#4 [중간] — **2-6 ①의 `list-tasks` 대조 시점이 없어 Step 1이 돌면 구조적으로 항상 판정 불가가 된다.** 수단 ①은 **Step 1을 켠 채로** 도는 시나리오인데, Step 1이 정상이면 10분 상한 안에 running task 집합이 통째로 바뀌고 새 태스크는 `credential_recovered`를 남기지 않으므로 **판정 시점의 `list-tasks`와는 교집합이 항상 공집합**이다. *"기본 수단"*이 **Step 1이 도는 한 성공할 수 없고**, 그러면 아껴 둔 ②(통제 창)를 **사실상 매번** 쓰게 된다 → **반영** — 결함 2의 성질에서 곧바로 따라 나오는 귀결인데 **혼자서는 보이지 않는 종류**다. **(0) 회전 *전* task ID 2개 기록** 단계를 ①에 신설했다. 리뷰어 지적대로 **②의 5단계(기준점 기록)와 같은 성격**이라 ①②가 같은 형태를 쓰게 되어 일관성도 올라간다. 판정 방법 문장에 *"판정 시점에 `list-tasks`를 실행하면 안 된다"*를 명시했다.
- [r12] codex-cli#4 / claude-ide#3 [중간, 2인 동일] — **`T_fail`을 남기는 로그가 어느 구현 단계에도 없다.** 시계 표는 `T_fail`의 관측 수단을 *"provider 로그"*로 선언하고 **다운타임을 `T_fail`→`T_ready`로 정의**하는데, 2-1의 로그 구현 항목 네 개에 첫 `28P01` 관찰이 **없다** — 즉 **plan이 정의한 다운타임을 계산할 수 없다** → **반영** — **r8 claude-ide#1·r11 codex-ide#2와 같은 형태의 세 번째 재발**이다(*"2-6이 쓰는 신호를 2-1이 구현하지 않았다"*). → **`auth_failed_observed`(첫 `28P01`, 세대당 1회, `task_id` 포함)**를 다른 로그와 같은 자리에 넣고 검증 ⑮로 고정했다. claude-ide가 짚은 **더 큰 값**도 그대로 반영했다: 이 로그가 없으면 *"기존 세션만 재사용돼 `28P01`이 안 옴"*과 *"Step 2 코드가 깨져서 안 남"*이 **로그상 완전히 동일**해, 통제 창 하나와 회전 스케줄 한 번을 쓰고도 **다음에 무엇을 고칠지 알 수 없다.** 2-6 체크리스트에 *"로그 없음 = `T_fail` 미도래 = 신규 연결 유도 실패이고 **Step 2 결함이 아니다**"*를 적었다. codex-cli가 제시한 대안(*"`T_fail`을 시계에서 제거"*)은 채택하지 않았다 — 다운타임 정의를 잃는 대가가 로그 한 줄보다 크다.
- [r12] codex-ide#1 [높음] — **Step 2의 AWS SDK 리전 공급 경로가 없어 프로덕션 `GetSecretValue`가 실행 불가하다.** `infra/prod/ecs.tf`에 `AWS_REGION`이 없고 Fargate에는 공유 config 파일도 없는데, SDK for Go v2는 **기본 리전이 없다** → **반영** — **실측 확인했다**: `environment` 블록에 리전이 없고 `grep`으로 저장소 전체(`infra/`·`app/`)에 `AWS_REGION`·`WithRegion`이 **하나도 없다.** **Lambda는 런타임이 자동 주입하지만 ECS는 하지 않는다**는 것이 이 결함의 핵심이고, 이 plan은 Step 1에서 Lambda를 다루면서 그 감각을 Step 2로 가져왔다. 그대로 구현하면 **자격증명은 task role에서 얻어도 endpoint·signing region을 정하지 못해 refresh가 전량 실패**하고, ⚠️ **production fail-fast도 이건 못 잡는다 — ARN은 있으니까.** 즉 **다음 회전 때까지 드러나지 않는다.** → 2-2b env에 **`AWS_REGION = var.region`**을 넣고, **코드에서도 ARN 파싱 리전을 `config.WithRegion`에 명시**하며 `AWS_REGION`과 불일치 시 **기동 오류**로 이중 방어한다. production 설정 단위 테스트로 고정. 리스크표에 행을 신설했다.
- [r12] codex-cli#3 [중간] — **EventBridge target DLQ의 SQS queue policy에 source 제한이 없다.** 같은 plan이 Lambda 리소스 정책은 rule ARN으로 제한하면서 **DLQ에는 그 confused-deputy 경계를 빠뜨렸다.** AWS 공식 예제도 `aws:SourceArn`을 rule ARN으로 제한한다 → **반영** — **실측 확인했다**: `:143`이 Lambda 쪽을 *"해당 rule ARN으로 제한"*이라 적는데 DLQ 항목은 *"`events.amazonaws.com`에 `sqs:SendMessage`를 주는 queue policy"*뿐이라 **계정 밖 EventBridge까지 이 큐에 쓸 수 있다.** **같은 경계를 한 곳에만 적용한 비대칭**이라는 지적이 정확하다. → queue ARN 한정 `Resource` + **`ArnEquals aws:SourceArn = <좁은 rule ARN>`**(+ `aws:SourceAccount`)을 계약에 넣고 **1-3에서 `get-queue-attributes`의 `Policy` 값을 단언**한다. 리스크표에 행을 신설했다.
- [r12] codex-cli#2 [높음] / codex-ide#4 [중간] / claude-ide#5 [중간] — **만료된 기한과 미확정 운영 상태가 plan 안에서 해소되지 않았다.** revision 12는 *"진행 전에 [사람]이 `describe-secret`으로 표의 날짜를 갱신한다"*로 적었는데, codex-cli가 짚은 대로 **날짜 갱신을 승인 후 실행 단계의 plan 수정으로 넘기면 이 저장소의 검토/승인 경계와 A-11의 절대 기한 원칙을 다시 우회한다** → **구조는 반영 · 날짜 확정은 사람 확인 대기(잔여 블로커 1)** — 지적이 전적으로 옳다. revision 12의 처리는 *"조건부 기한을 쓰지 않는다"*를 지킨 것처럼 보이지만 **실은 조건부 기한을 실행 단계로 옮긴 것**이었다. → **날짜 확정을 병합 단계로 끌어왔다**: 기한 절을 **⛔ 승인 게이트**로 바꾸고, [사람]이 줄 **read-only 상태 4개(ⓐ~ⓓ)**를 표로 못박았다(`describe-secret` / `describe-rule`·`list-targets-by-rule` / `filter-log-events` / 알람·브릿지 이력). ⚠️ 그리고 **claude-ide#5가 발견한 구조적 문제**를 함께 적었다 — **1-1a의 존재 이유(순서 불변식)가 08-30 창과 함께 사라졌다.** 1-1a는 *"1-3 apply **전에** 원문으로 전제를 검증한다"*를 사려고 신설됐는데(`:279-281`), 그 사이에 있던 회전 창이 지나가 **둘이 같은 창 앞에 몰리면서 1-1a 판정이 1-3보다 뒤로 간다** — **자기가 고치려던 상태로 되돌아간다.** → **(a) 1-1a를 1-1에 흡수 / (b) 유지하되 이유를 다음 창으로 갱신(1-3이 한 창 밀림)** 두 선택지를 **대가와 함께** 표로 적었다. ⚠️ **ⓐ~ⓓ 없이는 고를 수 없다** — ⓑ·ⓒ가 *"이미 원문이 잡혔다"*면 둘 다 불필요하고 1-1a는 완료 처리된다. **이것이 유일한 잔여 블로커다.**

*개선 제안*

- [r12] claude-ide 개선 — **2-6 통제 창의 기준점을 CloudWatch 지표가 아니라 Lambda 로그 이벤트로 잡아라.** 이 문서는 `:314-321`에서 이미 **EventBridge/CloudWatch 지표를 best-effort라며 확정 판정에서 내렸는데**, 5단계가 Lambda `Invocations` 지표를 기준점으로 쓰고 7단계가 그것으로 *"섞였는지"*를 확정 판정한다 → **반영** — **내가 r11에서 내린 판단을 한 절 건너에서 스스로 어긴 것**이다. 집계 지연 때문에 회전 직후 조회는 0을 반환할 수 있다. 기준점을 **로그 그룹의 마지막 이벤트 시각**으로 바꾸고(지연이 짧고 `outcome`·`versionId`까지 준다) 지표는 보조로 내렸다. 같은 5단계에 **`list-tasks` 기준선**도 넣어 ①의 (0)과 형태를 맞췄다.
- [r12] claude-ide 개선 — **`credential_recovered`가 새 태스크에서는 나지 않는다는 사실을 2-6에 한 줄로 적어라** — 결함 2·4의 처방이 전부 이 성질에 기대는데 지금은 `:1088`에서 **추론해야만** 나온다 → **반영** — 판정 문단에 ℹ️로 명시했다. *"이 로그의 존재 자체가 Step 2 경로의 증거"*라는 것이 문서에서 바로 읽힌다.
- [r12] claude-ide 개선 — **1-7의 *"제출됨 ≠ 복구됨"* 문장과 2-6을 같은 문장으로 맞춰라** → **반영** — 결함 2를 고치면서 2-6이 `:838`을 직접 인용하게 했다. 두 절이 다음 라운드에 다시 갈라지지 않는다.
- [r12] claude-ide 개선 — **"2026-08-30 창은 Step 1으로 막지 않는다" 절 전체가 지나간 창을 대상으로 남아 있다. 같은 계산을 09-06에 대해서도 하라** → **반영** — 오늘이 09-03이고 r11→r12 간격이 **실측 8일**이라 **09-06 창에도 못 댄다**는 것을 08-30과 같은 형태로 **사실로 확정**했다. ⚠️ 그 결과 **기한 절 (b)의 추가 대가가 *"한 창 더"*가 아니라 0일 수 있다**는 것이 드러났다 — (b)는 1-3을 09-06 창 뒤로 미는 것인데 **어차피 그 창에는 못 댄다.** 리뷰어가 *"(a)/(b) 택일 근거도 함께 선다"*고 예상한 그대로다.
- [r12] codex-cli 개선 — **1-1a의 실이벤트 수집을 아키텍처 전제의 유일한 근거로 두지 마라.** AWS 문서가 **`AWSPENDING`·`AWSPREVIOUS`에는 이벤트를 발행하지 않고 `detail.versionId`는 라벨이 붙은 새 버전 ID**라고 명시한다 → **반영** — 1차 출처 두 개를 1-1a 산출물 절에 인용하고 *"원문은 전제를 **운영 증거로 확인**하는 것이지 없으면 아키텍처가 무효가 되는 종류가 아니다"*를 적었다. **이 구분이 기한 절 (a) 선택지를 실질적으로 가능하게 만든다** — 리뷰어가 *"이미 놓친 08-30 원문 때문에 1-1a의 목적 전체가 무효가 되는 표현을 줄일 수 있다"*고 한 것이 정확하다.
- [r12] codex-ide 개선 — **2-0에 "갱신된 비밀번호가 `stdlib.GetConnector`의 고정 config를 어떻게 대체하는지" 데이터 흐름을 명시하고 두 dial이 다른 비밀번호를 받는 테스트를 추가하라** → **반영** — *"오류를 바깥 wrapper에서 관찰한다"*는 계약은 강했지만 **주입 경로가 구현자 추론에 남아 있었다**는 지적이 맞다. **매 시도마다 provider에서 현재 `(password, generation)`을 읽어 `pgx.ConnConfig` **사본**의 `Password`에 주입하고, 원본은 절대 변경하지 않는다**(동시 연결이 서로 덮어쓴다)를 계약으로 넣고 검증 ⑯으로 고정했다.
- [r12] codex-ide 개선 — **ADR의 `T0 삼분할`을 여섯 시각 모델로 고치고 리스크표의 로그 필드 행에도 `credential_recovered`를 포함해 폐기된 모델의 잔재를 제거하라** → **반영** — ADR 목록을 **여섯 시각**(특히 `T_avail` vs `T_visible` 분리)으로 고치고 **`credential_recovered`의 세대 결합 성질**도 ADR 항목에 추가했다(그것이 판정 근거이므로). 리스크표 로그 행은 **다섯 개 전부**(`task_id`·refresh·`credential_recovered`·`auth_failed_observed`·`secret_provider_mode`)로 갱신했다.
- [r12] codex-cli 개선 — **Python handler 테스트를 CI에 넣을 때 러너 기본 Python에 기대지 말고 `actions/setup-python`으로 3.13을 고정하라** → **반영** — Lambda 런타임과 같은 버전이어야 수집·문법 드리프트가 CI에서 잡힌다. 1-1 CI 항목에 한 줄 추가.

**[r12] 병합 자기점검**:
- **r11 자기점검의 예측이 그대로 적중했다.** *"다음 라운드의 1차 확인 대상은 이 여섯"*이라고 지목한 항목에서 **결함 13건 중 11건**이 나왔고, claude-ide는 자기 지적 다섯 건이 **전부** 그 안이라고 확인했다. 예측이 맞았다는 것은 위안이 아니라 **"내가 어디를 틀릴지 알면서도 틀렸다"**는 뜻이다 — r11에서 *"바뀐 절이 참조하는 다른 절을 다시 읽는다"*를 처방하고 실행하지 않아 결함이 났고, r12에서는 **실행했다고 적었는데도**(*"각각이 참조하는 절을 다시 읽고 숫자와 이름을 맞췄다"*) 같은 자리에서 났다. → **다시 읽는 것으로는 부족하다. 새 이름을 만들 때는 "이 이름이 가리키는 시각을 프로덕션에서 어떻게 관측하는가"를 표의 열로 강제한다** — 이번에 `T_avail` 행의 *"관측 불가"*를 적는 순간 24초 산술의 시작점이 틀렸다는 것이 즉시 보였다.
- **`T_visible` 결함은 r11 결함의 정확한 재판이다.** r11은 *"라벨 이동(`T_label`)과 값 가시화"*를 합쳤고, r12는 *"가시화(`T_avail`)와 취득(`T_visible`)"*을 합쳤다. **같은 실수가 한 칸씩 뒤로 밀리며 세 라운드 반복됐다.** 공통 원인은 **"AWS가 값을 바꾼 시각"과 "우리 코드가 그것을 안 시각"을 한 이름에 담는 습관**이다. 이제 시계 표의 모든 행에 **관측 수단** 열이 있으므로 같은 실수는 그 열이 비는 것으로 드러난다.
- **결함 2(`createdAt`)는 "다른 곳에서 이미 배운 것을 여기서 안 쓴" 형태다.** 이 plan은 Step 1에서 *"제출됨 ≠ 복구됨"*을 세 라운드에 걸쳐 세우고 `outcome` 이름까지 `redeploy_submitted`로 지었다. 그 규율이 **2-6에는 도달하지 않았다.** r11 자기점검의 처방(*"이 신호가 증명하는 것과 판정하려는 것을 나란히 적는다"*)을 `credential_recovered`에는 적용하고 `createdAt`에는 적용하지 않았다. → **판정식에 등장하는 모든 항에 그 한 줄을 붙인다. 예외 없이.**
- **Step 1 지적 2건(AWS_REGION·DLQ policy)은 성격이 다르다** — revision 12가 건드린 자리가 아니라 **처음부터 있던 구멍**이고, 열두 라운드 동안 3인이 못 봤다. 둘 다 **"코드/HCL을 실제로 열어 봐야 보이는" 종류**다. 리뷰어가 `ecs.tf`를 직접 읽고 `AWS_REGION` 부재를 확인한 것이 결정적이었다. → **plan이 "env를 추가한다"고 쓸 때, 그 env 블록의 현재 내용을 전수로 옮겨 적어 두면** 무엇이 없는지가 보인다.
- **revision 13에서 내가 새로 쓴 것**: `T_avail` 행과 24초 산술의 시작점 정정, 2-6 (0) 단계와 세대 결합 판정, `auth_failed_observed`, `AWS_REGION` 이중 방어, DLQ `aws:SourceArn`, 비밀번호 주입 데이터 흐름, 기한 승인 게이트와 (a)/(b) 표, 09-06 창 계산. **이 여덟이 다음 라운드의 1차 확인 대상이다.** 특히 **(a)/(b) 표는 사람의 ⓐ~ⓓ 응답 없이는 닫히지 않으므로**, 다음 라운드 전에 그 값이 오지 않으면 리뷰어도 같은 자리를 다시 지적할 수밖에 없다.

**[r11] revision 11 → 12** (3인 전원 제출, 전부 `reviewed-revision: 11`, 전부 `request-changes`.
**반영 16 · 기각 0**. 지적이 **두 라운드 연속 Step 2에 몰렸다** — Step 1은 이번에도 1-1a 지표 해석 하나뿐이고,
claude-ide는 *"Step 1(handler 계약·1-1a·알람 표·예산 표)은 손댈 것이 없다"*고 다시 평가했다.
**이번 라운드의 결함 6건 중 5건이 revision 11이 새로 쓴 T0 삼분할과 그 산술에서 나왔다** — r10 자기점검이
*"큰 설계 변경을 한 라운드에 넣으면 그 변경이 다음 라운드의 결함 목록이 된다"*고 적은 그대로다):

*결함*

- [r11] codex-cli#1 [높음] — **T0a가 `setSecret`과 첫 `28P01`을 같은 시각으로 놓아 plan 자신의 기존 연결 수명 전제와 충돌한다.** `setSecret`은 DB 값이 바뀐 시각이고 첫 `28P01`은 앱이 **새 연결을 열어 옛 비밀번호로 인증한** 시각이다. 기존 세션은 최대 5분 재사용되므로 둘은 같지 않고, Step 1이 먼저 재배포하면 첫 `28P01`이 **아예 관측되지 않을 수도** 있다 → **반영** — 지적이 정확하다. 이 plan의 배경 "왜 죽는가"가 이미 *"기존 커넥션은 잠시 살아 있다(상한 5분, 하한 0)"*라고 적어 놓고, T0 표에서 그 사실을 스스로 지웠다. **T0 삼분할을 폐기하고 시각을 다섯으로 나눴다**: `T_set`(DB 값 변경, 관측 불가) / `T_fail`(앱이 첫 `28P01` 관찰) / `T_label`(`AWSCURRENT` 이동) / `T_visible`(새 값이 실제로 조회됨) / `T_ready`(새 세대로 dial 성공). **다운타임 = `T_fail`→`T_ready`**, 회전 내부 창 = `T_set`→`T_label`, 자격증명 채택 = `T_visible`→`T_ready`로 각각 분리했고, **`T_set`→`T_fail`에 상한이 없다**(기존 풀만 재사용되면 다운타임 0)는 것도 표에 넣었다. 2-6의 *"거의 항상 결론이 난다"*에도 **"기존 세션이 재사용되지 않는 경우"**라는 선행 조건을 붙였다.
- [r11] codex-ide#1 [높음] — **T0b가 "라벨 이동"과 "새 값이 조회 가능"을 같은 것으로 취급한다.** Secrets Manager는 변경이 모든 endpoint에 즉시 보인다고 보장하지 않고([문제 해결](https://docs.aws.amazon.com/secretsmanager/latest/userguide/troubleshoot.html)), `LastRotatedDate`의 공식 계약도 라벨 이동 시각을 보장하지 않는다([`DescribeSecret`](https://docs.aws.amazon.com/secretsmanager/latest/apireference/API_DescribeSecret.html)). 그런데 이 plan의 Step 1은 **바로 그 이유로 60초 bounded wait를 두고 있다** → **반영** — **자기 문서 안의 정면 모순이라 기각할 여지가 없다.** 위 시계에서 `T_label`과 `T_visible`을 분리하고 **`T_label`→`T_visible`(전파)에도 상한이 없다**고 명시했다. **"약 24초"는 `T_visible` 기준**으로만 쓰고, 2-6의 T0도 **1-1a 로그의 이벤트 `time`**을 1차 증거로 하며 `LastRotatedDate`는 근사치로 격하했다. 리스크표의 회전 중 창 행에 **전파 구간을 함께** 적었다.
- [r11] codex-ide#2 [높음] — **2-6이 "새 비밀번호를 읽어 캐시를 갱신함"만으로 Step 2의 DB 복구를 판정해 Step 1이 false positive를 만든다.** refresh 로그는 `GetSecretValue`가 다른 값을 반환했다는 증거일 뿐 **그 값으로 dial/인증이 성공했다는 증거가 아니고**, plan의 T0 표 자신이 T0c 뒤에 *"다음 probe + dial"*이 더 있다고 적어 **자기모순**이다. wrapper의 비밀번호 주입이 깨져도 old task가 refresh 로그를 먼저 남기고 Step 1의 재배포가 `/readyz`를 200으로 되돌리면 **모든 조건이 통과**한다 → **반영** — 이번 라운드에서 가장 값이 큰 지적이다. **`credential_recovered` 로그**(새 세대로 첫 dial/ping 성공, **세대 전이에 묶인 1회**)를 **2-1 구현 항목**으로 넣고, **2-6의 T1과 두 `task_id` 조건을 그 로그로 바꿨다.** *"refresh 로그는 있는데 `credential_recovered`가 없으면 성공이 아니라 Step 2 결함"*도 명시했다. 2-1 검증에 **⑫(refresh는 성공, 새 세대 dial은 실패)** 를 넣어 이 경로를 테스트로 고정하고, 2-4에도 *"`credential_recovered`가 실제 쿼리 성공과 같은 시각에 난다"*를 넣었다.
- [r11] claude-ide#2 [중간] — **`refreshCtx` 8초 안에 "시도 2회 + backoff 2초"가 들어간다는 근거가 사라졌다.** revision 11이 *"connect 1초 / read 2초 / 시도 2회"* 행을 operation context 하나로 교체하면서 **시도당 상한을 지웠다.** `MaxAttempts = 2`·`MaxBackoff = 2초`는 재시도 정책이지 한 시도의 길이를 정하지 않으므로, **시도 1이 8초를 다 쓰면 재시도가 한 번도 일어나지 않고** `MaxAttempts = 2`를 명시한 의미가 사라진다. `:907`의 *"Secrets Manager 최악 6초"*도 근거를 잃는다 → **반영** — **Step 1의 예산 표(`시도 수 × 시도당 상한 + backoff ≤ 구간 상한`) 형식을 Step 2에 그대로 적용**했다: **시도당 3초 + backoff ≤2초 + 재시도 3초 = 최악 8초**. 리뷰어가 지적한 대로 **8초 ctx면 마진 0**이라(이 plan이 r6·r9에서 세 번 고친 형태) **operation 상한을 10초로 올렸다.** 그에 따라 detached refresh 상한·2-1 ⑪·리스크표·회복 산술의 refresh 항을 **전부 10초로 맞췄고**, (나)의 *"최악 6초"*를 **"최악 8초"**로 갱신했다(claude-ide 개선4). 2-1 검증 ⑬으로 *"시도 하나가 3초를 넘기면 잘리고 재시도가 실제로 일어난다"*를 고정한다.
- [r11] claude-ide#1 [중간] — **"약 20초"의 내역이, 바로 세 줄 위에서 "revision 10이 빠뜨렸다"고 지적한 항목(refresh를 *시작시킬* probe)을 여전히 포함하지 않는다.** 나열 순서상 probe가 refresh 뒤에 있어 **소비하는 probe**뿐이다. 실제 상태 기계는 다섯 구간(5+2+8+2+5 = 약 22초) → **반영** — **값 차이는 2초지만 문제는 유도라는 지적이 정확하다.** 이 문단의 존재 이유가 *"revision 10의 산술이 상태 기계 전체를 세지 않았다"*를 고치는 것인데 **같은 종류의 누락이 새 산술에 남았다.** 내역을 다이어그램으로 바꿔 **게이트를 여는 probe #1과 새 값을 소비하는 probe #2를 눈으로 구분**되게 했다. refresh 상한이 10초가 되어(위 claude-ide#2) 합은 **5+2+10+2+5 = 약 24초**이고, 목표 절·2-6·리스크표의 숫자를 함께 갱신했다.
- [r11] codex-cli#2 [중간] — **2-6 통제 창이 rule 비활성화 방법을 확정하지 않았고(HCL `state = "DISABLED"` vs 셸 `trap`) EventBridge 전파 확인이 없어, Step 2 단독 증거가 결정적이지 않다.** AWS는 rule 변경 뒤 들어오는 이벤트가 즉시 매칭을 중단하지 않을 수 있다고 명시한다([문제 해결](https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-troubleshooting.html)) → **반영** — 두 방식을 섞어 놓아 **중단 시 rule과 IaC state 중 어느 쪽이 어긋난 채 남는지 확정되지 않았다**는 지적이 맞다. **HCL은 `ENABLED`로 유지하고 [사람]이 실행하는 단일 CLI 스크립트로만 끈다**로 고정하고, 순서를 8단계 계약으로 못박았다: `trap` 먼저 → 브릿지 무장 → `disable-rule` → `State=DISABLED` 확인 → **안정화 5분** → **기준점 기록**(Lambda `Invocations`·`deployments[]`) → 회전 → 판정. **회전 이후 Lambda 호출 1건 또는 새 deployment가 있으면 Step 2 결과와 무관하게 판정 불가**로 종료한다. 리스크표에 행을 신설했다.
- [r11] codex-ide#3 [중간] — **2-4 배포 게이트의 15초 기준이 회전 중 창·SDK 실패 시나리오와 분리되지 않아 결과가 fixture 지연값에 따라 임의로 달라진다.** 2-0이 `T0a→T0b`에 상한이 없다고 확정했으므로 **구값 노출을 3초만 둬도 `Connect` 최악 13초와 합쳐 15초를 넘고**, 아주 짧게 두면 *"포기 없음"*을 시험하지 않고도 통과한다 → **반영** — *"합격 여부가 fixture 값에 따라 달라지면 배포 게이트가 아니다"*가 정확하다. 리뷰어가 제시한 형태 그대로 **시나리오 3개로 쪼개 표로 고정**했다: **(a) 새 값 즉시 가시화** — `T_set` 기준 ≤15초 / **(b) 구값 창 D = 20초** — **`T_visible`**(fixture가 새 값으로 전환한 시각) 기준 ≤15초 + D 동안 포기 없음·폭주 없음 / **(c) SDK K = 3회 실패** — **마지막 주입 실패가 끝난 시각** 기준 ≤15초. **D·K를 코드에 고정값으로 박고, 바꿀 때는 합격값도 함께 다시 계산한다**고 못박았다. *"`T_set` 기준으로 재는 것은 (a)뿐"*도 명시했다 — (b)·(c)의 지연은 plan이 통제하는 구간이 아니다.
- [r11] claude-ide#3 [낮음] — **1-5의 5xx 런북 문장이 "Step 2 라이브 이후"에만 참인데 작성 시점은 1-5(Step 1 직후)다.** 그 사이에 몇 주가 있고 그 기간의 회전 5xx는 **Step 1 재배포(수 분)까지 지속**되므로 *"짧은 5xx는 정상"*은 **틀린 안내**다. *"Step 2 라이브 이후:"* 접두만으로는 부족하다 — **런북을 읽는 사람은 접두보다 판별 기준을 먼저 본다** → **반영** — 이 지적의 관찰(접두 vs 읽는 순서)이 특히 정확하다. **1-5에는 시점 가드를 문장 안에 넣은 버전**(*"Step 2가 라이브가 아닌 동안 회전 5xx는 전부 브릿지 대상"*)만 남기고, *"짧은 5xx는 정상"* 문장은 **2-5의 런북 갱신 항목(신설 6번)으로 옮겼다.** 이렇게 하면 claude-ide 개선3(2-4 실측을 런북에 반영하는 경로가 없다)도 함께 닫힌다 — 2-5는 2-4 **뒤**라 그때는 실측값이 있다. 임계 조정 판단도 그 자리로 옮겼다.
- [r11] claude-ide#4 [낮음] — **2-5 smoke의 "정적 모드로 떠도 통과한다"는 이제 절반만 맞다.** revision 11이 추가한 production fail-fast 때문에 **env 누락 경로에서는 정적 모드로 뜨지 못한다.** 다만 **결론은 여전히 옳다** — 남는 경로는 **CI가 provider 코드 없는 옛 이미지를 배포하는 것**이고, 게이트 3은 task definition의 `environment`만 보므로 **이미지 내용을 확인하지 않는다** → **반영** — *"내가 방금 닫은 구멍이 이 문장의 근거였는데 결론은 다른 이유로 살아 있다"*를 정확히 갈라낸 지적이다. 이유를 **옛 이미지 경로**로 고치고, 처방대로 **기동 시 `secret_provider_mode=secret|static` 구조화 로그 한 줄**을 2-1에 넣어 **2-5 smoke에서 확인**한다(2-1이 이미 refresh 로그·`task_id`를 그 자리에서 구현하므로 추가 비용이 거의 없다). 리스크표에 행을 신설했다.

*개선 제안*

- [r11] codex-cli 개선 — **1-1a의 `TriggeredRules == 0 → 패턴 미매칭`을 확정 판정이 아니라 힌트로 낮춰라.** EventBridge의 CloudWatch 지표는 best-effort라 지연·누락될 수 있어([모니터링](https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-monitoring.html)) 0은 *"지표 누락"*·*"소스 이벤트 미도달"*도 포함한다 → **반영** — 이 지표는 r10에서 내가 claude-ide 개선으로 넣은 것인데 **"한 번의 조회로 갈린다"를 확정 판정으로 적었다.** *"0이면 패턴 미매칭 **쪽**"*으로 낮추고, **원문 없이 HCL 패턴을 바로 고치지 않는다**(추측 수정이 새 오류를 만든다)를 명시했다. `FailedInvocations > 0`은 원문 없이도 고칠 근거가 된다는 비대칭도 적었다. 어느 경로든 **1-6이 원문을 승계**하는 것은 그대로다.
- [r11] codex-cli 개선 / codex-ide 개선 [2인 동일] — **정상 회전의 `alb-target-5xx`는 "반드시 발화"가 아니라 "발화할 수 있음"이다.** 분당 33건은 **평균**이고 refresh 8초도 **상한**이라 refresh가 빨리 끝나면 5건 임계에 못 미친다 → **반영** — 1-5·2-6·리스크표 세 곳의 *"반드시"*를 내렸다. **2-6의 알람 정리 절차를 조건부**(발화한 경우에만)로 바꾸고, **발화 여부를 성공 판정에 쓰지 않는다**를 명시했다. codex-cli가 덧붙인 *"임계 조정 판단은 로컬 mock 기반 2-4보다 실제 AWS 지연·ALB 분배를 포함하는 2-6 관측을 우선하라"*는 절반만 채택했다 — 판단 **시점**은 2-5(2-4 실측 직후)로 두되, **2-6 관측이 나오면 그것으로 갱신**하는 것이 순서상 맞다. 2-4는 회복 창의 **길이**를 재는 유일한 자리이고 2-6은 실전 **건수**를 주므로 둘 다 쓴다.
- [r11] codex-ide 개선 — **앱 종료 시 provider가 `refreshCtx`를 명시적으로 취소하도록 lifecycle 경계를 둬라.** 상한이 있어 누수는 아니지만 `sql.DB.Close` 뒤 최대 8초 동안 작업이 남아 provider 상태를 갱신하는 상황을 테스트할 수 없다 → **반영** — provider에 `Close()`를 두고 `sql.DB.Close` 직후 호출하는 계약을 context 소유권 표에 넣고, 2-1 검증 ⑭로 고정했다.
- [r11] claude-ide 개선 — **`:933-934`의 "opener 점유도 없다"를 "무한 점유가 없다"로 좁혀라.** `database/sql`이 풀 보충으로 부르는 `Connect`는 **DB가 닫힐 때까지 사는 `db.ctx`**를 받으므로 `min(ctx 잔여, …)`가 제한이 되지 않고, 그 경로에서는 `Connect` 한 번이 **최악 13초 동안 opener를 점유**한다 → **반영** — *"이름이 사실보다 많은 것을 약속한다"*는 이 문서가 두 라운드 연속 고쳐 온 형태와 같다. **"이 설계가 주는 보장은 '유한'까지다"**로 고쳤다.
- [r11] claude-ide 개선 — **T0c 정의와 2-6 T1을 붙여 놓고, 값이 같으면 세대를 안 올려 로그도 안 난다는 것을 시계 표에 적어라** → **반영** — `T_visible` 행에 *"그동안 refresh는 성공하지만 값이 같아 세대를 올리지 않으므로 로그도 나오지 않는다"*를 넣었다. 2-6이 로그를 셀 때의 혼동이 닫힌다.
- [r11] claude-ide 개선 — **2-4의 503 실측을 런북에 반영하는 경로를 명시하라** → **반영** — claude-ide#3의 처방(런북 항목을 2-5로 이동)으로 함께 닫혔다. 2-4의 실측 항목에도 *"반영 단계는 2-5의 런북 갱신"*을 적었다. 계산값도 refresh 상한 10초 기준(**약 5~6건**)으로 갱신하면서 **"상한이고 평균이라 더 적을 수 있다"**를 함께 적었다.
- [r11] claude-ide 개선 — **`:907`의 "Secrets Manager 최악 6초"를 새 값으로 갱신하라** → **반영** — claude-ide#2와 함께 **"최악 8초"**로 고쳤다. 문장의 결론(*"1초 안에 끝날 수 없다"*)은 그대로다.

*병합 시점에 내가 추가한 것 (리뷰 지적 아님)*

- **⏰ 기한 표가 병합 시점에 이미 지났다.** revision 11은 08-26에 검토됐고 이 병합은 **2026-09-03**이라, 표의 *"08-29 18:00"*·*"08-30 09:00"*이 **지난 날짜**다. 이 plan 스스로 *"선언된 기한이 작성 시점에 이미 위반돼 있었다"*(r4)를 결함으로 다뤘으므로 같은 상태를 남길 수 없다. → 기한 표 아래에 **병합 시점 사실**과 **진행 전 [사람]이 `describe-secret`으로 세 날짜를 재확정하는 절차**를 적었다. 조건부 기한으로 바꾸지 않고 *"다시 읽어 절대일자로 박는다"*로 둔 것은 회고 A-11의 요구다.

**[r11] 병합 자기점검**:
- **이번 결함 6건 중 5건이 revision 11에서 내가 새로 쓴 T0 삼분할과 그 산술에서 나왔다.** r10 자기점검이 *"큰 설계 변경을 한 라운드에 넣으면 그 변경이 다음 라운드의 결함 목록이 된다"*고 적고 **처방까지 적어 뒀는데**(*"바뀐 절이 참조하는 다른 절을 명시적으로 다시 읽는다"*) **이번에 그 처방을 실행하지 않았다.** T0 표를 쓸 때 배경 "왜 죽는가"(기존 세션 5분)와 Step 1의 bounded wait(전파 지연) 두 절만 다시 읽었으면 codex-cli#1·codex-ide#1은 둘 다 잡혔다. **처방을 적는 것과 실행하는 것은 다르다.**
- **claude-ide#1이 특히 아프다** — *"revision 10이 refresh를 시작시킬 probe를 빼먹었다"*고 **내가 직접 쓴 세 줄 아래**에서 같은 항을 또 빠뜨렸다. 지적을 문장으로 옮기는 것과 그 지적을 새 산술에 적용하는 것이 분리돼 있었다.
- **codex-ide#2는 "증거의 종류"를 처음 문제 삼은 지적이다.** 아홉 라운드 동안 *"어떤 로그를 남기는가"*는 여러 번 다듬었지만 *"그 로그가 무엇을 증명하는가"*는 검토되지 않았다. refresh 로그가 캐시 갱신까지만 증명한다는 것은 시계 표 자신이 이미 적고 있었는데, 2-6이 그것을 복구 증거로 쓰는 모순은 **두 절을 나란히 읽어야만** 보인다. → **앞으로 판정 기준을 쓸 때 "이 신호가 증명하는 것"과 "판정하려는 것"을 한 줄로 나란히 적는다.**
- **revision 12에서 내가 새로 쓴 것**: 다섯 시각 시계 표, 시도당 예산 표, `credential_recovered` 계약, 2-6 통제 창 8단계, 2-5의 런북 갱신 항목(신설 6번), 기한 재확정 절차. **이번에는 처방대로** 각각이 참조하는 절(배경 "왜 죽는가" · Step 1 예산 표 · 2-1 검증 목록 · 1-5 · 리스크표)을 다시 읽고 숫자와 이름을 맞췄다 — 그래도 다음 라운드의 1차 확인 대상은 이 여섯이다.

**[r10] revision 10 → 11** (3인 전원 제출, 전부 `reviewed-revision: 10`, 전부 `request-changes`.
**반영 17 · 부분 기각 1**. 지적이 **Step 2 하나에 몰린 첫 라운드**다 — Step 1(handler 계약·1-1a·예산 표)은
3인 모두 *"이번에 손댈 것이 없다"*고 평가했다. 인용 코드 사실은 이번에도 전수 실측했고,
**그중 하나는 리뷰 쪽이 틀렸다**(아래 claude-ide#4)):

*결함*

- [r10] codex-cli#1 / codex-ide#1 [높음, 2인 동일] — **"무트래픽 최대 약 40초"가 상태 기계 전체를 세지 않았고, 목표·2-4·2-6의 T0가 서로 다르다.** 40초는 *"이미 시작된 refresh 한 번이 곧바로 새 값을 얻는 경우"*만 센다. 빠진 것 셋: ① **회전 중 창**에서 첫 refresh가 **같은 값**을 얻고 끝나는 경우(그 창의 길이는 이 plan이 통제하지 못한다), ② **refresh를 시작시킬 probe 한 번**(revision 10은 소비할 probe만 셌다 — codex-ide는 이 누락으로 최악이 `30+8+30+dial ≈ 69초`가 된다고 계산했다), ③ provider backoff 대기 → **반영** — **T0를 셋으로 나눴다**: **T0a**=첫 `28P01`(=`setSecret`) / **T0b**=`AWSCURRENT` 이동(`finishSecret`) / **T0c**=refresh 성공 로그. 그리고 **T0a→T0b 구간은 상한이 없다고 명시**했다 — Secrets Manager 회전 lambda 내부라 제어도 관측도 못 하고, **Step 1도 이 구간을 못 받는다**(라벨 확인이 `UpdateService`의 선행 조건이라 똑같이 `AWSCURRENT` 이동을 기다린다). 이건 0008 `:472`가 이미 *"무중단 보장 안 됨"*으로 승계한 항목이라 **리스크표에 잔여 위험으로 명시**했다. T0b 기준 무트래픽 회복은 **약 20초**(backoff ≤5 + refresh ≤8 + 다음 probe ~2 + dial ≤5)로 재산정했다. **"관측 목표이지 SLA가 아니다"**도 적었다 — Route53 체커가 비조정이라 엄밀한 상한이 아니다. 목표 절·2-4·2-6에 **각자 재는 구간이 다르다**는 문장을 넣었다.
- [r10] codex-cli#2 / codex-ide#2 [높음/중간, 2인 동일] — **같은 2-0 안에서 context 소유권이 정면 충돌한다.** (가)는 *"backoff와 SDK 호출은 전달받은 `context.Context` 취소를 따른다"*, (나)는 *"SDK 호출은 detached 8초 context에서 완료돼야 한다"* → 구현자가 앞 문장을 따르면 **revision 9의 무한 실패가 그대로 재발**한다 → **반영** — **context 소유권 표 하나를 단일 계약으로 못박았다**(dial#1·대기·dial#2 = caller `ctx` / `GetSecretValue` = provider 소유 `refreshCtx`(8초, `context.Background()` 파생) / backoff 게이트 = context 없음). *"다른 곳의 문장이 이와 다르면 이 표가 이긴다"*를 명시하고 (가)의 충돌 문장을 교체했다. **codex-cli의 `singleflight` 지적도 반영**: 동기 `Do`로 기다리면 호출자가 묶이므로 **`DoChan`**(또는 동등한 비동기 채널) + `select`로 `ctx.Done()`과 경합해야 *"호출자는 즉시 반환하지만 작업은 계속됨"*이 성립한다. **codex-ide의 경계 지적도 반영**: `refreshCtx`는 **요청 값을 담지 않는다**(`database/sql`이 요청과 무관하게 `Connect`를 부른다). 2-1 ⑪에 **취소 시점(첫 dial이 `28P01`을 반환한 뒤)**을 명시해 테스트가 다른 경로를 우연히 통과하지 않게 했다.
- [r10] claude-ide#1 [중간] — **같은 문서가 canary probe 간격을 30초와 "분당 33건"(≈1.8초) 두 값으로 쓰고, 둘이 각각 다른 결론을 떠받친다.** *"다음 probe 최대 30초"*를 유지하면 **08-30을 Step 1 없이 넘기기로 한 판단의 근거**(분당 33건 → `alb-target-5xx` 고정)가 무너지고, 33건/분을 유지하면 회복 시간이 내려간다 → **반영** — **33건/분(평균 약 1.8초)이 실측이므로 그것으로 통일**했다(ADR 0004, 2026-08-16 장애 증거 시간당 1,976건). `regions` 미지정이라 전체 리전 체커가 각 30초 주기로 **비조정** 프로빙한 결과라는 설명을 회복 시간 절에도 옮기고, **파라미터 표에 행을 추가**해 세 곳이 같은 값을 보게 했다. 동시에 **"평균이지 상한이 아니다"**(codex-cli#1·codex-ide#1·r9 codex-ide#4가 공통으로 든 Route53 비조정 사실)를 함께 적었다. r9에서 이 사실을 2-6에만 적용하고 회복 시간 표에는 적용하지 않은 것이 이번 불일치의 원인이다.
- [r10] claude-ide#2 [중간] — **Step 2가 정상 동작해도 회전마다 `alb-target-5xx`가 뜨는데 plan 어디에도 그 결과가 없다.** refresh가 도는 동안 도착한 `/readyz`는 전부 503이고, 분당 33건이면 8초 창에 **4~5건**, 2-6의 *"1초 간격 반복 호출"*이면 **8건 이상**이다. 기존 알람은 **Sum ≥ 5 / 5분**이라 임계를 넘는다 → **반영** — 실측 확인했다(`monitoring.tf:56-60`). 두 곳을 채웠다: **(a) 1-5 런북**에 *"회전 직후 수 초~수십 초의 5xx는 Step 2가 동작 중이라는 신호다. 브릿지 기준은 건수가 아니라 **지속 시간과 `/readyz` 회복 여부**"*를 적고, **(b) 2-6 체크리스트에 1-4와 같은 알람 정리 단계**를 넣었다. 임계 조정은 **2-4 실측 뒤 별도 판단**으로 남긴다. 리뷰어가 확인한 **ALB health check path가 `/healthz`라 타깃 헬스에는 영향이 없다**(`alb.tf:30`)도 명시했다. **이 plan이 1-1a에서 스스로 경계한 것과 같은 종류인데 여기서는 기존 알람이라 신뢰 손상이 더 크다**는 지적이 정확하다.
- [r10] codex-ide#3 [중간] — **`DB_SECRET_ARN` 누락이 production에서도 정적 모드로 조용히 폴백해, Step 2가 배포되지 않아도 smoke가 통과한다.** apply 순서 실수·잘못된 task definition base·env 유실이면 새 앱이 **구 비밀번호로 정상 기동**하고 `/readyz` 200·302까지 통과해 **다음 회전 때까지 부재가 드러나지 않는다** → **반영** — 실측 확인했다(`ecs.tf:40`의 `APP_ENV=production`, `config.go`의 *"운영 모드에서 DB가 없으면 조용히 인메모리로 떠 데이터가 증발하는 사고를 막는다(fail-fast)"*). **같은 자리에 같은 형태로** `APP_ENV=production && DB_SECRET_ARN==""` → 기동 오류를 추가하고 설정 단위 테스트로 두 분기를 고정한다. **2-5에 CI 배포 전 게이트**(최신 ACTIVE task definition의 `environment` 확인)를 넣고, smoke 항목에 *"smoke만으로는 Step 2 배포를 증명하지 못한다"*를 적었다.
- [r10] codex-ide#4 [낮음] — **`MaxBackoffDelay`는 `retry.StandardOptions`의 필드가 아니다.** 실제 필드는 `MaxAttempts`·**`MaxBackoff`**이고 `MaxBackoffDelay`는 **`retry.AddWithMaxBackoffDelay` 래퍼 함수** 이름이다. 그대로 옮기면 **컴파일이 깨진다** → **반영** — `retry.AddWithMaxBackoffDelay(retry.NewStandard(func(o *retry.StandardOptions) { o.MaxAttempts = 2 }), 2*time.Second)` 형태로 고치고, `config.WithRetryer` 팩토리가 **호출마다 새 retryer를 반환**해야 한다는 조건도 적었다.
- [r10] claude-ide#3 [낮음] — **2-6에 같은 절차가 두 번 있다**(둘 다 같은 근거·같은 문장·같은 상한). 체크리스트로 쓰는 문서라 *"두 번 호출하라는 것인가"*로 읽힐 여지가 있다 → **반영** — 절차·상한·실패 처리가 더 완전한 뒤쪽 블록만 남기고 앞 블록은 참조로 줄였다(*"이 문서에서 유일한 절차 기술이다"* 명시). 병합 과정에서 기존 불릿을 고치면서 새 불릿도 추가한 흔적이 맞다.
- [r10] claude-ide#4 [낮음] — **`health.go` 참조 3건이 실제 파일과 어긋난다**(`readinessTimeout`이 19행이 아니라 20행, 주석 블록이 11-18이 아니라 10-19) → **부분 기각 + 부분 반영** — 실측한 결과 **`readinessTimeout = 1 * time.Second`는 실제로 19행이 맞다**(`grep -n` 확인). plan의 `:19`는 **정확했고 리뷰 쪽이 한 줄 밀렸다.** `:42-43`도 맞다. 다만 **주석 블록의 시작이 10행**(plan은 `:11-18`)인 것은 리뷰가 맞아 **`:10-18`로 정정**했다. 지적의 취지 — *"줄 번호로 코드를 지목하는 관례가 이 문서의 검증 가능성을 지탱하므로 새 인용은 한 번 더 확인하라"* — 는 그대로 수용한다. r9 자기점검이 *"리뷰의 사실 주장도 같은 기준으로 확인해야 한다"*고 적었는데, **이번에 그 절차가 실제로 반증을 잡아냈다.**

*개선 제안*

- [r10] codex-cli 개선 — **EventBridge target과 Lambda async의 `maximum_event_age_in_seconds`는 서로 다른 계층의 독립 상한**이라 둘을 같게 했다고 end-to-end 창이 1시간으로 확정되지 않는다 → **반영** — EventBridge가 마지막에 전달한 이벤트가 Lambda 큐에서 다시 최대 1시간 대기할 수 있으므로 **end-to-end 상한을 약 2시간**으로 고쳤다. **TTL 7일이 충분하다는 결론은 그대로다**(2시간 대비 84배). *"1시간/168배"* 표현만 정정.
- [r10] codex-cli 개선 / codex-ide 개선 — **fake 주입만으로는 CI에 boto3를 안 넣는 계약이 성립하지 않는다** — `handler.py`가 module scope에서 `import boto3`하면 **테스트 수집 단계에서 실패**한다 → **반영** — **client factory를 handler 인자로 받고 기본 factory 안에서만 지연 import**한다고 고정했다. `sys.modules` monkeypatch가 필요 없어진다.
- [r10] codex-ide 개선 — **`go test -race`를 명시하라**(password·generation·backoff 상태와 detached goroutine을 여러 `Connect`가 동시에 만진다) → **반영** — 2-1 검증과 CI 명령에 넣었다.
- [r10] claude-ide 개선 — **1-1a 3분기 판정에 `TriggeredRules`를 추가하라** — `FailedInvocations` 하나로는 *"rule이 아예 매칭 안 함"*과 *"매칭·전달은 됐는데 로그가 없음"*을 구분할 수 없다(둘 다 0) → **반영** — `get-metric-statistics` 한 번으로 분기 2를 갈리게 했다(**0이면 패턴 미매칭 / >0인데 로그 비었으면 전달 경로**).
- [r10] claude-ide 개선 — **`Connect` 4단계의 대기가 `/readyz` 경로에서는 실질 0이라는 것을 적어라** — 1초 ctx에서 dial#1 뒤 남는 시간이 수백 ms라 *"3초 대기"*가 실행되지 않는다. 오해하면 `/readyz`가 Route53의 2초를 넘겨 **`health.go`의 1초가 막으려던 상황을 만든다** → **반영**.
- [r10] claude-ide 개선 — **2-4에 "정상 회전 시 5xx가 얼마나 나는가"를 실측 항목으로** — 결함 2의 4~5건은 계산이고 2-4가 회복 창의 실제 길이를 잴 수 있는 유일한 자리다 → **반영** — 1-5 런북 문장이 **추정이 아니라 실측**이 된다.
- [r10] claude-ide 개선 — **1-1 검증에 "신설 알람 6개"를 숫자로 적어라**(7개 중 1개는 1-1a 소속이라 표의 "작성 단계" 열에만 있었다) → **반영** — `plan` 출력 대조가 기계적으로 된다.

**[r10] 병합 자기점검**:
- **처음으로 리뷰 지적 하나를 실측으로 반증했다**(claude-ide#4의 `health.go:19`). r9 자기점검이 *"리뷰의 사실 주장도 같은 기준으로 확인해야 반영이 근거를 갖는다"*고 적은 절차가 **한 라운드 만에 실제로 작동**했다. 만약 그대로 반영했으면 **맞는 줄 번호를 틀린 것으로 바꿨을** 것이다.
- **이번 라운드의 결함은 전부 revision 9→10에서 내가 새로 쓴 문장에서 나왔다** — detached refresh(context 충돌), 40초(상태 기계 누락), canary 30초(같은 문서 안 불일치), `MaxBackoffDelay`(존재하지 않는 API). **큰 설계 변경을 한 라운드에 넣으면 그 변경이 다음 라운드의 결함 목록이 된다.** r8·r9에서도 같았다(신설 1-1a → r9에 구멍 셋). → **설계를 바꾼 라운드에는 "바뀐 절이 참조하는 다른 절"을 명시적으로 다시 읽는다.** 이번 canary 30초는 `:774`를 한 번만 다시 읽었으면 잡혔다.
- **"회전 중 창은 어느 쪽도 못 받는다"를 처음으로 문서에 적었다.** 아홉 라운드 동안 Step 1과 Step 2가 서로를 백스톱한다고 적어 왔는데, **둘 다 `AWSCURRENT` 이동을 기다린다**는 공통 전제는 이번에 처음 드러났다. 0008 `:472`가 이미 넘긴 항목이었고 이 plan이 **승계 표에 적어 놓고도** 회복 시간 계산에는 넣지 않았다.
- revision 11에서 새로 쓴 문장 — T0 삼분할과 T0a 상한 부재, context 소유권 표, `DoChan`, canary 평균 1.8초, production `DB_SECRET_ARN` fail-fast, `alb-target-5xx` 예상 발화와 런북 문장, `retry.AddWithMaxBackoffDelay`, `TriggeredRules` 분기 — 은 다음 라운드에 검증받아야 한다. 특히 **T0b 기준 20초 산술**과 **context 소유권 표가 2-0의 다른 문장들과 충돌하지 않는지**가 집중 검토 대상이다.

**[r9] revision 9 → 10** (3인 전원 제출, 전부 `reviewed-revision: 9`, 전부 `request-changes`. **기각 0건**.
**이번 라운드의 지적은 전부 "리소스·입력·시간이 실제로 존재하는가"**였다 — 설계 논리가 아니라
**구현 가능성**을 세 리뷰어가 각기 다른 지점에서 찔렀다. 인용된 코드 사실 5건은 병합 전 전부 실측 확인했다:
`health.go:19`의 1초, `ecs.tf:1`·`:71-74`의 비기본 클러스터, `eventbridge.tf:14`·`:74`의 rule/target 분리,
`monitoring.tf:62-63`의 `alarm_actions` 관례, `.gitignore`의 Python 항목 부재):

*결함*

- [r9] codex-cli#1 [높음] — **Lambda가 ECS `cluster`·`service`를 어디서 얻는지 계약이 없어 `UpdateService`를 구현할 수 없다.** 이벤트가 주는 값은 시크릿 ARN·라벨·`versionId`뿐이고, IAM을 서비스 ARN으로 제한한 것은 **요청 파라미터를 채워 주지 않는다.** 그대로 구현하면 하드코딩 여부가 실행자 판단으로 남거나 `cluster` 누락으로 `default` 클러스터를 봐 `ServiceNotFoundException`이 난다 → **반영** — 실측 확인했다: 이 서비스는 `aws_ecs_cluster.main` 소속이라(`ecs.tf:1`, `:71-74`) **`cluster` 생략은 반드시 실패한다.** **Terraform이 `ECS_CLUSTER_ARN`·`ECS_SERVICE_NAME`·`IDEMPOTENCY_TABLE_NAME`·`SECRET_ARN` 네 개를 환경변수로 주입**하고(전부 non-secret), handler는 **기동 시 빈 값이면 즉시 실패(fail-fast)**하도록 계약했다. 1-2에 **(j) 요청 파라미터 검증**(정확한 `cluster`·`service`·`forceNewDeployment=True`)을 넣고, 1-3 검증에 `Environment.Variables` 확인을, 리스크표에 한 행을 추가했다. **아홉 라운드 동안 아무도 이 입력을 묻지 않았다** — 설계 논쟁에 가려 *"그래서 무엇을 호출하는가"*가 비어 있었다.
- [r9] codex-ide#1 [높음] — **1-1a의 "세 리소스"에 `aws_cloudwatch_event_target`이 빠져 있다.** Terraform에서 rule과 target은 별도 리소스이고 이 저장소도 그 선례를 따르는데(`eventbridge.tf:14`/`:74`), 셋만 만들면 **`describe-rule`·resource policy 검사는 전부 통과하면서 target이 0개라 08-30 원문을 한 건도 얻지 못한다** → **반영** — 실측 확인했다. **범위를 다섯 리소스로 고쳤다**(rule + **Logs target** + log group + resource policy + `FailedInvocations` 알람). 검증에 **`list-targets-by-rule`로 target ARN 단언**을 넣었다 — 이 확인이 없으면 결함이 **08-31까지 드러나지 않는다.** 신설 단계가 자기 목적을 달성하지 못하는 형태였다.
- [r9] codex-cli#2 [높음] — **`/readyz`의 1초 context가 계획한 refresh/dial 예산보다 짧아 provider 수준의 "포기 없음"이 성립하지 않는다.** 무트래픽에서 `Connect`를 일으키는 유일한 주체가 이 canary인데(그래서 백그라운드 refresher를 명시적으로 배제했다), 매 시도가 1초에 취소되면 **30초 뒤 같은 1초 context로 다시 취소되는 무한 루프**다 → **반영** — 실측 확인했다(`health.go:19` `readinessTimeout = 1 * time.Second`, `:42-43`이 `PingContext`로 전달). codex-cli가 제시한 세 선택지 중 **"refresh 결과를 다음 호출로 승계"**를 택한다. **`/readyz`의 1초를 늘리는 것은 선택지가 아니다** — 그 값은 Route53이 2초 안에 2xx를 요구하기 때문에 **의도적으로 1초**이고(`health.go:11-18` 주석, ADR 0004 §1-a) 0008이 방금 그 경로로 apply됐다. → **refresh를 호출자 ctx와 분리된 detached context(8초 상한, 단일 비행)에서 돌리고**, `Connect`는 `min(ctx 잔여, 3초)`만 기다린 뒤 반환한다. **detached refresh는 상시 goroutine이 아니다**(28P01을 볼 때만 뜨고 8초 안에 반드시 끝난다) — 2-1 ⑦과 충돌하지 않고 `driver.Connector` 취소 계약도 지킨다. **목표 문구도 정직하게 고쳤다**: *"수 분 → 수 초"* → **"트래픽 있으면 수 초, 무트래픽이면 최대 약 40초"**(refresh 8 + 다음 probe ≤30). 2-1 ⑪과 **2-4에 "실제 `readinessHandler`의 1초 경로" 게이트**를 넣었다 — 여기서 실증하지 않으면 2-6 실전에서 처음 확인하게 된다.
- [r9] codex-cli#3 / codex-ide#4 [중간, 2인 동일] — **`Connect` 최악 11초가 두 dial 중 하나와 backoff를 누락했다.** 같은 요청에서 복구하려면 `옛 값 dial → 28P01 → refresh → 새 값 dial`이라 최소 16초이고, AWS SDK for Go v2 standard retryer의 **기본 최대 backoff는 20초**라 시도 수만 2로 고정해도 operation 전체가 6초에 끝나지 않는다 → **반영** — **`Connect` 1회 = dial 5 + refresh 대기 3 + dial 5 = ≤ 13초**로 다시 합산했다. Secrets Manager는 **operation 전체에 `context.WithTimeout(detached, 8초)`**를 걸고 `retry.NewStandard`로 **`MaxAttempts = 2`·`MaxBackoffDelay = 2초`**를 명시한다 — **operation context가 실제 강제 장치**다. **codex-ide#4의 "provider backoff를 누가 기다리는가"에도 답했다**: 아무도 `Connect` 안에서 기다리지 않는다. backoff는 *"다음 refresh를 언제 허용할지"*의 게이트라 창 안이면 refresh를 시작하지 않고 즉시 반환한다. **2-4 상한도 10초 → 15초**로 고쳤다 — revision 9의 10초는 자기가 선언한 `Connect` 최악 11초보다도 짧아 **자기 계약과 모순**이었다.
- [r9] codex-ide#3 [중간] — **알람 7개에 `dimensions`·`statistic`·`period`·`threshold`·`alarm_actions`가 없다.** 특히 action이 없으면 **상태만 바뀌고 Slack 카드도 수동 브릿지도 시작하지 않아**, *"알람 7개가 자동화 실패를 받는다"*는 이 plan의 완화가 **성립하지 않는다** → **반영** — 실측 확인했다(`monitoring.tf:62-63` 등 기존 알람은 전부 `alarm_actions`/`ok_actions = [aws_sns_topic.alarms.arn]`). **7행 설정 표**를 만들어 namespace/dimension(`RuleName`·`FunctionName`·`QueueName`)/stat/period/eval/threshold/`treat_missing_data`/작성 단계를 확정하고, **7개 전부 기존 `alarms` SNS에 연결**했다(새 토픽을 만들지 않는다). 1-3 검증에 `describe-alarms`의 `ActionsEnabled`·`AlarmActions`·`Dimensions`까지 대조를 넣었다.
- [r9] codex-ide#2 [중간] — **광역 Logs target의 resource policy principal이 AWS 공식 계약보다 좁다.** 공식 예제는 `events.amazonaws.com`과 **`delivery.logs.amazonaws.com`**을 함께 두고 resource도 stream 범위(`:*`)까지 지정하는데 revision 9는 principal이 하나뿐이었다 → **반영** — principal 둘·action 둘·resource를 `:*`까지로 계약에 넣고, 1-1a/1-3의 `describe-resource-policies` 검증을 **문서 존재 확인이 아니라 principal·action·resource 값 단언**으로 바꿨다. 결함 1을 고쳐 target을 붙여도 이게 좁으면 **첫 실이벤트가 `FailedInvocations`로 끝난다.**
- [r9] claude-ide#1 [중간] — **1-1a가 apply할 HCL을 누가 언제 작성하는지가 없다.** 단계 순서가 **1-1a(apply) → 1-1(작성)**이라 작성 단계가 apply 뒤에 온다 → **반영** — **Step 2에서 r8 codex-cli#4로 방금 고친 것과 같은 형태의 결함**이라는 지적이 정확하다. **1-1a를 "1-1a-작성 [에이전트] → 1-1a-apply [사람]"으로 쪼개고**, 2-5와 같은 수준으로 **apply 시점의 작업 트리 상태**를 명시했다(*"`-target`도 임시 revert도 쓰지 않는다"*).
- [r9] claude-ide#2 [중간] — **1-1a → 1-3 게이트에 "원문이 오지 않은 경우"의 분기가 없어 09-06 기한이 외부 이벤트에 묶였다.** 로그 그룹이 비는 경우가 셋(회전이 안 옴 / 배선 침묵 / 매칭 실패)인데 구분할 장치도 없고, 1-3이 오지 않는 원문을 기다리면 **이 plan이 가장 강하게 금지한 조건부 기한**이 된다(회고 A-11) → **반영** — 제안대로 **광역 rule `FailedInvocations` 알람을 1-1a로 앞당기고**(그래야 *"조용함 = 회전 없음"*과 *"조용함 = 배선 침묵"*이 구분된다), **판정 시각을 2026-08-31 09:00 KST로 절대일자화**하고 3분기 폴백을 적었다. **어느 경로에서도 09-06 기한은 유지한다**를 못박았다 — 1-1a는 *"있으면 좋은 선행 증거"*지 1-3의 차단 게이트가 아니다.
- [r9] claude-ide#3 [중간] — **예산 표의 backoff가 같은 설정인데 행마다 다르다**(행 1·4는 1초, 행 3은 2초). 2초가 맞으면 행 1·4의 최악이 6초라 예산 5초를 넘어 **단계 deadline이 정상적인 스로틀 재시도 한 번을 잘라내고**, `PutItem` 쪽은 (f) 경로를 타 **중복 재배포 + `Errors` 알람**으로 승격된다 → **반영** — **backoff 2초로 통일하고 행 1·4 예산을 6초로, 여유를 15 → 13초로** 조정해 합계 105초를 유지했다. 그리고 **botocore의 backoff 상한은 `Config`로 지정할 수 없다**는 사실을 명시해, 이 수치가 **가정이고 실제 강제 장치는 단계별 monotonic deadline**임을 적었다. r8 자기점검이 이 산술을 집중 검토 대상으로 지목했는데 **행 사이에서 어긋나 있었다.**
- [r9] claude-ide#4 [낮음] — **`task_id`만 2-6에 구현 지시가 남았고, 게다가 `/task` 경로가 없는 정정 전 문구다.** 같은 라운드에 refresh 로그는 2-1로 올렸는데 이것은 안 올렸다 → **반영** — 2-6을 **판정 방법(+`list-tasks` 집합 대조)만** 남기고 구현은 2-1 참조로 줄였다. 구현자가 base URI를 조회하면 `TaskARN`이 없어 `task_id=unknown`이 상시 발생하고, 그건 plan이 *"판정 불가"*로 규정한 상태다.
- [r9] codex-cli#4 [중간] — **2-6의 "5분 안에 풀 전체 교체 + canary가 두 태스크 모두 자극"은 사용하는 API가 주지 않는 보장이다.** `SetConnMaxLifetime`은 **최대 재사용 시간**을 정할 뿐이고 cleaner는 idle만 닫으며, Route53 헬스체커는 서로 조정되지 않아 타깃별 도달 보장도 없다. plan 스스로 바로 다음 문장에서 *"한 태스크에만 도달할 수 있다"*고 인정하면서 자극 불요를 결론냈다 → **반영** — *"자극 불요"* 결론을 **철회**하고, **[사람]이 `/readyz`를 1초 간격으로 반복 호출**해 **두 `task_id`가 모두 관측될 때까지**(상한 10분) 확인하는 절차로 바꿨다. 읽기 전용·비파괴이고 canary가 이미 30초마다 하는 것과 **같은 요청**이라 `pg_sleep` 진단 엔드포인트를 거부한 근거(새 표면 금지)와도 충돌하지 않는다.

*개선 제안*

- [r9] codex-cli 개선 — **1-7의 `deployments[]` 증거는 회전 뒤 CI 배포가 한 번 더 일어나면 사라진다** → **반영** — 검증 창 **배포 동결**을 런북 체크 항목으로 넣거나, 어려우면 **Lambda 로그의 PRIMARY deployment id를 ECS 서비스 이벤트 이력·CloudTrail과 대조**하는 대안을 함께 적었다.
- [r9] codex-cli 개선 — **`.gitignore`에 Python 항목이 없다** → **반영**(실측 확인) — `__pycache__/`·`*.pyc`를 zip 산출물과 같은 커밋에 추가한다. 이 저장소의 첫 Python 코드다.
- [r9] codex-ide 개선 — **handler 단위 테스트를 어느 CI 단계가 실행하는지 없다**(현재 워크플로는 Go 테스트만) → **반영** — 표준 라이브러리 `unittest`로 끝내고 **PR CI에 짧은 Python 테스트 단계**를 넣는다. CI에 boto3를 설치하지 않도록 **의존성을 handler 인자로 주입**하는 경계도 함께 적었다.
- [r9] codex-ide 개선 — **2-2a의 "작업 트리에 `skip_destroy`만"이 앱 코드와 충돌해 읽힌다** → **반영** — *"Terraform/HCL diff에 `skip_destroy`만"*으로 범위를 좁히고 *"앱 변경을 임시 revert하라는 뜻이 아니다"*를 명시했다.
- [r9] claude-ide 개선 — **1-2 (i)가 (a)와 구분되지 않는다**(handler가 rollout을 관측하지 않으므로 fake에 줄 입력이 없다) → **반영** — *"`rolloutState=IN_PROGRESS`여도 `redeploy_submitted`로 marker를 쓴다"*로 다시 썼다. `rolloutState`는 응답에 실제로 들어 있어 fake로 줄 수 있고 같은 것을 증명한다.
- [r9] claude-ide 개선 — **1-1a의 `retention_in_days`를 1-1에서 바꾸지 않는다고 적어라**(안 그러면 "변경 0" 게이트가 오탐) → **반영**.
- [r9] claude-ide 개선 — **`get-account-settings` 시점을 "1-1 작성 전"으로** → **반영**(예약 포함 여부는 리소스 인자라 HCL 작성 시점에 결정된다).
- [r9] claude-ide 개선 — **1-4 ①이 남기는 marker가 실제 `AWSCURRENT` 버전 것임을 적어라** → **반영** — 실해는 없지만 멱등 테이블 잔여 위험과 겹치는 유일한 실사례라 1-6·1-7의 로그 판독 혼동을 막는다.

**[r9] 병합 자기점검**:
- **아홉 라운드 동안 "handler가 무엇을 호출하는가"의 입력이 비어 있었다**(codex-cli#1). 멱등·fail-open·예산·marker 의미는 여섯 라운드를 돌았는데, **`UpdateService(cluster=?, service=?)`의 물음표는 아무도 묻지 않았다.** 설계 논쟁이 정교해질수록 *"그래서 이 함수의 입력은 어디서 오는가"*가 시야에서 사라졌다. → **다음 라운드부터 각 외부 호출마다 "인자 하나하나가 어디서 오는가"를 표로 적는다.** 이번에 환경변수 표를 만든 형식이 그것이다.
- **새 단계(1-1a)가 두 라운드 연속으로 자기 구멍을 만들었다** — r8에서 신설했고 r9에서 **target 누락(목적 자체가 무산)·HCL 작성 주체 부재·폴백 부재** 셋이 나왔다. 신설 단계는 기존 단계와 달리 **누적된 리뷰를 거치지 않았다**는 것이 원인이다. → **앞으로 단계를 신설하면 그 단계만 따로 "리소스 목록 / 작성 주체 / apply 주체 / 실패 시 분기"를 자문한다.**
- **이번 라운드는 처음으로 리뷰가 인용한 코드 사실을 병합 전에 전수 실측했다**(5건 전부 확인, 반증 0). r8 자기점검이 *"이름이 사실보다 많은 것을 약속한다"*를 재발 패턴으로 지목했는데, **리뷰의 사실 주장도 같은 기준으로 확인해야** 반영이 근거를 갖는다.
- revision 10에서 새로 쓴 문장 — 환경변수 네 개와 fail-fast, 1-1a 다섯 리소스와 08-31 판정 폴백, 알람 7행 설정 표, backoff 2초 통일, detached refresh 계약과 회복 시간 표(수 초 / 약 40초), `/readyz` 반복 호출 절차 — 은 다음 라운드에 검증받아야 한다. 특히 **detached refresh가 `database/sql`의 opener·취소 계약과 실제로 충돌하지 않는지**와 **1-1a 다섯 리소스가 1-1의 "변경 0" 게이트를 통과하는지**가 집중 검토 대상이다.

**[r8] revision 8 → 9** (3인 전원 제출, 전부 `reviewed-revision: 8`, 전부 `request-changes`. **기각 0건**.
**Step 1 handler 계약은 3인 모두 "내부적으로 일관된다"고 평가**했고, 남은 지적은
**① 마커의 의미 ② AWS 실제 재시도 동작 ③ 개정이 Step 2·`[사람]` 단계까지 전파되지 않은 것** 셋으로 갈렸다):

*결함*

- [r8] codex-cli#1 [높음] — **`UpdateService` 응답의 PRIMARY를 "배포 완료"로 간주해 rollout 실패를 완료 marker로 봉인한다.** PRIMARY는 *"가장 최근 배포"*라는 지위일 뿐이고 새 배포는 `IN_PROGRESS`로 시작해 `COMPLETED` 또는 (서킷브레이커 사용 시) `FAILED`가 된다. 새 태스크가 시크릿 주입·헬스체크에서 실패해 롤백돼도 marker는 남고, 같은 `versionId`의 후속 전달은 전부 skip되며, 최초 호출도 성공 반환했으므로 **on-failure 경로도 타지 않는다** → **반영** — codex-cli가 제시한 두 선택지 중 **"제출됨으로 이름을 바꾸고 rollout 실패 시 자동 재시도 부재를 명시"**를 택한다. rollout 추적은 **원리적으로 불가능**하다(롤링 수 분 vs 함수 예산 105초 — 추적하려면 함수를 수 분 돌리거나 Step Functions/deployment state change 이벤트를 새로 도입해야 하고, 기한 안에서 그 범위를 지지 않는다). **marker와 `outcome`을 `redeploy_submitted`로 명명**해 *"`UpdateService`가 200을 반환했다"*까지만 뜻하게 하고, rollout 실패는 **서킷브레이커 + 기존 ALB/ECS 알람 + 0008 탐지 → 수동 브릿지**가 받는다고 리스크표에 한 행으로 명시했다. 1-2 (i)로 *"marker가 남고 후속이 skip된다"*를 **의도임을 문서화하는 테스트**로 고정했다. **claude-ide 개선의 같은 지적**(*"PRIMARY id를 비교할 기준이 handler에 없다"*)도 함께 반영해 *"자기 성공으로 오인할 수 없다"*는 문장을 **"PRIMARY id는 사후 추적용 로그"**로 정정했다 — r7 결함 1과 같은 형태(이 설계가 주지 않는 보장을 약속)였다.
- [r8] codex-cli#2 / codex-ide#1 [중간, 2인 동일] — **boto3 `connect_timeout`·`read_timeout`만으로는 구간 예산이 강제되지 않는다.** 둘은 한 번의 연결/읽기 시도 제한이고 botocore는 별도 횟수·지수 backoff로 재시도하므로 `read_timeout=20초`만 정해서는 그 구간이 20초에 끝나지 않는다. `remaining < 15초` 검사를 호출 전에 해도 **이미 시작한 재시도를 끊지 못해 120초에 강제 종료**되고 약속한 `failed_deadline` 로그조차 안 남는다 → **반영** — 예산 표에 **`Config(retries={"mode":"standard","total_max_attempts":N})` 열과 "최악 실행시간" 열**을 추가해 `시도 수 × (connect+read) + backoff`가 구간 상한 안에 들도록 값을 정했다(`GetItem`/`PutItem` 1·1·2회 → 5초, `UpdateService` 2·6·2회 → 18초 ≤ 20초). **단계별 monotonic deadline**을 강제 방식으로 넣고, SDK 호출 진입 전마다 `min(단계 잔여, 함수 잔여-15초) < 그 호출의 최악 실행시간`이면 중단하도록 했다 — codex-cli가 든 *"16초 남았을 때 20초 read에 진입"* 사례가 정확히 이 검사로 막힌다. 테스트 (h)를 **①진입 차단 / ②느린 SDK가 단계 deadline을 넘김** 둘로 나눴다.
- [r8] codex-ide#2 [중간] — **재시도 두 계층 중 EventBridge target `retry_policy`가 정해지지 않았다.** 명시하지 않으면 기본이 **최대 24시간·185회**라 revision 8이 TTL 근거로 쓴 *"멱등 창의 실질 상한은 1시간"*과 *"7일이면 168배 여유"*가 **성립하지 않는다** → **반영** — **target에도 `maximum_retry_attempts = 2` / `maximum_event_age_in_seconds = 3600`을 명시**하고, 1-3 검증에 `list-targets-by-rule`의 `RetryPolicy` 확인을 넣었다. 두 계층이 같은 값이라 멱등 창 상한이 1시간으로 **확정**된다. 리스크표에 *"24시간 뒤 도착한 전달이 다음 회전 세대에 재배포"* 행을 추가했다.
- [r8] codex-cli#3 [중간] — **Step 2의 backoff·호출별 제한이 수치로 없어 "수 초 내 회복"을 구현·검증할 수 없다.** 초기 간격·배수·최대 간격·jitter·reset 조건·SDK/dial timeout이 전부 미정이라 **폭주를 막는 구현도, 수 초에 회복하는 구현도 둘 다 plan을 만족한다**고 읽힌다 → **반영** — 2-0에 **파라미터 표**를 넣었다(초기 200ms / 2배 full jitter / **최대 5초** / 값 변화 성공 시 reset / Secrets Manager connect 1·read 2·시도 2회 → 최악 6초 / pgx dial 5초 → `Connect` 1회 ≤ 11초). 최대 간격을 5초로 고정한 근거는 **목표가 "수 초 회복"**이라는 것이다. 2-1 검증에 **fake clock ⑨**(옛 값 반복 → 최대 간격 도달 → 새 값 노출 → 회복 → reset)를 넣고, **2-4에 T0 → 쿼리 성공 10초 상한**을 넣었다.
- [r8] codex-cli#4 [중간] — **Step 2의 HCL 작성 순서와 두 apply 게이트를 그대로는 실행할 수 없다.** 2-2가 `skip_destroy`·task role 정책·`DB_SECRET_ARN`을 한꺼번에 작성하는데 2-5의 apply ①은 *"`skip_destroy`만, replacement 없음"*을 요구한다 — 모든 변경이 트리에 있으면 일반 `apply`가 전부 적용하므로 게이트를 만족할 수 없고 임시 revert나 `-target`에 암묵 의존하게 된다 → **반영** — **2-2를 2-2a(`skip_destroy`만) / 2-2b(정책 + env)로 쪼개고**, 2-5에 **apply 시점의 작업 트리 상태**를 단계마다 명시했다(*"임시 revert나 `-target`을 쓰지 않는다"* 포함). 각 단계의 `terraform plan` 기대값도 나눠 적었다.
- [r8] codex-cli#5 [중간] — **조건부 쓰기 경합에서 성공 반환할 때 쓸 `outcome`이 없다.** `skipped_duplicate`는 선행 `GetItem` 적중으로 정의됐는데 (b-3)의 조건 실패 호출은 **이미 재배포를 수행했다**. 그렇다고 `redeployed`로 남기면 marker 경합과 허용된 중복을 식별할 수 없어 **1-7의 사후 증거가 실제 재배포 횟수를 잘못 설명**한다 → **반영** — `outcome`을 **네 값의 표**로 다시 정의했다: `redeploy_submitted` / **`redeploy_submitted_raced_marker`**(제안대로 신설) / `skipped_duplicate` / `failed_<지점>`. 각 값에 *"`UpdateService`를 불렀는가 / marker를 누가 썼는가"*를 붙여 이름과 사실을 일치시켰고, (b-3)·(e)에서 로그를 검증하도록 했다.
- [r8] claude-ide#1 [중간] — **2-6이 판정 근거로 요구하는 앱 로그 필드(`task_id`, refresh 성공·세대 전이)가 2-1·2-2 어느 구현 항목에도 없다.** 2-5가 **순서를 강제하는 게이트 배포**라, 2-6에서 부재를 발견하면 코드 수정 → CI 배포를 처음부터 다시 돌아야 하고 그때가 판별 수단 ②(rule `DISABLED` + 온디맨드 회전) 중이면 **통제 창 하나와 회전 스케줄 한 번을 통째로 버린다** → **반영** — 두 필드를 **2-1 구현 항목으로 올리고** 검증 ⑩으로 고정했다. 2-6은 *"2-1에서 구현한"*을 참조해 **소비만** 한다. 리스크표에도 한 행 추가.
- [r8] claude-ide#2 [중간] — **1-4의 fixture를 어떻게 만드는지가 없어, 임의 `versionId`로는 ①이 반드시 실패한다.** 라벨 확인이 하드 선행 조건이라 60초 소진 → 오류로 끝나고 ①-2도 marker가 없어 함께 무너지는데, `[사람]` 단계라 실행자가 *"자동화가 깨졌다"*로 오진한다 → **반영** — 1-4에 **① fixture의 `detail.versionId`는 `describe-secret`의 현재 `AWSCURRENT` 버전 id를 그대로 넣는다**를 명시하고, **② 실패 fixture는 "존재하지 않는 `versionId`"**로 정의했다. 대기 시간 계산도 *"timeout 120초 × 3"*이 아니라 **"라벨 확인 60초 소진 × 3"**으로 정정했다. **개선 제안대로 fixture 디렉터리를 `pattern/`(값 무의미) / `invoke/`(실제 값 필수)로 분리**해 실행자가 잘못 집을 여지를 없앴다.
- [r8] claude-ide#3 [중간] — **"회전 1회 = 매칭 이벤트 1개, `versionId`=라벨 획득 버전"이라는 전제가 실이벤트로 확인된 적이 없는데, 어긋나면 정상 회전마다 오탐 알람이 뜬다.** `finishSecret`은 옛 버전에 `AWSPREVIOUS`도 옮기므로 매칭 이벤트가 하나인지 불확실하고, 전제가 깨지면 **회전이 성공했는데도 Lambda `Errors`·DLQ depth 알람**이 뜬다. 그런데 실이벤트 원문을 **처음 보는 시점이 1-6**(1-3 apply 이후)이라 **전제 검증이 라이브 전환 뒤**다 → **반영** — **1-1a를 신설**해 광역 관찰 rule + 로그 그룹 + resource policy **셋만 선행 apply**한다(기한 **08-29 18:00 KST 전**). 재배포 경로가 아니라 08-30 창을 위험하게 하지 않고, **그 창의 실이벤트 원문으로 좁은 rule 패턴과 라벨 확인 계약을 대조 확정한 뒤** 1-3에 들어간다. 상단 기한 표와 단계 순서, 리스크표에 반영했다. **08-30 창을 지금 유일하게 활용하는 방법**이라는 리뷰어 평가에 동의한다.
- [r8] codex-ide#3 [낮음] / codex-cli 개선 — **예약 동시성 생략 분기의 근거가 아직 *"원자성은 DynamoDB 조건부 쓰기가 담보"*다.** 확정 계약은 조건부 쓰기가 **marker 작성자만** 원자화하고 `UpdateService`는 원자화하지 않는다 → **반영** — 1-3 검증과 리스크표를 *"marker 유일성은 조건부 쓰기가 보장하고, 예약 미설정 시 동시 중복 재배포는 at-least-once 잔여 위험으로 수용"*으로 좁혔다.
- [r8] codex-ide#4 / claude-ide#4 [낮음] / codex-cli 개선 — **Step 1에서 제거한 "지표" 약속이 Step 2에 그대로 남아 있다**(2-3 검증 (b), 2-6 판별 ①). 2-2가 task role에 주는 권한은 `secretsmanager:GetSecretValue` 하나뿐이라 `PutMetricData`가 없고, 저장소에 앱 커스텀 지표 발행 경로 자체가 없다 → **반영** — 두 곳의 *"·지표"*를 삭제하고 **Step 2도 구조화 로그까지로 통일**했다. r7 자기점검의 grep 목록에 `지표`가 있었는데도 **Step 1 범위에만 적용한 것**이 이번 전파 누락이다.
- [r8] claude-ide#5 [낮음] — **잔재 3건**: `timeout = 120` 근거가 폐기된 *"deployment 관측 확인"*을 가리키고 `GetItem`·`PutItem`이 빠짐 / ADR *"revision 3~7"* / ADR boto3 위험 평가의 API 목록이 **제거된 `describe_services`를 세고 DynamoDB·Secrets Manager를 빠뜨림** / 비용 문구 *"쓰기 1회/회전"* → **반영** — 넷 다 고쳤다. 특히 API 목록은 **위험 평가의 유일한 근거**라 목록이 틀리면 평가도 틀린다(codex-ide 개선도 같은 지적).

*개선 제안*

- [r8] codex-ide 개선 — **TTL 만료 아이템은 백그라운드 삭제 전까지 `GetItem`에 계속 보인다** → 억제 유지 기간은 *"TTL 만료 전"*이 아니라 **실제 삭제 전(7일 + 통상 수일)** → **반영** — 잔여 위험 문구와 노출 기간을 맞췄다.
- [r8] codex-cli 개선 — **`TaskARN`은 컨테이너 기본 응답이 아니라 `${ECS_CONTAINER_METADATA_URI_V4}/task` 응답의 계약이다** → **반영** — 경로를 `/task`로 명시하고 **짧은 HTTP timeout(2초)·응답 크기 제한·JSON 오류 처리**와 정상/실패 두 경우의 테스트를 2-1에 넣었다. `task_id=unknown`이 구현 실수로 상시 발생하면 2-6이 판정 불가가 되므로 중요한 지적이다.
- [r8] claude-ide 개선 — **1-4 ①이 프로덕션 롤링 재배포를 실제로 일으킨다는 사실을 단계 본문에 적어라**(1-6에는 *"통제된 수 분의 다운을 감수한다"*가 있는데 1-4에는 없다) → **반영** — 저트래픽 시간대 수행 + 서킷브레이커 확인을 함께 적었다.
- [r8] claude-ide 개선 — **예약 유무에 따라 (e) 테스트가 검증하는 대상이 달라진다** → **반영** — (e)에 *"예약이 잡히면 실전 발생은 사실상 없고 생략되면 잔여 위험으로 남는다. 테스트는 어느 쪽이든 유지"*를 한 줄로 연결했다.

**[r8] 병합 자기점검**:
- **r7의 자기점검이 *"순서를 바꾸면 그 단계가 제공하던 보장 목록을 먼저 적고 대조한다"*고 했는데, r8은 그 대조를 Step 1 안에서만 했다.** 지표 약속 제거도, `outcome` 재정의도 Step 1에서만 전파됐고 Step 2에는 그대로 남았다. **전파 범위를 "바뀐 절"이 아니라 "그 개념을 쓰는 모든 절"로 잡아야 한다.** 이번 grep 목록: `지표` / `redeployed` / `revision 3~` / `describe_services` / `원자성` / `DescribeServices` / `알람 7개` / `기록.*전이므로` — **Step 1·2 전 범위**에 돌리고 반영 후 재확인했다.
- **`UpdateService` 200을 "완료"라고 부른 것이 r7 결함 1과 같은 형태의 재발이다** — 그때는 조건부 쓰기가 *"중복 재배포를 막는다"*고, 이번엔 응답 PRIMARY가 *"배포 성공을 뜻한다"*고, **장치가 주지 않는 보장을 이름으로 약속**했다. 두 번 다 리뷰어가 이름과 사실의 어긋남을 먼저 잡았다. → **앞으로 `outcome`·marker·단계 이름을 지을 때 "이 이름이 참이려면 무엇이 관측돼야 하는가"를 한 줄로 적는다.** 이번에 `outcome` 표에 *"`UpdateService`를 불렀는가 / marker를 누가 썼는가"* 열을 붙인 것이 그 형식이다.
- **1-1a 신설은 이 plan에서 처음으로 "기한 안에 할 수 있는 것을 늘린" 변경이다.** 지금까지 08-30 창은 *"Step 1으로 막지 않는다"*로만 처리됐는데, 광역 관찰 rule은 **재배포 경로가 아니라서 그 창을 위험하게 하지 않으면서 원문을 준다.** 리뷰어 지적이 없었으면 창이 그냥 지나갔다.
- revision 9에서 새로 쓴 문장 — `redeploy_submitted` 계약과 rollout 잔여 위험, botocore `retries` 고정과 단계별 monotonic deadline, EventBridge target `retry_policy`, 1-1a, 2-2a/2-2b 분할, Step 2 backoff 파라미터 표, 2-1 로그 필드 — 은 다음 라운드에 검증받아야 한다. 특히 **예산 표의 "최악 실행시간" 산술**과 **1-1a가 1-1의 `plan` 기대값을 흔들지 않는지**가 집중 검토 대상이다.

**[r7] revision 7 → 8** (3인 전원 제출, 전부 `reviewed-revision: 7`, 전부 `request-changes`. **기각 0건**.
**두 라운드 연속으로 세 리뷰가 같은 [높음] 1건에 완전히 수렴**했다):

*결함*

- [r7] codex-cli#1 / codex-ide#1 / claude-ide#1 [높음, **3인 전원 동일**] — **`PutItem`을 `UpdateService` 뒤로만 옮긴 결과 중복 억제가 통째로 사라졌다.** 부작용 앞에 읽는 지점이 없어 중복 이벤트도 `UpdateService`를 부른 **뒤에야** `ConditionalCheckFailed`를 받는다. `skipped_duplicate`는 기록 충돌만 뜻하고 재배포는 이미 일어난 상태다. 그런데 테스트 (b)·(e)와 리스크표 2행은 여전히 *"조건부 쓰기가 중복 재배포를 막는다"*를 전제한다 → **반영** — **부작용 앞에 `GetItem`(`ConsistentRead=true`)을 되살렸다.** fail-open은 그대로 유지되고(marker는 여전히 `UpdateService` 성공 뒤에만 쓰인다) 순차 중복은 실제로 억제된다. **연쇄 수정 4건**: ① IAM에 `dynamodb:GetItem` 복원(revision 7이 `PutItem` 하나로 좁힌 것은 **중복 억제를 포기할 때만 성립하는 결론**이었다 — claude-ide 지적), ② (e)의 성공 기준을 *"둘 중 하나만 `UpdateService`"* → *"marker를 쓴 쪽이 정확히 하나, 진 쪽도 성공 반환"*으로 정정(선행 조회는 원자적이지 않다), ③ 리스크표 "동시 재배포" 행을 **완화 → 감수**로 정직하게 바꿈, ④ 1-3에 `GetItem` IAM 확인, 1-4에 **같은 fixture 2회 invoke** 추가.
  **처방 선택**: codex-cli는 *"at-least-once로 통일하고 억제 주장·테스트를 전부 제거"* 또는 *"claim/lease 상태 기계"*를, codex-ide와 claude-ide는 *"강한 일관성 선행 조회 + 사후 조건부 쓰기"*를 제시했다. → **선행 조회를 채택**한다. 근거: (1) 코드 한 줄과 IAM action 하나로 **순차 중복**(실제로 관측되는 형태 — Lambda 비동기 중복 전달·재시도)이 닫히고, (2) 억제 주장을 전부 버리면 r6에서 문제였던 *"중복마다 롤링"*이 그대로 남으며, (3) 상태 기계는 r6에서 기각한 근거(운영 부담 > 이득)가 그대로 유효하고 `clientToken` 부재로 동시 창은 어차피 남는다. **codex-cli가 요구한 "계약 통일"은 이 선택으로 충족된다** — 억제되는 것(순차)과 감수하는 것(동시·크래시)을 본문·테스트·리스크표에서 같은 말로 적었다.
- [r7] codex-cli#2 / codex-ide#2 / claude-ide#2 [중간, **3인 전원**] — **실행 예산 표가 자기 결론을 뒷받침하지 못한다.** DynamoDB 행이 둘(이동 전 잔재)이고 합계가 선언값 105초가 아니라 **120초**라, 바로 아래 *"합계가 함수 `timeout`과 같으면 안전 여유가 아니다"*가 가리키는 그 상태였다. 잔재 행을 지워도 115초로 여전히 초과 → **반영** — **확정 흐름의 호출 순서 그대로 5행으로 다시 합산**했다(`GetItem` 5 + 라벨 확인 60 + `UpdateService` 20 + `PutItem` 5 + 여유 15 = **105 = 내부 deadline**, 함수 `timeout` 120). 각 구간 값을 boto3 timeout으로 설정하도록 명시하고, deadline 중단 케이스를 테스트 (h)로 넣었다.
- [r7] codex-cli#3 / codex-ide#3 / claude-ide#4 [중간, **3인 전원**] — **커스텀 지표·알람 약속이 Terraform·IAM·비용 어디에도 연결되지 않았다.** 발행 방식·namespace·dimension·임계값·`treat_missing_data`가 없고, `PutMetricData`라면 IAM에도 없어 `AccessDenied`이며, 알람 수는 세 곳이 "7개"로 고정돼 자리가 없다 → **반영(약속 제거)** — **이번 범위를 구조화 로그 `outcome` 필드까지로 줄였다.** codex-cli가 제시한 두 선택지 중 *"범위를 줄여 알람 약속을 제거"*를 택한다. 근거: 결함 1을 고쳐 *"skip만 계속 나온다 = 봉인"* 위험 자체가 낮아졌고(봉인은 fail-closed의 산물이었다), 자동 복구가 죽는 신호는 알람 7개 + on-failure SQS + 1-7 + 0008 탐지가 이미 받는다. 필요해지면 **EMF(추가 IAM 불요)**를 1순위로 별도 라운드에서 다룬다. `skipped_duplicate`도 이제 **실제로 재배포를 건너뛴 것**을 뜻해 이름과 사실이 일치한다.
- [r7] codex-cli#4 / codex-ide 개선 / claude-ide 개선 [중간] — **`UpdateService`가 만든 배포를 식별하는 기준이 없다.** 단순히 PRIMARY를 보면 다른 CI 배포를 자기 성공으로 오인해 marker를 쓸 수 있다 → **반영** — **재배포 관측을 `UpdateService` 응답으로 끝낸다.** 응답 `service.deployments[]`의 PRIMARY `id`를 로그에 남기고 그것으로 판정하며, **별도 `DescribeServices` 조회를 하지 않는다.** 세 가지가 함께 해결됐다: ① 식별이 응답 자체로 확정, ② claude-ide 개선이 지적한 *"관측 확인이 중복을 만드는 쪽으로 뒤집혔다"*(ECS 최종 일관성으로 정상 재배포를 실패로 오판 → 재배포 1회 추가)가 사라짐, ③ **IAM에서 `ecs:DescribeServices`가 빠져 네 조각이 됨**. 테스트 (d)는 *"응답에 PRIMARY 없음 → 오류 반환"*으로, 1-3은 *"`DescribeServices`가 들어 있지 **않은지**"* 확인으로 바꿨다.
- [r7] claude-ide#3 [중간] — **revision 7이 명시적으로 폐기한 문장이 (b-3)에 그대로 살아 있다** (*"DynamoDB 기록은 `UpdateService` 전이므로 재시도가 안전하다"*). 같은 문서가 같은 문장을 한 곳에서 폐기하고 다른 곳에서 근거로 쓴다. (b-2)(b-3)의 *"재배포 반복 없음"*도 fail-open에서 거짓이라 (f)와 정면 충돌한다 → **반영** — 단위 테스트 블록 전체를 확정 흐름 기준으로 다시 썼다((a)~(h)). **자기점검 지적도 수용**: r6 자기점검이 *"grep으로 전수 확인했다"*고 선언해 놓고 `기록.*전이므로` 한 번이면 잡히는 문장을 놓쳤다 → **이번 라운드부터 실제로 실행한 grep 패턴을 아래 자기점검에 적는다.**
- [r7] claude-ide#5 [낮음] — IAM 3번(`ecs:DescribeServices`)의 근거가 폐기된 설계를 가리킨다 → **반영** — codex-cli#4 반영으로 **권한 자체가 제거**되면서 함께 해소됐다.

*개선 제안*

- [r7] claude-ide 개선 — **`PutItem` 조건 실패 vs 그 외 실패의 반환값을 계약 절에 명시하라** → **반영** — *"`GetItem` 적중 또는 `ConditionalCheckFailedException` = 성공 반환, 그 외 모든 실패 = 오류 반환"*을 한 줄로 못박았다. 스로틀·5xx를 조건 실패와 함께 삼키면 **fail-closed가 되살아난다**는 경고도 붙였다.
- [r7] claude-ide 개선 — **TTL 30일은 회전 주기 7일 대비 길다**(억제가 살아나면 운영 롤백 노출이 커진다) → **반영** — **보존 7일**로 줄였다. 멱등 창의 실질 상한은 `maximum_event_age_in_seconds = 3600`이라 168배 여유다.
- [r7] codex-cli 개선 / codex-ide 개선 — **2-6의 task 식별자를 어디서 얻는지 없다** → **반영** — `ECS_CONTAINER_METADATA_URI_V4`의 `TaskARN` 마지막 세그먼트를 구조화 로그 `task_id`로 고정 첨부하고, `list-tasks` 결과와 집합 대조한다. 조회 실패 시 **판정 불가**로 처리(증거 없이 성공 처리 금지).
- [r7] codex-cli 개선 — **비용의 "쓰기 1회"에 읽기·조건 실패 쓰기가 빠졌다** → **반영** — *"읽기 1회 + 조건부 쓰기 1회, 중복이 오면 그만큼 추가"*로 고쳤다.
- [r7] claude-ide 개선 — **"deployment 관측 확인" 단계의 목적을 다시 쓰거나 없애라** → **반영(없앰)** — codex-cli#4와 같은 처방으로 수렴했다.

**[r7] 병합 자기점검**:
- **r6 자기점검이 "grep으로 전수 확인했다"고 선언한 그 라운드에서, 폐기한 문장이 테스트 근거로 살아남았다.** 선언은 검증이 아니다. **이번 라운드에 실제로 실행한 grep 패턴을 남긴다**: `DescribeServices` / `describe-services` / `다섯 조각|5분할|네 조각|4분할` / `지표` / `30일` / `알람 7개|7개` / `deployments` / `PutItem` / `GetItem` / `105|120`. 반영 후 같은 패턴을 다시 돌려 잔재를 확인했다.
- **r6과 r7이 같은 형태의 실패다** — r6은 "lock을 잡고 죽으면?"을 안 물었고, r7은 그것을 고치면서 **"그 장치가 원래 무엇을 위해 있었는지"**를 안 물었다. 뒤집기(fail-closed → fail-open)는 **순서를 바꾸는 것이지 단계를 지우는 것이 아니었다.** 다음 라운드에 어떤 순서를 바꾸면, **바꾸기 전 그 단계가 제공하던 보장 목록을 먼저 적고 하나씩 대조한다.**
- **이번 라운드에서 처음으로 리소스를 뺐다**(`ecs:DescribeServices`, 커스텀 지표·알람). 세 라운드 동안 늘려 온 방향과 반대다 — 세 리뷰어가 공통으로 *"약속과 설계가 어긋난다"*고 지적한 항목은 **설계를 채우는 것보다 약속을 줄이는 쪽이 기한 안에서 옳다.**
- revision 8에서 새로 쓴 문장 — 선행 `GetItem` 흐름과 `ConsistentRead`, 예산 표 5행 재합산, `UpdateService` 응답 기반 관측과 IAM 4조각, 지표 약속 제거, TTL 7일, `task_id` 출처 — 은 다음 라운드에 검증받아야 한다. 특히 **IAM에서 권한을 뺀 것**은 r2 codex-cli#1이 잡았던 누락과 반대 방향의 위험이므로 집중 검토가 필요하다.

**[r6] revision 6 → 7** (3인 전원 제출, 전부 `reviewed-revision: 6`. **기각 0건**.
**세 리뷰가 같은 [높음] 1건으로 완전히 수렴**했다):

*결함*

- [r6] codex-cli#1 / codex-ide#1 / claude-ide#1 [높음, **3인 전원 동일**] — **DynamoDB 기록이 `UpdateService` 앞에 있어 fail-closed다.** 라벨 확인 OK → `PutItem` 성공 → `UpdateService` 실패(스로틀/timeout) → 오류 반환. 재시도는 아이템이 존재하므로 *"처리됨"*으로 **skip + 성공 반환** → **재배포 0회, 계층 2 on-failure에도 안 가고, 알람 7개 중 무엇도 울지 않고, 같은 `versionId`는 앞으로도 전부 skip = 영구 봉인** → **반영** — **이 plan이 스스로 "자동 복구가 조용히 죽는 것이 최악 실패 모드"라고 규정한 그 실패를, 중복을 막으려고 넣은 장치가 만들어냈다.** revision 6의 *"기록이 `UpdateService` 전이므로 재시도가 안전하다"*는 **라벨 확인 실패 경로에만 맞고 기록 이후 경로에는 정반대**였다(3인이 모두 이 문장을 지목했다). **기록을 `UpdateService` 성공 관측 뒤로 옮겨 fail-open으로 뒤집었다.** 1-2에 (f)기록 실패 후 재시도 / (g)`UpdateService` 실패 후 재시도 케이스를 넣었다.
  **처방 선택**: codex-cli·codex-ide는 `CLAIMED/COMPLETED` + owner/lease + 조건부 takeover 상태 기계를, claude-ide는 기록을 뒤로 옮기는 fail-open을 제시했다. → **fail-open을 채택**한다. 근거: (1) claude-ide의 *"중복 재배포와 재배포 소실 중 후자가 압도적으로 나쁘다 — 전자는 롤링 한 번, 후자는 서비스가 내려간 채 알람도 없다"*, (2) 회전이 7일 1회라 중복 빈도가 극히 낮고 서킷브레이커도 있다, (3) **ECS `UpdateService`에 `clientToken`이 없어**(codex-ide#1) API 수준 exactly-once가 애초에 불가능하므로 상태 기계를 늘려도 그 창은 남는다, (4) AGENTS.md 운영 책임 원칙(사람이 이해하고 운영할 수 있어야). **계약을 at-least-once로 명시**하고 상태 기계는 채택하지 않는다.
- [r6] codex-cli#2 / codex-ide#2 / claude-ide#2 [중간, **3인 전원**] — **원자성 근거가 세 곳에서 아직 `reserved_concurrent_executions = 1`이다.** 본문은 예약을 defense-in-depth로 내렸는데 단위 테스트 절·1-3 검증·리스크표는 옛 계약 그대로라, *"예약 없이 진행"*이라는 정상 분기가 **자기 완료 조건을 통과할 수 없다.** fake 목록에 DynamoDB도 빠졌다 → **반영** — 세 곳을 조건부 쓰기로 통일하고 1-3을 **분기형**(예약 가능하면 1, 아니면 미설정)으로 바꿨다. **claude-ide의 사실 정정도 반영**: 조건부 쓰기의 동시 케이스는 fake가 `ConditionalCheckFailedException`을 던지면 **단위 테스트로 증명된다** — revision 6의 *"증명되지 않는다"*는 과도한 유보였고, 이제 1-3 설정 확인에 기대지 않는다. fake 목록에 DynamoDB client를 넣었다.
- [r6] codex-cli#3 / codex-ide#4 / claude-ide#4 [중간/낮음, **3인 전원**] — **Step 2 검증의 "최대 5분"이 풀 교체 보장 5분과 마진 0이라 정상 구현을 판정 불가로 오판한다.** Go 계약상 만료 커넥션은 **재사용 전에 지연 폐쇄될 수 있고**, 마지막 커넥션 잔여 수명 + 다음 probe(≤30초) + ALB 라우팅이 붙는다 → **반영** — **10분으로 완화**했다(1-6·1-7과 통일). claude-ide가 인용한 0008 `:466`의 선례(*"마진 13초 → 10분으로 완화"*)가 정확히 같은 형태다. **codex-ide#4의 추가 지적도 반영** — 태스크가 2개이고 canary가 ALB를 거치므로 한쪽에만 도달할 수 있다 → **구조화 로그에 task 식별자를 넣어 두 running task 모두에서 refresh 증거를 요구**하고, 한쪽만 나오면 판정 불가로 종료한다.
- [r6] codex-ide#3 [중간] — **실행 예산 합계(60+5+30+25=120)가 함수 `timeout`과 정확히 같아 안전 여유가 아니다.** 모든 구간이 상한에 닿으면 반환·직렬화 전에 종료되고, 중단 임계값도 정하지 않았다 → **반영** — **handler 내부 deadline 105초 / 함수 `timeout` 120초**로 벌리고, `get_remaining_time_in_millis()` **15초 미만이면 즉시 중단**을 명시했다. 결함 1 수정으로 `PutItem`이 뒤로 갔으므로 예산 표도 다시 계산했다.
- [r6] codex-cli#4 / codex-ide 개선 [낮음] — **`describe-table`로는 TTL 상태를 검증할 수 없다**(`TimeToLiveStatus`는 `DescribeTimeToLive`가 반환) → **반영** — 1-3을 `describe-table`(키·billing·ACTIVE) + **`describe-time-to-live`**(`ENABLED` + 속성명)로 나눴다. codex-ide 개선대로 **TTL은 claim lease가 아니라 보존 정리용**임도 명시했다(TTL 삭제는 만료 즉시가 아니라 통상 수일 내).
- [r6] codex-cli 개선 / codex-ide#5 / claude-ide#3 [낮음, **3인 전원**] — **IAM 조각 수가 세 곳에서 다르다**(아키텍처 "네 조각"·handler "5분할"·ADR "4분할")이고 `DescribeSecret`·DynamoDB가 목록에서 빠졌다 → **반영** — **다섯 조각 단일 목록**으로 통일했다(리소스 정책 / ECS 2 action / `DescribeSecret` / `dynamodb:PutItem` + Logs·SQS). 그리고 **결함 1의 fail-open 채택으로 `UpdateItem`·`GetItem`이 불필요해져 `PutItem` 하나로 줄였다**(claude-ide#3이 지적한 연쇄). ADR 문구도 맞췄다.
- [r6] claude-ide#3 [낮음] — 리스크표가 뒤집힌 결론을 안 따라간다(*"신규 연결 자극 방법 확정"* — r5에서 **자극 불필요**로 확정했다), ADR이 아직 *"revision 3~4"* → **반영**.

*개선 제안*

- [r6] claude-ide 개선 — **조건부 쓰기 결과를 로그·지표로 구분하라**(`redeployed` / `skipped_duplicate` / `failed_*`) → **반영** — 결함 1을 고친 뒤에는 **이 구분이 "자동 복구가 실제로 동작했는지"를 판정하는 유일한 흔적**이고, 1-7 사후 증거가 여기 의존한다. *"skip만 계속 나온다 = 봉인 상태"*를 지표로 잡을 수 있게 했다.
- [r6] claude-ide 개선 — **TTL 속성명을 적어라**(안 적으면 TTL이 설정만 되고 아무것도 만료되지 않는다) → **반영** — `expiresAt`(epoch 초)을 `ttl` 블록과 `PutItem` 항목 **둘 다**에 명시했다.
- [r6] codex-cli 개선 — 파티션 키가 `versionId` 하나라 **같은 버전에 `AWSCURRENT` 재부여(운영 롤백)**도 30일간 중복 취급된다 → **반영(잔여 위험 명시)** — 이 plan 범위에는 그런 롤백이 없으므로 감수하고, 필요해지면 `event.id` 복합 키로 바꾼다고 적었다.
- [r6] codex-cli / codex-ide 개선 — 비용의 *"회전당 쓰기 2회"*를 실제 횟수로 → **반영**(조건부 쓰기 **1회**).

**[r6] 병합 자기점검**:
- **r5의 자기점검이 "두 수정의 교차 검산 누락"을 반성했는데, r6에서는 그 수정 자체가 새 실패 모드를 만들었다.** 멱등 장치를 넣으면서 **"이 lock을 잡고 죽으면?"**을 묻지 않았다. 정확히 같은 질문(*"실패하면 어느 쪽으로 넘어지나"*)을 r6에서 3인이 동시에 물었다.
- **처음으로 "더 정확한 처방"을 의도적으로 기각했다.** codex 2인의 상태 기계가 fail-open보다 정확하지만, **ECS API에 멱등 파라미터가 없어 어느 쪽이든 중복 창이 남고**, 이 규모에서 상태 기계는 운영 부담이 이득보다 크다. **기각이 아니라 "우선순위를 선언한 채택"**으로 기록했다 — 0007의 교훈 적용 두 번째 사례다.
- **전파 누락이 r3~r6 네 라운드 연속**이다(이번은 IAM 조각 수). r5 자기점검에서 *"번호나 기준을 바꾸면 grep으로 전 문서 참조를 확인한다"*고 적어 놓고 지키지 않았다. **revision 7 병합 후 `grep`으로 IAM·기록 시점·대기 상한을 전수 확인했다.**
- revision 7에서 새로 쓴 문장 — fail-open 흐름, at-least-once 계약, 내부 deadline 105초, 두 태스크 refresh 조건, 판정 결과 로그 구분 — 은 다음 라운드에 검증받아야 한다.

**[r5] revision 5 → 6** (3인 전원 제출, 전부 `reviewed-revision: 5`. **기각 0건**):

*결함*

- [r5] codex-cli#1 / codex-ide#1 / claude-ide#1 [높음, **3인 전원**] — **`credentialsReadyAt`을 invocation마다 계산하면 순차 중복 억제가 깨진다.** 지연 중복 호출은 더 늦은 기준값을 갖고 기존 deployment의 `createdAt`은 항상 그보다 과거라 **skip 조건을 영영 만족하지 못해 중복마다 재배포를 보장**한다. 오류 반환 후 Lambda 재시도도 같은 경로다 → **반영** — **revision 5가 같은 라운드에 넣은 두 처방 (A)기준 교체와 (B)순차 중복 억제가 서로를 무효화**했다. claude-ide의 시각 추적이 결정적이었다. 핵심 통찰도 리뷰어 것이다 — *"이벤트 `time`이 dedup에 작동했던 이유가 모든 전달·재시도에서 같은 값이기 때문"*. **시각 비교로 멱등을 구현하지 않기로 하고 `versionId`를 키로 한 DynamoDB 조건부 쓰기**(`attribute_not_exists`)로 바꿨다. 라벨 확인은 **비교 기준이 아니라 `UpdateService`의 선행 조건**으로 강등했다 — claude-ide가 지적한 대로 r4 codex-cli#2의 문제는 그것만으로 닫히고 **비교 기준까지 바꿀 필요가 없었다.**
- [r5] codex-cli#2 / codex-ide#2 [높음] — **"예약 없이 진행" 분기가 원자성을 포기시킨다.** 미예약 동시성이 부족할 때 예약 없이 진행하도록 허용하면 **plan 스스로 확인한 read-then-write race를 다시 활성화**하는 것이고, 중복 횟수 상한도 Lambda 중복 전달로 보장되지 않는다 → **반영** — 지적이 맞다. **위 DynamoDB 조건부 쓰기가 이 결함도 함께 닫는다** — 원자성이 **계정 동시성 한도와 무관**해지므로 `reserved_concurrent_executions = 1`을 **defense-in-depth로 강등**하고 필수 게이트에서 내렸다. 두 결함을 한 장치로 해소했다.
- [r5] codex-cli#3 / codex-ide#3 / claude-ide#5 [중간, **3인 전원**] — **두 번째 bounded poll의 상한이 없어 `timeout = 120`의 근거가 완결되지 않는다.** 라벨 확인만 60초로 고정됐고 deployment 관측은 *"상한 있음"*뿐이라, 두 번째 poll이 남은 시간을 넘기면 **handler가 오류를 반환하기 전에 런타임이 강제 종료해 어떤 실패인지 로그조차 안 남는다** → **반영** — r4에서 3인이 [높음]으로 잡은 것과 **같은 종류의 공백**(계약을 정하고 그 계약이 돌아갈 수치를 안 정함)이라는 claude-ide 지적이 정확하다. **실행 예산 표**를 만들어 60 + 5 + 30 + 25 ≤ 120으로 확정하고 **`context.get_remaining_time_in_millis()` 기반 중단점**을 넣었다. 1-2에 관측 상한 테스트를 추가했다.
- [r5] codex-cli#4 / claude-ide#2 [중간] — **DLQ depth 알람의 `treat_missing_data = "missing"` 근거가 1차 출처와 반대다.** SQS는 6시간 넘게 비활성이면 지표 전송을 중단하므로, 회전 7일 1회인 이 두 DLQ는 `missing`이면 **영구 INSUFFICIENT_DATA에 상주**한다 → **반영** — revision 5의 *"정상 시 0이 발행되므로"*가 틀렸다. **7개 전부 `notBreaching`**으로 통일했다. claude-ide가 덧붙인 **"활성화 시 최대 15분 지연"**도 반영해 **빠른 신호(1분 지표) vs DLQ depth(확인용)** 순서를 런북(1-5)에 넣도록 했다 — *"DLQ가 조용하니 괜찮다"* 오판 방지.
- [r5] codex-ide#4 / claude-ide#4 [중간] — **Step 1 본문(`:213`)의 기한이 헤드라인 표와 다르다.** r4에서 지적한 자기모순이 **헤드라인에서만 해소되고 실행 단계에 남았다** → **반영** — *"실행자는 Step 1 절을 펴서 읽으므로 여기가 오히려 더 자주 읽힌다"*는 지적이 맞다. `09-06 09:00 전`(드릴 시 +7일)로 표와 일치시켰다.
- [r5] codex-ide#5 / claude-ide#3 [중간] — **2-5 안에서 apply ① 게이트가 정면 모순.** numbered step은 *"replacement 없이 in-place, `-/+`면 중단"*인데 롤백 절에 revision 4의 *"변경 0이어야 한다"*가 남았다 → **반영** — apply 게이트라 실행자가 어느 쪽을 보느냐로 판단이 갈린다. 옛 문장을 정정했다.
- [r5] codex-ide#6 / codex-cli 개선 [중간] — **2-6의 신규 연결 자극 방법이 미확정이고 두 방법이 동등하지 않다.** k6로 짧은 쿼리를 보내도 idle 25개 동시 점유가 보장되지 않고, `pg_sleep` 진단 엔드포인트는 **프로덕션에 지연 유발 표면**을 새로 만든다 → **반영** — **자극이 애초에 불필요하다**로 확정했다. `SetConnMaxLifetime(5분)`·`SetConnMaxIdleTime(5분)`(`db.go:29-30`)이 5분 안에 풀 전체 교체를 보장하며, **이것이 애초에 장애를 만든 바로 그 메커니즘**이다. canary가 30초마다 `/readyz`를 프로빙하므로 신규 연결이 확실히 생긴다. **프로덕션에 새 코드·부하를 넣지 않는다.**
- [r5] claude-ide#6 [낮음] — **revision 5의 개정이 본문으로 전파되지 않은 3건**: `:141`·`:176-177`의 *"1-5 드릴"*(하필 **순서 강제** 문장이라 게이트가 런북 갱신 단계를 가리켰다), 리스크표의 *"1-7 종료 후 7일"*(하필 **"Step 2를 무기한 미룸"** 행이라 미루는 근거로 쓰인다), ADR·리스크표의 *"`time` vs `createdAt`"* → **반영** — 셋 다 고쳤다. **번호를 바꾼 라운드에서 참조를 전파하지 않는 실수가 r3부터 반복**되고 있다.
- [r5] claude-ide#7 [낮음] — **plan 0010 권고에 날짜가 없다 — 이 plan이 방금 금지한 형태다.** `:379`는 *"남은 라운드 수를 근거로"* 08-30 불가를 확정했는데 0010 권고는 날짜 없이 *"사용자 결정"*으로 끝난다 → **반영** — 같은 시계를 0010에 적용하면(리뷰 0건, 5일) **비현실적**이다. **"08-30 판단은 실측 없이 간다"로 확정**하고, 0010 드릴은 **1-3 apply 이후 1-6 드릴과 묶어 수행**하도록 위치를 정했다(자동 복구가 있어 드릴 비용이 낮고 두 실측을 한 번에 얻는다).

*개선 제안*

- [r5] codex-cli 개선 / claude-ide 개선 — 1-4의 *"최대 대기 시간을 정하고"*가 여전히 실행 시점 결정 → **반영** — **최대 10분**(최초 + 재시도 2회 × timeout·백오프 + 전달 여유)으로 확정하고, 확인을 **`receive-message`로 본문 확인(즉시)** / **알람 전이 확인(최대 15분 + SQS 활성화 지연)**으로 나눴다.
- [r5] codex-ide 개선 / claude-ide 개선 — 1-2 (b)가 두 상황을 한 문장으로 합쳐 결함 1을 놓치기 쉽다 → **반영** — (b)지연 중복 / (b-2)`DescribeServices` 반영 지연 / **(b-3)오류 반환 후 재시도 경로**로 분리했다. (b-3)이 결함 1이 실제로 드러난 경로다.
- [r5] claude-ide 개선 — *"분당 약 33건"*의 출처를 남겨라(두 판단이 같은 전제에 의존한다) → **반영** — ADR 0004 실측(시간당 1,976 ÷ 60)과 *"헬스체크 설정이 바뀌면 함께 재검토"*를 적었다.
- [r5] codex-cli 개선 — 2-6의 `pg_sleep` 엔드포인트를 채택할 경우의 보호·제거 조건 → **불요** — 자극 자체를 없앴으므로 해당 없음.

**[r5] 병합 자기점검**:
- **이번 [높음]은 "같은 라운드에 넣은 두 처방이 서로를 무효화한 것"**이다. r4에서 codex-cli#2와 codex-cli#3을 각각 반영했는데 **둘의 상호작용을 검산하지 않았다.** [[cr-merge-minimal-edits]]가 경고한 재발 패턴의 네 번째 형태다 — 이전엔 "반대편으로 과교정"이었고 이번엔 **"두 수정의 교차 검산 누락"**이다. **한 라운드에 같은 알고리즘의 두 지점을 고칠 때는 합친 뒤 시나리오를 손으로 한 번 돌린다.**
- 다행히 두 결함(멱등 기준·원자성 게이트)이 **한 장치(DynamoDB 조건부 쓰기)로 함께 닫혔다.** r1~r4에서 "리소스를 하나 더 만들지 않는다"며 미뤘던 선택인데, 세 라운드에 걸쳐 시각 기반 멱등이 반복 실패한 뒤에야 채택했다. **처음부터 원자적 저장소를 썼으면 r2~r5의 멱등 논쟁이 없었다.**
- 번호·참조 전파 누락이 r3·r4·r5 연속으로 잡혔다. **다음 라운드부터 번호나 기준을 바꾸면 `grep`으로 전 문서 참조를 확인한 뒤 병합을 끝낸다.**
- revision 6에서 새로 쓴 문장 — DynamoDB 멱등 흐름, 실행 예산 표, `notBreaching` 통일, 풀 자연 교체 논거, 0010 위치 — 은 다음 라운드에 검증받아야 한다.

**[r4] revision 4 → 5** (3인 전원 제출, 전부 `reviewed-revision: 4`. **기각 0건**.
claude-ide는 이번 라운드 **[높음] 0건** — 지적이 세 지점으로 좁혀졌다):

*결함*

- [r4] codex-cli#1 / codex-ide#1 / claude-ide#1 [높음, **3인 전원**] — **Lambda 함수 `timeout`이 없다.** 기본값 3초인데 revision 4가 60초 bounded wait를 넣어, 그대로 apply하면 폴링이 시작되자마자 매 호출이 강제 종료 → 재시도 2회 소진 → on-failure 큐로만 쌓이고 **`UpdateService`에 한 번도 도달하지 못한다** → **반영** — **Step 1이 통째로 죽는 설정 누락**이다. `timeout = 120`·`memory_size = 256`·boto3 connect/read timeout을 확정하고, bounded wait의 *"예: 60초"*에서 **`예:`를 떼어 값을 고정**했다(함수 timeout이 여기서 파생되므로). 1-3 검증에 `get-function-configuration`을 넣고 리스크표에 행을 만들었다.
- [r4] codex-ide#2 / claude-ide#3 [높음/중간] — **절대 기한이 문서 안에서 자기모순**이다. 헤드라인은 *"08-30 전"*인데 본문은 *"08-30까지 불가능하다고 사실로 확정"*하고 목표를 그다음 창으로 옮긴다. **선언된 기한이 작성 시점에 이미 위반**돼 있다 → **반영** — **내가 어제 우발 계획을 확정 판단으로 격상하면서 만든 모순**이다. 기한을 **표로 셋으로 분리**했다: 브릿지 사전 무장 **08-30 09:00 전**, Step 1 라이브 **09-06 09:00 전**(드릴 시 +7일), Step 2 착수 **1-6 완료 후 7일**. 이 plan의 존재 이유가 *"조건부·모호한 기한이 미끄러지는 것"*을 막는 것인데 헤드라인이 그 상태였다는 지적이 정확하다.
- [r4] codex-cli#3 / codex-ide#4 [높음/중간] — **`reserved_concurrent_executions = 1`은 순차 중복을 못 막는다.** 첫 호출이 `UpdateService` 응답 직후 반환하면 큐의 중복 이벤트가 다음 invocation으로 실행되고, 그 `DescribeServices`가 새 deployment를 아직 못 보면(ECS도 eventual consistency) **또 재배포를 건다.** Lambda 비동기 큐는 성공한 이벤트도 중복 전달한다 → **반영** — r3의 read-then-write 결함이 *좁아졌을 뿐 해소되지 않았다*는 지적이 맞다. 첫 호출이 **`createdAt >= credentialsReadyAt`인 deployment를 bounded 확인하고 반환**하도록 계약을 바꿨다. 테스트도 *"동시"*가 아니라 **"첫 호출 반환 직후 같은 이벤트 재호출"**로 케이스를 고쳤다.
- [r4] codex-cli#2 vs codex-ide#7 / claude-ide#5 [높음/중간] — **bounded wait의 API 미확정.** **상충하는 처방이었다**: codex-cli는 *"`DescribeSecret`은 이벤트가 이미 알려준 메타데이터를 다시 읽는 것일 뿐 ECS의 `GetSecretValue` 경로 증거가 아니다 → `GetSecretValue(VersionId=…)` 성공을 `credentialsReadyAt`으로"*, codex-ide·claude-ide는 *"`VersionIdsToStages`만으로 판정되고 `GetSecretValue`는 비밀번호 원문을 함수 메모리로 끌어온다(이 함수는 이벤트를 로그로 찍는다) → `DescribeSecret`으로 확정"* → **`DescribeSecret`을 채택하고 codex-cli의 지적은 잔여 위험으로 명시** — **우선순위: 최소권한(가드레일 #2) > 확인 강도.** 근거: `GetSecretValue`로 바꿔도 **그 역시 제3의 호출**이라 ECS execution role의 읽기를 증명하지 못한다. 즉 codex-cli가 지적한 갭은 어느 API로도 닫히지 않으므로, **bounded wait는 창을 좁히는 heuristic이지 폐쇄가 아니다**라고 적고 잔여 구간의 대응이 *"탐지 → 수동 브릿지"*임을 리스크표에 넣었다(claude-ide 개선과 같은 결론). **codex-cli #2의 나머지 절반은 별개로 반영**했다 — skip 기준을 이벤트 `time`이 아니라 **`credentialsReadyAt`**으로 바꿨다(자격증명 가용 확인 **전에** 시작된 배포가 `createdAt > event.time`이라는 이유로 skip되면 안 된다).
- [r4] codex-ide#3 [높음] — **Step 2 착수 게이트가 Step 1 완료와 충돌**한다. 1-6 드릴이 "Step 1 실질 완료"인데 Step 2는 1-7(자동 창) 종료 후 7일이고, 드릴이 창을 +7일 미므로 **근본 대응이 1~2주 밀린다** → **반영** — *"조건부 게이트로 미루기"*(A-11)를 그대로 재현하는 것이라는 지적이 정확하다. Step 2 착수를 **1-6 완료 후 7일 이내**로 바꾸고 1-7을 **병행 운영 추적 항목**으로 분리했다.
- [r4] claude-ide#2 [중간] — `reserved_concurrent_executions = 1`은 **계정 동시성 한도** 때문에 apply가 실패할 수 있다(*"Unreserved account concurrency minus 100"*). 개인 계정은 축소 한도로 시작하는 경우가 있고 이 저장소엔 Lambda 선례가 없다 → **반영** — 1-1 착수 전 `get-account-settings` read-only 확인과 **부족 시 대안**(한도 증설 / 예약 없이 순차 멱등 계약만으로 진행 + 잔여 위험 명시)을 넣었다. `reserved=1`이 원자성의 유일한 근거였으므로 이게 막히면 구멍이 남는다는 지적이 맞다.
- [r4] claude-ide#4 [중간] — *"탐지는 이미 고쳐져 있다"*가 **저장소 런북이 명시한 유보를 버렸고**, *"1시간 이내"* 목표는 야간 구간에 근거가 없다 → **반영** — 두 런북을 확인했고 인용이 정확했다(`alarm-response.md:197`, `secret-rotation-bridge.md:141-142` — 둘 다 *"실제 DB 장애에서 울린다는 실측은 아직 없다"*). **유보를 원문 그대로 옮기고**, 목표를 **구간별 표**로 다시 썼다(깨어 있는 구간 = 폴링 간격 + 복구 2분 / **야간 = 보장 없음**). *"야간 회전 시 몇 시간 다운은 여전히 가능하다"*를 명시했다. **(c) plan 0010 드릴을 08-30 전에 돌리는 선택지**도 평가해 **권고: 수행**으로 적고, 다만 0010의 범위이므로 **사용자 결정 항목**으로 남겼다.
- [r4] codex-cli#4 / codex-ide 개선 [중간] — 1-4의 실패 fixture를 **기본 동기 호출로 실행하면 on-failure destination을 전혀 시험하지 못한다** → **반영** — `--invocation-type Event` 명시, 재시도 2회 완료까지의 최대 대기 시간, **[사람]이 테스트 메시지 삭제 + 알람 정상화**(안 하면 DLQ depth 알람이 계속 ALARM)까지 넣었다.
- [r4] codex-cli#5 / codex-ide 개선 [중간] — *"알람 6개"*인데 세면 **7개**다 → **반영** — 개수와 비용을 7개 기준(약 $0.8/월)으로 바로잡았다.
- [r4] codex-ide#5 [중간] — `skip_destroy` 2단계 apply의 **성공 기준 "변경 0"이 틀렸다.** 미설정 기본이 `false`이므로 `false/null → true` in-place 반영이 뜬다 → **반영** — 성공 기준을 **"replacement 없이 in-place 반영 + state에서 `true` 확인"**으로 고치고, **`-/+`가 뜨면 중단**하는 게이트를 넣었다. 2-5 번호 목록도 두 apply로 펼쳤다.
- [r4] codex-ide#6 [중간] — 2-6의 신규 연결 자극이 여전히 *"…등 방법과 최대 대기 시간을 정하고"*로 **실행 시점에 선택을 미룬다.** 짧은 요청 26개로는 idle 25개 점유를 보장하지 못하고, `defer`라는 **표현만으로는 예외·터미널 종료 시 자동 복귀가 일어나지 않는다** → **반영** — 자극 방법(`pg_sleep`/k6로 동시 30 이상 2초 유지), **최대 대기 5분**, **`trap` + 마지막 `describe-rule`로 `State=ENABLED` 확인까지가 완료 조건**으로 확정했다.
- [r4] claude-ide#6 [낮음] — 신규 알람의 **`treat_missing_data` 미지정.** 기존 13개는 전부 명시하고 희소 지표는 기본값이면 INSUFFICIENT_DATA에 앉는다 → **반영** — `FailedInvocations`(2)·`Errors`·전달 실패 2개 = **`notBreaching`**, DLQ depth 2개 = **`missing`**으로 값을 적었다.
- [r4] claude-ide#7 [낮음] — 2-0의 *"백그라운드 refresh"*가 미결정이고 2-1 ⑦(goroutine 누수 0)과 긴장한다 → **반영** — **백그라운드 goroutine을 두지 않고 복구는 다음 `Connect`가 이끈다**로 확정했다. 근거도 리뷰어 제안대로 적었다 — **canary가 `/readyz`를 30초마다 상시 프로빙하므로 무트래픽에서도 `Connect` 기회가 끊기지 않는다.**
- [r4] claude-ide#8 [낮음] — 우발 계획이 08-30에만 날짜가 박혀 **09-06 창에 적용되는 문장이 없다** → **반영** — *"Step 1이 라이브가 아닌 **모든** 회전 창에 대해, 창이 열리기 전 브릿지를 사전 무장한다"*는 일반 규칙으로 다시 썼다.

*개선 제안*

- [r4] claude-ide 개선 — bounded wait 잔여 위험을 리스크표에 → **반영**(위 충돌 조정과 같은 결론).
- [r4] claude-ide 개선 — `aws_cloudwatch_log_resource_policy`는 리전 단위라 **새 `policy_name`**으로 → **반영 예정**(1-1 구현 시 이 plan 전용 이름 사용).
- [r4] claude-ide 개선 — 1-3 검증 목록 확장 → **반영**(`Timeout`/`MemorySize`, 알람 7개의 `treat_missing_data`).
- [r4] claude-ide 개선 — 1-6 드릴에 **컷오프 10분**(1-7과 일관) → **반영**.
- [r4] codex-cli 개선 — 2-5 번호 목록을 두 apply로 펼치기 → **반영**(codex-ide#5와 같은 곳).
- [r4] codex-cli 개선 — 배경에 *"1-5 드릴"* 잔재(번호 이동 후 미정리) → **반영** — 전부 1-6으로 맞췄다.
- [r4] codex-cli 개선 — `archive_file`의 `output_path`와 zip의 git 추적 방침 → **반영**(`.gitignore` 대상 경로).
- [r4] codex-ide#2 후반 — 1-5에서 고친 런북 창 날짜는 **드릴 전 값**이라 1-6 후 다시 기록해야 한다 → **반영**.

**[r4] 병합 자기점검**:
- **이번 [높음] 3건 중 2건이 "revision 4가 새 장치를 넣으면서 실행 파라미터를 비워 둔 것"**이다 — bounded wait를 넣고 `timeout`을 안 정했고, reserved concurrency를 넣고 계정 한도를 안 봤다. r1이 "API가 있다 ≠ 쓸 수 있다", r2가 "권한이 있다 ≠ 다 있다"였다면 **r4는 "계약을 정했다 ≠ 그 계약이 돌아갈 설정을 했다"**다. 같은 층위의 실수가 형태만 바꿔 반복된다.
- **기한 모순은 내가 어제 만든 것**이다. 우발 계획을 확정 판단으로 격상하면서 헤드라인 기한을 함께 고치지 않았다. **한 곳을 고치면 그것이 참조되는 모든 곳을 같은 커밋에서 확인한다.**
- **상충 처방을 처음으로 명시적으로 조정했다**(bounded wait API). 0007의 교훈대로 **우선순위를 먼저 선언**(최소권한 > 확인 강도)하고, 기각된 쪽의 지적을 **잔여 위험으로 살려 두었다.** 어느 한쪽을 조용히 떨어뜨리지 않았다.
- revision 5에서 새로 쓴 문장 — `credentialsReadyAt` 기준, 순차 중복 bounded 확인, 기한 3분할 표, 구간별 목표 표, 2-6 `trap` 계약 — 은 다음 라운드에 검증받아야 한다.

**[r3] revision 3 → 4** (3인 전원 제출, 전부 `reviewed-revision: 3`. **기각 0건**):

*결함*

- [r3] codex-cli#1 [높음] — 좁은 rule의 `detail.labelUpdated`를 *"문자열이며 배열 아님"*으로 확정한 것이 **event pattern 문법과 반대**다. 이벤트 **본문**은 문자열이지만 **패턴의 비교 값은 배열**이어야 하고 AWS 공식 예제도 `["AWSCURRENT"]`를 쓴다 → **반영** — revision 3이 만든 사실 오류다. 1-1을 `["AWSCURRENT"]`로 고치고, **1-2 fixture에서 패턴(배열)과 이벤트 본문(문자열)을 구분해 적도록** 명시했다. 다이어그램(`:110`)도 맞췄다. 이대로 뒀으면 `PutRule` 단계에서 깨졌다.
- [r3] claude-ide#1 / codex-ide#1 [높음] — **1-6(구 1-5) 1-6 드릴이 성공하면 1-7의 "2026-08-30 창"은 존재하지 않는다.** `AutomaticallyAfterDays`는 *"이전 회전을 기준으로 다음 회전일을 계산"*하며 매 성공 회전 후 재계산된다(API 레퍼런스 원문 인용) → **반영** — revision 3은 이 질문을 *"착수 전 확인"*으로 실행 단계에 미뤘는데, **r1이 막았던 것과 같은 종류**(plan 시점에 1차 출처로 닫을 수 있는 전제를 미뤘고 그 답이 헤드라인 날짜를 바꾼다)라는 지적이 정확하다. 배경에 **"회전 스케줄은 드릴로 이동한다"** 절을 신설해 **예상 동작으로 확정 기재**하고, 절대 기한을 *"현재 `NextRotationDate`로 계산한 다음 창이 열리기 전"*으로 재정의했다. 1-7 대상을 재계산된 창으로, 우발 계획을 *"다음 창 하루 전"*으로 연동하고 런북 `(e)` 갱신을 1-5에 넣었다. **추가로 claude-ide가 지적한 순서 강제** — *"Step 1이 라이브가 아닌 상태에서 드릴을 돌리면 장애를 하나 더 만드는 것"* — 을 1-6의 **⛔ 선행 조건**으로 못박았다.
- [r3] codex-cli#2 [높음] — Secrets Manager **eventual consistency**를 Step 1이 무시한다. 이벤트 직후 곧바로 재배포하면 새 태스크가 **옛 `AWSCURRENT`를 주입받아 실패**할 수 있고, handler는 이미 성공 반환해 추가 이벤트가 보장되지 않는다. **Step 2에서는 그 창을 인정하면서 Step 1에서만 즉시 반영을 가정하는 것은 일관되지 않다** → **반영** — 일관성 지적이 날카롭다. `UpdateService` **전에** 이벤트 `detail.versionId`가 `AWSCURRENT`에 붙었는지 확인하는 **bounded wait(≤60초)**를 handler 계약에 넣고, 상한 초과 시 **오류 반환**으로 재시도 경로를 타게 했다. 이 확인에 쓰는 API를 **Lambda execution role에 추가**(시크릿 ARN 한정)하고 1-2 (c) 테스트 케이스로 넣었다.
- [r3] codex-cli#3 / codex-ide#4 [높음/중간] — `time` vs `createdAt` 대조만으로는 **동시 중복 호출**을 막지 못한다. 두 Lambda 실행이 동시에 `DescribeServices`를 읽으면 둘 다 재배포하는 read-then-write race이고, **프로세스 로컬 lock은 다른 execution environment에 효력이 없어 fake 단위 테스트가 통과해도 운영 원자성은 증명되지 않는다** → **반영** — **`reserved_concurrent_executions = 1`**로 직렬화한다(회전은 7일 1회라 처리량 제약 없음). DynamoDB 조건부 기록은 리소스를 하나 더 만들어야 해 이 규모에서 채택하지 않았다. 1-2에 *"동시성은 단위 테스트로 증명되지 않는다 — 1-3에서 설정 확인으로 갈음"*을 명시했다.
- [r3] codex-cli#4 / codex-ide#3 / claude-ide#2 [중간, **3인 전원**] — 광역 관찰 rule의 **CloudWatch Logs 타깃에 resource-based policy가 없다.** EventBridge는 Logs 타깃에 `role_arn`을 쓰지 않고 log group의 resource policy로 `events.amazonaws.com`에 `logs:CreateLogStream`/`PutLogEvents`를 허용해야 한다 → **반영** — 빠지면 *"좁은 rule이 미매칭일 때 원문을 남기려던 장치까지 함께 침묵"*(claude-ide)해 이 rule의 존재 이유가 사라진다. (a) `aws_cloudwatch_log_group`(`/aws/events/` 접두사, `retention_in_days`) (b) `aws_cloudwatch_log_resource_policy`(log group ARN 한정) (c) **타깃에 `role_arn`을 주지 않는다**를 1-1에 넣고, 1-3 검증과 광역 rule `FailedInvocations` 알람도 추가했다.
- [r3] codex-ide#2 [높음] — 2-0의 *"terminal cap을 두면 영구 실패하므로 무한 backoff"*가 **전제부터 틀렸다.** `Connect`가 오류를 반환해도 `sql.DB`는 다음 요청에서 다시 호출한다. 반대로 `database/sql`은 풀 보충을 위해 `Connect`를 직렬 호출하므로 그 안에서 무한 루프를 돌면 **connectionOpener를 점유**한다. 공식 계약도 dial timeout을 별도로 두라고 명시 → **반영** — **r2 claude-ide#2를 반영하면서 반대편으로 과교정한 것**이다(r2 병합의 전형적 재발 패턴). 계약을 다시 썼다: **`Connect` 1회는 호출별 timeout으로 유한하게 실패하고, "포기 없음"은 호출 수준이 아니라 provider 전역 상태의 계약**이다. 2-1 검증에 ⑦goroutine 누수·폭주 없음, ⑧`Connect`의 유한 종료를 추가했다.
- [r3] codex-cli#5 / codex-ide#6 / claude-ide#4 [중간/낮음, **3인 전원**] — `skip_destroy` 택일을 2-5 실행 시점으로 미뤄 두면 **승인된 plan대로 2-2를 해도 최종 HCL과 `plan` 결과가 확정되지 않는다.** r1이 아키텍처 미확정을 막은 것과 같은 이유 → **반영** — **`skip_destroy = true`를 채택 확정**했다(장애 중 "직전 revision을 다시 가리키는" 빠른 롤백이 가능해야 하고, 누적 ACTIVE revision은 이 규모에서 실질 비용이 없다). **claude-ide#4의 적용 순서 지적도 반영** — 한 apply에서 replacement가 어느 설정값을 보는지 자명하지 않으므로 **① `skip_destroy`만 apply → ② env 변경 apply**로 분리했다. 앱 이미지 롤백은 CI가 최신 ACTIVE를 base로 재등록하므로 이것과 별개임을 명시했다.
- [r3] codex-ide#5 / codex-cli 개선 [중간] — **DLQ depth 알람으로는 "DLQ로 보내는 동작 자체의 실패"를 못 본다** — 그때 depth는 계속 0이고, 권한 오류가 바로 이 실패를 만든다 → **반영** — r2에서 내가 *"DLQ 적재·전송 실패 관측 → depth 알람으로 반영"*이라고 적은 것이 부정확했다. EventBridge `InvocationsFailedToBeSentToDlq`와 Lambda `DestinationDeliveryFailures` 알람을 추가해 **알람 6개**로 늘리고 비용도 갱신했다(약 $0.7/월).
- [r3] codex-ide#7 [중간] — 2-6이 **기존 pool에서 신규 연결을 어떻게 만들지** 정의하지 않았다. idle 25개 + 수명 5분이라 회전 직후 요청이 기존 세션만 재사용하면 **refresh 로그가 안 나와 판정 불가**인데 Step 1은 계속 꺼진 상태로 남는다 → **반영** — 신규 연결 자극 방법·최대 대기 시간·**판정 불가 시 rule 즉시 재활성화(`defer`)**를 2-6에 넣고 리스크표에 행을 만들었다. T0/T1 정의도 명시했다.
- [r3] claude-ide#3 [중간] — **`source_code_hash`가 없으면 `handler.py`를 고쳐도 배포되지 않는다**(`plan`에 아무것도 안 뜸). `hashicorp/archive` provider도 `versions.tf`·lock에 없다 → **반영** — lock에 aws 하나뿐인 것을 확인했다. `source_code_hash = archive_file.output_base64sha256` 필수와 provider 선언·lock 갱신을 산출물 경계에 넣었다. **1-6 드릴에서 멱등 로직 결함을 발견해 고쳐야 할 때 조용히 막히는 함정**이라는 지적이 맞다.
- [r3] claude-ide#5 [낮음] — *"두 SQS의 queue policy"*가 한쪽에 해당하지 않는다. **Lambda on-failure 큐는 Lambda 서비스가 함수 execution role로 넣으므로** 필요한 것은 execution role의 `sqs:SendMessage`뿐이고 `events.amazonaws.com` queue policy는 불필요하다 → **반영** — 그대로 뒀으면 **쓰이지 않는 신뢰 관계가 하나 생긴다.** IAM 최소권한이 이 plan의 값어치인 만큼 두 큐의 권한 근거를 나눠 적었다.
- [r3] claude-ide#6 [낮음] — revision 3의 내부 불일치 3건(ADR 절 "IAM 3분할" vs 본문 4분할 / 다이어그램의 `labelUpdated` 표현 / ADR 목록에 revision 3의 새 결정 누락) → **반영** — 셋 다 고쳤다. ADR 목록에 Python 런타임 선택 이유와 트레이드오프, 2계층 실패 보존, **멱등 판정 근거**, 광역 rule 존재 이유, `skip_destroy` 근거를 넣었다.

*개선 제안*

- [r3] codex-cli 개선 — `maximum_retry_attempts`·`maximum_event_age_in_seconds`를 "설정한다"로 끝내지 말고 값과 근거를 확정 → **반영**(`2`, `3600` + 근거).
- [r3] codex-cli 개선 — 승계표의 `:476` 반영 위치가 "2-5"인데 실제는 2-6 → **반영**(확인 후 정정).
- [r3] codex-cli 개선 — 1-2의 *"에이전트 수행 — read-only"*가 handler·fixture **작성**까지 금지하는 것처럼 읽힌다 → **반영** — *"변경형 AWS 명령 금지, 저장소 파일 작성은 구현 범위"*로 가드레일의 뜻을 분리했다.
- [r3] codex-ide 개선 — `hashicorp/archive` provider 선언·lock 갱신 → **반영**(claude-ide#3과 같은 곳).
- [r3] codex-ide 개선 — on-failure destination 검증에 **실패 fixture를 직접 invoke**해 실제 SQS 메시지와 `DestinationDeliveryFailures=0` 확인 → **반영**(1-4 ②). 설정 조회만으로는 경로가 증명되지 않는다.
- [r3] claude-ide 개선 — 2-6의 판별 수단 ②는 **백스톱을 스스로 끄고 스케줄을 또 민다** → **반영** — ①을 기본으로, ②는 ①이 불확정일 때만 쓰는 조건부로 낮추고 복귀 체크리스트·담당을 못박았다.
- [r3] claude-ide 개선 — **1-6(런북)을 드릴 앞으로** → **반영** — 순서를 1-5(런북) → 1-6(드릴)로 바꿨다. 드릴 중 자동화가 실패하면 그 순간 필요한 것이 계층 구분법이라는 지적이 맞다.
- [r3] claude-ide 개선 — Lambda 로그 그룹을 Terraform으로 명시(자동 생성은 retention 무기한) → **반영**(1-7 사후 증거가 이 로그에 의존).
- [r3] claude-ide 개선 — 회전 창 좁히기(`Duration`)에 한 줄 결론 → **반영** — **채택하지 않는다**로 결론과 근거를 남겼다(Step 1·2가 창 길이와 무관하게 동작해 의존성이 없고, RDS 관리 시크릿이라 규칙 변경 가능 여부 확인이 별도로 필요하다). 확인·채택은 plan 0011 이후. **이후 라운드에서 재제기하지 않는다.**
- [r3] claude-ide 개선 — 관리형 런타임의 boto3 버전은 AWS가 고정해 주지 않는다 → **반영**(ADR 트레이드오프 한 줄).

**[r3] 병합 자기점검**:
- **r2 병합에서 내가 새로 쓴 문장 두 개가 이번에 잡혔다** — `labelUpdated` "문자열, 배열 아님"(패턴 문법과 반대)과 "무한 backoff"(전제 자체가 틀림). 후자는 **r2 claude-ide 지적을 반영하다 반대편으로 넘어간 것**으로, [[cr-merge-minimal-edits]]가 경고한 재발 패턴의 세 번째 사례다. **서로 다른 리뷰어의 처방을 합칠 때 충돌 입력의 우선순위를 먼저 명시**한다는 0007의 교훈이 여기에도 적용된다.
- **"확인 사항으로 미룬 것"이 또 결함이 됐다** — r1에서 (P1)/(P2)를 실행 단계로 미뤄 막혔는데, r2에서 드릴의 스케줄 이동을 똑같이 *"착수 전 확인"*으로 미뤘다. **plan 단계에서 1차 출처로 닫을 수 있는 질문은 미루지 않는다.**
- revision 4에서 새로 쓴 문장 — bounded wait 계약, reserved concurrency 1, CWL resource policy 구성, `Connect`의 유한 예산/provider 전역 backoff 분리, `skip_destroy` 2단계 apply, 알람 6개 — 은 다음 라운드에 검증받아야 한다.

**[r2] revision 2 → 3** (codex-cli / codex-ide / claude-ide **3인 전원 제출**, 전부 `reviewed-revision: 2`. **기각 0건**):

*결함*

- [r2] codex-cli#1 / codex-ide#1 [높음] — Lambda가 합류 판정에 `DescribeServices`를 호출하는데 execution role에는 `ecs:UpdateService`만 계획돼 **첫 호출이 `AccessDenied`로 끝난다** → **반영** — IAM을 **3분할에서 4분할로** 바꿔 서비스 ARN 한정 `ecs:DescribeServices`를 넣었다. 기존 배포 role도 같은 이유로 두 권한을 함께 갖는다(`github_oidc.tf:104-111`, 리뷰어 지적대로 확인). `failures[]`가 비었지 않으면 성공 skip이 아니라 **오류 반환**하는 계약도 1-1·1-2(d)에 넣었다.
- [r2] codex-cli#2 / codex-ide#2 [높음] — "진행 중이면 skip"은 멱등 조건이 아니다. ①회전 **전에** 시작된 배포가 진행 중이면 skip해 옛 비밀번호 태스크가 잔존 ②첫 배포 완료 후 지연 중복은 `deployments==1 && COMPLETED`라 다시 재배포 → **반영** — 지적이 정확하다. 내 조건은 **선행 배포 경합에는 누락, 지연 중복에는 중복**을 만든다. 멱등 판정을 **이벤트 `time` vs primary deployment `createdAt` 대조**로 바꾸고, 이벤트보다 오래된 진행 중 배포면 skip이 아니라 **재배포를 건다**로 뒤집었다. 1-2 단위 테스트에 (a)선행 진행 중 (b)완료 후 재호출 (c)동시 중복 (d)`DescribeServices` 실패 4케이스를 명시했다.
- [r2] codex-cli#3 / codex-ide#3 [높음] — EventBridge target DLQ는 **호출 전달 실패만** 보존한다. rule의 Lambda 타깃은 비동기라 큐 적재 시 target 호출은 성공으로 끝나고, 이후 handler의 `AccessDenied`·타임아웃·예외는 그 DLQ로 가지 않는다 → **반영** — 실패 보존을 **두 계층**으로 분리했다: (계층 1) EventBridge target DLQ + 재시도 정책, (계층 2) **Lambda asynchronous invocation on-failure destination(SQS)** + `maximum_retry_attempts`·`maximum_event_age_in_seconds`. 두 SQS의 queue policy·Lambda role 권한, DLQ depth 알람 2개, 계층 구분 런북(1-6)까지 넣었다. **"조용히 버려지지 않는다"는 완료 조건이 계층 1만으로는 성립하지 않는다는 지적이 맞다.**
- [r2] claude-ide#1 [높음] — 리스크표 완화 칸의 *"Step 1이 이미 배포돼 있어 최악에도 자동 재배포로 복구된다"*가 **거짓**이다. `force-new-deployment`는 같은 task definition = **같은 이미지**를 다시 띄우므로 Step 2의 코드 결함은 그대로 재현되고, 서킷브레이커 `rollback`도 같은 revision이면 무동작 → **반영** — **0008 `:474`가 0009로 승계한 항목을 내가 완화 칸에서 정면으로 위반했다.** 완화 칸에서 그 주장을 삭제하고 **"Step 1은 이 경우의 백스톱이 아니다"**를 명시했다. 목표 절의 *"두 Step이 서로의 백스톱"*도 **한 방향만 참**(Step 1 → Step 2의 *refresh 실패*만 받아준다)으로 정정했다. 유일한 백스톱은 **이전 이미지 태그로 CI 재배포**다.
- [r2] claude-ide#2 [높음] — **회전 중 `AWSCURRENT`가 아직 옛 값인 구간**이 설계에 없다(0008 `:472` 승계 미반영). `setSecret`(DB 변경) → `testSecret` → `finishSecret`(라벨 이동) 순서라 **DB는 새 비밀번호인데 조회는 옛 값**인 창이 있고, 그때 `28P01`→재조회→같은 값→`28P01`이 반복된다 → **반영** — revision 2는 목표를 "수 초 내 회복"으로 낮췄을 뿐 그 구간의 **동작을 설계하지 않았다**는 지적이 정확하다. 2-0에 (나)절을 신설해 두 계약을 못박았다: **①재시도 상한은 terminal cap이 아니라 rate limit**(포기하면 그 창에서 영구 실패로 남는다) **②값이 바뀌지 않은 refresh는 세대를 올리지 않는다**(올리면 이어지는 `28P01`이 전부 새 세대 실패로 집계돼 재조회 급증). 2-1 ⑥과 2-4에 재현 테스트를 넣었다.
- [r2] codex-cli#4 [중간] — Step 2 실행 순서가 배포 게이트 선언과 충돌한다. 2-2가 "apply 후 CI 배포 1회"인데 뒤의 2-3·2-4가 "전부 통과해야 배포"라, 순서대로 실행하면 **미검증 connector가 먼저 프로덕션에 간다** → **반영** — 2-2를 **코드·HCL 작성과 `terraform plan`까지만**으로 자르고, 배포를 **2-5 [사람] apply → CI 배포 1회 → smoke**로 분리했다. `terraform plan` 예상도 "정책 1건만"이 아니라 **실제 resource action 기준**(정책 신설 + task definition `-/+`)으로 고쳤다.
- [r2] codex-ide#4 / claude-ide#6 [높음/중간] — **Step 1을 켠 채로는 Step 2의 회복을 입증할 수 없다.** 기존 세션이 최대 5분 살아 있고 Step 1이 수 분 내 재배포하므로 Step 2 경로가 한 번도 실행되지 않을 수 있고, 그 `/readyz` 200은 Step 1이 env를 다시 주입한 증거일 뿐이다 → **반영** — 0008 `:476` 승계 미반영분이다. 2-6에 판별 수단을 **둘 다** 넣었다: ①앱이 "refresh 성공, 세대 N→N+1"을 구조화 로그·지표로 남기고 그 타임스탬프가 `deployments[].createdAt`보다 **앞서는지 대조** ②통제 창에서 **rule을 일시 `state="DISABLED"`**로 두고 온디맨드 회전(수동 브릿지 사전 무장 필수, 검증 후 즉시 복귀).
- [r2] codex-ide#5 [중간] — `aws_ecs_task_definition.app`에 `skip_destroy`가 없어 env 변경 replacement 때 이전 revision이 **deregister(INACTIVE)** 되고, INACTIVE로는 서비스를 되돌릴 수 없어 "직전 revision으로 롤백"이 실패한다. main push가 자동 배포를 시작하므로 앱 merge가 인프라보다 먼저 가면 안 된다. 로컬 `DATABASE_URL` 경로에는 ARN이 없다 → **반영** — `skip_destroy` 부재를 코드로 확인했다. 2-5에 **`skip_destroy=true`로 이전 ACTIVE 보존 / 롤백을 "HCL revert → 새 revision 등록 → CI 재배포"로 재정의** 중 **택일 확정**을 넣고 리스크표에도 행을 만들었다. 배포 순서 강제와 **로컬 계약**(`DB_SECRET_ARN` 미설정 시 정적 비밀번호로 동작 — P0 로컬 완결성 유지)도 2-2에 넣었다.
- [r2] claude-ide#3 [중간] — 1-6(구 1-7)에 증거 수집 절차가 없다. **회전 창이 24시간**이라 "회복 시간을 실측한다"를 실행할 방법이 없고, 새벽에 조용히 성공하면 T0도 T복구도 안 남는다 → **반영** — 1-7을 **사후 증거 조합**으로 재작성했다(`LastRotatedDate` + Lambda 로그의 수신 이벤트 `time`/`id` + `deployments[].createdAt`·`rolloutState` 전이 + 알람 이력). 이를 위해 **1-1에 "Lambda가 수신 이벤트의 `id`·`time`·`detail.versionId`를 구조화 로그로 남긴다"**는 요구를 추가했다(비밀값 아님).
- [r2] claude-ide#4 [중간] — **온디맨드 회전 명령이 0009에서 사라졌다**(0008 `:470` 승계). `aws rds modify-db-instance --rotate-master-user-password --apply-immediately`를 쓰면 08-30을 종단 첫 시험으로 삼지 않아도 된다 → **반영** — 승계 항목을 조용히 떨어뜨린 것이 이번 재발의 패턴이라는 지적이 정확하다. **1-5 [사람] 온디맨드 회전 드릴**을 신설하고 사전 조건(브릿지 사전 무장·(b-0) 관측 루프 선행)과 감수 사항(통제된 수 분 다운)을 명시했다. **⚠️ 착수 전 확인 항목**으로 *"온디맨드 회전이 `LastRotatedDate`를 갱신해 다음 자동 창이 +7일로 밀리는가"*를 넣었다 — 밀리면 08-30 기한의 의미가 바뀐다.
- [r2] claude-ide#5 [중간] — Lambda 산출물 경계 미정(런타임·경로·패키징). 저장소의 첫 Lambda이고 CI에 빌드 단계가 없어, Go를 택하면 기한 내 작업량이 바뀐다 → **반영** — r1이 막았던 "실행 중 범위가 흔들린다"와 같은 종류의 미정이라는 지적이 옳다. **Python 3.13 + boto3**(런타임 내장이라 패키징·빌드 불요), **`infra/prod/lambda/rotation_redeploy/handler.py`**, **`data "archive_file"` 인라인 zip**으로 확정했다.
- [r2] claude-ide#7 [낮음] — 1-2의 fixture 경로 `infra/prod/fixtures/`가 **존재하지 않으며** 0005 선례가 아니다 → **반영** — 확인했다(0005·0006 모두 plan 디렉터리 하위). **`docs/plans/0009-p4d2-rotation-resilience/fixtures/`**로 바로잡았다.

*개선 제안*

- [r2] codex-cli 개선 — Lambda handler 단위 테스트로 프로덕션 직접 invoke가 첫 로직 시험이 되지 않게 → **반영**(1-2에 4케이스).
- [r2] codex-cli / codex-ide 개선 — DLQ 적재·전송 실패 자체의 관측 → **반영**(두 DLQ depth 알람).
- [r2] codex-cli 개선 — connector의 `(password, generation)`을 **호출별 로컬 상태**로 묶어라. 공유 콜백의 가변 필드는 25개 동시 연결에서 서로 덮어쓴다. backoff·SDK는 `context.Context` 취소를 따라야 한다 → **반영**(2-0 가).
- [r2] codex-cli / codex-ide 개선 — 비용 "월 $0.2 미만"이 부정확하다. 이미 표준 알람 13개라 무료 10 한도를 넘겼을 가능성이 높다 → **반영** — **약 $0.5/월 수준, Free Tier 사용량에 따라 변동**으로 고쳤다(알람이 2개→4개로 늘어난 것도 반영).
- [r2] claude-ide 개선 — **미매칭은 무증상**이다. `FailedInvocations`는 호출 시도가 있어야 운다 → **반영** — `detail` 필터 없는 **광역 관찰 rule(Logs 타깃)** 병설을 1-1에 넣고 리스크표에 행을 만들었다.
- [r2] claude-ide 개선 — `is_enabled`는 provider 6.x에서 deprecated, `state = "DISABLED"`가 맞다 → **반영**(리스크표).
- [r2] claude-ide 개선 — `AWSPREVIOUS`는 **애초에 이벤트가 발행되지 않아** 반례로 무의미하다 → **반영** — 반례를 (a)다른 ARN (b)커스텀 스테이징 라벨 (c)다른 `detail-type`으로 교체했다.
- [r2] claude-ide 개선 — 기한을 놓쳤을 때의 **우발 계획**(조건부 기한이 아니다) → **반영** — *"08-29까지 Step 1이 라이브가 아니면 [사람]이 브릿지를 사전 무장한다"*를 Step 1 끝에 넣었다.
- [r2] claude-ide 개선 — 2-4 로컬 Postgres 실행 주체 → **반영**(`[사람]` 표시).
- [r2] codex-ide 개선 — 두 DLQ를 합칠지 분리할지와 계층 구분 런북 → **반영**(분리 + 1-6 런북에 구분법).

**[r2] 병합 자기점검**:
- **0008 승계 8건 중 3건을 누락하고 1건을 위반했다**(claude-ide 총평). 승계 목록이 0008에 있는데 revision 2를 쓰면서 그 목록을 대조하지 않았다. → 배경에 **승계 전수 대조표**를 만들어 이후 라운드마다 항목별로 확인한다. **이 누락 패턴 자체가 8/23 재발의 원인**(8/16 액션 #10을 백로그로 미룸)과 같은 것이다.
- r1의 두 결함이 "API가 있다 ≠ 쓸 수 있다"였다면, r2의 결함은 **"권한이 있다 ≠ 필요한 권한이 다 있다"**(`DescribeServices` 누락)와 **"DLQ가 있다 ≠ 내 실패를 잡는다"**(계층 혼동)였다. 둘 다 *한 겹 더 들어가서 확인하지 않은* 것이다.
- revision 3에서 내가 새로 쓴 문장 — 멱등 판정(`time` vs `createdAt`), 2계층 실패 보존, 회전 중 창의 두 계약, Lambda 산출물 확정, 1-5 온디맨드 드릴, 2-5 롤백 택일 — 은 다음 라운드에 검증받아야 한다.

**[r1] revision 1 → 2** (codex-cli / codex-ide 2인 검토. claude-ide 미제출. **기각 0건**):

- [r1] codex-cli#1 / codex-ide#1 [높음] — Step 1-0이 아키텍처를 실행 단계로 미뤄 승인 후 재설계가 필요하고, 기한 직전에 범위가 흔들린다. 두 전제는 지금 확정 가능하다 → **반영** — Step 1-0을 삭제하고 "아키텍처 확정" 절을 배경에 신설했다. **(P1) 참**이나 쓸 이벤트가 다르다(네이티브 `Secret Label Updated`, `resources[0]`에 ARN). **(P2) 거짓** — event rule의 ECS 타깃은 `RunTask`뿐이고 universal target은 Scheduler 기능이다 → **Lambda 확정**. 리뷰가 제시한 1차 출처를 그대로 인용했다.
- [r1] codex-cli#2 [높음] — `RotationSucceeded`(CloudTrail)를 쓰면 ARN 정확 매칭 근거가 실제 스키마와 달라 영구 미매칭 가능. 실제 2차 장애 이벤트에서 `Resources=[]`, ARN은 `detail.additionalEventData.SecretId` → **반영** — 리뷰어가 실이벤트(`EventId=0c0b51dc-…`)를 조회한 증거가 결정적이다. CloudTrail 경로를 **채택하지 않는다**고 명시하고 사유(빈 `resources`, best-effort 전달)를 남겼다.
- [r1] codex-cli#3 / codex-ide#2 [높음] — Step 1-3의 `put-events`가 변경형인데 `[사람]` 표시가 없어 가드레일 충돌. 게다가 **고객 이벤트는 `aws.` source를 쓸 수 없어** 운영 rule 종단 검증 자체가 불가 → **반영** — 내가 놓친 기술적 사실이다. 검증을 셋으로 분리했다: **1-2** 패턴 fixture(에이전트, read-only) / **1-4** [사람] Lambda 직접 invoke / **1-6** 실제 회전에서 rule→Lambda 종단. "여기서 종단 검증됐다고 적지 않는다"를 1-4에 못박았다.
- [r1] codex-cli#4 / codex-ide#3 [높음] — `OptionBeforeConnect`는 연결 **전**에만 돌아 `28P01`을 관찰 못 한다. plan 0008이 이미 승계한 결함인데 0009가 해소하지 않았다 → **반영** — `stdlib/sql.go:266-273`을 직접 확인했다(훅 → `ConnectConfig`가 오류를 곧장 반환). **`stdlib.GetConnector`를 `driver.Connector`로 감싸 `Connect` 오류를 관찰**하는 설계로 2-0을 재작성하고, `errors.As(*pgconn.PgError)`·`Code=="28P01"`·세대 기반 조건부 무효화·singleflight·재시도 상한을 확정했다. **0008 `:109-110`·`:471`이 "0009에서 해소한다"고 넘긴 것을 내가 다시 놓쳤다는 지적이 정확하다.**
- [r1] codex-cli#5 / codex-ide#4 [높음] — 앱에 시크릿 ARN을 전달하는 경로가 없어 Step 2가 런타임에 동작 불가. task role 권한만으로는 부족하고, env 추가는 task definition 새 revision을 만들어 CI 배포 1회가 필요 → **반영** — 2-2를 신설해 `DB_SECRET_ARN` env, `Config`·`db.Open` 경계 변경, task role 정책을 함께 넣었다. **revision 1의 "정책 신설 1건만, 다른 변경 0"이 틀렸음을 본문에 명시**하고 `ignore_changes`+ADR 0001 순서(apply → CI 배포 1회)를 배경과 2-2·리스크표에 넣었다.
- [r1] codex-cli#6 / codex-ide#6 [중간] — Step 2 종단 검증을 실전 회전까지 미루면 되돌림 피드백이 너무 늦다 → **반영** — **2-4 통합 테스트를 배포 게이트로** 신설했다. 로컬 Postgres에서 실제 비밀번호 변경 → 풀 예열 상태에서 재시작 없이 회복, 동시 요청 시 조회 1회 합류, SDK 일시 실패 후 회복까지 포함했다.
- [r1] codex-cli#7 / codex-ide 개선 [중간] — 타깃 실패·중복 호출을 다루지 않아 "10분 내 회복"을 보장 못 한다 → **반영** — DLQ + 재시도 정책 + `FailedInvocations`·Lambda `Errors` 알람 + 멱등/coalescing(진행 중 배포면 skip)을 1-1에 넣고 1-4에서 연속 2회 호출로 검증한다. 런북(1-5)에 자동화 실패 시 수동 브릿지 전환 분기를 넣었다.
- [r1] codex-ide#5 [중간] — 2-3 폴백이 회전 **후**에는 무효다(실행 중 태스크의 env는 이미 옛 비밀번호) → **반영** — 폴백 계약을 **(a) 기동 시 SDK 장애 = 유효** / **(b) 회전 후 refresh 장애 = 복구 불가**로 나누고, (b)는 Step 1 자동 재배포가 맡는다고 명시했다. **두 Step이 서로의 백스톱**이라는 구조가 이 지적으로 분명해졌다.
- [r1] codex-cli 개선 — `AWSCURRENT`만 사용, `AWSPENDING` 미사용 방침 확정 → **반영**(2-1).
- [r1] codex-cli 개선 — Lambda IAM 3분할(리소스 정책 / execution role의 `UpdateService` / Logs) → **반영**(배경 "아키텍처 확정").
- [r1] codex-cli 개선 — 비용 변화 기재 → **반영**("비용" 절 신설).
- [r1] codex-ide 개선 — ADR로 이벤트·타깃 선택과 IAM 경계를 남기고, 로그에 비밀값 미포함을 테스트로 고정 → **반영**("ADR" 절 신설, 1-1·2-1에 로그 금지 명시).

**[r1] 병합 자기점검**:
- 이번 라운드의 핵심 결함 2건(**Lambda 필요**, **훅이 `28P01`을 못 본다**)은 모두 **내가 존재만 확인하고 동작을 확인하지 않아서** 생겼다. `OptionBeforeConnect`가 "있다"는 것과 "쓸 수 있다"는 것은 다르고, 그 차이를 plan 0008이 이미 적어 둔 것을 다시 놓쳤다. 승인 전 자기점검에서 **"이 API가 내가 원하는 시점에 불리는가"** 를 매번 확인한다.
- revision 2에서 내가 새로 쓴 문장(아키텍처 확정 절, 2-0 설계, 2-2 배포 순서)은 다음 라운드에 검증받아야 한다.
