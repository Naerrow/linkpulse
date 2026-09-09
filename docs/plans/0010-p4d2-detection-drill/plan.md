---
status: in-review
revision: 1
created: 2026-08-21
---

# 0010. P4(d)-2 탐지 드릴 — DB 장애 시 알람 경로 실측

> **번호 주의**: `0009`는 **로테이션 근본 대응**(연결 시점 시크릿 조회)으로 예약돼 있다.
> plan 0008 전체가 그 번호로 지칭하므로 이 드릴은 0010을 쓴다.

## 목표

plan 0008이 바꾼 탐지 경로(**Route53 헬스체크 `/readyz` → `HealthCheckStatus` → us-east-1 SNS → Chatbot → Slack**)가
실제 DB 장애에서 동작하는지 **실측**한다. 0008의 목표 1("전면 실패 시 기대값 약 6분 · 상한 8분")이 참인지 확인하는 단계다.

부수 산출물로 **풀 블라인드 구간**(DB 차단 → 첫 503)과 **태스크 간 풀 만료 편차**를 얻는다.
이 두 값은 plan 0009의 목표 설정(무중단이냐 N초 내 자동 회복이냐)에 직접 쓰인다.

## 배경/제약

**분리 경위**: 이 내용은 원래 plan 0008의 Step 1-7이었다. 0008 r3~r4 두 라운드 연속으로 블로킹 지적이
이 단계 하나에 집중됐고(T0 정의 오류 → T1 정의 오류, 게이트 논리 모순), 그동안 **8/23 로테이션을 막을 수 있는
탐지 변경(0008 Step 1-0~1-6)이 대기 상태로 묶였다.** 프로덕션에 의도적 장애를 일으키는 설계는 본질적으로
검토가 오래 걸리므로 별도 plan으로 분리한다. 아래 본문은 **0008 r5 시점의 Step 1-7을 그대로 승계**한 것이며,
0008 r2~r4의 검토 이력(리뷰어 3인)이 이미 반영돼 있다.

**선행 조건**: plan 0008이 승인·apply되어 canary가 `/readyz`를 프로빙하고 있어야 한다.
그렇지 않으면 이 드릴은 아무것도 검증하지 못한다.

**일정 제약**: 2026-08-23 로테이션 브릿지(0008 Step 1-6)와 **같은 날 수행하지 않는다**(아래 preflight 7).

**가드레일**(AGENTS.md #1): 변경형 `aws` 명령과 `terraform apply`는 **사람이 실행한다.**

## 실행 단계

**방법**: 데이터 SG의 5432 인바운드 규칙(`aws_vpc_security_group_ingress_rule.data_from_app`,
`infra/prod/security_groups.tf:116-123`)을 일시 제거해 **신규 DB 연결만** 차단한다.

**이 드릴이 2026-08-16 장애를 잘 재현하는 이유**: 두 경우 모두 **기존 커넥션은 정상 · 신규 연결만 실패**다.
로테이션은 인증 계층에서, SG 차단은 네트워크 계층([connection tracking](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/security-group-connection-tracking.html))에서
그렇게 되며, 관측 결과가 일치한다.

#### 시계를 넷으로 분리한다

`ConnMaxLifetime`(5분, `db.go:29`)이 태스크마다 독립적으로 흐르므로 차단 시각·부분 실패·전면 실패가 전부 다르다.
r3에서 T0→T1을 고친 것과 **같은 구조의 오류가 태스크 단위에 한 번 더** 있었다.

| 시점 | 정의 | 용도 |
| --- | --- | --- |
| **T0** | SG 규칙 제거 **API 성공 응답** 시각 | 기록만 |
| **T_partial** | 10초 폴링에서 관측한 **첫 503** (태스크 하나의 풀만 소진된 상태일 수 있다) | `alb-target-5xx` 기준점 |
| **T_unavailable** | **연속 6회(60초) 이상 503**이 처음 관측된 구간의 **시작 시각**, 또는 `HealthCheckStatus`가 처음 0이 된 시각 중 **이른 쪽** | **`canary_down` 판정 기준점** |
| **T2** | Slack 알람 수신 시각(알람별로 각각) | 판정 대상 |

- **필수 성공 조건**: `canary_down`의 ALARM 전이 + **us-east-1 SNS→Chatbot→Slack 통지**를
  **T_unavailable로부터 8분 이내** 수신한다. 이 plan이 바꾸는 경로가 바로 이것이므로 단독으로 검증해야 한다.
- **보조 기록**: `alb-target-5xx` 수신 시각을 **T_partial 기준으로** 남긴다. 부분 실패 구간에서도 canary가
  분당 약 16건의 503을 만들어 임계(5건/5분)를 넘기므로 **먼저 울릴 가능성이 높다.** "부분 실패 구간에서 발화했는가"를
  기록하면 두 알람의 역할 분담(**부분 실패 = `alb-target-5xx` / 전면 실패 = `canary_down`**)이 실측으로 정리되고
  런북 `:205` 갱신에 그대로 쓸 수 있다. 단 **필수 조건을 대체하지 않는다**(그것만으로 통과시키면 Route53 경로가
  고장 나 있어도 드릴이 성공해 버린다).
- **별도 기록**: **T0→T_partial = 풀 블라인드 구간**(0009의 목표 설정에 직접 쓰인다),
  **T_partial→T_unavailable = 태스크 간 풀 만료 편차**.
- **원장**: 10초 폴링의 **전체 상태코드 시계열을 파일로 보존**한다(간헐적 200이 섞인 구간을 사후 판정할 수 있어야 한다).
  T0 · T_partial · T_unavailable · 각 CloudWatch ALARM 전이 · 각 Slack 수신 · T_restore · `/readyz` 회복 시각.

#### 시작 전 preflight (하나라도 미충족이면 시작하지 않는다)

1. **`terraform plan` = `No changes`** — 기준선이 깨끗해야 한다. 다른 drift가 남아 있으면 뒤의 "1 add만"을 판정할 수 없다.
   (드릴 **전에는 규칙이 살아 있으므로** `No changes`가 정상이다. "1 add"는 규칙을 지운 뒤에만 나온다.)
2. 대상 SG id · **rule id(`sgr-...`)** · source SG를 읽기 전용으로 확인해 **원장에 고정**한다.
3. **알람 시작 상태**: `/healthz` 200 · `/readyz` 200 · `HealthCheckStatus`=1 연속 유지 ·
   **`canary_down` `StateValue=OK`** · ECS stable.
   이미 ALARM/INSUFFICIENT_DATA면 차단해도 **새 ALARM 전이와 SNS 통지가 발생하지 않아 결과를 해석할 수 없다.**
4. 10초 간격 `/readyz` 폴링을 **드릴 시작 전부터** 걸어 두고 시계열을 파일로 남긴다.
5. **중단 상한**: `T_unavailable + 8분` 또는 `T0 + 20분` 중 **먼저 도달하는 시점**에 즉시 복구한다.
   (20분 산출: T0→T_partial 최대 5분 + T_partial→T_unavailable 최대 5분 + 상한 8분 = 18분, 마진 2분.)
6. 드릴 중 **배포를 실행하지 않는다**(리스크표 참고). CI는 terraform을 돌리지 않으므로
   (`deploy.yml` — "terraform apply는 하지 않는다") 자동 원복으로 드릴이 조용히 끝날 위험은 없다.
7. **2026-08-23 브릿지(Step 1-6)와 같은 날 겹치지 않는다.** 로테이션발 장애와 드릴발 장애가 섞이면
   원인 분리도 실측도 무의미해진다. 드릴은 08-23 이전에 끝내거나, 브릿지 완료를 확인한 뒤 충분한 간격을 둔다.

#### 실행 시퀀스

1. **T0** — 규칙 revoke(사람). API 성공 응답 시각을 T0로 기록한다.
2. **revoke 직후** — `terraform plan -out=drill-restore.tfplan`으로 `data_from_app` **1 add만**인지 확인하고
   **plan 파일로 저장한다.** 저장한 plan을 그대로 apply해야 확인과 적용 사이에 다른 변경이 끼지 않는다.
   → **1 add가 아니면 즉시 비상 복구로 전환한다**(아래).
3. **관측** — 위 원장 항목을 기록한다.
4. **복구** — 저장한 `drill-restore.tfplan`을 **사람이 apply**한다.
5. **최종 검증** — `terraform plan`이 **`No changes`** + `/readyz` 200 + 링크 생성→리다이렉트 왕복 성공.

**비상 복구**(2에서 1 add가 아닐 때): CLI로 규칙을 즉시 재생성해 **서비스를 먼저 살린다.**
그 뒤 `terraform import`(또는 state 정합화)를 사람 승인 아래 별도로 수행한다.
**서비스 복구가 state 정합보다 우선한다.**

## 리스크/롤백

| 리스크 | 완화 |
| --- | --- |
| **드릴이 Terraform state에 drift를 남긴다** | 복구를 CLI가 아니라 **`terraform apply`**로 규정(실행 시퀀스 4). preflight 1에서 `No changes` 기준선을 확인하고, revoke 직후 `1 add`만인지 검증한 plan을 파일로 저장해 그대로 apply한다. 종료 후 `terraform plan` **No changes**를 최종 검증에 둔다 |
| **드릴 중 태스크가 교체되면 기동 실패 루프에 빠진다** | 새 태스크는 `db.Open`의 `pingWithRetry`(15회×1초, `db.go:49-53`)가 전부 실패해 기동하지 못한다 → 재시도 루프 + `ecs-running-tasks-low` 발화. 다만 ALB 타깃그룹은 `/healthz`를 보므로(`alb.tf:30`) **기존 태스크는 healthy를 유지해 교체되지 않는다**. 따라서 **드릴 중 배포 금지**가 핵심 조건이다(preflight 6) |
| 차단이 예상보다 길어져 실사용자에게 영향 | preflight 5의 중단 상한(`T_unavailable + 8분` 또는 `T0 + 20분` 중 먼저)에 도달하면 즉시 복구한다 |
| `canary_down`이 상한 내에 오지 않는다 | 그것이 이 드릴의 **발견**이다(실패가 아니라 결과). 그 경우 `failure_threshold`·`evaluation_periods` 튜닝을 후속 과제로 등록하되, 0008 리스크표의 조건(목표값·런북 §13·드릴 기준을 함께 갱신하고 재실측)을 지킨다 |

**롤백**: 실행 시퀀스 4~5(저장한 plan을 apply → `No changes` 확인). 실패 시 비상 복구(본문 참고).

## 검토 반영 로그

<!-- 형식: [rN] 리뷰어#번호 지적요약 → 반영|기각 — 사유 -->

**[승계] plan 0008 r2~r4의 이 단계 관련 지적은 이미 본문에 반영돼 있다.** 요약:
- [0008 r2] codex-cli#1 후반 — 로테이션 드릴은 성공 시 장애가 나지 않아 탐지 경로를 검증하지 못한다 → 이 드릴을 신설한 근거.
- [0008 r3] claude-ide#1 / codex-cli#1 / codex-ide#1 — SG connection tracking 때문에 T0 기준 판정은 정상 동작도 실패로 만든다 → T0/T1 분리.
- [0008 r3] claude-ide#2 / codex-cli#2 / codex-ide#3 — Terraform 관리 SG 규칙 수동 변경의 state drift → 복구를 `terraform apply`로 규정.
- [0008 r3] codex-ide#2 — "두 알람 중 하나"면 Route53 경로가 고장 나 있어도 통과 → `canary_down` 단독 필수화.
- [0008 r4] claude-ide#1 / codex-ide#2 — `desired_count=2`라 태스크별 풀 만료가 독립적 → T_partial/T_unavailable 재분리.
- [0008 r4] claude-ide#2 / codex-cli#1 / codex-ide#1 — 시작 전 `1 add` 게이트는 논리적으로 불가능 → preflight(`No changes`)와 revoke 후 검증으로 분리.
- [0008 r4] codex-cli#2 — 드릴 직전 `canary_down`이 OK인지 확인하는 게이트 부재 → preflight 3.
- [0008 r4] codex-ide 개선 / codex-cli — 복구 plan 파일 저장·비상 복구·중단 상한·폴링 시계열 보존 → 실행 시퀀스와 preflight에 반영.
