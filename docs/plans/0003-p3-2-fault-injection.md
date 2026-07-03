# P3-2 — 의도적 장애 주입(GameDay) + 회고 계획

- 날짜: 2026-07-02
- Phase: **P3 (관측성)** — 이번 범위는 **P3-2: 의도적 장애 → 감지·대응 실증 → 회고(runbook)**
- 상태: **검토 완료(3차, 2026-07-03) — Step 1 산출물 작성됨.** 드릴(Step 2)은 Step 0 잔여(Slack 배선·수신 실증·P3-1 머지) 완료 후.
- 브랜치: 계획 시점은 `infra/p3-cloudwatch-slack-alerts`(P3-1). 드릴 산출물은 P3-1 머지 후 `infra/p3-2-fault-injection`에서.
- 관련: [`docs/plans/0002-p3-observability.md`](0002-p3-observability.md)(P3-1), [`docs/adr/0002-alerting-design.md`](../adr/0002-alerting-design.md)

> 방법론: **가설 먼저 → 주입 → 관측 → 복구 → 예측 vs 실측 회고**. 예측이 빗나간 지점이 이 드릴의 수확이다.
> 역할: mutating 실행(이미지 push·배포 트리거·desired 변경·apply)은 전부 **사람**. 에이전트는 read-only 관측·기록·문서 초안(가드레일 #1).

## 1. 배경·목표

P3-1이 감지·통지 배선(알람 12 + SNS + Chatbot→Slack)을 만들었다. P3-2는 그 배선이 **실제 장애에서 동작함을 실증**하고, 알람을 받은 사람이 따라갈 **runbook**과 **회고**를 남긴다(AGENTS.md P3의 마지막 항목). 부수 목표: P2의 자동 방어(circuit breaker 롤백)와 수동 롤백 경로를 실장애 조건에서 확인.

## 2. 계획의 전제 (레포·AWS 실측, 2026-07-02)

- **circuit breaker**: `enable=true, rollback=true`(`ecs.tf`). 롤링 기본값 min 100%/max 200% → 실패 배포 중 **구 태스크 2개 유지** 예상.
- **실패 임계**: desired×0.5(최소 3, 최대 200) → desired 2면 **실패 태스크 3개**에서 deployment FAILED+자동 롤백.
- **헬스체크**: `/healthz`, matcher 200, 30s×3(unhealthy 판정 ~90s), ECS grace 60s, deregistration 30s → FAILED 확정까지 **약 8~15분** 추정.
- **taskdef는 ARM64**(Graviton) → chaos 이미지도 `linux/arm64`로 빌드해야 한다(불일치 시 exec format error로 "의도와 다른" 크래시 장애가 됨).
- **deploy.yml**: `workflow_dispatch`에 `image_tag`를 주면 **그 입력 자체로 `build_image=false`가 되어 CI(checks)가 생략**되고, **ECR에 그 태그가 존재하는지는 별개의 preflight 게이트**가 확인한다(없으면 job fail — 두 메커니즘은 독립. chaos 드릴에선 둘 다 성립). 태그 규칙 `^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$` → `chaos-healthz-v1` 유효. ECR은 MUTABLE.
- **deploy 액션은 롤백을 실패로 처리한다**(검토 1차 High 반영 — **@v2 현행 소스 기준**, 2026-07-02 실측 시점 v2.6.3): 안정화 대기 후 자기 deployment를 재조회해 **부재(=circuit breaker가 롤백해 목록에서 사라짐) 또는 `rolloutState=FAILED`면 throw** → run **red**(`updateEcsService`의 사후 검증 블록. 로직은 소스 `index.js`로 확인, 실행 엔트리는 그 번들인 `dist/index.js` — action.yml `runs.main` 확인). **@v2는 floating major 태그**이므로 이 전제는 드릴 당일 run의 resolved 버전 기록(P4)으로 재확인한다. 검증은 새 deployment가 생겼고 `wait-for-service-stability=true`일 때 동작(S1은 둘 다 해당). 대기 한도 기본 30분(action.yml 문서, max 360분) > 예상 롤백 8~15분이라 보통 타임아웃 전에 판정.
- **P3-1 미적용 확인**(알람 0개) → Step 0이 게이트. **[2026-07-03 실측 갱신]** 알람 12개 apply 완료·전부 OK, 스택 가동(taskdef rev 10, 태그 `7d5f880…`, ECR 유일 이미지), healthz 200. 단 **SNS 구독 0개 = Slack 배선 미완** → Step 0 잔여는 Slack OAuth·tfvars·재apply·`set-alarm-state` 수신 실증·PR 머지.

## 3. 결정 (검토 포인트 — 권장안 적용, 검토에서 뒤집을 수 있음)

1. **S2 실다운 포함.** desired 0으로 lpulse.live를 5~10분 실제 다운 — 런칭 전 트래픽이 없는 지금이 가장 싼 시점이고, 핵심 알람(HealthyHostCount, treat_missing=breaching)의 실전 발화와 MTTD/MTTR을 실측할 유일한 방법.
2. **S1 주입은 chaos 이미지 + 정식 배포 경로.** 앱 코드 무변경, main 오염 없음, 실제 사고와 동일 경로(workflow_dispatch). 앱에 chaos 플래그를 넣는 안은 taskdef env 배선(Terraform↔CI 경계) 때문에 기각.
3. **확장(대시보드·로그 메트릭 필터·배포실패 통지)은 드릴 결과의 액션 아이템으로만.** P3-2는 드릴+runbook+회고로 얇게 유지(완성 우선).
4. chaos 이미지 위치는 `load/chaos/`(부하·장애 주입 도구로 `/load`를 확장 해석). 이견 있으면 검토에서 조정.

## 4. 시나리오 · 예측표

### S1 — 나쁜 이미지 배포 (자동 방어 검증, 무중단 예상)

- **주입**: busybox(arm64) 기반 "8080을 열지만 `/healthz`가 404"인 이미지(프로세스는 계속 살아 있음 — 크래시가 아니라 헬스체크 실패 유형) → 사용자가 ECR에 `chaos-healthz-v1` push → main에서 `deploy.yml` dispatch(`image_tag=chaos-healthz-v1`).
- **예측** (각각 드릴에서 판정):
  - **P1** 무중단: 구 태스크 2개 유지, HealthyHostCount 2 유지, 외부 curl 폴링 내내 200.
  - **P2** `alb-unhealthy-hosts` 알람 ALARM→Slack 수신(배치당 unhealthy 노출 ~1–2분 × 2배치 — 발화 가능성 높음이나 단정 아님. 안 울리면 그 자체가 발견 → A-2).
  - **P3** 실패 태스크 3개째에서 deployment **FAILED + 자동 롤백**, 주입~롤백 완료 8~15분. **증거 = 완료 조건 1의 1차 증거(§6)**: `describe-services` 이벤트("deployment failed…"/"rolling back…")와 롤백 deployment의 `rolloutState`/`rolloutStateReason`(circuit breaker 명시)을 기록 — 액션 버전과 무관한 ECS 측 사실.
  - **P4** GitHub Actions run은 **red로 끝난다** — deploy 액션 v2가 롤백(deployment 부재)·FAILED를 명시적으로 실패 처리한다(§2 소스 실측. 계획 초안의 "green 오인" 예측은 검토 1차에서 정정됨). run 색은 **보조 신호**(1차 증거는 P3의 ECS 이벤트). 기록: 에러 메시지("not found after stabilization…" 또는 "FAILED: …"), run의 **resolved 액션 버전(SHA)**, **판정 경로**(부재/FAILED 감지 vs waiter timeout). **green이면 액션 버전 회귀/핀 문제 의심 → A-1 재론.**
  - **P5** 침묵 예상: 5xx 알람(헬스체크 실패는 5xx 지표에 안 잡힘, 사용자 트래픽은 구 태스크로만), HealthyHostCount(2 유지), RunningTaskCount(2→4→2, 미달 없음).
- **관측**: 사용자 터미널 curl 폴링(5s 간격), `describe-services`(events/deployments), `describe-target-health`, `describe-alarm-history`, Slack 수신 시각, Actions run 결과.
- **복구**: 자동(롤백). 실패 시 수동 = dispatch(`image_tag=<기준선 B-1에서 기록한 직전 정상 sha>`) — P2에서 검증된 경로. **B-1 기록 없이는 S1을 시작하지 않는다**(자동 롤백이 지연·실패하는 순간 이 값이 즉시 필요). **단(4차 검토)**: dispatch는 deploy.yml의 concurrency 직렬화(`cancel-in-progress: false`)에 걸려 chaos run이 끝날 때까지 큐 대기할 수 있다 — **사용자 영향 진행 중엔 B-1의 taskdef ARN으로 `aws ecs update-service --task-definition` 직접 실행이 1순위**(runbook·기록지 공통 **R-1e**).
- **중단 기준**: HealthyHostCount<2 또는 실사용자 오류 관측 즉시 수동 롤백 — 긴급은 GitHub 우회 경로(위 복구 항의 직접 `update-service`) 우선.

### S2 — 전체 다운 드릴 (감지→대응 리허설, 실다운 5~10분)

- **주입**: `aws ecs update-service --cluster linkpulse-prod-cluster --service linkpulse-prod-app --desired-count 0`(사용자). 직후 curl로 첫 503을 확인한 뒤 **503 응답 10건 이상을 몇 초 간격 연사로, 타임스탬프와 함께 기록**한다. (P7 임계는 5건/5분이지만 CloudWatch 5분 버킷은 벽시계 정렬(:00/:05/…)이라 5건은 경계에서 3+2로 갈려 미발화할 수 있다 — **10건이면 어떤 분할이든 한 버킷 ≥5가 보장**된다.) 기록된 503이 10건 미만이면 P7 미발화는 알람 결함이 아니라 **절차 결함**으로 판정한다.
- **예측**:
  - **P6** `alb-no-healthy-hosts`: 드레인(~30s) 후 metric 0 또는 missing → breaching 설계대로 **~4–6분 내 ALARM→Slack** = MTTD 실측. (5차 검토에서 추가한 무트래픽 사각지대 차단 알람의 실전 검증.)
  - **P7** `alb-elb-5xx`: 503 합계 ≥5/5분 → ALARM(보통 ~5–7분, 벽시계 버킷 경계에 걸리면 +5분까지). ELB-only dimension 설계 검증.
  - **P8** `ecs-running-tasks-low`: Insights가 0을 내보내면 ALARM, 태스크 0에서 metric 자체가 끊기면 INSUFFICIENT_DATA(침묵). **어느 쪽인지가 이 드릴의 관찰 질문** — 침묵이면 알람 한계로 회고에 기록(A-3).
  - **P9** 침묵 예상: target-5xx(타깃 없음), RDS 계열.
- **대응(runbook 리허설)**: Slack ALARM 수신 → runbook 절차대로 진단(`describe-services`에서 desired=0 발견) → desired 2 복원 → ~2–3분 내 healthy → 알람 OK→Slack. **MTTR 기록**.
- **정합성**: 드릴 중 desired=0은 Terraform(값 2, `ignore_changes` 아님)과의 **의도된 일시 드리프트** — **드릴 창 동안 `terraform apply`를 하지 않는다**(가드레일 #1상 사람만 apply하므로 "안 하기"로 충분. 중간에 apply하면 드릴이 조기 종료·교란됨). 복원 후 `describe-services`로 desired=2 재확인 → 드리프트 소멸.

### S3 (선택) — 태스크 1개 강제 종료 (자가치유 관찰, 5–10분)

`stop-task` 1개 → ECS 보충 ~2분 → **알람 침묵 예상**(3분 평가창보다 빠른 자가치유). "자가치유가 감지보다 빠르다"의 실측. 여유 있으면 수행.

## 5. 절차 (Step별 · 역할)

- **Step 0 — P3-1 완료 게이트 (사용자)**: plan 확인 → apply → Slack OAuth·tfvars → 재apply → 알람 12+chatbot 확인 → `set-alarm-state` 테스트로 Slack 수신 확인 → PR·머지. 상세는 plans/0002 §6·`infra/README.md`. **Slack 통지가 실제로 와야 드릴 의미가 있다.**
- **Step 1 — 준비 (Claude 작성, 이 계획 검토 통과 후)**: 새 브랜치에서 ① runbook 초안 `docs/runbooks/alarm-response.md`(**12개 알람 전수**: 의미/심각도/1차 확인 명령/복구 절차/오탐 시 튜닝) ② `load/chaos/Dockerfile`+README(빌드는 **`--platform linux/arm64` 명시** → 로컬 검증: `docker run` 후 `curl -i localhost:8080/healthz` 404 확인 → push → **`docker buildx imagetools inspect <ECR>:chaos-healthz-v1`로 아키텍처=arm64 확인**까지가 완료 조건 — §2 ARM64 리스크의 이중 차단) ③ 회고 템플릿 `docs/postmortems/TEMPLATE.md` ④ 예측표(§4)를 기록지로. — **드릴 전에 runbook 초안이 있어야 "따라 하는" 리허설이 된다.**
- **Step 2 — GameDay (사용자 실행 + Claude 관측)**: **B-1 기준선 기록**(현재 서빙 taskdef ARN·리비전·이미지 태그를 `describe-services`→`describe-task-definition`으로 기록하고, 그 태그가 ECR에 존재함을 `describe-images`로 확인 = **수동 롤백 대상 확정, S1의 사전 게이트**) → **B-2** 전 알람 상태 정상·Slack 배선 동작 확인 → S1 → 안정화 확인 → S2 → 복구 확인 → (S3) → 증거 수집(alarm history·서비스 이벤트·run 링크·Slack 캡처·타임스탬프).
- **Step 3 — 회고·확정 (Claude 초안, 사용자 확정)**: `docs/postmortems/2026-07-XX-gameday-01.md`(타임라인, 예측 vs 실측 표, MTTD/MTTR, 잘 된 것/사각지대, 액션 아이템) → runbook에 실측 반영 → 액션 아이템 각각 반영 커밋 or 백로그 판정 → PR.

## 6. 완료 조건 (P3-2 = P3 종료)

1. S1: **자동 롤백 실증 — 1차 증거는 ECS 측**(서비스 이벤트 "deployment failed…"/"rolling back…" + 롤백 deployment의 `rolloutStateReason`, P3) + 무중단 확인(curl 기록). Actions run red(P4)는 보조 신호(액션 버전 의존).
2. S2: 실다운이 **Slack ALARM으로 감지**(MTTD 기록) → **runbook 따라 복구**(MTTR 기록) → OK 통지 수신.
3. `docs/runbooks/alarm-response.md`에 12개 알람 전수 대응 절차.
4. 회고 문서(예측 vs 실측 + 액션 아이템 판정 포함).
5. 채택된 액션 아이템 반영(또는 명시적 백로그 이관).

## 7. 리스크 · 중단 기준 · 액션 아이템 후보

- **S1 리스크**: 예측과 달리 구 태스크가 영향받으면 즉시 수동 롤백(P2 경로). chaos 컨테이너는 기존 taskdef의 env/secrets를 주입받지만 사용하지 않음(busybox httpd, DB 접근 없음).
- **S2 리스크**: 복구는 desired 2 한 줄. 안 되면 서비스 이벤트 확인 → 정상 태그 재배포. RDS·데이터는 전 시나리오에서 무접촉.
- **비용/시간**: 드릴 자체 비용 무시 가능(추가 태스크 수 분 + ECR 수 MB). 전체 반나절(드릴 1–1.5h).
- **증거 보존(3차 검토 추가)**: `scripts/full-destroy-prod.sh`(2026-07-03 도입)는 CloudWatch 알람 이력·로그·ECR 이미지를 지운다 — **회고 확정(Step 3) 전 full destroy 금지**, 증거 덤프는 드릴 직후 수행(기록지 §5). full apply로 재생성한 뒤 드릴하려면 chaos 이미지 재push 필요.
- **액션 아이템 후보(선등록 — 드릴에서 실증된 것만 채택)**: **A-1** 배포 워크플로 taskdef 검증 step — **기각(검토 1차)**: deploy 액션 v2가 동일 검증을 이미 수행(§2 실측). S1에서 red 확인으로 종결하되, green이 나오면 재론 · **A-2** EventBridge `SERVICE_DEPLOYMENT_FAILED`→기존 SNS→Slack(배포 실패 통지. run red로 이미 드러나므로 "사각지대 메움"이 아니라 **통지 채널 일원화** 가치 — push 배포처럼 run을 안 보고 있는 상황 대비. 드릴 후 판단) · **A-3** RunningTaskCount 알람 한계 문서화 또는 대체 지표.

## 8. 검토 반영

### 1차 (외부 2팀, 2026-07-02) — 4건 전수 수용

1. **(Medium) S1 수동 복구의 전제(known-good 태그)가 기준선에 없음** → 수용: Step 2에 **B-1**(서빙 taskdef ARN·이미지 태그 기록 + ECR 존재 확인) 신설, S1 사전 게이트로 지정.
2. **(High) "run green 오인" 예측(P4)이 현행 액션과 불일치** → 수용(에이전트가 소스로 확정): `amazon-ecs-deploy-task-definition@v2`(v2.6.3) `index.js` L265–293이 안정화 후 deployment 부재(롤백)·FAILED를 throw → **P4를 "red + 자동 롤백 성공"으로 정정**, A-1 기각. 초안 예측이 구식 지식이었음.
3. **(Low) S2 "curl 10~20회"는 P7 판정 기준으로 느슨** → 수용: "첫 503 확인 후 503 5건 이상 타임스탬프 기록"으로 결과 기준화(미달 시 절차 결함 판정).
4. **(Low) chaos 이미지 arm64 검증 부족** → 수용: `--platform linux/arm64` 명시 빌드 + push 후 `imagetools inspect` 아키텍처 확인을 Step 1 완료 조건에 추가.

### 2차 (외부 3팀, 2026-07-02) — 7건: 수용 6 · 변경 불요 1

1. **(중간) 완료 조건 1의 1차 증거가 run 색에 의존** → 수용: §6-1을 ECS 증거(서비스 이벤트+`rolloutStateReason`) 중심으로 재작성, P4(run red)는 보조 신호로 격하 — floating 태그라 액션 동작은 가변, ECS 이벤트는 불변.
2. **(낮음) 5xx 5건이 벽시계 5분 버킷 경계에서 3+2로 갈릴 수 있음** → 수용: S2 절차를 **503 10건 이상**으로 상향(비둘기집 — 어떤 경계 분할이든 한 버킷 ≥5 보장), P7에 경계 지연 주석.
3. **(낮음) 대기 한도 "30분" 숫자의 불확실성** → 수용: P4 기록 항목에 **판정 경로**(부재/FAILED 감지 vs waiter timeout) 추가. 30분 기본·360분 상한 자체는 action.yml 문서로 재확인함.
4. **(낮음) "ECR에 태그 존재하면 CI 생략"은 인과 혼동** → 수용: CI 생략의 원인(=`image_tag` 입력→`build_image=false`)과 preflight ECR 존재 게이트(별개, 없으면 fail)를 분리 서술.
5. **(경미) S2 desired 드리프트 처리 미완** → 수용: "드릴 창 동안 apply 금지 + 복원 후 desired=2 재확인"을 정합성 노트에 명시.
6. **(Low) "v2.6.3·행번호" 표기 과신 — 실행 엔트리는 `dist/index.js`, @v2는 floating** → 수용: "@v2 현행 소스 기준"으로 완화, 실행 엔트리(action.yml `runs.main`) 병기, 행번호 인용 제거, resolved 버전 기록으로 드릴 당일 재확인.
7. **(Low) floating 태그라 resolved 버전을 증거로 남겨야 함** → **변경 불요**: 1차 반영 때 P4 기록 항목에 이미 포함됨(검토자도 "수용 가능" 판정) — 확인만.

### 3차 (Claude, 2026-07-03) — 레포·AWS 실측 재검증, 결함 0 · 갱신 2 · 추가 1

1. **§2 전제 전수 재검증 — 전부 일치**: circuit breaker(enable/rollback), min/max 미설정(기본 100/200%), 헬스체크(30s×3·matcher 200·grace 60·dereg 30), ARM64, 컨테이너 레벨 healthCheck·command 오버라이드 **없음**(busybox CMD 실행 보장 — 계획이 전제한 "헬스체크 실패형" 성립), deploy.yml(build_image/preflight 분리·태그 regex·`wait-for-service-stability`·job `timeout-minutes` 없음 → 액션 waiter 30분이 지배), ECR MUTABLE·lifecycle 30개, 알람 12개 정의(elb-5xx Sum≥5/300s·LB-only, no-healthy-hosts Avg<1·60s×3·breaching, unhealthy-hosts Max≥1·60s×2, running-tasks-low <2·60s×3·missing), S2 명령의 클러스터/서비스명.
2. **상태 갱신(위 §2 주석)**: P3-1 apply 완료·Slack 미배선 — Step 0 잔여 명확화. 기록지 §0에 반영.
3. **추가(위 §7)**: full-destroy 운용(계획 작성 이후 도입)과 드릴 증거 보존의 상호작용 — destroy 전 증거 덤프 절차를 기록지 §5로 강제.

### 4차 (외부 AI 2팀, 2026-07-03 — Step 1 산출물 검토) — 5건: 수용 4 · 변경 불요 1

1. **(High) S1 수동 롤백 dispatch가 concurrency 직렬화에 큐 대기 가능** → 수용: 문제 run이 안정화 대기(waiter 최대 30분)로 `deploy-production` 그룹을 점유하는 동안 신규 dispatch는 pending — **긴급 경로 신설**(B-1 taskdef ARN 직접 `update-service`, 새 deployment가 문제 deployment를 즉시 대체 + 큐 run 정리 절차): §4 복구·runbook/기록지 **R-1e**(기록지 번호는 5차에서 runbook 체계로 통일).
2. **(Medium) runbook R-1 fallback("현재 서빙 태그 확인")이 배포 진행 중엔 문제 리비전을 가리킴** → 수용: `services[0].taskDefinition` 사용 금지 명시, deployments의 `COMPLETED`(배포 중엔 구 `ACTIVE`) 기준으로 교정.
3. **(Medium) 기록지 I-3가 첫 503 확인 없이 즉시 연사 — 드레인 창(~30s)의 200 혼입 시 503 10건 미달 위험(본 계획 §4 원문과 불일치)** → 수용: `until` 첫 503 대기 후 12발로 교정(첫 503 시각 자동 기록 포함).
4. **(Low, 2팀 공통) 증거 원본(계정 ID·ARN·Slack ID 포함 가능)을 커밋 경로에 저장** → 수용: `docs/postmortems/evidence/`를 .gitignore(로컬 보관 전용), 회고 문서엔 확인 거친 발췌만.
5. **(Medium) P3-2 산출물이 P3-1 브랜치에 untracked로 존재 — P3-1 PR 혼입 우려** → **변경 불요**: 전부 신규 untracked 파일이고 커밋은 사람이 수동으로만 하므로(레포 규칙) 혼입에는 명시적 `git add`가 필요하다. 커밋 대상(`infra/p3-2-fault-injection`, P3-1 머지 후)은 본 계획 서두에 명시돼 있다. 운영 주의 1줄로 충분: **P3-1 잔여 커밋 시 `git add -A`를 쓰지 않는다.**

### 5차 (외부 AI 2팀, 2026-07-03 — 4차 반영분 재검토) — 4건: 수용 3 · 변경 불요 1

1. **(Medium) 기록지 R-* 번호가 runbook R-*와 충돌** — 기록지 R-3(긴급 롤백) vs runbook R-3(`--force-new-deployment`): S1 중 혼동해 runbook R-3을 실행하면 서비스 taskDefinition이 chaos 리비전이므로 **chaos 이미지가 재배포**되어 복구 지연 → 수용: 기록지 번호를 runbook 체계로 통일(R-1 dispatch / R-1e 긴급 / R-2 desired 복원), 중단 기준·타임라인 참조 갱신, 치트시트에 "R-* 번호는 runbook과 동일 체계" 명시.
2. **(Medium) runbook §1-3(롤백도 실패)·§8-1(기동 실패)이 큐에 걸리는 R-1만 안내** — 두 상황 모두 문제 run이 안정화 대기로 살아 있을 공산이 큼 → 수용: 두 절차와 R-1 행 자체에 조건부 연결("사용자 영향+run 진행 중이면 R-1e, run 종료 후면 R-1").
3. **(Low) R-1 fallback 문구가 `rolloutState`/`status` 값을 혼용**(`ACTIVE`는 deployment `status` 값) → 수용: 필드명 분리 서술 + 4차 교정의 잔여 공백 보강 — 배포가 이미 끝난 경우(단일 `PRIMARY`=문제 리비전)는 `list-task-definitions --sort DESC` 직전 리비전 경로(정상 서빙 여부는 Actions 성공 run으로 확인).
4. **(Low) untracked 산출물의 P3-1 PR 혼입 재지적** → **변경 불요**(4차 5번과 동일 사안): 검토자 스스로 "명시 staging만 하면 문제없음" 결론 — 기존 운영 주의(`git add -A` 금지) 유지.

## 9. 다음 (P3 종료 후)

P4 하드닝: IAM 최소권한(ADR 0002에서 이관한 Chatbot 스코프 축소 포함), Secrets 로테이션, RDS 백업+복원 리허설, Redis 캐시, 레이트리밋, k6 부하(이때 `load/`가 본래 용도로 확장).
