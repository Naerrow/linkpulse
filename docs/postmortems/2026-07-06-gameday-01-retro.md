# 회고(Postmortem) — 2026-07-06 P3-2 GameDay 01 (S1/S2/S3 의도적 장애 드릴)

> 원본 타임라인·예측표 전체는 [`2026-07-06-gameday-01.md`](2026-07-06-gameday-01.md)(기록지)를 근거로 한다. 이 문서는 그 요약·분석·액션 아이템 판정이다.

- 날짜(드릴): 2026-07-06 (1차 세트 13:31~15:35, 2차 세트 16:08~16:32 KST)
- 작성자: Claude / 검토자: 사용자(2026-07-06 확정) + 외부 AI 교차검토(수 회)
- 심각도: **드릴**
- 상태: **확정**(2026-07-06 — 사용자 P3-2 종결 결정. 액션 아이템 A-1~A-10 전수 판정 완료. 커밋·PR은 사용자)
- 관련: 계획 [`0003-p3-2-fault-injection.md`](../plans/0003-p3-2-fault-injection.md)(P3-2 설계·예측 P1~P9 원본 — 이 회고로 종결), runbook [`alarm-response.md`](../runbooks/alarm-response.md), Actions run [`28774201504`](https://github.com/Naerrow/linkpulse/actions/runs/28774201504)(S1 2차 세트, failure)

## 1. 한 줄 요약

나쁜 이미지 배포(S1) 시 circuit breaker가 3회 전부 자동 롤백에 성공했고(무중단), 전체 다운(S2) 시 Slack 알람이 감지해(MTTD 3분37초~4분54초) runbook 절차로 복구(MTTR 3분34초~4분11초, 즉 총 다운타임 7분11초~9분5초)했다. 사용자 영향은 드릴 목적상 S2에서만 의도적으로 발생했고 실제 고객은 없다(런칭 전). 가장 큰 수확은 **예측이 지목한 MTTD 알람(`alb-no-healthy-hosts`)이 아니라 다른 알람(`alb-elb-5xx`)이 실제로 더 빠르고 일관되게 감지를 담당했다**는 발견이다.

## 2. 영향

드릴이라 "영향"은 시나리오별 의도된 다운타임이다.

| 시나리오 | 기간(2차 세트 기준) | 사용자 영향 | 데이터 영향 |
| --- | --- | --- | --- |
| S1(나쁜 이미지) | T0 16:10:20 → 롤백완료 16:17:55(7분35초) | **없음**(C-1 healthz 200 연속, 비200 0건 — 정량 확인) | 없음 |
| S2(전체 다운) | T0 16:19:48 → 복구 16:26:59(총 다운타임 7분11초) | **있음**(의도됨) — 503 81건(C-1 폴링 기준) | 없음(RDS 무접촉, chaos/정상 이미지 둘 다 DB 미사용 경로) |
| S3(태스크 강제종료) | T0 16:31:33 → 보충 16:32:13(40초) | 없음(자가치유가 알람 평가창보다 빠름) | 없음 |

## 3. 타임라인 (S2 — MTTD/MTTR 실측 대상, 2차 세트)

| 시각(KST) | 이벤트 | 출처 |
| --- | --- | --- |
| 16:19:48 | I-2 실행(desired 0), 실다운 시작(**T0**) | `aws ecs update-service` |
| 16:19:53 | 첫 503 관측 | C-1 로그 |
| 16:23:25 | `alb-elb-5xx` Slack ALARM 수신 (**MTTD 기준점**) | CloudWatch + Slack 스크린샷 |
| 16:26:03 이전 | R-2 실행(desired 2, 진단→조치) | `aws ecs update-service` |
| 16:26:59 | steady state 도달, healthz 200 회복 (**MTTR 기준점**) | C-4·C-1 |

- **MTTD** = T0(16:19:48) → 첫 Slack ALARM(16:23:25): **3분 37초**
- **MTTR** = 첫 Slack ALARM(16:23:25) → 정상 확인(16:26:59): **3분 34초** — 프로젝트 정의(ALARM→정상, TEMPLATE.md·기록지 §3 상단). 이게 "알람 받고 얼마나 빨리 고쳤나"의 실제 대응 시간이다.
- **총 다운타임**(T0 → 정상) = **7분 11초** = MTTD 3분37초 + MTTR 3분34초 (사용자 영향 지속 시간 — MTTR과 혼동 금지)
- (1차 세트 재현치: MTTD 4분54초 / MTTR 4분11초 / 총 다운타임 9분5초 — 두 차례 다 같은 알람 `alb-elb-5xx`가 MTTD를 담당)

## 4. 감지 — 무엇이 알려줬나

- **발화한 알람(순서대로)**: S1 — 없음(2차 세트 기준, 1차 세트 1차 시도에서만 `alb-unhealthy-hosts` 1회). S2 — `alb-elb-5xx`만(2차 세트). S3 — 없음.
- **침묵한(또는 예측과 다르게 늦은) 알람**: `alb-no-healthy-hosts`(예측 MTTD 알람이었으나 2회 모두 예측과 다르게 동작 — 미발화 또는 복구 후 발화), `ecs-running-tasks-low`(다운이 길 때만 뒤늦게 발화).
- **감지는 빠르나 회복 통지는 느리다(비대칭)**: `alb-elb-5xx`는 ALARM은 3분37초 만에 왔지만 OK 통지는 복구(16:26:59) 후 **17분26초 뒤**(16:44:25)에야 왔다(1차 세트에선 같은 알람이 2분49초 — 편차 큼). 원인은 `HTTPCode_ELB_5XX_Count`가 sparse 카운트 메트릭(5xx 0이면 데이터포인트 부재)이라 복구 후 missing→`notBreaching` 확정이 지연되기 때문. OK 카드에도 "no datapoints were received for 1 period ... treated as NonBreaching"로 명시됨. → **복구 확인은 OK 알림이 아니라 healthz·`describe-services`로**(A-9).
- **runbook이 실제로 유효했나**: 진단 절차(desired=0 확인 → desired=2 복원)는 문제없이 작동. 단 runbook이 "S2 감지 시 확인할 1순위 알람"으로 암묵적으로 가정했을 `no-healthy-hosts` 계열은 실전에서 신뢰도가 낮았다 — `alb-elb-5xx`를 1순위로 재정의 필요(A-8), 회복 판정은 OK 알림에 의존하지 말 것(A-9).

## 5. 원인 분석 — "MTTD 알람이 예측과 다르게 동작한 이유" (왜 5번)

1. **직접 원인**: `alb-no-healthy-hosts`가 S2 2회 실측 모두 예측(4~6분)과 다르게 동작 — 1차 세트는 9분48초(복구 후)에 발화, 2차 세트는 아예 미발화.
2. **왜?** → desired=0으로 태스크가 완전히 종료되면 타깃 그룹에서 타깃이 "unhealthy"가 아니라 "존재 자체가 없음(deregistered)" 상태가 된다. 이 알람이 전제한 "unhealthy 판정 로직(헬스체크 실패 누적)"과 다른 메트릭 발행/평가 경로를 탈 가능성이 높다.
3. **왜?** → 계획 수립 시(0003 §2) 이 알람의 **breaching 설계**(무트래픽 상황도 놓치지 않기 위함)는 검증했지만, "desired=0 완전 다운" 시나리오에서 실제 지표 타이밍을 사전에 실측·벤치마크하지 않고 예측치만 세웠다.
4. **왜?** → CloudWatch/Container Insights 계열 지표의 발행 지연 특성이 AWS 공식 문서에 정량적으로 명시돼 있지 않아, 계획 검토 단계(1~5차 외부 검토 포함)에서 이 부분이 검증 항목으로 들어가지 않았다.
5. **설계 수준 결론**: 이 알람류(`no-healthy-hosts`, `running-tasks-low`)를 "1차 MTTD 신호"로 신뢰해서는 안 된다. 실측상 `alb-elb-5xx`(사용자 트래픽 기반 지표, 2회 다 3~5분대 일관 발화)가 더 빠르고 일관적이므로 runbook의 "S2 최초 확인 알람"을 이걸로 재정의한다(**A-8**). **단 이건 증상 회피(임시 운영 우회)다** — `alb-elb-5xx`는 트래픽이 있을 때만 5xx가 생기므로, 진짜 무트래픽 다운(새벽 등)에선 이 알람이 안 뜬다. 그 상황의 유일한 방어선이 바로 `alb-no-healthy-hosts`인데 이번 실측에서 그게 지연/미발화했다 = **무트래픽 방어선이 미검증 상태로 남았다**. 이 알람 자체의 신뢰성 규명(튜닝/교체 판단 포함)이 **A-5**이며, A-5가 끝나야 A-8이 임시가 아닌 항구 대책이 되거나 알람 구성이 바뀐다. **덧붙여**: 이번 MTTD가 3~5분으로 나온 건 C-1 폴링이라는 synthetic 트래픽이 있었기 때문이다 — 상시 canary를 두면 무트래픽에서도 `alb-elb-5xx`가 감지하게 되어 A-5의 유력한 대안이 된다(**A-10**). **[2026-07-13 갱신]** A-5·A-10 모두 P4(c) canary로 종결 — 위 "A-5가 끝나야 A-8이 항구 대책"의 조건이 충족됐다: canary가 무트래픽에도 synthetic 503을 만들어 `alb-elb-5xx`를 발화(드릴 실측 약 3분44초)시키므로 **A-8은 이제 임시 우회가 아니라 canary로 뒷받침되는 항구 대책**이다([ADR 0004](../adr/0004-notraffic-canary.md) §판정).

## 6. 예측 vs 실측 (통합, 2차 세트 기준 — 상세는 기록지 참조)

| # | 예측 | 실측 | 판정 | 배운 것 |
| --- | --- | --- | --- | --- |
| P1 | S1 무중단 | C-1 연속 로그로 확정(비200 0건) | 적중 | 정량 폴링 로그 없이는 "무중단"을 주장할 수 없다 — 다음부터 필수 |
| P2 | `alb-unhealthy-hosts` 발화 가능성 높음(단정 아님) | 3회 중 1회만 발화 | 부분 | "가능성 높음, 단정 아님"이라는 계획의 신중한 표현이 실측으로 그대로 뒷받침됨 |
| P3 | S1 8~15분 내 자동 롤백 | 3회 전부 성공(6분32초~8분17초) | 적중 | circuit breaker는 신뢰할 수 있는 안전망 |
| P4 | run red(보조 신호) | 3회 전부 `failure`, 판정 경로 = **부재 감지** | 적중 | 액션이 "deployment not found after stabilization → circuit breaker 롤백"으로 red 처리 — 계획 §2가 예측한 경로 그대로(waiter timeout 아님) |
| P5 | S1 중 5xx·no-healthy·running-low 침묵 | 3회 중 1회만 `no-healthy-hosts` 이례적 발화 | 적중(대체로) | 드문 이례가 있을 수 있음, 재현성 낮음 |
| P6 | `alb-no-healthy-hosts`가 S2 MTTD 담당(4~6분) | 2회 모두 예측과 다름(9분48초/복구후, 또는 미발화) | **빗나감** | 이번(트래픽 있는) 드릴에선 MTTD를 `alb-elb-5xx`에 내줌 — 다만 이 알람의 원래 역할인 **무트래픽 다운 감지의 적합성은 미규명**(A-5). "MTTD 신호로 부적합"이라 단정하지 않는다. **[2026-07-13 갱신]** A-5는 [ADR 0004](../adr/0004-notraffic-canary.md)로 종결 — 무트래픽 감지 부적합의 근본원인(지표 소실=구조적 결함)을 실측 확정, 무트래픽 방어선은 canary |
| P7 | `alb-elb-5xx` ~5~7분 | 2회 모두 실제 MTTD 담당(3분37초/4분54초) | 적중(더 빠름) | 계획이 지목 안 한 알람이 실제 주역이었음 |
| P8 | `running-tasks-low` 발화 또는 침묵(관찰 질문) | 다운 길이에 좌우(9분대 지연 발화/7분대 침묵) | 부분 | 짧은 다운에선 이 알람에 의존 불가 |
| P9 | target-5xx·RDS 침묵 | 2회 모두 침묵 확인 | 적중 | — |

## 7. 잘 된 것 / 아쉬운 것 / 운이 좋았던 것

- **잘 된 것**: circuit breaker 자동 롤백이 3회 전원 성공(S1 신뢰성 실증). Slack 배선이 실전에서 여러 차례(총 6건 이상) 확실히 동작. 1차 세트의 절차 공백(C-1 부재·중복 실행·조기 복구)을 인지한 뒤 **2차 세트를 처음부터 재실행**해 정량 증거로 스스로 메꿈 — Claude가 드릴 시작 전부터 C-1을 걸고 run id를 dispatch 직후 고정한 것이 유효했다.
- **아쉬운 것**: 1차 세트에서 S1이 원인 불명으로 두 번 dispatch됨, S2 첫 시도가 22초 만에 조기 복구돼 무효, Actions run 확인 자체를 누락 — 전부 "사람이 세션 밖에서 계획 승인 전에 단독 실행"한 데서 비롯됨.
- **운이 좋았던 것**: 1차 세트의 S1 중복 dispatch가 실제로는 문제 없이 둘 다 자동 롤백에 성공했다 — circuit breaker가 실패하는 케이스였다면 두 번째 dispatch가 상황을 더 꼬이게 했을 수 있다. 중복의 **원인 자체는 특정하지 못했지만**, 어느 경우든 dispatch 전 "이미 진행 중인 run 없는지" 확인을 두면 운에 맡기지 않는 안전판이 된다(A-7).

## 8. 액션 아이템 (전수 판정 — 반영/백로그/기각 중 하나로 종결)

> 선등록분 A-1~A-3(0003 §7)와 이번 드릴 신규분 A-4~A-10을 전수 판정한다. 외부 AI 교차 검토(2026-07-06, 수 회) 반영 완료. **판정별 반영 상태는 판정 칼럼에 표기**(반영 완료 = 문서 working tree 수정 끝, 커밋만 사람 몫).

| # | 항목 | 판정 | 근거 |
| --- | --- | --- | --- |
| A-1 | 배포 워크플로 taskdef 검증 step 추가 | **기각(확정)** | 0003 §7에서 이미 기각(deploy 액션 v2가 동일 검증 수행). 그때의 "green이 나오면 재론" 조건도 이번 P4 실측으로 해소 — S1 run 1·2차 세트 통틀어 **3회 전부 `failure`**, 2차 세트 로그에서 판정 경로가 **부재 감지**(`Deployment … not found after stabilization … rolled back by … circuit breaker`)로 확인돼 액션이 배포 실패를 스스로 검증·throw함이 실증. 재론 사유 소멸 |
| A-2 | EventBridge `SERVICE_DEPLOYMENT_FAILED`→SNS→Slack 배포 실패 통지 | **종결(P4(b) · 2026-07-10) — 반영** | plan 0005로 구현·apply·종단검증 완료(PR #10 머지, `infra/prod/eventbridge.tf`: 규칙→IAM 실행 role `role_arn`으로 SNS `alarms` 재사용→Chatbot→Slack). chaos 나쁜배포→서킷브레이커 롤백→**Slack custom notification 카드 수신 실증**. 이 드릴이 예고한 "사람이 Actions run 확인을 놓치는 사각지대"를 이 통지가 메움. 상세 [ADR 0003](../adr/0003-deploy-failure-alerts.md). 갭=GitHub Actions단 실패(러너 미획득 등)는 ECS 미도달로 이 경로 밖 |
| A-3 | `ecs-running-tasks-low` 알람 한계 문서화 | **A-6으로 통합(반영)** | 동일 파일·동일 커밋(runbook §8 갱신) — 중복 카운트하지 않음. 실체는 A-6에서 실행 |
| A-4 | C-1(healthz 5초 폴링)을 GameDay 표준 절차로 채택 | **반영 완료**(`load/chaos/README.md`·치트시트) | 1차 세트엔 없어 P1(무중단)을 정량 판정 못 했으나, 2차 세트에서 드릴 시작 전부터 백그라운드로 걸어 **비200 0건**으로 무중단을 정량 확정. `load/chaos/README.md`의 "GameDay 주입 전 체크"에 "S1 dispatch 전 C-1 선가동"으로 명시함 |
| A-5 | `alb-no-healthy-hosts` 지연/미발화 원인 규명 | **종결(P4(c) · 2026-07-13)** | 근본원인 실측 확정 — desired=0 다운 시 `HealthyHostCount`가 0이 아니라 **missing**(AWS 문서 _"Reported if there are registered targets"_ + 드릴 7분 데이터 공백 실측): 감시 지표가 대상 장애에서 소멸하는 **구조적** 결함이라 튜닝 무효. 3회 드릴 전부 이 알람 MTTD 실패(9분48초/미발화/미발화). 무트래픽 방어선은 canary로 재구성(→A-10). 상세 [ADR 0004](../adr/0004-notraffic-canary.md) §A-5. 알람은 느린 백스톱으로 유지·비신뢰 |
| A-6 | runbook §1·§8·§0-2에 "`no-healthy-hosts`·`running-tasks-low`는 늦거나 침묵할 수 있음 → 다운 시 `alb-elb-5xx`·`describe-services` 우선" 명시(§8 P8 실측 갱신 포함) | **반영 완료**(runbook §0-2·§1·§8) | 기존 §0-2의 `503→§1 직행`·§1의 "울리면 진짜 다운" 확신형이 실측과 정반대였던 것을 바로잡음. §8의 "실측 후 갱신" 미완 문구도 실측치로 채움. 저비용 고효과 |
| A-7 | S1 dispatch 전 "진행 중 run 없는지" 확인 절차 추가 | **반영 완료**(`load/chaos/README.md`·치트시트 I-1) | 1차 세트 중복 dispatch의 **원인은 특정하지 못했으나**, 어느 경우든 dispatch 전 진행 중 run 확인이 안전판. `alarm-response.md`는 사고 후 대응 문서라 부적합 → `load/chaos/README.md`와 기록지 치트시트 I-1에 `gh run list`(진행 중 확인+run id 고정) 반영 |
| A-8 | runbook §0-2의 "S2 1순위 확인 알람"을 `no-healthy-hosts`가 아니라 `alb-elb-5xx`로 재정의 (**caveat**: 트래픽/synthetic curl 있는 다운 한정) | **반영 완료**(runbook §0-2·§2) | 2회 실측 모두 이 알람이 실제 MTTD 담당(3분37초·4분54초) — 회고 핵심 발견. **단 무트래픽 다운에선 5xx 자체가 안 생겨 이 알람이 안 뜰 수 있으므로 "1순위"는 트래픽 있는 다운으로 한정**, 무트래픽 방어선은 A-5로 남긴다(A-8=임시 운영 우회). caveat까지 §0-2·§1·§2에 함께 반영. **[2026-07-13 P4(c) 종결]** A-5·A-10 canary 종결로 무트래픽 caveat 해소 — canary synthetic 트래픽이 무트래픽에도 5xx를 만들어 A-8을 뒷받침하므로 **A-8은 이제 항구 대책**([ADR 0004](../adr/0004-notraffic-canary.md)) |
| A-9 | runbook에 "회복 판정은 OK 알림이 아니라 healthz 200·`describe-services`로 직접, `alb-elb-5xx` OK는 복구 후 2~17분 늦게 온다(sparse 메트릭)" 명시 | **반영 완료**(runbook §1 해소확인·§2) | 2차 세트 OK가 복구 후 **17분26초** 지연(1차 세트 2분49초 — 편차 큼). 온콜이 OK를 복구 신호로 오인하면 대응 판단을 그르침. §1 해소확인의 "Slack OK 통지" 회복기준을 제거하고 healthz·describe-services 직접 판정으로 교체 |
| A-10 | 상시 synthetic canary(주기적 healthz/기능 요청) 도입 검토 | **종결(P4(c) · 2026-07-13) — 반영** | Route53 헬스체크(무트래픽 canary) 도입·머지(PR #11). 무트래픽 드릴 실증 — canary 요청이 무트래픽에도 `alb-elb-5xx` 발화(약 3분44초)시키고, 전용 `canary_down`(`HealthCheckStatus`) 백스톱도 발화(약 5분47초)·Slack 수신. A-5의 대안이 아니라 **무트래픽 1차 방어선**으로 확정. 상세 [ADR 0004](../adr/0004-notraffic-canary.md) |

**요약**(2026-07-13 갱신 — A-2는 P4(b), A-5·A-10은 P4(c)에서 종결 → 백로그 0): 반영 완료 5(A-4·A-6·A-7·A-8·A-9 — 문서 working tree 수정 끝, 커밋만 사람) + 통합 1(A-3→A-6) + 기각 1(A-1) + **P4(b) 종결 1(A-2 — [ADR 0003](../adr/0003-deploy-failure-alerts.md))** + **P4(c) 종결 2(A-5·A-10 — [ADR 0004](../adr/0004-notraffic-canary.md), 무트래픽 canary 드릴 실측)** = **전수 10건 종결**.

## 9. 참고

- 증거: `docs/postmortems/evidence/gameday-01/`(로컬 전용, gitignore) — `c1-healthz.log`(C-1 전체), `alarm-history-full.json`(드릴 전체 알람 상태변화 14건, **MTTD 근거 재현성 확보**), `ecs-service-events.json`(S1 롤백 이벤트 포함), `s1-run-28774201504.log`(**A-1 기각 근거** — S1 run 에러문구·resolved SHA `c465972`·판정경로=부재 감지), `s1-rerun-ecs-poll.log`·`s2-recovery-poll.log`·`s3-poll.log`(2차 세트 감시), Slack 스크린샷(사용자 보관)
- 상세 타임라인·1차 세트 전체 기록: [`2026-07-06-gameday-01.md`](2026-07-06-gameday-01.md)
- runbook 반영 완료: [`alarm-response.md`](../runbooks/alarm-response.md) §0-2·§1·§2·§8·해소확인(A-6·A-8·A-9) / `load/chaos/README.md`·기록지 치트시트 I-1(A-4·A-7) — 전부 working tree 반영, **커밋은 사람**
