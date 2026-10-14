# Runbook — CloudWatch 알람 대응 (linkpulse-prod)

Slack에 알람 카드가 오면 **이 문서를 위에서부터** 따라간다. 1인 운영 전제 — 에스컬레이션 대상은 없고, 대신 모든 판단·시각을 기록해 회고로 넘긴다.

- 통지 경로: CloudWatch Alarm → SNS(`linkpulse-prod-alarms`) → Chatbot → Slack ([ADR 0002](../adr/0002-alerting-design.md)). **단 무트래픽 canary(§13)만 us-east-1 전용 토픽(`linkpulse-prod-canary-alarms`)을 거친다**([ADR 0004](../adr/0004-notraffic-canary.md))
- 알람 정의(임계값의 소스오브트루스): `infra/prod/monitoring.tf` — **임계값은 전부 초기값**, 오탐이 반복되면 §튜닝 노트를 보고 Terraform으로 조정(사람 apply)
- 표기: 명령은 read-only가 기본. 상태를 바꾸는 명령은 **[사람]** 표기 — 에이전트 자율 실행 금지(가드레일 #1)
- 로그 조회 쿼리 모음·Slack 배선 점검은 [`infra/README.md`](../../infra/README.md) 참고
- 🔔 **plan 0015부터 클러스터 안 Prometheus 알림이 같은 Slack 채널에 온다** — 경로가 다르다: Alertmanager → SNS(`linkpulse-prod-prom-alerts`, **SMS 없음**) → Chatbot → Slack. 카드 꼬리에 "Custom notification"이 붙고 제목이 `FIRING <알림 이름> · <등급> (<건수>)`다. 그 알림은 아래 [Prometheus 알림](#prometheus-알림-plan-0015--slack만) 절(P-번호)을 따른다. 사람을 깨우는 SMS는 여전히 이 문서의 CloudWatch 알람·canary만 보낸다([ADR 0008](../adr/0008-in-cluster-prometheus.md)).
- 🔁 **plan 0014부터 앱은 EKS에서 돈다**(ECS 없음). 명령은 `kubectl`이고, 배포는 ArgoCD가 한다(CI는 GHCR 푸시만). 처음 한 번 `aws eks update-kubeconfig --name linkpulse-prod-eks --region ap-northeast-2`. 설계 근거는 [ADR 0007](../adr/0007-ecs-to-eks.md).
- ✅ **2026-10-07부터 로테이션은 사람 개입 없이 회복된다** — Step 2(앱이 런타임에 비밀번호 재조회)가 라이브다. 온디맨드 회전 실측: 503 약 61초, 앱 자체 회복. **plan 0014에서 Step 1(회전 때 ECS 강제 재배포 Lambda)은 지웠다** — EKS에서의 확인은 측정 창의 회전 드릴이다. 회전 직후 알람의 처리는 아래 "비밀번호 로테이션 직후라면"을 따른다. 아래 ⛔ 줄은 Step 2 이전의 이력이다(다음 창은 브릿지 (a)로 재계산 — 10-07 기준 `NextRotationDate` 10-15 08:59:59 KST).
- ⛔ **예정된 RDS 비밀번호 로테이션(7일 주기)으로 세 번 죽었다 — 1차 2일 18시간 / 2차 24시간 23분 / 3차 4일 22시간 23분(2026-08-30 13:08:48 회전 → 09-04 11:31:42 복구). 3차가 1·2차를 합친 것보다 길다.** 알람을 기다리지 말고 [`secret-rotation-bridge.md`](secret-rotation-bridge.md)를 **선제적으로** 수행한다. ⚠️ **3차는 주간 13:08에 났고 탐지 경로도 apply돼 있었는데 5일 넘게 갔다 — 실패한 것은 탐지가 아니라 응답이다.** 그 런북의 **(f)**(알람 도달 실증·창 당일 능동 확인)를 (a)보다 먼저 읽는다. ⚠️ **로테이션은 시각이 아니라 약 24시간짜리 창이다** — **다음 창 = 2026-09-06 09:00 ~ 09-07 08:59:59 KST**(2026-09-04 실측). 실제 창은 브릿지 (a)로 매번 재계산한다

---

## 0. 공통 첫 5분 (어떤 알람이든 이 순서)

1. **시각 기록** — Slack 카드 수신 시각(초 단위). MTTD/회고의 기준점.
2. **사용자 영향 확인** — **두 엔드포인트를 함께 본다.** 하나만 보면 2026-08-16 로테이션 장애(66시간 무탐지)를 놓친 것과 같은 실수를 반복한다:
   ```bash
   curl -sk -o /dev/null -w 'healthz %{http_code}\n' --max-time 5 https://lpulse.live/healthz
   curl -sk -o /dev/null -w 'readyz  %{http_code}\n' --max-time 5 https://lpulse.live/readyz
   ```
   - `/healthz` = **liveness**. 프로세스가 살아 요청을 처리하는지만 본다 — **DB를 조회하지 않는다.**
   - `/readyz` = **readiness**. DB 핑까지 하고 실패하면 503이다. P4(d)-2부터 canary(§13)가 프로빙하는 경로다.
   - **둘 다 `200`** → 영향 없음/미미. 침착하게 해당 알람 절로.
   - **`healthz 200` + `readyz 503`** → 프로세스는 살아 있고 **DB 연결이 끊긴 것**이다. 오탐이 아니라 **실장애**다 → **§3(앱 5xx)의 "DB 연결 실패" 경로**로 간다. 2026-08-16 장애가 정확히 이 모양이었다.
   - **`healthz`도 `503`/timeout** → **다운. 먼저 `alb-elb-5xx`(§2)와 0-4의 파드 상태를 본다.** GameDay 실측(2026-07-06, ECS 시절): 전면 다운 시 **실제로 가장 먼저·일관되게 울린 건 `alb-elb-5xx`**(MTTD 3~5분)였고, `no-healthy-hosts`(§1)·`running-tasks-low`(지금은 없다)는 **늦거나(복구 후) 아예 침묵**했다. **무트래픽 다운(새벽 등 실사용 0)이어도 P4(c) canary가 `lpulse.live/readyz`를 상시 외부 프로빙(§13)**하므로, 그 synthetic 요청으로 `alb-elb-5xx`가 발화하고 canary 자신의 `canary_down`(§13, us-east-1)이 결정론적 백스톱이 된다. `no-healthy-hosts`(§1)는 **느린 최후 백스톱**으로만 남고 MTTD 신호로 신뢰하지 않는다(회고 A-5, [ADR 0004](../adr/0004-notraffic-canary.md)).
3. **비정상 알람 전수 확인** (동반 알람 조합이 원인을 말해준다). **두 리전 모두 본다** — 나머지 알람은 ap-northeast-2, 무트래픽 canary(§13)는 **us-east-1**에 있다(빠뜨리면 canary 다운을 놓친다):
   ```bash
   aws cloudwatch describe-alarms --alarm-name-prefix linkpulse-prod \
     --query 'MetricAlarms[?StateValue!=`OK`].[AlarmName,StateValue,StateReason]' --output table --region ap-northeast-2
   # canary(§13)는 us-east-1
   aws cloudwatch describe-alarms --alarm-name-prefix linkpulse-prod \
     --query 'MetricAlarms[?StateValue!=`OK`].[AlarmName,StateValue,StateReason]' --output table --region us-east-1
   ```
4. **클러스터가 스스로 말하는 것부터 듣는다** (파드 수·재시작·HPA·ArgoCD 상태·최근 이벤트):
   ```bash
   kubectl -n linkpulse get deploy,hpa,pods -o wide
   kubectl -n argocd get application linkpulse -o jsonpath='{.status.sync.status}/{.status.health.status} {.status.summary.images}{"\n"}'
   kubectl -n linkpulse get events --sort-by=.lastTimestamp | tail -20
   ```
   `READY`가 `n/n`이 아니거나 `RESTARTS`가 오르면 R-6. ArgoCD가 `OutOfSync`·`Degraded`면 그 이유가 곧 원인 후보다.
5. **맥락 자문**: 방금 배포가 있었나(Actions run → Image Updater가 새 다이제스트를 집었나)? 내가 인프라를 건드렸나(드릴·apply)? — 최근 변경이 있으면 그것이 제1 용의자.

### 공통 복구 도구 (아래 각 알람에서 R-번호로 참조)

| # | 도구 | 명령 |
| --- | --- | --- |
| R-1 | **[사람]** 롤백(이미지 고정) | 배포는 Image Updater가 `main` 다이제스트를 따라가며 한다. 롤백은 (1) Image Updater를 멈추고 (2) ArgoCD Application에 직전 이미지를 고정한다. 직전 이미지는 옛 ReplicaSet에 남아 있다(`kubectl -n linkpulse get rs -o custom-columns=CREATED:.metadata.creationTimestamp,IMAGE:.spec.template.spec.containers[0].image --sort-by=.metadata.creationTimestamp`) — `linkpulse:` 뒤(`main@sha256:…`)가 tag 값이다. 아래 ①②. git sha로 고정해도 된다(CI가 `<sha>` 태그도 민다) |
| R-1x | **[사람]** 롤백 해제 | 수정이 머지돼 GHCR `main`이 새 이미지가 된 뒤: `kubectl -n argocd patch application linkpulse --type json -p '[{"op":"remove","path":"/spec/source/helm/parameters"}]'` → `kubectl -n argocd scale deploy/argocd-image-updater-controller --replicas=1`. Image Updater가 현재 `main` 다이제스트를 다시 넣는다 |
| R-2 | **[사람]** ArgoCD 강제 동기화 | Deployment가 없거나 git과 다를 때. selfHeal이 보통 알아서 맞춘다. 바로 맞추려면 `kubectl -n argocd annotate application linkpulse argocd.argoproj.io/refresh=hard --overwrite` |
| R-3 | **[사람]** 파드 전체 재기동(이미지 그대로) | `kubectl -n linkpulse rollout restart deploy/linkpulse` — 한 번에 하나씩 바꾸고(surge 1) 새 파드가 ALB에서 healthy가 된 뒤 옛 파드를 뺀다. **배포 사고 진행 중엔 금지** — 지금 이미지가 문제 이미지일 수 있다(그 상황은 R-1) |
| R-4 | 로그 조회 | CloudWatch Logs Insights, 그룹 `/eks/linkpulse-prod/app`(Fluent Bit) — 쿼리 모음은 `infra/README.md`. 지금 막 뜨는 로그는 `kubectl -n linkpulse logs -l app.kubernetes.io/name=linkpulse --prefix --since=15m --tail=200` |
| R-5 | 타깃 헬스 확인 | `aws elbv2 describe-target-health --region ap-northeast-2 --target-group-arn $(aws elbv2 describe-target-groups --names linkpulse-prod-tg --query 'TargetGroups[0].TargetGroupArn' --output text --region ap-northeast-2)` — 타깃은 파드 IP다. 등록이 안 되면 LB 컨트롤러: `kubectl -n linkpulse describe targetgroupbinding linkpulse`, `kubectl -n kube-system logs deploy/aws-load-balancer-controller --tail=50` |
| R-6 | 죽은 파드의 사인(死因) | `kubectl -n linkpulse describe pod <pod>`의 `Last State`(Reason·Exit Code: `OOMKilled`/137=메모리, 1=앱 에러) → `kubectl -n linkpulse logs <pod> --previous`. 이미지 문제면 이벤트에 `ImagePullBackOff`(GHCR 패키지가 공개인지, 태그가 있는지) |

①: `kubectl -n argocd scale deploy/argocd-image-updater-controller --replicas=0` — 멈추지 않으면 다음 주기에 `main` 다이제스트로 다시 덮어쓴다.
②: `kubectl -n argocd patch application linkpulse --type merge -p '{"spec":{"source":{"helm":{"parameters":[{"name":"image.tag","value":"<위 tag 값 또는 git sha>"}]}}}}'` — ArgoCD가 곧바로 롤링한다. `kubectl -n linkpulse rollout status deploy/linkpulse`로 끝을 본다.
배포 전 기준선(B-1)으로 **지금 이미지(`kubectl -n linkpulse get deploy linkpulse -o jsonpath='{.spec.template.spec.containers[0].image}'`)를 기록해 두는 것**이 원칙이다.

**[참고 — 복구 도구 아님]** 백업에서 **격리된 별도 인스턴스**로 복원해 데이터가 조회되는지 확인하는 **검증 절차**는 [`rds-restore.md`](rds-restore.md)에 있다. **사고 복구·cutover 절차가 아니다** — 손상된 운영 데이터의 역복사, 쓰기 동결, 복원본으로의 전환, 서비스 헬스 확인, 롤백은 그 문서 범위 밖이며 **아직 구현되지 않았다.** 실제 데이터 손실 사고에서는 그 절차만으로 서비스가 복구되지 않는다는 것을 전제로 판단한다.

### 심각도

- **SEV-1 즉시 대응(사용자 영향·고위험)**: §1 no-healthy-hosts · §2 elb-5xx · §3 target-5xx · §10 rds-free-storage-low · **§13 canary-down(us-east-1, 무트래픽 방어선 — 단 첫 발화는 오탐 진위 확인 후)**
- §6·§7·§8(ECS CPU·메모리·실행 태스크 수)은 plan 0014에서 알람째 지웠다. 파드 CPU·파드 수 알람은 0015(Prometheus)에서 둔다. 그 사이 가용성은 ALB 알람과 canary가 본다.
- **SEV-2 당일 대응(성능·용량 경고)**: 나머지. 단 **여러 SEV-2가 동시에 오면 SEV-1로 취급**(복합 장애 신호).

---

## SEV-1

### §1 `linkpulse-prod-alb-no-healthy-hosts` — 정상 타깃 0 = 서비스 다운

- **의미**: 타깃그룹에 healthy 타깃이 3분간 없거나, **지표 자체가 소실**(타깃 전원 deregister — `treat_missing=breaching` 설계). 원래 무트래픽 다운의 방어선으로 설계했으나, **P4(c) canary 도입 후에는 느린 최후 백스톱으로 강등**됐다(무트래픽 1차 방어선은 canary/§13). **이 알람이 울리면 진짜 다운이지만, 그 역(다운이면 이 알람이 곧 울린다)은 실측상 성립하지 않았다** — GameDay(2026-07-06) 전면 다운 2회 + 무트래픽 canary 드릴(2026-07-13) 1회 = **3회 모두** 이 알람은 9분48초 만에(복구 후) 울리거나 아예 미발화했다. **즉 빠른 MTTD 신호로 신뢰하지 말 것** — 다운 감지는 `alb-elb-5xx`(§2)·canary(§13)가 먼저 한다. (근본원인은 P4(c)에서 **실측 확정** — [ADR 0004](../adr/0004-notraffic-canary.md) §A-5: desired=0 시 `HealthyHostCount`가 missing이 되어 지표가 소멸하는 **구조적 결함**이라 주기·임계 튜닝 무효, 백스톱 유지·비신뢰.)
- **먼저**: 공통 0-2(대개 503)·0-4. Deployment가 있는지, `READY` 숫자가 첫 갈림길.
- **원인 → 복구**:
  1. **Deployment가 없음/ArgoCD `Missing`·`OutOfSync`**: Application이나 사이트 값(ConfigMap·Secret)이 빠졌다 → **R-2**. Application 자체가 없으면 `infra/k8s` apply(사람).
  2. **전 파드 반복 크래시/기동 실패**: `CrashLoopBackOff`·`RESTARTS` 증가 → **R-6**으로 사유 확인 → 앱 문제면 §3과 동일 진단, 직전 이미지가 원인이면 **R-1**. 기동이 아예 안 되면 앱 로그의 첫 줄(설정 로드 실패·비밀번호 조회 실패 `secret_seed_failed`)을 본다.
  3. **`ImagePullBackOff`**: GHCR 패키지가 비공개로 바뀌었거나 태그가 없다 → 패키지 공개 확인, 아니면 **R-1**(있는 이미지로).
  4. **파드는 Ready인데 타깃이 없음/unhealthy**: TargetGroupBinding·LB 컨트롤러·SG 문제 → **R-5**(TGB 이벤트, 컨트롤러 로그, `Target.ResponseCodeMismatch` 등). 최근 `infra/prod`(SG)·`infra/k8s` apply가 있었는지 본다.
- **해소 확인**: healthy 회복(R-5) + healthz 연속 200 + `kubectl -n linkpulse get deploy linkpulse`의 `READY`가 `n/n`으로 **직접 판정**. **Slack OK 통지는 회복 기준으로 쓰지 말 것**(§2 실측: `alb-elb-5xx` OK가 복구 후 최대 17분 지연 — A-9), OK 수신 시각은 별도 병기만. **MTTR(첫 ALARM→정상) 기록.**
- **튜닝**: 오탐 여지 거의 없음(설계 의도). 유지.

### §2 `linkpulse-prod-alb-elb-5xx` — ALB 자신이 낸 5xx ≥5/5분

- **의미**: 요청이 타깃까지 못 갔다 — 503(정상 타깃 없음)·502(타깃 연결 거부/비정상 응답)·504(타임아웃). 사용자에게 오류가 **보이고 있다**. dimension이 LB 전용이라 타깃 5xx(§3)와 구분된다.
- **전면 다운의 1순위 신호(실측)**: GameDay 2회 모두 이 알람이 전면 다운을 **가장 먼저**(MTTD 3분37초·4분54초) 감지했다 — 503이 뜨면 §1보다 이 절을 먼저 본다. **P4(c) canary 도입 후에는 실사용 트래픽이 0이어도 canary의 synthetic 프로빙이 ALB 503을 만들어 이 알람이 발화**하므로 무트래픽에서도 1순위다(canary 자체 신호는 §13 `canary_down`). **단 이는 타깃이 전부 빠진 전면 다운에 한정된다** — P4(d)-2에서 canary 경로가 `/readyz`로 바뀌었고, DB 장애에서는 ALB 타깃그룹이 여전히 `/healthz`를 보고 healthy를 유지하므로 canary의 503은 **ALB가 아니라 앱이 낸 5xx**다 → 이 알람이 아니라 **`alb-target-5xx`(§3)** 가 1순위다. canary·외부 요청까지 전부 없는 극단에서만 이 알람이 침묵하고, 그때는 §1(느린 백스톱)이 최후 보루다.
- **⚠ OK 통지는 신뢰하지 말 것(회복 판정용 아님)**: 이 지표(`HTTPCode_ELB_5XX_Count`)는 **sparse 카운트**(5xx=0이면 데이터포인트 부재)라, 복구 후 missing→`notBreaching` 확정에 시간이 크게 걸린다 — 실측 OK 지연이 **복구 후 2분49초~17분26초**로 편차가 컸다. **회복 여부는 OK 알림을 기다리지 말고 `curl .../healthz`=200 + 0-4의 `READY` `n/n`으로 직접 확인**(MTTR 기준점). (회고 A-9)
- **먼저**: §1 동반 여부(0-3) — 동반이면 §1 절차가 곧 복구. 단독이면:
  - **502 위주**: 파드가 죽는 순간의 잔여 연결 가능성 → 0-4 events·R-6(방금 파드 교체가 있었나 — 롤링·HPA 축소), R-5. 롤링·축소마다 502가 섞이면 preStop sleep(차트)이 동작하는지 본다(`kubectl -n linkpulse get events | grep -i prestop`).
  - **504 위주**: 앱이 느리다 → §5(p95)와 §9(RDS CPU) 확인.
  - 어떤 코드인지는 CloudWatch 지표 `HTTPCode_ELB_502_Count`/`503`/`504`(콘솔) 또는 상황상 자명(다운=503).
- **복구**: 원인별 — 다운이면 §1, 파드 불안정이면 **R-3**(재기동으로 시간 벌기) 후 원인 추적, 배포 직후면 **R-1**.
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

- **의미**: 일부 타깃이 헬스체크(`/healthz` 200, 10s×unhealthy 3) 실패. **healthy가 남아 있으면 사용자 영향 없음** — §1과의 차이.
- **먼저**: 0-2로 영향 확인 → 0-4로 **롤링 중인지**(`kubectl -n linkpulse rollout status deploy/linkpulse`) → **R-5**로 어느 타깃·사유.
- **원인 → 복구**:
  1. **롤링 중 새 파드 unhealthy**: readiness gate가 새 파드의 ALB healthy를 기다리므로 옛 파드는 남아 있다(surge 1, unavailable 0). **자동 롤백은 없다** — 10분(`progressDeadlineSeconds`) 안에 안 되면 Deployment가 `Progressing=False`로 멈춘다. 새 이미지 문제면 **R-1**.
  2. **기존 파드 1개 이상**: R-6·R-4로 원인(OOM·행·panic) → kubelet 재시작(liveness)으로 자가치유되는지 확인, 반복이면 **R-3**.
- **해소 확인**: R-5 전부 healthy + Slack OK.
- **튜닝**: 정상 배포마다 울리면 소음 — evaluation_periods 상향 또는 "배포 창은 무시" 운영 규칙. GameDay S1의 P2가 이 알람의 실전 데이터 1호 — 실측 후 판단.

### §5 `linkpulse-prod-alb-latency-p95` — p95 ≥1s (1분×3)

- **의미**: 느려졌다(오류는 아님). 저샘플 왜곡은 `evaluate_low_sample_count_percentiles=ignore`로 이미 완화.
- **먼저**: **R-4** 느린 요청 쿼리로 경로 특정:
  ```
  fields @timestamp, path, status, duration_ms | filter duration_ms > 1000 | sort duration_ms desc | limit 50
  ```
  앱 포화인지는 `kubectl -n linkpulse top pods`·`kubectl -n linkpulse get hpa`(파드 CPU가 한도 250m에 붙고 HPA가 최대 6이면 앱 포화), §9/§12(RDS) 동반이면 DB 병목.
- **복구**: 원인별 — 특정 쿼리(인덱스·N+1)면 코드 수정, 전반 포화면 HPA 상한·파드 CPU(차트 values, PR → ArgoCD)를 올린다. 단 연결 예산(§12)을 같이 다시 계산한다. DB면 §9.
- **튜닝**: 실트래픽 관측 후 임계 재설정(ADR 0002 — 초기값).

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
- **먼저**: 산수 — 파드 수 × 풀 상한(`DB_MAX_OPEN_CONNS`, 차트 11)이 80을 넘는 구성인지. 설계값은 (HPA 최대 6 + 롤링 surge 1) × 11 = 77이다(차트 values.yaml). 아니면 누수/유휴:
  `SELECT state, count(*) FROM pg_stat_activity GROUP BY 1;` (idle 과다 = 반환 안 됨)
- **복구**: 앱 풀 상한 하향·유휴 타임아웃 설정 후 배포, 급하면 **R-3**(재기동으로 연결 리셋).
- **튜닝**: 파라미터그룹의 실제 max_connections 확인 후 임계 보정(ADR 0002 — 초기값 근사치).

### §14 `linkpulse-prod-clicks-oldest-age` — 클릭 큐의 가장 오래된 배치가 5분 넘게 대기

- **의미**: 워커가 클릭 배치를 반영하지 못하고 있다(plan 0013). 리다이렉트는 정상이지만 링크 통계의 클릭 수가 늘지 않는다.
  배치는 큐에 쌓여 있으므로 원인이 풀리면 워커가 알아서 반영한다. 중복은 앱이 배치 ID로 거른다.
- **먼저**: 앱 로그에서 어느 단계가 막혔는지 본다.
  ```bash
  aws logs tail /eks/linkpulse-prod/app --since 15m --region ap-northeast-2 | grep -E '"click queue"|클릭 (반영|수신|발행) 실패' | tail -20
  ```
  - `클릭 반영 실패`가 이어짐 → DB 문제다. 0-2의 `/readyz`를 보고 §3의 "DB 연결 실패" 경로로 간다.
  - `클릭 수신 실패`가 이어짐 → SQS에 닿지 못한다(파드 역할 `linkpulse-prod-pod-app`의 Pod Identity 연결, NAT, SQS 장애).
  - `click queue` 줄조차 없음 → 파드가 없거나 로그가 오지 않는다. 0-4로 파드를 보고, 파드가 있으면 Fluent Bit(`kubectl -n logging logs ds/fluent-bit --tail=50`)를 본다.
- **복구**: 원인만 고치면 된다. 같은 배치가 5번 실패하면(약 2.5분) DLQ로 옮겨진다 → §15.

### §15 `linkpulse-prod-clicks-dlq-depth` — 클릭 DLQ에 배치가 들어옴

- **의미**: 배치가 반영에 5번 실패했거나(약 2.5분 넘는 DB 장애) 본문이 깨졌다. **그 배치의 클릭은 아직 DB에 없다.**
- **먼저**: 몇 개인지와 이유를 본다. 로그의 `클릭 반영 실패`에 `reason=decode`가 있으면 깨진 본문이다 — 재처리해도 다시 DLQ로 온다.
  ```bash
  DLQ_URL=$(aws sqs get-queue-url --queue-name linkpulse-prod-clicks-dlq --region ap-northeast-2 --query QueueUrl --output text)
  aws sqs get-queue-attributes --queue-url "$DLQ_URL" --attribute-names ApproximateNumberOfMessages QueueArn --region ap-northeast-2
  ```
- **복구 [사람]**: 원인(DB 장애)이 풀렸는지 `/readyz` 200으로 확인한 뒤 원래 큐로 되돌린다. 목적지를 생략하면 원래 큐로 간다.
  ```bash
  DLQ_ARN=$(aws sqs get-queue-attributes --queue-url "$DLQ_URL" --attribute-names QueueArn --region ap-northeast-2 --query Attributes.QueueArn --output text)
  aws sqs start-message-move-task --source-arn "$DLQ_ARN" --region ap-northeast-2
  ```
  재처리된 메시지는 새 메시지(새 보존 시각)로 들어간다. 워커가 반영하면 §14 알람과 함께 DLQ 깊이가 0으로 돌아온다.
- ⚠️ **재처리한 배치가 다시 DLQ로 오면 원인부터 고친다.** 앱은 반영한 배치 ID를 21일 동안 기억한다(`click_batches`).
  같은 배치를 처음 보낸 뒤 21일이 지나 다시 넣으면 이미 반영된 배치를 두 번 셀 수 있다.

---

## SEV-1 (크로스리전 — us-east-1)

### §13 `linkpulse-prod-canary-down` — Route53 헬스체크 DOWN = 무트래픽 방어선 (us-east-1 알람)

무트래픽 다운의 **1차 방어선**이다(회고 A-5·A-10, [ADR 0004](../adr/0004-notraffic-canary.md)). Route53 헬스체크가 `lpulse.live/readyz`를 외부에서 상시 프로빙해, 실사용 트래픽이 0이어도 `HealthCheckStatus`(1=정상/0=다운)로 down을 잡는다. **이 알람·지표는 us-east-1에만 있다** — 조회는 전부 `--region us-east-1`.

**P4(d)-2에서 프로빙 경로가 `/healthz` → `/readyz`로 바뀌었다.** 그래서 이 알람은 이제 프로세스 다운뿐 아니라 **DB 연결 장애**도 잡는다. 2026-08-16 RDS 비밀번호 로테이션 장애 때 `/healthz`는 DB를 보지 않아 66시간 내내 200을 반환했고, canary와 ALB 타깃그룹이 **둘 다 `/healthz`** 를 보고 있어 어느 쪽도 발화하지 못했다. ✅ **2026-08-30 로테이션 장애에서 이 알람이 실제로 울렸다 — 설계값이 실측으로 확인됐다**(`describe-alarm-history`, 2026-09-04):

| 알람 | ALARM 진입 | 회전(13:08:48) 이후 | 전이 |
| --- | --- | --- | --- |
| `alb-target-5xx`(§3) | 08-30 13:15:37 | **+6분 49초** | ALARM 1 · OK 1 |
| `canary_down`(이 절) | 08-30 13:17:04 | **+8분 16초** | ALARM 1 · OK 1 |

  plan 0008의 목표(*"전면 실패 시 기대값 약 6분 · 상한 8분"*)에 **부합한다.** 그리고 **플래핑이 0회**다 — 2차 때 19회 ALARM↔OK 왕복에 OK 8건이 무트래픽 오탐이었던 것을 `/readyz` + canary 전환이 없앴고, **118시간 내내 ALARM을 끊김 없이 유지**했다. 복구는 09-04 11:26:01 재배포 → **11:31:42 OK**(5분 41초). **plan 0010 탐지 드릴이 사려던 실측을 실장애가 대신 줬다.**

⛔ **그런데 이 알람이 정확히 울렸는데도 다운이 4일 22시간 갔다.** 탐지가 아니라 **알람과 사람 사이**가 끊겼다. `alb-target-5xx`(§3)를 함께 감시하는 것은 그대로 유효하지만, **감시 대상이 하나 더 있다 — 알람이 폰까지 닿는지**(브릿지 런북 (f-2)).

- **⚠ 최초 발행 레이스**: 헬스체크 생성 직후 지표 최초 발행 전 공백이 `breaching`으로 잡혀 **서비스는 정상인데 down 카드가 1회** 뜰 수 있다. **첫 canary ALARM은 아래 진위 확인 전까지 실장애로 단정하지 않는다.**
- **먼저(진위 확인)**: §0-2의 **두 curl을 함께** 본다. **`/healthz` 200만으로 오탐이라고 판단하지 않는다** — DB 장애에서는 `healthz 200` + `readyz 503`이 **정상적인 실장애 신호**다(2026-08-16 장애를 66시간 놓친 오판이 정확히 이것이었다).
  - `healthz 200` + `readyz 200`(둘 다 정상)일 때만 오탐을 의심하고, 헬스체크 상태·지표 발행을 확인한다:
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
- **대응 분기(§0-2 curl 결과로 갈린다)**:
  1. **`healthz` 503/timeout** → **전면 다운.** `alb-elb-5xx`(§2)도 곧/이미 발화한다 → **§2/§1** 경로로 복구(**R-1**, Deployment가 없으면 **R-2**).
  2. **`healthz 200` + `readyz 503`** → **DB 연결 장애.** 이때 ALB 타깃은 `/healthz`로 healthy를 유지하므로 **`alb-elb-5xx`는 울리지 않고 `alb-target-5xx`(§3)가 울린다** → 진단은 **§3의 "DB 연결 실패/고갈"** 경로(로그의 connection 계열 에러 → §12 → RDS 상태). **먼저 RDS 유지보수·재부팅 여부**를 본다 — 아래 **세 가지를 함께** 봐야 한다. `describe-events`는 *최근 생성된 이벤트*를 돌려줄 뿐 **지금 유지보수 중이라는 증거가 아니다.**
     ```bash
     R="--region ap-northeast-2"; ID=linkpulse-prod-pg
     aws rds describe-events --source-identifier $ID --source-type db-instance --duration 120 $R  # 단위: 분(최근 2시간)
     aws rds describe-db-instances --db-instance-identifier $ID --query 'DBInstances[0].DBInstanceStatus' $R
     aws rds describe-pending-maintenance-actions --filters Name=db-instance-id,Values=$ID $R  # 필터 없으면 계정 전체가 나온다
     ```
     **`DBInstanceStatus` 값으로 갈린다 — "`available`이 아니다"만으로 유지보수라고 보면 안 된다.** RDS의 비정상 상태에는 즉시 대응해야 할 것이 섞여 있다([AWS DB 인스턴스 상태 표](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/accessing-monitoring.html)).
     - **`maintenance`·`rebooting`·`modifying`·`upgrading`** *이면서* `describe-events`나 pending action이 **이 DB를 대상으로 같은 시각대에** 걸려 있을 때만 유지보수/재부팅으로 본다. 그때는 관찰하되 **상한 15분**을 둔다 — 15분 안에 `available` + `/readyz` 200으로 돌아오지 않으면 유지보수 분류를 버리고 아래 로테이션 확인과 §3 진단을 진행한다. (별개 장애가 최근 유지보수 이벤트와 겹치면 무기한 기다리게 된다.)
     - **`storage-full`·`failed`·`incompatible-*`·`inaccessible-encryption-credentials`** → **기다리지 않는다.** 관찰 없이 곧바로 §3 진단으로 간다(AWS는 `storage-full`을 즉시 조치 대상으로 명시한다).
     - **그 밖의 값(`available` 포함)** → 유지보수가 아니다. 아래 로테이션 확인과 §3 진단을 진행한다.

     **비밀번호 로테이션 직후라면** → plan 0014부터 회복은 Step 2(앱이 인증 실패 때 비밀번호를 다시 읽음) 하나뿐이다. Step 1(자동 재배포 Lambda)은 없다.

     ✅ **회전 직후 약 1분의 5xx는 정상이다.** 실측(2-6 온디맨드 회전, ECS): 503 **약 61초**, 대부분 회전 Lambda 내부 구간(DB 비밀번호는 바뀌었고 새 값은 아직 비공개)이라 앱이 줄일 수 없다. 앱은 새 값이 보인 직후 0~3초에 스스로 회복했다. **기준은 건수가 아니라 지속 시간과 `/readyz` 회복 여부다.**
     1. `/readyz`를 본다. **2분 안에 200이면 끝**이다. 앱 로그에서 `auth_failed_observed` → `secret_refreshed` → `credential_recovered` 순서를 확인해 둔다(R-4).
     2. 2분이 넘어도 503이면 **[사람] R-3**(`kubectl -n linkpulse rollout restart deploy/linkpulse`) — 새 파드는 기동 때 Secrets Manager에서 현재 값을 읽는다(Step 1이 하던 일을 손으로 한다). 회전 시각은 광역 관찰 로그 `/aws/events/linkpulse-prod-secret-events`에서 본다.
     3. R-3 뒤에도 안 되면 `secret_refresh_failed`·`secret_seed_failed`의 `error`(AccessDenied면 Pod Identity 연결·파드 역할)를 본다. 회고에 남기고, Step 1을 EKS용으로 다시 만드는 plan을 연다(plan 0014 리스크 표).
     ⚠️ **`alb-target-5xx`는 오류가 멎은 뒤에도 약 15분 ALARM에 머문다**(실측 01:49:38 ALARM → 02:05:38 OK, 503은 01:47:20에 끝남). 오류가 계속되는 것이 아니다 — 이 알람은 평가 기간이 1(5분)이라, CloudWatch가 더 넓은 평가 범위에 남은 마지막 실제 값(503이 담긴 5분 구간)으로 계속 판정하고 이때 `notBreaching`은 무시된다([missing data 평가 규칙](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/alarms-and-missing-data.html#alarms-evaluating-missing-data)). **ALARM 지속 시간으로 장애 여부를 판단하지 않는다 — `/readyz`로 판단한다.**

  3. **둘 다 200인데 알람이 지속** → 오탐 후보. 최초 발행 레이스(위)이거나, `/readyz` 응답이 Route53의 **2초 제한**에 걸린 경우다. 후자는 외부에서 `curl -w '%{time_connect} %{time_starttransfer}'`로 `/readyz`를 여러 번 재어 **`time_starttransfer - time_connect`** 가 2초에 얼마나 가까운지 확인한다(운영자 위치 지연이라 참고값).
- **역할 분리(MTTD) — 장애 종류에 따라 1순위가 다르다**:
  - **전면 다운**(프로세스·파드가 없음): `alb-elb-5xx`(canary 트래픽 기반 실측 3분37초·4분54초)=빠른 1차, `canary_down`(≈flip 90s + 알람 3분, 실측 5분47초)=결정론적 백스톱. 확인 순서는 여전히 **§2 우선**.
  - **DB 장애**(프로세스는 살아 있음): 타깃이 `/healthz`로 healthy를 유지해 ALB가 503을 만들지 않으므로 **`alb-elb-5xx`는 울리지 않는다.** canary의 503이 앱 5xx로 집계돼 **`alb-target-5xx`(§3)가 1순위**이고, `canary_down`이 백스톱이다. 확인 순서는 **§3 우선**.
  - 두 경우 모두 `no-healthy-hosts`(§1, 9분48초/무발화)보다 빠르다.
  - **MTTD 실측값: plan 0010 실측 후 기입.** 위 숫자는 P4(c) 전면 다운 드릴 실측이며, DB 장애 경로는 아직 실측되지 않았다.
- **통지가 안 오면**: 이 알람은 **us-east-1 전용 토픽**(`sns_canary_topic_arn`)→Chatbot 경로다. `aws sns list-subscriptions-by-topic --topic-arn "$(terraform output -raw sns_canary_topic_arn)" --region us-east-1`로 구독 실존 확인(`infra/README.md` §모니터링).

---

## Prometheus 알림 (plan 0015 — Slack만)

규칙 정의(소스오브트루스)는 앱 차트 `deploy/helm/linkpulse/files/rules.yaml`(linkpulse 규칙)과 kube-prometheus-stack 기본 규칙이다. 수신처·라우팅은 `infra/k8s/values/kube-prometheus-stack.yaml.tftpl`. **임계값은 전부 초기값이다.**
이 알림들은 클러스터 안에서 평가된다 — **클러스터나 Prometheus가 죽으면 조용해진다.** 가용성은 그때도 클러스터 밖 알람(§1·§2·§13)이 본다.

**보는 법**(읽기 전용, 인터넷에 열지 않았다). 각각 다른 터미널 탭에서 띄우고 Ctrl+C로 닫는다.

```bash
kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090
kubectl -n monitoring port-forward svc/kube-prometheus-stack-alertmanager 9093:9093
kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80
```

- Prometheus http://localhost:9090/alerts(규칙·발화 상태), Alertmanager http://localhost:9093(묶인 알림·침묵), Grafana http://localhost:3000/d/linkpulse-overview(익명 Viewer — 로그인 없이 본다).
- 지금 울리는 것: `curl -s localhost:9090/api/v1/alerts | jq -r '.data.alerts[] | select(.state=="firing") | .labels.alertname' | sort | uniq -c`
- 잠깐 조용히 시키기(배포·드릴 중) **[사람]**: Alertmanager 화면의 Silence. 규칙을 영구히 끄는 것은 코드로 한다(아래 "시끄러운 기본 규칙").

### P-0 알림 경로 시험 / Slack에 안 올 때

- **시험**(측정 창 7-2) **[사람]**: 늘 울리는 시험 규칙을 넣고, Slack에 `LinkpulseTestAlert` 카드가 오면 지운다. 규칙 평가(30초) + Alertmanager 대기(30초)라 보통 2분 안에 온다.
  ```bash
  kubectl apply -f infra/k8s/manifests/test-alert-rule.yaml
  kubectl delete -f infra/k8s/manifests/test-alert-rule.yaml
  ```
  `amtool alert add`로 Alertmanager에 직접 넣는 시험은 규칙 평가를 거치지 않아 쓰지 않는다.
- **안 올 때** 순서: ① Prometheus `/alerts`에 그 알림이 Firing인가(아니면 규칙 문제 — `/api/v1/rules`에서 linkpulse 그룹 `health`가 `ok`인지, 없으면 선택자 계약이 빠졌다) → ② Alertmanager 화면에 들어왔는가 → ③ Alertmanager 로그의 SNS 오류:
  ```bash
  kubectl -n monitoring logs alertmanager-kube-prometheus-stack-alertmanager-0 -c alertmanager --tail=100 | grep -iE 'sns|notify|error'
  ```
  `AccessDenied`·자격증명 오류면 Pod Identity 연결(`infra/prod` iam.tf `pod_identities.alertmanager`, 서비스 어카운트 `monitoring/alertmanager`)을 본다. 연결은 파드가 뜰 때 붙으므로 연결을 고친 뒤에는 파드를 다시 띄운다 **[사람]** (`kubectl -n monitoring delete pod alertmanager-kube-prometheus-stack-alertmanager-0`).
  → ④ 발행은 되는데 Slack이 조용하면 Chatbot 쪽이다: 채널 설정의 토픽에 `prom-alerts`가 있는지(`infra/prod` chatbot.tf), "통지가 안 올 때"(부록)를 본다. 메시지 형식이 틀리면 Chatbot이 조용히 버린다 — 형식은 `scripts/check-charts.sh`가 사용자 지정 알림 JSON인지 검사한다.

### P-1 SLO 소진 — `LinkpulseAvailability*Burn`·`LinkpulseLatency*Burn`

- **의미**: 리다이렉트·링크 조회의 5xx 비율(가용성 99.5%) 또는 100ms 초과 비율(지연 99%)이 버짓을 빠르게(14.4배, critical) 또는 느리게(6배, warning) 쓰고 있다. 1시간 요청률이 초당 0.2 미만이면 울리지 않는다.
- **첫 조치**: 0의 "공통 첫 5분"으로 사용자 영향부터 본다. 그다음 Grafana 대시보드에서
  - 가용성: "경로별 5xx 비율"이 어느 경로인가 → 앱 5xx라 CloudWatch §3(앱이 낸 5xx)의 원인 분기(DB 연결 실패·회전 직후 등)를 그대로 따른다.
  - 지연: "p95·p99", "DB 풀 대기", "파드 CPU·스로틀", "HPA 파드 수"를 같은 시간대로 본다. HPA가 최대(6)에 붙어 있으면 `KubeHpaMaxedOut`도 같이 울린다 — 용량 문제다(측정 결과 `load/k6/results/`의 무릎과 비교).
- ALB가 만든 502·504는 이 SLI에 없다(§2가 본다).

### P-2 클릭 파이프라인 — `LinkpulseClicksDropped`(critical)·`LinkpulseClickPublishErrors`·`LinkpulseClickApplyErrors`

- **유실(Dropped)**: 파드가 큐로 못 보낸 코드가 상한(10만)까지 쌓여 새 클릭을 버리기 시작했다. 대개 SQS 발행이 오래 실패한 끝이다 — 발행 실패 알림이 먼저 와 있어야 한다.
- **발행 실패 5분 지속**: 파드 로그의 이유를 본다.
  ```bash
  kubectl -n linkpulse logs -l app.kubernetes.io/name=linkpulse --prefix --since=15m | grep -E '클릭 발행 실패' | tail -5
  ```
  권한(`AccessDenied` → Pod Identity·`infra/prod` iam.tf `pod_app`), 네트워크(NAT·SQS 엔드포인트), 큐 삭제 여부를 차례로 본다. 큐 쪽 적체는 §14(`clicks-oldest-age`)가 같이 본다.
- **반영 실패**: 워커가 DB에 못 더했다. 메시지는 큐에 남아 다시 오고, 계속 실패하면 DLQ로 간다(§15). DB 상태(§3의 DB 분기, §12 연결 수)를 본다.

### P-3 관리자 인증 실패 — `LinkpulseAdminAuthFailures`

- **의미**: 관리자 경로 401이 10분에 20건을 넘었다. 본인이 비밀번호를 잘못 넣은 것이 아니면 추측 시도다.
- **첫 조치**: 출처와 경로를 본다(IP는 기록·대화에 붙여 넣지 않는다).
  ```bash
  kubectl -n linkpulse logs -l app.kubernetes.io/name=linkpulse --since=15m | grep '"status":401' | jq -r '.path' | sort | uniq -c
  ```
  쓰기 경로는 레이트리밋(분당 20)이 이미 막는다. 계속되면 관리자 비밀번호를 바꾼다 **[사람]**: 새 해시를 `infra/k8s/terraform.tfvars`에 넣고 `infra/k8s` apply → `kubectl -n linkpulse rollout restart deploy/linkpulse`(해시 만드는 법은 `infra/k8s/terraform.tfvars.example`).

### P-4 배포 — `LinkpulseArgoCDAppUnhealthy`

- **의미**: ArgoCD Application이 15분 넘게 `Synced`·`Healthy`가 아니다. 사용자 영향은 대개 없다 — 새 파드가 healthy가 되지 않으면 롤링이 멈추고 옛 파드가 남는다.
- **첫 조치**:
  ```bash
  kubectl -n argocd get application linkpulse -o jsonpath='{.status.sync.status}/{.status.health.status}{"\n"}{range .status.conditions[*]}{.type}: {.message}{"\n"}{end}'
  kubectl -n linkpulse get pods; kubectl -n linkpulse get events --sort-by=.lastTimestamp | tail -15
  ```
  새 이미지가 뜨지 않으면(ImagePullBackOff·CrashLoop) R-1로 이전 이미지에 고정한다. 차트 렌더 오류면 `scripts/check-charts.sh`가 main에서 통과하는지 본다.

### 기본 규칙 (kube-prometheus-stack)

- `KubePodCrashLooping`: 파드가 재시작을 반복한다 → `kubectl -n <ns> describe pod <파드>`·`logs --previous`. 앱이면 0-4.
- `KubeHpaMaxedOut`: HPA가 최대(6)에 15분 넘게 붙어 있다 → 용량 한계. 측정 중이면 정상이다.
- `TargetDown`: Prometheus가 긁지 못하는 대상이 있다 → `localhost:9090/targets`에서 어느 job인지 본다. 앱이면 9090 포트·ServiceMonitor, 그 밖은 해당 구성 요소.
- 그 밖(노드 디스크·메모리, Prometheus 자체 등)은 알림의 `runbook_url`(upstream 런북)을 따른다.
- **시끄러운 기본 규칙**: 측정 창 첫 30분에 본 노이즈는 결과 문서에 이름을 남기고 끈다 — `infra/k8s/values/kube-prometheus-stack.yaml.tftpl`의 `defaultRules.disabled.<알림 이름>: true` → `infra/k8s` apply **[사람]**.

---

## 부록

### INSUFFICIENT_DATA 일반 해석

- RDS 계열(재부팅 공백)에서 정상적으로 나타날 수 있음(`treat_missing=missing` 설계). **§1·§10은 breaching이라 데이터 소실도 (결국) ALARM** — 이 둘의 INSUFFICIENT_DATA는 비정상이니 조사. **단 §1(`no-healthy-hosts`)은 지표 소실 시 발화가 느리거나 안 나기도 한다(실측 9분48초/미발화, [ADR 0004](../adr/0004-notraffic-canary.md) §A-5) — MTTD 신호로 신뢰 말 것, 무트래픽 감지는 canary(§13)가 담당.**

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

### EKS 운영 (plan 0014)

- **ArgoCD 보기**: 인터넷에 열지 않았다. 상태는 0-4의 `kubectl`로 충분하다. 화면이 필요하면 `kubectl -n argocd port-forward svc/argocd-server 8443:443` → https://localhost:8443, 사용자 `admin`. 초기 비밀번호는 `kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d` — **그 출력은 대화·문서에 붙여 넣지 않는다**(클러스터를 지우면 바뀌는 일회용이지만 원칙은 같다).
- **배포가 반영되지 않을 때**: GHCR에 새 `main`이 올라갔는지(Actions `deploy`) → Image Updater 로그(`kubectl -n argocd logs deploy/argocd-image-updater-controller --tail=50`) → Application의 `image.tag` 파라미터(`kubectl -n argocd get application linkpulse -o jsonpath='{.spec.source.helm.parameters}'`) 순서로 본다. Image Updater를 0개로 줄여 둔 채(R-1)라면 반영되지 않는 게 정상이다.
- **사이트 값을 바꿀 때**(관리자 해시, 레이트리밋 예외 등): `infra/k8s` apply 뒤 `kubectl -n linkpulse rollout restart deploy/linkpulse` — envFrom은 ConfigMap·Secret 변경을 다시 읽지 않는다. 관리자 해시는 `infra/k8s/terraform.tfvars`에 있다(plan 0014 전에는 `infra/prod/terraform.tfvars`).
- **생성·삭제 순서**: 생성은 `infra/prod` → `infra/k8s`(kube-prometheus-stack → 컨트롤러 → site(ConfigMap·Secret·TGB) → gitops(Application·ImageUpdater)). 삭제는 역순이고, **Application과 TargetGroupBinding을 각 컨트롤러가 살아 있을 때 먼저 지운다**(finalizer). Prometheus·Alertmanager CR에 finalizer가 있으면 그것도 operator가 살아 있을 때 지운다(plan 0015). `scripts/full-apply-prod.sh`·`full-destroy-prod.sh`가 이 순서대로 한다.
- **finalizer에 걸려 삭제가 멈췄을 때**(컨트롤러가 먼저 사라짐 — `kubectl get ... ` 결과에 `deletionTimestamp`가 있는데 안 지워짐): 컨트롤러가 다시 살 수 없다면 finalizer를 손으로 뗀다 **[사람]**. 그 경우 컨트롤러가 하던 정리(ArgoCD는 앱 리소스 삭제, LB 컨트롤러는 타깃 등록 해제)가 일어나지 않는다 — 타깃 그룹은 `infra/prod` destroy가 통째로 지우니 남는 것은 없다.
  ```bash
  kubectl -n argocd patch application linkpulse --type merge -p '{"metadata":{"finalizers":null}}'
  kubectl -n linkpulse patch targetgroupbinding linkpulse --type merge -p '{"metadata":{"finalizers":null}}'
  kubectl -n monitoring patch prometheus kube-prometheus-stack-prometheus --type merge -p '{"metadata":{"finalizers":null}}'
  kubectl -n monitoring patch alertmanager kube-prometheus-stack-alertmanager --type merge -p '{"metadata":{"finalizers":null}}'
  ```

> 이 runbook은 GameDay(P3-2) 실측으로 검증·갱신된다. 실측과 다른 서술을 발견하면 회고 액션 아이템으로 수정한다.
