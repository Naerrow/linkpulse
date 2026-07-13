# Runbook — CloudWatch 알람 대응 (linkpulse-prod)

Slack에 알람 카드가 오면 **이 문서를 위에서부터** 따라간다. 1인 운영 전제 — 에스컬레이션 대상은 없고, 대신 모든 판단·시각을 기록해 회고로 넘긴다.

- 통지 경로: CloudWatch Alarm → SNS(`linkpulse-prod-alarms`) → Chatbot → Slack ([ADR 0002](../adr/0002-alerting-design.md)). **단 무트래픽 canary(§13)만 us-east-1 전용 토픽(`linkpulse-prod-canary-alarms`)을 거친다**([ADR 0004](../adr/0004-notraffic-canary.md))
- 알람 정의(임계값의 소스오브트루스): `infra/prod/monitoring.tf` — **임계값은 전부 초기값**, 오탐이 반복되면 §튜닝 노트를 보고 Terraform으로 조정(사람 apply)
- 표기: 명령은 read-only가 기본. 상태를 바꾸는 명령은 **[사람]** 표기 — 에이전트 자율 실행 금지(가드레일 #1)
- 로그 조회 쿼리 모음·Slack 배선 점검은 [`infra/README.md`](../../infra/README.md) 참고

---

## 0. 공통 첫 5분 (어떤 알람이든 이 순서)

1. **시각 기록** — Slack 카드 수신 시각(초 단위). MTTD/회고의 기준점.
2. **사용자 영향 확인**:
   ```bash
   curl -sk -o /dev/null -w '%{http_code}\n' --max-time 5 https://lpulse.live/healthz
   ```
   - `200` → 영향 없음/미미. 침착하게 해당 알람 절로.
   - `503`/timeout → **다운. 먼저 `alb-elb-5xx`(§2)와 0-4의 `describe-services`(desired/running)를 본다.** GameDay 실측(2026-07-06): 전면 다운 시 **실제로 가장 먼저·일관되게 울린 건 `alb-elb-5xx`**(MTTD 3~5분)였고, `no-healthy-hosts`(§1)·`running-tasks-low`(§8)는 **늦거나(복구 후) 아예 침묵**했다. **무트래픽 다운(새벽 등 실사용 0)이어도 P4(c) canary가 `lpulse.live/healthz`를 상시 외부 프로빙(§13)**하므로, 그 synthetic 요청으로 `alb-elb-5xx`가 발화하고 canary 자신의 `canary_down`(§13, us-east-1)이 결정론적 백스톱이 된다. `no-healthy-hosts`(§1)는 **느린 최후 백스톱**으로만 남고 MTTD 신호로 신뢰하지 않는다(회고 A-5, [ADR 0004](../adr/0004-notraffic-canary.md)).
3. **비정상 알람 전수 확인** (동반 알람 조합이 원인을 말해준다). **두 리전 모두 본다** — 12개 알람은 ap-northeast-2, 무트래픽 canary(§13)는 **us-east-1**에 있다(빠뜨리면 canary 다운을 놓친다):
   ```bash
   aws cloudwatch describe-alarms --alarm-name-prefix linkpulse-prod \
     --query 'MetricAlarms[?StateValue!=`OK`].[AlarmName,StateValue,StateReason]' --output table --region ap-northeast-2
   # canary(§13)는 us-east-1
   aws cloudwatch describe-alarms --alarm-name-prefix linkpulse-prod \
     --query 'MetricAlarms[?StateValue!=`OK`].[AlarmName,StateValue,StateReason]' --output table --region us-east-1
   ```
4. **ECS가 스스로 말하는 것부터 듣는다** (desired/running·배포 상태·최근 이벤트):
   ```bash
   aws ecs describe-services --cluster linkpulse-prod-cluster --services linkpulse-prod-app \
     --query 'services[0].{desired:desiredCount,running:runningCount,
     deployments:deployments[].{status:status,rollout:rolloutState,reason:rolloutStateReason,taskDef:taskDefinition,failed:failedTasks},
     events:events[0:10].[createdAt,message]}' --output json --region ap-northeast-2
   ```
5. **맥락 자문**: 방금 배포가 있었나(Actions run)? 내가 desired·인프라를 건드렸나(드릴·apply)? — 최근 변경이 있으면 그것이 제1 용의자.

### 공통 복구 도구 (아래 각 알람에서 R-번호로 참조)

| # | 도구 | 명령 |
| --- | --- | --- |
| R-1 | **[사람]** 정상 태그 재배포(수동 롤백, 정식 경로) | `gh workflow run deploy.yml --ref main -f image_tag=<직전 정상 sha>` — sha는 배포 전 기록한 기준선(B-1) 우선. **문제 run이 아직 돌고 있으면 이 dispatch는 큐에 밀린다(concurrency 직렬화) → 사용자 영향 진행 중이면 R-1e 먼저.** 기준선이 없을 때: 문제 배포 **진행 중**(deployments 2개)이면 **`status=ACTIVE`인 구 deployment**의 taskDefinition → ②로 태그. 배포가 **끝난 뒤**(deployments 1개 = 그 `PRIMARY`가 문제 리비전)면 `aws ecs list-task-definitions --family-prefix linkpulse-prod-app --sort DESC --max-items 5 --region ap-northeast-2`로 직전 리비전 → ②로 태그(그 리비전이 실제 정상 서빙했는지는 Actions 성공 run으로 확인). 배포 중 `services[0].taskDefinition`은 새(문제) 리비전을 가리키므로 **쓰지 않는다** |
| R-1e | **[사람]** **긴급 롤백(GitHub 우회)** — 문제 run이 돌고 있고 사용자 영향이 진행 중일 때 | `aws ecs update-service --cluster linkpulse-prod-cluster --service linkpulse-prod-app --task-definition <known-good taskdef ARN> --region ap-northeast-2` — deploy.yml은 concurrency 직렬화(`cancel-in-progress: false`)라 문제 run이 안정화 대기 중이면 R-1 dispatch가 **큐에 밀린다**. 이 명령은 즉시 새 deployment로 문제 deployment를 대체한다. 점유 중이던 run은 red로 끝나며(정상), 큐 run은 `gh run list --workflow=deploy.yml` → `gh run cancel <id>`로 정리 |
| R-2 | **[사람]** desired 복원 | `aws ecs update-service --cluster linkpulse-prod-cluster --service linkpulse-prod-app --desired-count 2 --region ap-northeast-2` |
| R-3 | **[사람]** 태스크 전체 재기동(이미지 그대로) | 위 명령에 `--force-new-deployment` (desired 옵션 대신). **배포 사고 진행 중엔 금지** — 그때의 서비스 taskDefinition은 문제 리비전일 수 있어 그걸 다시 깐다(그 상황은 R-1/R-1e) |
| R-4 | 로그 조회 | CloudWatch Logs Insights, 그룹 `/ecs/linkpulse-prod-app` — 쿼리 모음은 `infra/README.md` |
| R-5 | 타깃 헬스 확인 | `aws elbv2 describe-target-health --region ap-northeast-2 --target-group-arn $(aws elbv2 describe-target-groups --names linkpulse-prod-tg --query 'TargetGroups[0].TargetGroupArn' --output text --region ap-northeast-2)` |
| R-6 | 중지된 태스크의 사인(死因) | `aws ecs list-tasks --cluster linkpulse-prod-cluster --service-name linkpulse-prod-app --desired-status STOPPED --region ap-northeast-2` → `aws ecs describe-tasks --cluster linkpulse-prod-cluster --tasks <arn> --query 'tasks[].{stopped:stoppedReason,exit:containers[].exitCode,reason:containers[].reason}' --region ap-northeast-2` (중지 후 ~1시간만 조회 가능) |

②: `aws ecs describe-task-definition --task-definition <arn> --query 'taskDefinition.containerDefinitions[0].image' --output text --region ap-northeast-2` — 태그는 이미지 URI의 `:` 뒷부분. known-good taskdef ARN·태그는 사고 후 찾는 게 아니라 **배포 전 기준선(B-1)으로 기록해두는 것**이 원칙이다.

### 심각도

- **SEV-1 즉시 대응(사용자 영향·고위험)**: §1 no-healthy-hosts · §2 elb-5xx · §3 target-5xx · §8 running-tasks-low · §10 rds-free-storage-low · **§13 canary-down(us-east-1, 무트래픽 방어선 — 단 첫 발화는 오탐 진위 확인 후)**
- **SEV-2 당일 대응(성능·용량 경고)**: 나머지. 단 **여러 SEV-2가 동시에 오면 SEV-1로 취급**(복합 장애 신호).

---

## SEV-1

### §1 `linkpulse-prod-alb-no-healthy-hosts` — 정상 타깃 0 = 서비스 다운

- **의미**: 타깃그룹에 healthy 타깃이 3분간 없거나, **지표 자체가 소실**(타깃 전원 deregister — `treat_missing=breaching` 설계). 원래 무트래픽 다운의 방어선으로 설계했으나, **P4(c) canary 도입 후에는 느린 최후 백스톱으로 강등**됐다(무트래픽 1차 방어선은 canary/§13). **이 알람이 울리면 진짜 다운이지만, 그 역(다운이면 이 알람이 곧 울린다)은 실측상 성립하지 않았다** — GameDay(2026-07-06) 전면 다운 2회에서 이 알람은 9분48초 만에(복구 후) 울리거나 아예 미발화했다. **즉 빠른 MTTD 신호로 신뢰하지 말 것** — 다운 감지는 `alb-elb-5xx`(§2)·canary(§13)가 먼저 한다. (이 지연/미발화의 원인 규명은 P4(c)에서 진행 — [ADR 0004](../adr/0004-notraffic-canary.md), 판정=백스톱 유지·비신뢰, 근본원인 확정은 step 8.)
- **먼저**: 공통 0-2(대개 503)·0-4. `desired`/`running` 숫자가 첫 갈림길.
- **원인 → 복구**:
  1. **desired=0** (사람 실수, 드릴 뒤 복원 누락): 0-4에서 `desired: 0` → **R-2**. 복구 ~2–3분.
  2. **전 태스크 반복 크래시/기동 실패**: `running`이 0이거나 요동, events에 "stopped"/"unable to place" → **R-6**으로 exit code·사유 확인 → 앱 문제면 §3과 동일 진단, 직전 배포가 원인이면 **R-1**.
  3. **배포 전멸 + 자동 롤백도 실패**: deployments에 FAILED만 남음 — 이때 배포 run이 안정화 대기로 아직 살아 있을 공산이 크다 → **R-1e가 1순위**(R-1 dispatch는 큐 대기), run 종료를 확인했다면 R-1(기준선 태그).
  4. 위 전부 정상인데 unhealthy만 지속: 헬스체크 경로 자체 문제(SG·TG 설정 변경 여부, 최근 apply 확인) → **R-5**로 사유(`Target.ResponseCodeMismatch` 등) 확인.
- **해소 확인**: healthy 2 회복(R-5) + healthz 연속 200 + `describe-services`의 `running=desired`로 **직접 판정**. **Slack OK 통지는 회복 기준으로 쓰지 말 것**(§2 실측: `alb-elb-5xx` OK가 복구 후 최대 17분 지연 — A-9), OK 수신 시각은 별도 병기만. **MTTR(첫 ALARM→정상) 기록.**
- **튜닝**: 오탐 여지 거의 없음(설계 의도). 유지.

### §2 `linkpulse-prod-alb-elb-5xx` — ALB 자신이 낸 5xx ≥5/5분

- **의미**: 요청이 타깃까지 못 갔다 — 503(정상 타깃 없음)·502(타깃 연결 거부/비정상 응답)·504(타임아웃). 사용자에게 오류가 **보이고 있다**. dimension이 LB 전용이라 타깃 5xx(§3)와 구분된다.
- **전면 다운의 1순위 신호(실측)**: GameDay 2회 모두 이 알람이 전면 다운을 **가장 먼저**(MTTD 3분37초·4분54초) 감지했다 — 503이 뜨면 §1보다 이 절을 먼저 본다. **P4(c) canary 도입 후에는 실사용 트래픽이 0이어도 canary의 `/healthz` synthetic 프로빙이 ALB 503을 만들어 이 알람이 발화**하므로 무트래픽에서도 1순위다(canary 자체 liveness는 §13 `canary_down`). canary·외부 요청까지 전부 없는 극단에서만 이 알람이 침묵하고, 그때는 §1(느린 백스톱)이 최후 보루다.
- **⚠ OK 통지는 신뢰하지 말 것(회복 판정용 아님)**: 이 지표(`HTTPCode_ELB_5XX_Count`)는 **sparse 카운트**(5xx=0이면 데이터포인트 부재)라, 복구 후 missing→`notBreaching` 확정에 시간이 크게 걸린다 — 실측 OK 지연이 **복구 후 2분49초~17분26초**로 편차가 컸다. **회복 여부는 OK 알림을 기다리지 말고 `curl .../healthz`=200 + 0-4의 `running=desired`로 직접 확인**(MTTR 기준점). (회고 A-9)
- **먼저**: §1 동반 여부(0-3) — 동반이면 §1 절차가 곧 복구. 단독이면:
  - **502 위주**: 태스크가 죽는 순간의 잔여 연결 가능성 → 0-4 events·R-6(방금 태스크 교체가 있었나), R-5.
  - **504 위주**: 앱이 느리다 → §5(p95)와 §9(RDS CPU) 확인.
  - 어떤 코드인지는 CloudWatch 지표 `HTTPCode_ELB_502_Count`/`503`/`504`(콘솔) 또는 상황상 자명(다운=503).
- **복구**: 원인별 — 다운이면 §1, 태스크 불안정이면 **R-3**(재기동으로 시간 벌기) 후 원인 추적, 배포 직후면 **R-1**.
- **튜닝**: 인터넷 스캐너가 간헐 502 소량을 만들 수 있음 — 사용자 영향 없이 반복 발화하면 threshold 상향 검토.

### §3 `linkpulse-prod-alb-target-5xx` — 앱이 낸 5xx ≥5/5분

- **의미**: 타깃(앱)이 5xx를 응답. 앱 코드·의존성(DB) 문제.
- **먼저**: **R-4** 로그에서 어떤 경로·에러인지:
  ```
  fields @timestamp, status, method, path, duration_ms | filter status >= 500 | sort @timestamp desc | limit 50
  ```
  panic이면: `filter msg = "panic recovered"`. RDS 알람 동반(0-3)이면 DB 쪽(§9–§12) 먼저.
- **원인 → 복구**:
  1. **직전 배포가 원인**(배포 시각과 5xx 시작 일치): **R-1**.
  2. **DB 연결 실패/고갈**: 로그에 connection 계열 에러 → §12 확인, RDS 상태 `aws rds describe-db-instances --db-instance-identifier linkpulse-prod-pg --query 'DBInstances[0].DBInstanceStatus' --region ap-northeast-2`.
  3. **특정 입력이 유발하는 panic**: 로그로 재현 조건 특정 → 코드 수정 배포(정상 절차). 그동안 오류율이 낮으면 관찰.
- **튜닝**: 실트래픽에서 임계 5건이 과민하면 오류율 기반(수식 알람)으로 교체 검토 — 회고 액션 아이템으로.

### §8 `linkpulse-prod-ecs-running-tasks-low` — running < desired(2) 3분

- **의미**: 자가치유(ECS 보충)가 3분 내 못 따라잡음 = 반복 크래시·기동 불가·리소스 부족. (출처: Container Insights)
- **먼저**: 0-4(events에 stop/placement 사유) → **R-6**(exit code: 137=OOM/강제종료, 1=앱 에러, `CannotPullContainerError`=이미지 문제).
- **원인 → 복구**:
  1. **나쁜 이미지/기동 실패**(배포 직후): circuit breaker가 롤백 중인지 deployments의 `rolloutState` 확인 — 롤백 중이면 개입하지 말고 완료 대기(S1 드릴 실증 경로), 실패면 **사용자 영향+run 진행 중일 땐 R-1e**(dispatch는 큐 대기), 아니면 R-1.
  2. **OOM 반복**(exit 137, §7 동반): **R-3** 후 task_memory 상향 검토(Terraform+CI 1회 — ADR 0001 함정).
  3. **이미지 pull 실패**: ECR 태그 존재 확인(`aws ecr describe-images ...`) → 없으면 **R-1**(존재하는 태그로).
- **한계(GameDay 2026-07-06 실측 반영)**: 전면 다운(desired=0) 2회에서 이 알람은 **다운 지속시간에 좌우**됐다 — 9분대 다운에선 **복구 완료 후 17초 뒤에야** ALARM(Container Insights 지표 발행 지연), 7분대 다운에선 **아예 미발화**. 즉 실시간 다운 감지용으로 신뢰 불가이며, **전면 다운 방어선은 이 알람도 §1도 아니고 실측상 `alb-elb-5xx`(§2)였다**. 이 알람이 뜨면 **먼저 0-4 `describe-services`로 현재 desired/running부터 확인**할 것 — 이미 복구된 장애를 진행 중으로 오인하지 않도록(뒤늦게 오는 특성). (회고 A-6, 원 예측 P8)
- **튜닝**: 오토스케일링 도입으로 desired가 가변이 되면 임계 재설계(ADR 0002 §결과).

### §10 `linkpulse-prod-rds-free-storage-low` — 여유 스토리지 ≤10%(2GB)

- **의미**: 느리게 진행하는 고위험. 가득 차면 쓰기 실패·인스턴스가 storage-full로 멈춘다. `treat_missing=breaching`.
- **먼저**: 추세 확인(콘솔 FreeStorageSpace 2주 그래프) — 며칠 여유인지 판단. 원인: 데이터 증가 vs 로그/블로트.
  ```bash
  aws rds describe-db-instances --db-instance-identifier linkpulse-prod-pg \
    --query 'DBInstances[0].{alloc:AllocatedStorage,maxAlloc:MaxAllocatedStorage,status:DBInstanceStatus}' --region ap-northeast-2
  ```
- **복구**: ① 급하면 **[사람]** `db_allocated_storage` 상향 Terraform apply(gp3 온라인 확장, 단 확장 후 6시간 재확장 불가) ② 근본: 큰 테이블 확인(`SELECT relname, pg_size_pretty(pg_total_relation_size(oid)) FROM pg_class ORDER BY pg_total_relation_size(oid) DESC LIMIT 10;`) → 정리·보존정책. `max_allocated_storage`(오토스케일링) 설정 여부도 확인 — 설정돼 있으면 자동 확장이 먼저 동작한다.
- **튜닝**: 없음 — 이 알람은 이르게 우는 편이 싸다.

---

## SEV-2

### §4 `linkpulse-prod-alb-unhealthy-hosts` — unhealthy 타깃 ≥1 (1분×2)

- **의미**: 일부 타깃이 헬스체크(`/healthz` 200, 30s×3) 실패. **healthy가 남아 있으면 사용자 영향 없음** — §1과의 차이.
- **먼저**: 0-2로 영향 확인 → 0-4로 **배포 중인지**(deployments 2개 = 롤링 진행 중; 새 태스크가 잠깐 unhealthy인 것은 정상 소음일 수 있음) → **R-5**로 어느 타깃·사유.
- **원인 → 복구**:
  1. **배포 중 새 태스크 unhealthy**: circuit breaker가 알아서 판정(성공 or 롤백) — 개입하지 않고 관찰, FAILED로 끝나면 §8-1.
  2. **기존 태스크 1개 이상**: R-6·R-4로 원인(OOM·행·panic) → 자가치유(ECS가 교체) 확인, 반복이면 **R-3**.
- **해소 확인**: R-5 전부 healthy + Slack OK.
- **튜닝**: 정상 배포마다 울리면 소음 — evaluation_periods 상향 또는 "배포 창은 무시" 운영 규칙. GameDay S1의 P2가 이 알람의 실전 데이터 1호 — 실측 후 판단.

### §5 `linkpulse-prod-alb-latency-p95` — p95 ≥1s (1분×3)

- **의미**: 느려졌다(오류는 아님). 저샘플 왜곡은 `evaluate_low_sample_count_percentiles=ignore`로 이미 완화.
- **먼저**: **R-4** 느린 요청 쿼리로 경로 특정:
  ```
  fields @timestamp, path, status, duration_ms | filter duration_ms > 1000 | sort duration_ms desc | limit 50
  ```
  동반 알람 조합: §6(ECS CPU)이면 앱 포화, §9/§12(RDS)면 DB 병목.
- **복구**: 원인별 — 특정 쿼리(인덱스·N+1)면 코드 수정, 전반 포화면 **[사람]** desired 상향(임시) 또는 task_cpu 상향(Terraform+CI), DB면 §9.
- **튜닝**: 실트래픽 관측 후 임계 재설정(ADR 0002 — 초기값).

### §6 `linkpulse-prod-ecs-cpu-high` — 서비스 CPU ≥80% (1분×3)

- **의미**: 태스크 CPU 포화 임박. 지연(§5)으로 번지기 전 경고.
- **먼저**: 트래픽 급증인지(콘솔 ALB RequestCount), 특정 경로의 비효율인지(R-4 duration 상위), 배포 직후인지(새 코드 회귀).
- **복구**: 트래픽이면 **[사람]** desired 상향(임시) → 지속되면 task_cpu 상향 또는 오토스케일링(백로그, P4+). 코드 회귀면 **R-1**.
- **튜닝**: 짧은 스파이크로 반복 발화 시 evaluation_periods 상향.

### §7 `linkpulse-prod-ecs-memory-high` — 서비스 메모리 ≥80% (1분×3)

- **의미**: CPU와 달리 메모리는 **한계 도달 시 OOMKill**(태스크 사망, exit 137) — §4/§8로 번진다.
- **먼저**: 추세(콘솔 그래프) — 계단식 증가면 누수 의심. R-4로 트래픽 대비 비정상 여부.
- **복구**: 누수 의심이면 **R-3**(재기동으로 리셋, 시간 벌기) → 근본은 프로파일링·수정. 정상 워킹셋 증가면 task_memory 상향(Terraform+CI 1회).
- **튜닝**: Go 앱 특성상 GC 후 반환이 느릴 수 있음 — 80%에서 소음이면 85–90%로.

### §9 `linkpulse-prod-rds-cpu-high` — RDS CPU ≥80% (5분×3)

- **의미**: DB 병목. **t4g.micro는 버스터블** — CPU 크레딧 소진 시 성능이 급락하므로 크레딧을 같이 본다(콘솔 `CPUCreditBalance`).
- **먼저**: 앱 지연(§5)·연결 수(§12) 동반 확인, 느린 쿼리 존재 여부(앱 로그 duration 상위 경로의 쿼리).
- **복구**: 쿼리 최적화(인덱스)가 근본. 크레딧 소진이 반복이면 **[사람]** 인스턴스 클래스 상향(Terraform, 재부팅 수반 — 점검창에).
- **튜닝**: 15분 평가라 이미 보수적. 유지.

### §11 `linkpulse-prod-rds-freeable-memory-low` — 여유 메모리 ≤100MB (5분×3)

- **의미**: t4g.micro(1GB)에서 워킹셋 압박. PostgreSQL은 캐시를 적극 쓰므로 **낮다고 곧 장애는 아님** — 스왑과 함께 봐야 한다.
- **먼저**: 콘솔에서 `SwapUsage` 증가 추세 + §5/§9 동반 여부. 셋이 같이 움직이면 진짜 메모리 부족.
- **복구**: 연결 수 과다면 §12(연결이 곧 메모리), 근본 부족이면 인스턴스 클래스 상향.
- **튜닝(ADR 0002 명시)**: 만성 발화하면 임계 하향 또는 SwapUsage 알람으로 교체 — 실트래픽 관측 후 판단.

### §12 `linkpulse-prod-rds-connections-high` — 연결 수 ≥80 (5분×2)

- **의미**: t4g.micro 기본 max_connections ~112 근접. 도달 시 신규 연결 거부 → 앱 5xx(§3)로 번진다.
- **먼저**: 산수 — 태스크 2 × 앱 풀 상한이 80을 넘는 구성인지(앱 DB 풀 설정 확인). 아니면 누수/유휴:
  `SELECT state, count(*) FROM pg_stat_activity GROUP BY 1;` (idle 과다 = 반환 안 됨)
- **복구**: 앱 풀 상한 하향·유휴 타임아웃 설정 후 배포, 급하면 **R-3**(재기동으로 연결 리셋).
- **튜닝**: 파라미터그룹의 실제 max_connections 확인 후 임계 보정(ADR 0002 — 초기값 근사치).

---

## SEV-1 (크로스리전 — us-east-1)

### §13 `linkpulse-prod-canary-down` — Route53 헬스체크 DOWN = 무트래픽 방어선 (us-east-1 알람)

무트래픽 다운의 **1차 방어선**이다(회고 A-5·A-10, [ADR 0004](../adr/0004-notraffic-canary.md)). Route53 헬스체크가 `lpulse.live/healthz`를 외부에서 상시 프로빙해, 실사용 트래픽이 0이어도 `HealthCheckStatus`(1=정상/0=다운)로 down을 잡는다. **이 알람·지표는 us-east-1에만 있다** — 조회는 전부 `--region us-east-1`.

- **⚠ 최초 발행 레이스**: 헬스체크 생성 직후 지표 최초 발행 전 공백이 `breaching`으로 잡혀 **서비스는 정상인데 down 카드가 1회** 뜰 수 있다. **첫 canary ALARM은 아래 진위 확인 전까지 실장애로 단정하지 않는다.**
- **먼저(진위 확인)**: §0-2 `curl .../healthz`가 `200`이면 오탐 의심 → 헬스체크 상태·지표 발행 확인:
  ```bash
  aws cloudwatch describe-alarms --alarm-names linkpulse-prod-canary-down \
    --query 'MetricAlarms[0].[StateValue,StateReason]' --output table --region us-east-1
  # 헬스체크ID는 알람 dimension에서 바로 뽑는다(terraform 불요). state 접근 가능하면 `terraform output -raw canary_health_check_id`도 가능.
  HCID=$(aws cloudwatch describe-alarms --alarm-names linkpulse-prod-canary-down --region us-east-1 \
    --query "MetricAlarms[0].Dimensions[?Name=='HealthCheckId'].Value | [0]" --output text)
  aws cloudwatch get-metric-statistics --namespace AWS/Route53 --metric-name HealthCheckStatus \
    --dimensions Name=HealthCheckId,Value="$HCID" --statistics Minimum --period 60 \
    --start-time <ISO8601> --end-time <ISO8601> --region us-east-1
  ```
  `503`/timeout이면 **진짜 다운** → `alb-elb-5xx`(§2)도 곧/이미 발화한다. §2/§1 경로로 복구(**R-1/R-1e**, desired=0이면 **R-2**).
- **역할 분리(MTTD)**: `alb-elb-5xx`(canary 트래픽 기반 ~3–5분)=빠른 1차, `canary_down`(≈flip 90s + 알람 3분)=결정론적 백스톱. 둘 다 `no-healthy-hosts`(§1, 9분48초/무발화)보다 빠르다 — S2 확인 순서는 여전히 **§2 우선**.
- **통지가 안 오면**: 이 알람은 **us-east-1 전용 토픽**(`sns_canary_topic_arn`)→Chatbot 경로다. `aws sns list-subscriptions-by-topic --topic-arn "$(terraform output -raw sns_canary_topic_arn)" --region us-east-1`로 구독 실존 확인(`infra/README.md` §모니터링).

---

## 부록

### INSUFFICIENT_DATA 일반 해석

- `running-tasks-low`(Insights 활성 직후·태스크 0), RDS 계열(재부팅 공백)에서 정상적으로 나타날 수 있음(`treat_missing=missing` 설계). **§1·§10은 breaching이라 데이터 소실도 ALARM** — 이 둘의 INSUFFICIENT_DATA는 비정상이니 조사.

### 수동 테스트·드릴로 울린 알람 구분

- `set-alarm-state`로 만든 ALARM은 이력의 StateReason에 수동 사유가 남는다. 드릴 중 통지는 기록지(`docs/postmortems/`)에 시각을 남겨 실장애와 구분.

### 알람 이력 (회고·MTTD 산출용)

```bash
aws cloudwatch describe-alarm-history --alarm-name <알람명> --history-item-type StateUpdate \
  --start-date <ISO8601> --end-date <ISO8601> --output json --region ap-northeast-2
# canary(§13 canary-down)는 us-east-1: 위 명령에서 --region us-east-1
```

### 통지가 안 올 때 (알람은 ALARM인데 Slack 침묵)

`infra/README.md` §모니터링 — ① 채널에 `@Amazon Q` 초대 ② SNS 구독 실존 ③ raw message delivery OFF 확인.

> 이 runbook은 GameDay(P3-2) 실측으로 검증·갱신된다. 실측과 다른 서술을 발견하면 회고 액션 아이템으로 수정한다.
