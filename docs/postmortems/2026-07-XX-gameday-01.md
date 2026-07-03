# GameDay 01 기록지 — P3-2 의도적 장애 주입 (드릴 당일 이 문서를 채운다)

- 계획·예측 원문: [`docs/plans/0003-p3-2-fault-injection.md`](../plans/0003-p3-2-fault-injection.md) §4
- runbook(따라 할 문서): [`docs/runbooks/alarm-response.md`](../runbooks/alarm-response.md)
- 역할: **주입·복구(mutating) = 사람** / 관측·기록 = 에이전트(read-only) — 가드레일 #1
- 드릴 일자: 2026-07-\_\_ (확정 시 파일명도 실제 날짜로 변경)
- 상태: **드릴 전** → 드릴 완료 후 본 기록을 근거로 회고 확정(TEMPLATE.md 양식)

> 시각은 전부 **KST, 초 단위까지**(`date "+%H:%M:%S"`) 기록한다. MTTD/MTTR 산출 근거다.

---

## 0. 사전 게이트 (하나라도 미충족이면 드릴 시작 금지)

Step 0 잔여 — 2026-07-03 실측 기준 **알람 12개는 이미 apply·전부 OK, 스택 가동 중**.
남은 것은 Slack 배선(SNS 구독 0개 확인됨)과 P3-1 머지다.

- [ ] **Slack 배선**: 콘솔 OAuth → `terraform.tfvars`에 `slack_team_id`/`slack_channel_id` → apply(사람) → 확인:
  `aws sns list-subscriptions-by-topic --topic-arn $(terraform -chdir=infra/prod output -raw sns_alarms_topic_arn) --region ap-northeast-2` → 구독 1개(https/chatbot)
- [ ] **Slack 수신 실증**: `set-alarm-state` 테스트(ALARM→OK, `infra/README.md` 절차)로 카드 실수신 확인
- [ ] **P3-1 PR 머지** → 드릴 산출물 브랜치 `infra/p3-2-fault-injection` 생성(main 기준)
- [ ] **스택 가동**: `curl -sk -o /dev/null -w '%{http_code}\n' https://lpulse.live/healthz` = 200
- [ ] **chaos 이미지 준비 완료**: `load/chaos/README.md` 4단계(빌드→로컬 404 검증→push→**arm64 확인**) 전부 통과
- [ ] **B-1 기준선 기록**(아래 표) — **이것 없이 S1 시작 금지**(자동 롤백이 지연·실패하는 순간 즉시 필요)
- [ ] **B-2**: 알람 12개 전부 OK 확인(치트시트 C-2) + 위 Slack 수신 실증 완료
- [ ] **드릴 창 동안 `terraform apply`/`destroy` 금지 합의** — 특히 `full-destroy-prod.sh`는 **알람 이력·로그·ECR을 지워 드릴 증거가 소실**된다(§5)

### B-1 — 수동 롤백 기준선 (드릴 당일 재실측 필수)

참고: 2026-07-03 실측값 — taskdef `linkpulse-prod-app:10`, 이미지 태그 `7d5f88073ec4040bb80408ffb1af6aa916766e69`(ECR 유일 이미지). **당일 값이 다르면 당일 값이 기준.**

| 항목 | 기록(당일) |
| --- | --- |
| 기록 시각 |  |
| 서빙 taskdef ARN·리비전 (C-3 ①) |  |
| 이미지 태그 = known-good (C-3 ②) |  |
| ECR에 그 태그 존재 확인 (C-3 ③) | [ ] 확인됨 |
| chaos-healthz-v1 ECR 존재·arm64 | [ ] 확인됨 |

---

## 1. 명령 치트시트 (드릴 중 복붙용 — C-* 관측은 read-only, I-*/R-*는 사람 실행. **R-* 번호는 runbook과 동일 체계**)

```bash
# C-1 외부 관측(별도 터미널에 상시): 5초 간격 healthz 폴링 + 타임스탬프
while true; do echo "$(date '+%H:%M:%S') $(curl -sk -o /dev/null -w '%{http_code}' --max-time 4 https://lpulse.live/healthz)"; sleep 5; done

# C-2 알람 상태 전수(비정상만)
aws cloudwatch describe-alarms --alarm-name-prefix linkpulse-prod \
  --query 'MetricAlarms[?StateValue!=`OK`].[AlarmName,StateValue]' --output table --region ap-northeast-2

# C-3 기준선: ①서빙 taskdef → ②이미지 태그 → ③ECR 존재
aws ecs describe-services --cluster linkpulse-prod-cluster --services linkpulse-prod-app \
  --query 'services[0].taskDefinition' --output text --region ap-northeast-2
aws ecs describe-task-definition --task-definition <위 ARN> \
  --query 'taskDefinition.containerDefinitions[0].image' --output text --region ap-northeast-2
aws ecr describe-images --repository-name linkpulse/app --image-ids imageTag=<태그> --region ap-northeast-2

# C-4 서비스 이벤트·배포 상태(S1의 1차 증거 — rolloutState/rolloutStateReason 포함)
aws ecs describe-services --cluster linkpulse-prod-cluster --services linkpulse-prod-app \
  --query 'services[0].{desired:desiredCount,running:runningCount,
  deployments:deployments[].{id:id,status:status,rollout:rolloutState,reason:rolloutStateReason,taskDef:taskDefinition,failed:failedTasks},
  events:events[0:10].[createdAt,message]}' --output json --region ap-northeast-2

# C-5 타깃 헬스
aws elbv2 describe-target-health --region ap-northeast-2 --target-group-arn \
  $(aws elbv2 describe-target-groups --names linkpulse-prod-tg --query 'TargetGroups[0].TargetGroupArn' --output text --region ap-northeast-2)

# C-6 알람 이력(드릴 후 증거 덤프에도 사용)
aws cloudwatch describe-alarm-history --alarm-name <알람명> --history-item-type StateUpdate \
  --start-date <드릴시작 ISO> --end-date <드릴종료 ISO> --output json --region ap-northeast-2

# I-1 [사람] S1 주입: main에서 chaos 태그 배포
gh workflow run deploy.yml --ref main -f image_tag=chaos-healthz-v1
gh run watch   # 또는 Actions 웹 UI — run 링크·resolved 액션 버전(SHA)을 기록지에 남긴다

# I-2 [사람] S2 주입: 실다운
aws ecs update-service --cluster linkpulse-prod-cluster --service linkpulse-prod-app \
  --desired-count 0 --region ap-northeast-2

# I-3 [사람] S2: 먼저 첫 503을 기다린 뒤(드레인 창 ~30s 동안 200 혼입 방지) 503 연사 12발
#     계획 §4 원문 순서 그대로 — "첫 503 확인 후 연사". 503이 10건 이상이어야 P7 버킷 분할 보장 성립.
until [ "$(curl -sk -o /dev/null -w '%{http_code}' --max-time 4 https://lpulse.live/)" = "503" ]; do sleep 2; done
echo "first 503: $(date '+%H:%M:%S')"
for i in $(seq 1 12); do echo "$(date '+%H:%M:%S') $(curl -sk -o /dev/null -w '%{http_code}' --max-time 4 https://lpulse.live/)"; sleep 2; done

# R-1 [사람] S1 수동 롤백(정식 경로 — 여유 있을 때만. = runbook R-1): B-1의 known-good 태그 재배포
#     주의: chaos run이 deploy.yml concurrency(직렬화)를 점유한 동안 이 dispatch는 큐에 밀린다.
gh workflow run deploy.yml --ref main -f image_tag=<B-1의 태그>

# R-1e [사람] S1 긴급 롤백(사용자 영향 진행 중 — GitHub 우회, 즉시. = runbook R-1e): B-1의 taskdef ARN 직접 지정.
#      새 deployment가 chaos deployment를 즉시 대체. 점유 중이던 run은 red로 끝난다(정상).
aws ecs update-service --cluster linkpulse-prod-cluster --service linkpulse-prod-app \
  --task-definition <B-1의 taskdef ARN> --region ap-northeast-2
# 이후 큐 run 정리: gh run list --workflow=deploy.yml → 필요 시 gh run cancel <id>

# R-2 [사람] S2 복구: desired 복원 (= runbook R-2)
aws ecs update-service --cluster linkpulse-prod-cluster --service linkpulse-prod-app \
  --desired-count 2 --region ap-northeast-2
```

---

## 2. S1 — 나쁜 이미지 배포 (자동 방어 검증, 무중단 예상)

**중단 기준**: C-1에서 200 아닌 응답 관측 또는 HealthyHostCount<2 → **즉시 R-1e**(긴급, GitHub 우회 — R-1 dispatch는 chaos run이 concurrency를 점유한 동안 큐에 밀리므로 긴급용이 아니다).

### 타임라인

| 시각 | 이벤트 | 출처 |
| --- | --- | --- |
|  | I-1 dispatch 실행 (**T0**) | 터미널 |
|  | 새 deployment 생성 확인 | C-4 |
|  | unhealthy 타깃 첫 관측 | C-5 |
|  | (발화 시) `alb-unhealthy-hosts` Slack ALARM | Slack |
|  | ECS 이벤트 "deployment failed / rolling back" | C-4 |
|  | 롤백 deployment 완료(구 이미지, COMPLETED) | C-4 |
|  | Actions run 종료(색 기록) | run 링크 |
|  | (발화했다면) 알람 OK 복귀 | Slack |

### 예측 vs 실측 (판정: 적중/부분/빗나감)

| # | 예측(계획 §4 요약) | 실측 | 판정 |
| --- | --- | --- | --- |
| P1 | 무중단: C-1 내내 200, HealthyHostCount 2 유지 |  |  |
| P2 | `alb-unhealthy-hosts` ALARM→Slack (가능성 높음, 단정 아님 — 1분×2 연속 충족 여부가 관건) |  |  |
| P3 | 실패 태스크 3개째서 FAILED+자동 롤백, T0+8~15분. **1차 증거 = C-4 이벤트·rolloutStateReason** |  |  |
| P4 | run **red**(보조 신호). 에러 문구·resolved 버전·판정 경로 기록. green이면 A-1 재론 |  |  |
| P5 | 침묵: 5xx 계열·no-healthy-hosts·running-tasks-low |  |  |

### 증거 칸 (원문 붙여넣기)

- ECS 이벤트(failed/rolling back 원문):
- 롤백 deployment의 `rolloutState`/`rolloutStateReason`:
- Actions run 링크: / resolved 액션 버전(SHA): / 에러 문구: / 판정 경로(부재 감지·FAILED 감지·waiter timeout 중):
- C-1 폴링 요약(총 N회 중 200 N회):

---

## 3. S2 — 전체 다운 드릴 (감지→대응 리허설, 실다운 5~10분)

**전제**: S1 종료 후 안정 상태(알람 전부 OK, healthy 2) 재확인.
**MTTD** = T0(I-2 실행) → 첫 Slack ALARM 수신. **MTTR** = 첫 Slack ALARM → healthz 연속 200 회복(복원 후). OK 통지 시각은 별도 병기.

### 타임라인

| 시각 | 이벤트 | 출처 |
| --- | --- | --- |
|  | I-2 실행 (**T0**) | 터미널 |
|  | 첫 503 관측 | C-1 |
|  | I-3 연사 완료(503 ≥10건: \_\_건) | I-3 출력 |
|  | `alb-no-healthy-hosts` Slack ALARM (**MTTD 기준**) | Slack |
|  | `alb-elb-5xx` Slack ALARM | Slack |
|  | runbook 따라 진단: desired=0 발견 | C-4 |
|  | R-2 실행(복구 조치) | 터미널 |
|  | healthz 200 연속 회복 (**MTTR 기준**) | C-1 |
|  | 알람 OK 통지 수신 | Slack |
|  | desired=2 재확인(드리프트 소멸) | C-4 |

- **MTTD = \_\_분 \_\_초** / **MTTR = \_\_분 \_\_초**
- I-3에서 기록된 503이 10건 미만이면 P7 미발화는 알람 결함이 아니라 **절차 결함**으로 판정.

### 예측 vs 실측

| # | 예측(계획 §4 요약) | 실측 | 판정 |
| --- | --- | --- | --- |
| P6 | `alb-no-healthy-hosts`: ~4–6분 내 ALARM→Slack (breaching 설계의 실전 검증) |  |  |
| P7 | `alb-elb-5xx`: ≥5/5분 → ALARM (~5–7분, 버킷 경계 시 +5분) |  |  |
| P8 | `ecs-running-tasks-low`: ALARM **또는** 지표 단절로 침묵 — **어느 쪽인지가 관찰 질문**(침묵 시 A-3) |  |  |
| P9 | 침묵: target-5xx·RDS 계열 |  |  |

---

## 4. S3 (선택) — 태스크 1개 강제 종료 (자가치유 관찰)

- [사람] `aws ecs stop-task` 1개 → 예측: ~2분 내 보충, **알람 전부 침묵**(3분 평가창보다 빠른 자가치유)
- T0: / 보충 완료: / 울린 알람(없음 예상):

---

## 5. 증거 수집 (드릴 직후, 같은 날 — **full-destroy 전에 반드시**)

`full-destroy-prod.sh`는 CloudWatch 알람(이력 포함)·로그·ECR 이미지를 지운다. **회고 확정 전 destroy 금지.**
`docs/postmortems/evidence/`는 **.gitignore 대상(로컬 보관 전용)** — 원본 JSON·캡처에는 계정 ID·ARN·Slack ID 같은 운영 메타데이터가 들어간다. 커밋되는 회고 문서에는 **내용을 확인한 발췌만** 싣는다.

- [ ] C-6으로 발화·침묵 알람 이력 JSON 덤프 → `docs/postmortems/evidence/gameday-01/` 저장
- [ ] C-4 서비스 이벤트·deployments JSON 덤프(S1 롤백 증거 포함)
- [ ] Actions run 링크·resolved 버전·로그 발췌
- [ ] Slack 알람 카드 스크린샷(ALARM·OK)
- [ ] C-1/I-3 터미널 출력 원문
- [ ] 회고 초안 작성(TEMPLATE.md) → 액션 아이템 판정(A-2·A-3 포함 전수) → runbook에 실측 반영
