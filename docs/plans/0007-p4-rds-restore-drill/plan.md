---
status: approved # in-review | approved
revision: 12
created: 2026-07-13
---

# 0007. P4(d)-1 RDS 백업+복원 리허설

## 목표

이 plan이 끝나면:

1. **복원 절차가 문서로 존재한다** — `docs/runbooks/rds-restore.md`에 PITR(시점 복원)·스냅샷 복원, 비공개 RDS 검증 접속, 측정, 비용 정리까지 온콜이 그대로 따라 할 수 있는 절차가 있다. **이번 드릴에서 실제 실행하는 것은 PITR 하나**이고, 스냅샷 복원은 **실행하지 않되 실행 가능한 수준으로 기술**한다(§스냅샷 복원 절이 담을 항목).
2. **백업이 실제로 복원 가능함을 실증한다** — 사람이 운영 백업에서 **별도 인스턴스로 실제 복원**해, 인스턴스가 available이 되고 **데이터가 쿼리 가능**함을 확인한다("복원해 보지 않은 백업은 백업이 아니다").
3. **DB 복원 검증 소요시간과 복구 지점을 실측한다(r11 라벨 정정 — 아래 §측정 정의)** — `T0`(복원 API 호출) → `T_available` → `T_creds_ready` → `T_verified` 네 시각을 기록해 **DB 복원 검증 소요시간 = T_verified − T0**을 산출하고, **복구 지점 지연 = T0 − 실제 복원 시각(restore point)** 을 RPO의 **1차 지표**로 기록한다. **이 값을 서비스 RTO로 부르지 않는다** — 복원본으로의 cutover·애플리케이션 헬스 확인·데이터 역복사가 이 plan의 범위 밖이라 끝점이 측정되지 않기 때문이다. 행 수준 손실 창은 **sentinel-A(복원 지점 이전 생성 → 존재해야 함)** 와 **sentinel-B(복원 지점 확정 후·T0 이전 생성 → 부재해야 함)** 로 **양쪽을 브래킷**해 실증한다(§측정 정의).
4. **운영 서비스·RDS 설정 무변경 + 드릴 리소스 전량 정리(r4 정정 — "인프라 무변경"은 과장이었다)** — 드릴은 운영 인스턴스(`linkpulse-prod-pg`)의 **RDS 설정을 바꾸지 않고 기존 행도 변경·삭제하지 않으며**, 운영 ECS 서비스·ALB·IaC(`.tf`)를 건드리지 않는다. 다만 드릴은 **운영 계정과 운영 클러스터(`linkpulse-prod-cluster`)에 부수 리소스를 만든다** — 임시 태스크정의 2개, 임시 IAM role, **1회성 태스크 실행 최대 5회**(재시도 포함 — TD-A ≤2 · TD-B ≤3), 운영 로그그룹의 드릴 로그 스트림, sentinel 링크 행. **이 열거가 허용의 전부**이며(§운영 가드레일의 표), 로그 스트림·sentinel 행을 뺀 전부는 정리 게이트에서 **종결 상태까지 확인**해 과금과 권한을 끊는다.

성공 판정은 **네 축을 각각 PASS/FAIL로 따로 기록**한다(r4 정정 — 한 덩어리로 묶으면 구성이 적용되지 않은 드릴을 성공으로 오분류한다):

| 축 | PASS 조건 |
| -- | --------- |
| **(a) 구성 동등성** | 복원본이 available이고 `describe`상 **비공개·암호화·기대 SG/서브넷 그룹·엔진/클래스** 일치 **∧ 붙은 파라미터그룹 이름 = 운영 그룹 ∧ 그 적용이 `in-sync`**(이름 없이 `in-sync`만 보면 **기본 그룹도 통과**한다 — r12) ∧ 복원본에서 조회한 **`rds.force_ssl` 실효값 = `on`** |
| **(b) 데이터 복원** | **sentinel-A 전건 존재 ∧ sentinel-B 부재 ∧ `count(*)`·`sum(clicks)` ≥ baseline** |
| **(c) 측정** | DB 복원 검증 소요시간·복구 지점 지연이 숫자로 기록됨 |
| **(d) 정리** | **§정리 게이트 「잔존 목록 2분류」의 (A) 조치 필요 잔존 = 0.** 분류 정의는 **그 절이 단일 소스**이고 이 표는 참조만 한다(r7 정정 — 같은 판정을 축 표·부수효과 표·잔존 목록 세 곳에 따로 서술했다가 (A)만 좁게 남아 **미완 정리가 PASS로 출력**될 수 있었다). 요약: 드릴 DB **삭제 waiter 통과** ∧ 임시 role **부재** ∧ **드릴 태스크 전건이 (가) 종결로 확정**(`STOPPED`·`DELETED` 관측, 또는 **종결 관측 이력이 있는** MISSING) ∧ **TD revision 전건이 §TD 종결 2단계의 즉시 게이트를 통과**(판정은 그 절이 단일 소스 — 요청 단위 "호출 성공"이 아니라 **revision별 삭제 응답** 기준. r10) ∧ 드릴 시크릿 **부재 또는 삭제 예약** |

**전체 verdict = 넷 전부 PASS.** (a)가 FAIL이면 (b)가 PASS여도 **드릴을 성공으로 기록하지 않는다** — "운영과 동일한 구성으로 복원된다"가 미검증이기 때문이다. 파라미터그룹 축의 FAIL 사유는 둘이고 성격이 다르다(r12 정정): **① 이름 불일치** = 애초에 운영 그룹이 붙지 않았다(= `--db-parameter-group-name` 누락·오타) **② 이름은 맞으나 `in-sync`가 아님** = 운영 그룹의 파라미터가 적용됐다는 증거가 없다. ②에서는 server-side `rds.force_ssl` 적용도 입증되지 않는다(클라이언트 `sslmode=require`는 "이 연결이 TLS였다"의 증거일 뿐 "서버가 비TLS를 거부한다"의 증거가 아니다). 어느 쪽이든 **검증은 계속하고**(§실행 단계 (e)) **"데이터 복원 PASS / 구성 동등성 FAIL"의 부분 성공**으로 ADR에 기록한 뒤 후속 항목을 남긴다.

## 배경/제약

### 왜 이 작업인가 (DR 태세 — 리뷰어 핵심 검토 대상)

현재 RDS 태세(`rds.tf`·`variables.tf` 실측): **Single-AZ**(자동 failover 없음) + **자동 백업 7일 보관**(`db_backup_retention=7` → PITR 창 7일) + **RDS 관리 마스터 비밀번호**(`manage_master_user_password=true`, Secrets Manager). 백업은 **켜져 있지만 한 번도 복원해 본 적이 없다** — 실제 장애 때 복원이 되는지, 얼마나 걸리는지, 데이터 손실이 얼마인지 모른다. 이 plan은 그 사각지대를 **비파괴 드릴**(운영과 분리된 별도 인스턴스로 복원 → 검증 → 삭제)로 메운다.

**범위 밖(명시적 제외 — 향후/별도):** Multi-AZ 전환(비용 2배, RTO를 자동 failover로 단축 — P5+ 업그레이드), 크로스리전 백업 복사(리전 장애 DR), 백업 보관 연장. 이번은 **"현재 백업으로 복원이 되는가"의 리허설**만 다룬다. P4(d)의 나머지 두 항목(Secrets 로테이션·IAM 최소권한)은 각각 별도 plan(0008·0009).

### 복원 방식 (두 가지, PITR이 주)

- **주: PITR(point-in-time restore)** — `restore-db-instance-to-point-in-time`로 **새 인스턴스**를 만든다. 자동 백업 + 트랜잭션 로그 체인을 검증하는, "장애 직전으로 되돌리는" 실제 DR 능력.
  - **복원 시각은 `--restore-time`으로 명시**(기본안). `--use-latest-restorable-time`은 편하지만 **복원 지점이 사후에만 확정**돼 baseline과의 대조 기준이 흐려진다. 드릴은 ① sentinel-A 생성 → baseline 캡처(`T_baseline`) → ② 원본 `LatestRestorableTime`이 `T_baseline`을 **여유 있게 지날 때까지 폴링**(로그 업로드 주기만큼 대기, 이 대기는 **측정 구간 밖**의 계측 단계 — `T0` 이전이다) → ③ **복원 지점 확정** → ④ sentinel-B 생성 → ⑤ 복원(T0). 이러면 "복원본 ⊇ baseline"이 **논리적으로 성립해야 하는 명제**가 되어 대조가 판정 가능해진다.
  - **복원 지점은 "관측값 그 자체"가 아니다(r3 정정).** CLI 도움말 Constraints는 `Must be **before** the latest restorable time`(strictly before, 로컬 help 129행 실측)이다. 폴링 직후 관측값을 그대로 넣으면 요청 시점의 `LatestRestorableTime`이 아직 같은 값이라 **경계에서 `InvalidParameterValue`로 튕길 수 있다** — 그것도 baseline 캡처를 이미 끝낸, 가장 되돌리기 번거로운 지점에서. 따라서 **`T_baseline + 60초 ≤ restore point < 현재 LatestRestorableTime`을 만족하는 값**(예: 관측값에서 수십 초 뺀 시각)으로 정의하고, 경계가 `<`인지 `≤`인지는 **사람이 실행 전 확인**한다(§가드레일 1차 출처 규율). **하한의 60초 마진은 DB 시계와 RDS 제어면 시계가 다르기 때문**이다 — 근거는 §측정 정의의 교차 시계 (i)(r6 정합: r5에서 이 절만 옛 규칙 `T_baseline < restore point`로 남아 plan 안에 규칙이 두 개 있었다).
- **보조: 스냅샷 복원** — `restore-db-instance-from-db-snapshot`로 최근 자동/수동 스냅샷에서 복원. **이번 드릴에서 실행하지는 않지만** 런북 절은 온콜이 그대로 따라 할 수 있어야 한다(goal #1). PITR과 **공통인 부분은 참조로 처리**(구성 게이트·자격증명 전환·검증 접속·정리 게이트가 전부 동일)하고, **다른 것만 기술**한다:
  - 스냅샷 **식별자 선정**(`describe-db-snapshots`로 `linkpulse-prod-pg`의 자동/수동 스냅샷 중 대상 선택, `Status=available` 확인).
  - **복구 지점 = `SnapshotCreateTime`** (PITR의 `restore point` 대응) → 복구 지점 지연 = `T0 − SnapshotCreateTime`. 스냅샷은 시점을 고를 수 없으므로 지연이 PITR보다 크게 나오는 것이 정상.
  - **baseline/sentinel 판정 조건이 뒤집힌다**: 스냅샷이 baseline보다 **앞서면** `count ≥ baseline` 부등식이 성립하지 않는다 → 대조 기준을 "`SnapshotCreateTime` 이전에 심은 sentinel만 존재해야 함"으로 바꾸거나, baseline을 스냅샷 시각 기준으로 재정의한다.
  - 복원 명령 옵션(식별자·`--db-subnet-group-name`·`--vpc-security-group-ids`·`--db-parameter-group-name`·`--no-publicly-accessible`·`--no-multi-az`·`--no-deletion-protection`·`--tags`)과 **PostgreSQL 자격증명 전환**은 PITR과 동일(이 명령의 `--manage-master-user-password`도 **Oracle 전용**임을 실측 확인 — 아래).

### 복원본 자격증명 흐름 (r2 정정 — 지원되지 않는 플래그였음)

**정정 사실(로컬 `aws rds ... help` 실측, 2026-07-27):** `restore-db-instance-to-point-in-time`과 `restore-db-instance-from-db-snapshot`의 `--manage-master-user-password`는 도움말 Constraints에 **`Applies to RDS for Oracle only`** 로 명시돼 있다. 대상은 PostgreSQL 16이므로 **복원 명령으로는 복원본 전용 관리 시크릿이 생기지 않는다**(r1의 설계는 성립 불가). 반면 `modify-db-instance --manage-master-user-password`에는 엔진 제약이 없다(Oracle CDB multi-tenant만 제외).

**채택 흐름:**

1. PITR 복원 명령에는 `--manage-master-user-password`를 **넣지 않는다**.
2. 복원본이 available이 된 뒤 **드릴 식별자에만** `modify-db-instance --db-instance-identifier linkpulse-restore-drill --manage-master-user-password --apply-immediately`를 적용한다.
3. `describe-db-instances`로 **드릴 인스턴스의** `MasterUserSecret.SecretStatus == active`가 될 때까지 대기 → 그때 반환된 `MasterUserSecret.SecretArn`(=드릴 시크릿)만 사용한다. **ARN을 손으로 적지 않고 반드시 드릴 인스턴스 조회로 도출**한다(운영 시크릿과 이름이 `rds!db-<resource-id>`로 육안 구분이 어렵다 — §리스크).
4. 이 전환에 걸리는 시간은 **DB 복원 검증 소요시간에 포함**한다(`T_creds_ready`).
5. 정리: RDS 관리 시크릿은 연결된 인스턴스 삭제 시 RDS가 함께 정리하는 것으로 알려져 있으므로 **`delete-secret`을 필수 단계로 두지 않는다.** 대신 인스턴스 삭제 완료 후 해당 ARN을 조회해 **세 결과 중 하나로 판정**한다(r7 — 수명주기가 미확정인 채 "부재/잔존" 두 칸만 두면 **정상 동작이 (d) 거짓 FAIL**로 기록된다):
   - ① **부재** → 종결.
   - ② **존재하되 삭제 예약됨**(`describe-secret`에 `DeletedDate`가 있고 복구 창 대기) → **(B) 정상 비동기 진행**으로 분류하고 지연 확인 대상으로만 남긴다. 예약된 시크릿은 값 조회가 이미 거부되고 임시 role도 지워진 뒤라 **권한·과금이 없다**(TD `DELETE_IN_PROGRESS`와 같은 성질).
   - ③ **존재하고 예약도 아님** → **드릴 ARN임을 운영 ARN과 대조한 뒤에만** 수동 삭제. 단 삭제 전에 **`describe-secret`의 `OwningService`가 `rds`인지 확인한다(r8 — codex-ide)**: RDS가 관리하는 **서비스 연결 시크릿은 사용자가 직접 삭제하지 못할 수 있다.** 거부되면 **같은 명령을 반복하지 말고 "RDS 측 잔존 이상"으로 기록·에스컬레이션**한다(인스턴스가 이미 삭제됐는데 시크릿만 남은 상태 자체가 조사 대상이다).
   - 실제 동작이 ①인지 ②인지(즉시 삭제 vs 복구 창)는 **사람이 실행 전 1차 출처로 확인**하고 ADR 인용 목록에 넣는다. **"삭제 예약" 상태와 판별 필드가 실재하는 것은 확인됐다(r8, claude-ide 실측)** — `describe-secret help`의 `DeletedDate` _"The date the secret is scheduled for deletion. If it is not scheduled for deletion, this field is omitted … recovery window of at least 7 days"_. 즉 ②의 판별은 `DeletedDate` 유무이고, **남은 미지는 "RDS가 인스턴스 삭제 시 즉시 지우는가, 복구 창을 두고 예약하는가" 하나**로 좁혀졌다.

> 대안(불채택): 복원본은 원본의 마스터 자격증명을 승계하므로 **운영 시크릿 값으로 복원본에 접속**할 수도 있다. 운영 자격증명을 드릴 경로에 끌어들이고 운영 시크릿 로테이션과 얽히므로 **탈락**.

### 비공개 RDS 검증 접속 (이 plan의 핵심 함정 — 반드시 반영)

RDS는 **격리 data 서브넷**(인터넷 경로 없음)·`publicly_accessible=false`·data SG(`aws_security_group.data`)로 보호된다. 복원본에 붙어 데이터를 검증할 경로가 필요하다.

**확정 사실(레포 실측 — `security_groups.tf`):** SG 체인은 `alb → app → data`. `data` SG는 **`app` SG에서만 5432 inbound**를 받는다(`data_from_app`, SG 참조). 즉 **`app` SG를 단 러너**면 data SG를 가진 인스턴스에 접속할 수 있다.

**채택안: one-off ECS run-task로 psql 검증(운영 baseline·복원본 검증 **둘 다**).** app 서브넷·`app` SG로 **일회성 Fargate 태스크**(psql 포함 이미지)를 띄워 대상 엔드포인트에 접속해 쿼리를 돌리고, 결과는 CloudWatch 로그로 받는다. **컴퓨팅은 실행 후 종료돼 지속 인프라(과금·권한)가 남지 않는다.** 다만 **제어면 기록은 일정 시간 남는다** — STOPPED 태스크는 한동안 `list-tasks`/`describe-tasks`로 조회되고, TD revision은 `DELETE_IN_PROGRESS`를 거쳐 삭제된다(r6 정정 — r5의 "소멸해 아무것도 남지 않는다"는 정리 게이트의 재탐색·지연 확인 절차와 어긋나는 표현이었다). 이 잔재는 **비용도 권한도 없다.**

**운영 baseline도 같은 경로로 읽는다(r2 추가).** 성공 판정이 "복원본을 운영과 대조"인데 운영 DB 역시 동일하게 비공개이고, 공개 API에는 전역 집계 엔드포인트가 없다(`GET /api/links/{code}`는 단건). 따라서 baseline 캡처도 one-off 태스크 **1회**로 수행한다.

**운영 가드레일의 정확한 정의(r3 신설 — "무접촉"은 사실이 아니었다 / r4에서 ECS·IAM 축까지 확장).** sentinel 생성은 운영 **데이터 쓰기**이고, 검증 태스크는 운영 **계정·클러스터에 리소스를 만든다.** 둘 다 "무변경"과 양립하지 않으므로 다음으로 일원화한다(plan 전체에서 이 정의만 쓴다):

- **금지**: 운영 인스턴스에 대한 `modify`/`restore`/`delete`/`reboot` 등 **설정 변경 계열 API 전면 금지**, 기존 행의 `UPDATE`/`DELETE` 금지, 스키마 변경 금지, 운영 ECS 서비스·TD·ALB·IaC(`.tf`) 변경 금지, 기존 실행 role의 정책 변경 금지.
- **허용되는 운영 데이터 쓰기**: `SELECT` 읽기 + **공개 `POST /api/links` 경로로 만드는 sentinel 링크 최대 5건**(A 최대 3 + B 최대 2 — B는 마진 미달 시 1회 재생성 여유, §측정 정의). 사전 승인 항목으로 **URL은 무해한 고정값(`https://example.com/`)**, **개수 상한**, **삭제 API가 없어 영구 잔존한다는 사실**을 사람이 드릴 전에 확인한다.
  - **식별 표식은 접두사가 아니다(r4 정정).** 공개 생성 DTO는 `url`만 받고 code는 서버가 무작위 생성하므로(`httpapi/links.go:22-25` `createLinkRequest{URL}`, `links/service.go:36-44` `randomCode`) 클라이언트가 code를 지정할 수 없다 — 실측 확인. 대신 **응답으로 받은 정확한 code 목록 + A/B 구분 + 응답 `created_at`(DB 시계)** 을 드릴 기록에 고정하는 것이 식별 기준이다.
  - 생성이 `429`면 앱 장애가 아니라 **레이트리밋**(`POST /api/links` = 20/min·burst 10, `httpapi/ratelimit.go:18-19` 실측)이므로 잠시 후 재시도한다. 최대 5건은 한도 안이다.
- **허용되는 운영 계정 부수효과(r4 신설 — 이 표가 허용의 전부. 표에 없는 운영 계정 리소스 생성·변경은 금지)**:

  | 부수효과 | 대상 | 정리 게이트 종결 상태 |
  | -------- | ---- | --------------------- |
  | 임시 태스크정의 TD-A·TD-B | 새 family 2개(운영 TD 무관) | `deregister` → `delete-task-definitions` → **`DELETE_IN_PROGRESS` 또는 조회 불가 = 종결**(비동기 — §TD 종결 2단계) |
  | 임시 IAM role + 인라인 정책 | 드릴 전용 고정 이름 | 인라인 삭제 → 관리형 detach → `delete-role` → **부재 확인** |
  | 1회성 태스크 실행 **최대 5회(TD-A ≤2 · TD-B ≤3, 최초 호출 포함한 총 시도)** | **운영 클러스터 `linkpulse-prod-cluster`** — 이 서비스의 **Terraform 관리 운영 클러스터**(`ecs.tf:1`+`locals.tf:2`). 계정 전체의 유일성은 레포 코드로 증명할 수 없으므로 **프리플라이트에서 `list-clusters`로 실측 확인**(r5 정정) | **전 시도가 (가) 종결로 확정**(정의·MISSING 규율은 §정리 게이트 ②). 비종결은 `desiredStatus` 기준으로 **waiter만 / `stop-task` 후 waiter**. 성공·실패 무관 **모든 `taskArn`이 대상** |
  | 드릴 로그 스트림 | 운영 로그그룹 `/ecs/linkpulse-prod-app`, 접두사 `restore-drill-<YYYYMMDD-HHMMSS>` | **잔존(정리하지 않음)** — 증거로 남긴다 |
  | sentinel 링크 행 최대 5건 | 운영 DB `links` | **잔존(삭제 API 없음)** — code·`created_at` 기록 |

- **알람 간섭 없음(리뷰 실측 — `infra/prod/monitoring.tf`)**: ECS 알람 3개(`ecs-cpu-high`·`ecs-memory-high`·`ecs-running-tasks-low`)는 차원이 `ClusterName`+`ServiceName`이라 **서비스에 속하지 않는 드릴 태스크를 세지 않고**, RDS 알람 4개는 차원이 `DBInstanceIdentifier = linkpulse-prod-pg`라 **드릴 인스턴스를 보지 않는다** → 드릴은 온콜을 깨우지 않는다. **역방향 규칙**: 드릴 도중 실제 알람이 울리면 **드릴을 중단하고 정리 게이트로 진입한 뒤 알람 대응을 우선**한다(운영 사고 > 드릴).
- 장애·중단 판단 시에도 이 정의를 그대로 적용한다("무접촉"이라는 표현은 쓰지 않는다).

**비밀번호는 평문으로 흐르지 않는다(r2 정정).** r1의 "값을 꺼내 태스크 env/override로 주입"은 비밀번호가 `RunTask` 요청 파라미터·`DescribeTasks` 응답(`overrides…environment[].value`)·CloudTrail·셸 히스토리에 평문으로 남는다. 대신 **임시 태스크 정의의 `secrets`로 시크릿 ARN을 참조**해 ECS 에이전트가 주입하게 한다(`ecs.tf`의 운영 방식과 동일 경계). 호스트·쿼리처럼 비민감한 값만 command override로 넘긴다.

**임시 리소스 2조(순서상 필연 — 드릴 시크릿은 복원 후에야 존재):**

| # | 시점 | 태스크 정의 | 실행 role | 참조 시크릿 |
| - | ---- | ----------- | --------- | ----------- |
| ① baseline | T0 이전 | 임시 TD-A | **기존 `linkpulse-prod-ecs-execution` 재사용** | 운영 시크릿 ARN(이미 정책에 허용 — `iam.tf`) |
| ② 복원본 검증 | T_creds_ready 이후 | 임시 TD-B | **임시 role**(`AmazonECSTaskExecutionRolePolicy` + 드릴 시크릿 ARN 한정 인라인 정책) | 드릴 시크릿 ARN |

임시 role을 새로 만드는 이유: 기존 실행 role의 Secrets 정책은 **운영 시크릿 ARN 하나로 한정**돼 있어(`iam.tf` `ReadDBPassword`) 드릴 시크릿을 읽지 못한다. 운영 role에 권한을 붙였다가 떼는 것보다 **드릴 전용 role을 만들고 정리 게이트에서 지우는** 편이 최소권한·원복 모두 깔끔하다. RDS 관리 시크릿은 AWS 관리형 KMS 키(`aws/secretsmanager`)라 별도 kms 정책이 필요 없다(`iam.tf` 주석의 기존 판단 재사용). **task role은 부여하지 않는다** — 컨테이너가 AWS API를 호출하지 않으므로 불필요(경계를 런북에 명시).

**태스크 기동 전제(하나라도 빠지면 당일 기동 실패 — r3에서 5개 추가):**

- **시크릿 JSON 키(r3 — 빠지면 인증 실패)**: RDS 관리 시크릿 값은 `username`/`password`/`host` 등을 담은 **JSON**이고, ECS는 키를 생략하면 **SecretString 전체**를 환경변수에 넣는다. 운영 정의가 `:password::`를 쓰는 이유가 이것이다(`ecs.tf:55`). TD-A·TD-B 모두 **`name=PGPASSWORD`, `valueFrom=<시크릿 ARN>:password::`** 로 못박는다(`PGPASSWORD`면 psql이 자동 사용). 나머지 접속정보(`PGHOST`/`PGUSER`/`PGDATABASE`/`PGSSLMODE=require`)는 비민감이라 일반 env.
- **플랫폼 버전(r3)**: 시크릿의 JSON 키 주입은 **Linux Fargate platform 1.4.0 이상**이 필요 → `run-task --platform-version 1.4.0` 명시.
- **이미지 digest 고정(r3)**: 운영 ECR 이미지는 Go 바이너리라 psql이 없다 → **public `postgres:16`(Docker Hub)** 를 NAT egress로 pull한다. 그런데 **TD-A는 이 컨테이너에 운영 마스터 비밀번호를 주입**하고 `app` SG에는 **`0.0.0.0/0:443` egress가 열려 있다**(`security_groups.tf`의 `app_https` — 실측). 부동 태그는 언제든 다른 이미지를 가리킬 수 있으므로, **실행 전 확인한 불변 digest(`postgres:16@sha256:...`)로 TD-A·TD-B 모두 고정**하고 digest를 비민감 증거에 기록한다. Docker Hub rate limit 폴백(ECR pull-through/수동 push)도 **같은 digest를 보존**해야 한다(임의의 새 이미지로 갈아타지 않는다). 더 강한 경계가 필요하면 검증한 이미지를 ECR에 미러링해 쓴다(선택 — 드릴 1회 규모에는 digest 고정으로 충분하다고 판단).
- **로그 그룹**: 실행 role의 `AmazonECSTaskExecutionRolePolicy`에는 **`logs:CreateLogGroup`이 없다** → `awslogs-create-group=true`로 새 그룹을 만들려 하면 **태스크가 기동조차 못 한다.** **기존 `/ecs/linkpulse-prod-app`을 재사용**하고 `awslogs-stream-prefix`를 **`restore-drill-<YYYYMMDD-HHMMSS>`로 고정**해 사후에 드릴 출력만 골라낼 수 있게 한다(이 로그그룹에는 메트릭 필터가 없어 오탐 알람 위험 없음 — 리뷰 실측).
- **임시 role의 신뢰정책·PassRole(r3)**: `create-role`에는 `--assume-role-policy-document`가 필수이고 principal은 **`ecs-tasks.amazonaws.com`**(기존 `iam.tf`의 `ecs_assume`와 동일 형태) — 런북에 JSON을 통째로 박아 당일 검색 시간을 없앤다. 실행자에게는 **`iam:PassRole`**(+ IAM/ECS/RDS/Secrets 관련 권한)이 필요하므로 프리플라이트에서 확인한다(관리자 권한을 전제하지 않는다). role 이름은 고정하고 **기존 잔존 여부·TD family 충돌**도 프리플라이트에서 검사한다. 방금 만든 role은 **IAM 전파 지연**으로 첫 `run-task`가 "unable to assume role" 계열로 실패할 수 있다 → 30초 간격으로 재시도하되 **최초 호출을 포함한 총 3회(= 재시도 최대 2회)** 로 센다(r6 정합 — r5에서 여기만 "최대 3회 재시도"로 남아 총 4회로 읽혔고, 그러면 부수효과 상한 5회를 넘었다). 상한 초과 시 CloudShell 폴백 또는 중단(§가드레일 대기 상한 규율).
- **네트워크**: app 서브넷 + `app` SG + `assignPublicIp=DISABLED`(NAT egress로 이미지 pull·Secrets/Logs 엔드포인트 도달).
- **결과 판정 순서(r3)**: CloudWatch 로그를 읽기 **전에** `describe-tasks`의 `containers[].exitCode == 0`을 확인한다. 0이 아니면 `stoppedReason`·컨테이너 `reason`을 런북 진단 절에 기록(로그가 비어 있는 실패를 "결과 없음"으로 오판하지 않기 위함).

- **대안(문서화만):** ① VPC 지원 CloudShell 환경(app 서브넷·`app` SG)에서 psql 직접 — 태스크 정의·임시 role 불요, 자격증명이 AWS 제어면 객체에 남지 않는 장점. **검증 접속 실패 시 1순위 폴백.** ② Bastion 호스트/SSM 포워딩 — 인프라가 붙어 **탈락**(드릴용으로 과함).

### 측정 정의 (r2 신설 — 리뷰 3인 공통 지적)

r1의 `RPO = 운영 max(created_at) − 복원본 max(created_at)`은 **무효**다: `created_at`은 신규 행 생성 시에만 기록되고 클릭은 `clicks`만 갱신하며(타임스탬프 없음), 이 서비스는 실사용 쓰기 트래픽이 사실상 없어 드릴 창 동안 신규 행이 0건일 공산이 크다 → **"손실 없음"이 아니라 "측정 실패"인 0**이 나온다. 또 baseline 캡처 시각이 정의되지 않아 델타의 기준 시각도 없었다. 대체 정의:

- **시각 4개를 전부 기록**: `T0`(복원 API 호출 **직전**) / `T_available`(`wait db-instance-available` 통과) / `T_creds_ready`(드릴 시크릿 `active`) / `T_verified`(검증 쿼리 결과 확보).
- **DB 복원 검증 소요시간 = `T_verified − T0`** (단일 정의. "available부터 시작" 같은 중복 표현 금지). 중간 구간(`T_available − T0` 등)은 참고 내역으로 함께 남긴다.
  - **이 값을 "RTO"로 부르지 않는다(r11 정정 — 코드 리뷰 r6에서 3인 공통 지적).** r2~r10은 같은 산식을 `RTO`라고 불렀지만, 이 드릴은 **격리된 별도 인스턴스를 복원해 조회되는지 확인한 뒤 지운다.** 서비스가 복원본을 사용하도록 전환(cutover)하거나, 손상된 운영 데이터를 역복사하거나, 애플리케이션이 정상 응답하는 것을 확인하는 단계가 **전부 이 plan의 범위 밖**이라 `T_verified`는 **서비스 복구의 끝점이 아니다.** 이 숫자를 RTO로 적으면 ADR을 읽는 사람이 "장애 시 이만큼이면 복구된다"로 오독하고, 실제 사고에서 계획 근거로 쓰인다.
  - **라벨 확정(step 5에서 바꾸지 않는다)**: ① **"DB 복원 검증 소요시간" = `T_verified − T0`**, ② **"선택한 복원 지점의 나이" = `T0 − restore point`**, ③ **"백업 시스템 복구 지점 지연(참고)" = `T0 − LatestRestorableTime(T0 시점)`**.
  - **서비스 RTO를 실측하려면** 별도 plan에서 cutover·쓰기 정합·서비스 검증·롤백까지 구현하고 끝점을 측정해야 한다(P5+ 후속 백로그).
- **복구 지점 지연(RPO 1차 지표) = `T0 − restore point`** — `restore point`는 `--restore-time`에 지정한 시각. 트래픽 유무와 무관하게 **항상 측정된다.** 이 지표와 아래 sentinel 판정은 **서로 다른 측정**이며 ADR에도 따로 기록한다.
  - **이 값을 "백업 시스템의 최신 복구 가능 지연"으로 읽지 않는다(r5 정정).** 드릴 절차상 restore point 확정 후 **sentinel-B 60초 마진 + 사람 조작 시간**이 T0까지 강제로 끼어들므로, 이 숫자에는 **의도적 대기가 최소 60초 포함**돼 있다. ADR·기록에는 **"이번 드릴에서 선택한 복원 지점의 나이"** 로 표기하고, 백업 시스템 자체의 능력치는 **T0 근처에 관측한 `LatestRestorableTime`** 을 별도 기록해 `T0 − LatestRestorableTime(T0 시점)`으로 참고한다. 두 값을 구분하지 않으면 ADR에 시스템 능력을 과소평가한 숫자가 박힌다.
- **행 수준 손실 창(RPO 실증) = sentinel 양방향 브래킷(r3 정정).** r2는 sentinel을 "T0 직전"이라 했지만 실제 순서상 sentinel은 **복원 지점보다 확실히 앞선다** → 그 존재 확인은 설계상 참이어야 하는 **하한 확인**일 뿐이고, "손실 경계"라는 주장은 근거보다 강했다. 양쪽을 닫는다:
  - **sentinel-A** (최대 3건, `T_baseline` 캡처 직전 생성, 복원 지점보다 **이전**) → 복원본에 **전건 존재해야** 한다.
  - **sentinel-B** (**복원 지점 확정 후 · T0 이전** 생성, 복원 지점보다 **이후**) → 복원본에 **부재해야** 한다.
  - **B의 최소 마진 60초 규율(r4 추가 — 성공한 드릴을 실패로 오판하지 않기 위함).** restore point 확정 후 **60초 이상 지난 뒤** B를 만들고, 응답 `created_at`(= DB가 `INSERT … RETURNING`으로 채운 값이라 **DB 시계** — `links/postgres.go:22-26` 실측)이 restore point보다 **60초 넘게 뒤인지 T0 전에 검사**한다. 미달이면 마진을 늘려 **B를 1회 더 생성**(상한 2건)하고 **채택한 B가 어느 건인지 기록**한다. 마진 없이 붙여 만들면 DB 시계와 RDS 제어면 시계의 오차·로그 재생 경계 해상도만으로 판정이 뒤집혀 **정상 복원을 "복원 지점 위반"으로 오판**할 수 있다 — A 쪽에는 이미 "여유 있게 폴링" 규율이 있는데 B에만 없었다.
  - 얻는 것 둘: ① 손실 창이 `[A 존재, B 부재]`로 브래킷돼 goal #3의 문구가 근거를 얻는다. ② **PITR이 지정한 `--restore-time`을 실제로 지켰는지**(조용히 최신 지점으로 복원하지 않았는지) 검증된다. **B가 존재하면 복원 지점이 의도와 다르다는 신호**이므로 실패로 판정하되, **B의 `created_at`(DB 시계) − restore point 값을 함께 남겨 "복원 지점 미준수"인지 "마진 부족"인지를 증거로 구분**한다(ADR에 남을 결론이라 근거 없이 단정하지 않는다).
  - sentinel은 삭제 API가 없어 그대로 남는다(총 최대 5건, 무해한 고정 URL). 드릴 기록의 **필수 항목**: 각 건의 **code · A/B 구분 · 응답 `created_at`(DB 시계, UTC)**.
- **`T_baseline`은 DB 시계로 정한다(r3 추가).** 로컬 셸·CloudWatch 시각을 쓰면 DB·AWS 시계와 오차가 생기고, 집계를 여러 `SELECT`로 나누면 그 사이의 쓰기 때문에 restore point가 `T_baseline`을 지났는데도 복원본 집계가 baseline보다 작아지는 **거짓 실패**가 가능하다. → sentinel-A 생성 응답이 끝난 뒤 TD-A에서 **단일 SQL(단일 스냅샷)** 로 `clock_timestamp()`·`count(*)`·`sum(clicks)`·`max(created_at)`·sentinel-A code 존재 여부를 **함께** 출력하고, 그 **DB 시각을 `T_baseline`으로 정의**한다.
- **시각 직렬화·시계 출처 고정(r4 추가 — 서로 다른 시계를 섞지 않기 위함).** 모든 시각은 **UTC ISO-8601 초 단위(`YYYY-MM-DDTHH:MM:SSZ`)** 로 직렬화하고, 기록에는 **값과 함께 어느 시계인지**를 적는다. 출처는 항목별로 못박는다:
  - `T_baseline` · sentinel `created_at` → **DB 시계**(`clock_timestamp()` / `RETURNING created_at`)
  - `restore point` · `LatestRestorableTime` · `SnapshotCreateTime` → **RDS 제어면 시계**
  - `T0` · `T_available` · `T_creds_ready` · `T_verified` → **실행자 셸의 `date -u`** 하나로 통일(드릴 도중 출처를 바꾸지 않는다). CloudWatch 로그 타임스탬프는 참고용이며 측정에 쓰지 않는다.
  - **교차 시계 비교는 하나가 아니라 셋이다(r5 정정 — r4의 "유일한 비교" 단정은 틀렸다).** 셋을 전부 열거하고 각각의 흡수 방법을 못박는다:

    | # | 비교 | 어디에 쓰이나 | 흡수 방법 |
    | - | ---- | ------------- | --------- |
    | (i) | `T_baseline`(DB) **vs** `restore point`(제어면) | **성공 판정 (b)의 근거** — "복원 지점 > `T_baseline`"이라야 `count(*) ≥ baseline` 단조성 논증이 성립 | **`restore point ≥ T_baseline + 60초`** 숫자 마진(r5 신설). r4까지는 "여유 있게 폴링"이라는 정성 표현뿐이라, DB 시계가 제어면보다 앞서 있으면 겉보기로 지난 restore point가 실제로는 `T_baseline` 이전일 수 있고 → sentinel-A 누락·`count(*) < baseline` → **정상 복원을 (b) FAIL로 오판**한다. B에만 숫자를 박고 정작 판정 근거 쪽에 없던 것이 공백이었다. |
    | (ii) | `T0`(셸 `date -u`) **vs** `restore point`(제어면) | **1차 지표** 복구 지점 지연 = `T0 − restore point` | ① 프리플라이트에서 **셸↔AWS 시계 오차를 1회 측정해 기록**(예: `date -u`와 AWS 엔드포인트 응답 `Date` 헤더 대조 — 근사값이며 정밀 동기화 주장은 하지 않는다) ② 복원 응답의 **`DBInstance.InstanceCreateTime`(제어면 시계, help 1268행 실측)** 을 함께 기록해 **제어면 단독 산출값을 병기**한다. 두 값을 나란히 남기면 지표가 교차 시계라는 사실이 증거로 드러난다. |
    | (iii) | sentinel-B `created_at`(DB) **vs** `restore point`(제어면) | sentinel-B 부재 판정 | **60초 마진**(r4) |
- **집계 대조**: 복원본은 `count(*) ≥ baseline`, `sum(clicks) ≥ baseline`이어야 한다(복원 지점 > `T_baseline`이고, 라우터에 DELETE 경로가 없어 `clicks`는 증가만 하므로 — 단조성은 리뷰에서 실측 확인됨). **미만이면 실패 신호.** 복원 후 운영을 다시 읽어 비교하지 않는다(운영은 계속 변하므로 서로 다른 시점 비교가 된다).
- **증거에서 `url`은 제외**한다 — URL에 토큰·개인정보가 실릴 수 있으므로 `code`·`clicks`·`created_at`만 남긴다(CloudWatch 로그·evidence 공통).

### 확정 사실 (레포·AWS 실측)

- DB: `linkpulse-prod-pg`, PostgreSQL 16, `db.t4g.micro`, gp3 20GB **암호화**, Single-AZ, `deletion_protection=true`, `skip_final_snapshot=false`(`rds.tf`).
- 스키마(`app/internal/db/schema.sql`): **단일 테이블 `links`**(`code` PK, `url`, `clicks BIGINT`, `created_at TIMESTAMPTZ DEFAULT now()`). 검증 쿼리 = `count(*)`·`max(created_at)`·`sum(clicks)`·최근행 표본.
- 앱 DB 접속(`ecs.tf`): `DB_HOST/PORT/NAME/USER/SSLMODE=require`(env) + `DB_PASSWORD`(secret, `master_user_secret[0].secret_arn:password::`). **sslmode=require** → 검증 접속도 TLS.
- **ECS Exec 미활성**(`ecs.tf`에 `enable_execute_command` 없음) → 검증은 exec가 아니라 one-off run-task.
- `scripts/`에 `full-apply-prod.sh`·`full-destroy-prod.sh`(사람이 실행하는 bash 관례) 존재. app 서브넷은 NAT egress 있음(443), data 서브넷은 인터넷 경로 없음.
- **파라미터그룹**: 운영은 커스텀 `aws_db_parameter_group.main`(`name_prefix = linkpulse-prod-pg16-`, `rds.force_ssl=1`, `apply_method=pending-reboot`)를 쓴다(`rds.tf`). **PITR 복원본은 지정하지 않으면 엔진 기본 그룹**으로 뜬다 → 복원 명령에 `--db-parameter-group-name`으로 **운영 그룹을 명시**한다(복원본은 신규 부팅이라 pending-reboot 파라미터도 적용됨).

  **명시하는 이유는 TLS가 아니라 구성 동등성이다(r12 정정 — r1~r11의 "기본 그룹이면 `force_ssl`이 빠진다"는 이 엔진에서 사실이 아니었다).** AWS는 _"The `rds.force_ssl` parameter default value is **1 (on) for RDS for PostgreSQL version 15 and later**"_ 라고 명시하고([Using SSL with a PostgreSQL DB instance](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/PostgreSQL.Concepts.General.SSL.html)), 대상은 PostgreSQL 16이므로 **기본 그룹으로 떠도 비TLS 접속은 거부된다.** 즉 `pg_settings`의 `rds.force_ssl` 조회는 **그룹 누락을 잡아 주지 못한다** — (a)가 측정하려는 것은 "운영과 같은 파라미터 집합으로 복원되는가"이고, 앞으로 커스텀 파라미터가 늘수록 그 차이가 커진다.

  **따라서 사후 확인은 두 가지를 함께 본다**: **① 붙은 그룹 이름 = 운영 `$PROD_PG`**(`describe-db-instances`의 `DBParameterGroups[0].DBParameterGroupName`) **② 그 적용이 `in-sync`**(`ParameterApplyStatus`). `in-sync`만 보면 **기본 그룹도 통과**하므로 이름 대조 없이는 `--db-parameter-group-name` 누락이 그대로 (a) PASS로 기록된다.
- **로그·IAM**: 로그 그룹 `/ecs/linkpulse-prod-app`(`logs.tf`), 실행 role `linkpulse-prod-ecs-execution`(`iam.tf`) — 관리형 정책에 `logs:CreateLogGroup` **없음**, Secrets는 **운영 시크릿 ARN 하나로 한정**.
- **CLI 제약(로컬 `help` 실측, 2026-07-27)**: 복원 계열 명령의 `--manage-master-user-password`는 **Oracle 전용**, `modify-db-instance`의 동일 옵션은 **엔진 제약 없음**(§복원본 자격증명 흐름).
- **`--restore-time` 제약(r3 실측)**: `Must be a time in UTC format` + **`Must be before the latest restorable time`**(strictly before) + `--use-latest-restorable-time`과 **동시 지정 불가**.
- **태스크 정의 정리(r3 실측)**: `deregister-task-definition`은 TD를 지우지 않고 **`INACTIVE`로 남긴다.** 실제 삭제는 별도 `aws ecs delete-task-definitions`이며 도움말에 **"You must deregister a task definition revision before you delete it"** 로 순서가 못박혀 있다.
- **egress**: `app` SG에 **`0.0.0.0/0` 443 egress**(`app_https`, "HTTPS egress via NAT")가 열려 있다 → 검증 컨테이너는 외부로 나갈 수 있다(§이미지 digest 고정의 근거).
- **시크릿 주입 관례**: 운영은 `valueFrom = "<secret_arn>:password::"`로 **JSON의 `password` 키만** 주입한다(`ecs.tf:55`).
- **단조성**: 라우터에 DELETE 경로가 없고 `clicks`는 증가만 한다 → `count(*)`·`sum(clicks)`는 시간에 대해 단조 증가(리뷰 실측). `/ecs/linkpulse-prod-app`에는 메트릭 필터가 없어 드릴 로그가 섞여도 오탐 알람이 나지 않는다(리뷰 실측).
- **ECS 클러스터(r4 실측)**: `aws_ecs_cluster.main`의 이름은 `${local.name_prefix}-cluster`이고 `name_prefix = "${var.project}-${var.environment}"` = `linkpulse-prod`(`ecs.tf:1`, `locals.tf:2`) → 이 레포가 만드는 클러스터는 **`linkpulse-prod-cluster` 하나뿐**이다. **다만 이것으로 "AWS 계정 전체에 클러스터가 이것뿐"임을 증명할 수는 없다(r5 정정 — r4의 "계정 유일" 단정은 레포 코드의 범위를 넘었다)** → 드릴에 필요한 사실은 "**이 서비스의 Terraform 관리 운영 클러스터**"이고, 실제 유일성은 **프리플라이트의 `list-clusters` 실측**으로 확인한다. 어느 쪽이든 드릴의 `run-task --cluster`는 **운영 클러스터를 쓸 수밖에 없다**(§운영 가드레일 표에 반영).
- **code는 클라이언트가 지정할 수 없다(r4 실측)**: 공개 생성 DTO는 `createLinkRequest{URL string}`뿐이고(`httpapi/links.go:22-25`), 서비스가 `randomCode(s.codeLen)`로 뽑아 충돌 시 재시도한다(`links/service.go:36-49`). → sentinel 식별은 **응답 code 목록**으로만 한다.
- **레이트리밋(r4 실측)**: `POST /api/links`는 per-IP **20/min·burst 10**(`httpapi/ratelimit.go:18-19`). sentinel 최대 5건은 한도 안이며, `429`는 앱 장애가 아니다.
- **알람 차원(r4 실측 — `monitoring.tf`)**: ECS 알람 3개는 `ClusterName`+`ServiceName`(160-161·180-181·202-203행), RDS 알람 4개는 `DBInstanceIdentifier = aws_db_instance.main.identifier`(225·243·261·279행) → 드릴 태스크·드릴 인스턴스는 **어느 알람에도 잡히지 않는다.**
- **ECS 정리 API(r4 실측, 로컬 `help`)**: `aws ecs stop-task` 존재, `aws ecs wait`에 **`tasks-stopped`·`tasks-running`** waiter 존재 → 중단 경로에서 `RUNNING` 태스크를 "확인"이 아니라 **정지시키고 대기**할 수 있다(§정리 게이트).
- **태스크 "종결" 집합은 일부만 실측됐다(r7 신설 → r8에서 ⓒ 해소·ⓑ 부분 확인. 단정 금지, 나머지는 확인 전 드릴 금지)**: 리뷰어 2인이 ECS 태스크 수명주기 문서를 근거로 ① `STOPPED` **뒤의 `DELETED`도 `describe-tasks`에 보일 수 있고**, ② `StopTask`는 **"running task"를 정지하는 API**라 이미 종결·회수 불가인 태스크에 다시 호출하면 오류가 날 수 있으며, ③ `wait tasks-stopped`는 `tasks[].lastStatus == STOPPED`만 성공으로 보므로 **이미 사라진(`describe-tasks` `failures[].reason=MISSING`) 태스크에서는 상한까지 대기**할 수 있다고 지적했다. **이 세 가지는 이번 병합 시점에 로컬 `help`로 확인하지 못했다**(CLI 실행 미승인) → **런북 작성 전에 사람이 `aws ecs stop-task`/`describe-tasks`/`wait tasks-stopped` 도움말 + ECS 태스크 수명주기 문서로 확인**하고 ADR 인용 목록에 넣는다([[infra-plan-review-and-first-source]] — 다른 에이전트의 주장을 액면 그대로 plan의 확정 사실로 올리지 않는다). 절차는 **확인 전에도 안전한 방향**으로 설계한다: `DELETED`는 실재하지 않더라도 **매칭되지 않을 뿐 무해**하고, 이미 정지 요청된 태스크에는 **waiter만** 걸며, **MISSING은 종결 근거가 있을 때만 종결로 인정**한다(§정리 게이트 ②).
  - **ⓒ는 r8에서 닫혔다(claude-ide r7 실측, 로컬 `help` — 메인 코더 미재현이라 런북 작성 시 1회 재확인)**: `aws ecs wait tasks-stopped help` _"Wait until JMESPath query **`tasks[].lastStatus`** returns STOPPED for **all elements** when polling with describe-tasks. It will poll **every 6 seconds** … exit with a return code of **255 after 100 failed checks**."_ ⇒ ① waiter 상한 = **6초 × 100회 = 600초, 초과 시 exit 255** ② 매처가 **`tasks[]`** 만 보므로 `failures[]`에 담기는 **MISSING 태스크는 성공 조건을 영원히 만족하지 못한다**(= 10분 헛대기 후 255) ③ **"for all elements"** 라 여러 ARN을 한 번에 넘기면 **하나 때문에 배치 전체가 600초를 소진**한다 → **ARN 하나씩** 건다(§정리 게이트 ②).
  - **ⓑ는 부분 확인**: `stop-task` 도움말 첫 줄이 _"Stops a **running** task."_ 라 대상이 running task임은 분명하나, **이미 종결된 태스크에 호출했을 때 오류인지 무해한 no-op인지는 로컬 help에 없다** → 1차 출처 확인 유지(절차는 (나)/(다) 분기로 이미 중복 호출을 피한다).
- **ECS API는 결과적 일관성(eventual consistency)이다(r8 — codex-ide 지적, 1차 출처 확인 대상 ⓗ)**: `run-task` 직후의 조회에 새 태스크가 아직 보이지 않을 수 있어 AWS는 **`describe-tasks` 재시도**를 권한다. 따라서 **`MISSING`을 "이미 끝났다"로 읽으면 안 된다** — 이 plan에서 그 오독은 **뒤늦게 기동해 운영 마스터 비밀번호를 주입받는 태스크를 "정리 완료"로 기록**하는 결과가 된다(§정리 게이트 ②의 MISSING 규율).
- **드릴 리소스 재탐색 경로(r5 실측, 로컬 `help`)** — 원장이 비어도 AWS에서 되찾을 수 있다:
  - `run-task`에 **`--started-by`(최대 128자, 회수 키)** 와 **`--client-token`(최대 64자, _"ensure the idempotency of the request"_ — 회수와 무관한 멱등 장치)** 존재. 둘은 **역할이 다르다**(r6 정정 — r5는 이를 묶어 서술했다).
  - `list-tasks`에 `--started-by`·`--family`·`--desired-status` 필터가 있으나 **조합에 제약이 있다(r6 실측 — r5의 "토큰 하나로 모든 시도 회수"는 틀렸다)**: help _"When you specify startedBy as the filter, **it must be the only filter that you use**."_ → `--started-by`는 **단독으로만** 쓴다(`--cluster`는 필터가 아니라 조회 범위라 함께 쓸 수 있다). 또 _"Although you can filter results based on a desired status of `PENDING`, this doesn't return any results. Amazon ECS never sets the desired status of a task to that value (only a task's `lastStatus` may have a value of `PENDING`)."_ ⇒ **활성 회수와 종료 회수를 두 경로로 분리**해야 한다(§정리 게이트 (3)).
  - `list-task-definitions --status`의 유효값에 **`ACTIVE`·`INACTIVE`·`DELETE_IN_PROGRESS`** 전부 포함(**단일 enum이라 상태별로 따로 호출**). `INACTIVE` 조회는 help상 _"as long as an active task or service still references them"_ 제약이 있어 **참조가 끊긴 INACTIVE TD는 누락될 수 있다** → 원장의 family+revision으로 `describe-task-definition` 병행.
- **TD 삭제는 비동기다(r5 실측 — `delete-task-definitions help`)**: 삭제 요청 시 revision은 _"immediately transitions from the INACTIVE to `DELETE_IN_PROGRESS`"_ 이고, _"A task definition revision will stay in `DELETE_IN_PROGRESS` status until all the associated tasks and services have been terminated."_ **전용 waiter는 없다.** ⇒ 호출 성공을 곧바로 "물리적 삭제 완료"로 판정할 수 없다(§TD 종결 2단계). 리뷰어 2인이 인용한 **"영구 삭제까지 최대 1시간"** 은 로컬 help에 없고 **ECS 개발자 가이드** 근거이므로 런북 작성 시 1차 출처로 확인하고 ADR 인용 목록에 넣는다.
- **복원 응답에 제어면 시각이 있다(r5 실측)**: `restore-db-instance-to-point-in-time` 응답의 `DBInstance.InstanceCreateTime`(timestamp, help 1268행) → 복구 지점 지연을 **제어면 시계 단독으로도** 산출해 병기할 수 있다(§측정 정의 교차 시계 표 (ii)).

### 가드레일 (AGENTS.md #1·#2 — 위반 금지)

- 에이전트는 **문서(runbook·ADR)까지만.** `terraform` 변경은 **이 plan에 없다**(드릴은 CLI + 소멸성 리소스라 IaC로 박제하지 않는다). **모든 `aws` CLI(복원·검증 태스크·삭제), 과금 발생, 드릴 실행은 전부 사람**([[ask-before-external-services]]).
- **1차 출처 규율**([[infra-plan-review-and-first-source]]): AWS CLI 플래그와 동작(`--restore-time`, `--db-parameter-group-name`, `modify-db-instance --manage-master-user-password`, 관리 시크릿의 인스턴스 삭제 시 수명주기, 트랜잭션 로그 업로드 주기)은 **단정 금지** — 사람이 `aws rds ... help`·AWS 문서로 확인 후 실행. **r1이 `--manage-master-user-password`를 복원 명령에 쓸 수 있다고 단정했다가 Oracle 전용임이 드러난 것이 바로 이 규율의 사례다.** ADR의 DB 복원 검증 소요시간/복구 지점 수치는 드릴 실측으로 확정(플레이스홀더 → step 5).
- **대기 상한 규율(r4 추가 — 무기한 대기·무한 재시도 금지)**: 모든 폴링·대기에 상한을 두고, 초과하면 **드릴을 중단하고 정리 게이트로 진입**한다(중단도 정상 종료 경로이며, 정리 게이트는 어느 지점에서 들어와도 동작한다 — §정리 게이트). 기준값(런북에 숫자로 박는다): `LatestRestorableTime` 폴링 **최대 30분** — 초과 시 **`T_baseline`·최종 관측 `LatestRestorableTime`·경과 시간을 남기고** 중단한다. 이건 단순 실패가 아니라 **의미 있는 DR 발견**(복구 지점 지연의 하한을 알려준다)이므로 실패한 드릴에서도 ADR에 쓸 숫자가 나오게 기록 형식을 고정한다(r5). 이 시점은 T0 전이라 잔존물이 sentinel-A뿐이다. / 드릴 시크릿 `SecretStatus=active` 대기 **최대 15분** / `run-task` 재시도는 **최초 호출을 포함한 총 시도 상한**으로 센다(TD-A 2회·TD-B 3회, 30초 간격) — 초과 시 CloudShell 폴백 또는 중단 / `wait db-instance-available`·`db-instance-deleted`는 **각각 30초 × 60회 = 1800초(30분), 초과 시 exit 255**(r9 실측 인용 — claude-ide 로컬 `help`, 메인 코더 미재현이라 런북 작성 시 1회 재확인)이고, **상한 초과 시 `describe`로 상태를 직접 확인해 재호출/중단을 판단**한다. `db-instance-deleted`의 매처는 **`length(DBInstances) == 0`** — 즉 **조회에서 사라져야** 성공이라, 정리 게이트 ⑤의 "삭제 요청이 아니라 삭제 완료를 과금 종료로 본다"는 규율과 정확히 대응한다(r9) / **`wait tasks-stopped` = 6초 × 100회 = 600초, 초과 시 exit 255**(r8 실측 인용). ARN **하나씩** 걸고, 초과해도 절차를 끊지 않고 `describe-tasks` 결과로 분류한다. TD 물리 삭제는 **폴링하지 않는다**(§TD 종결 2단계 — 즉시 인정 + 지연 확인).
- **비용 고지(추정 — 실행 전 요금 페이지 확인)**: 드릴 인스턴스 `db.t4g.micro` + gp3 20GB. 30~60분 드릴이면 **$1 미만**으로 추정되나, 흔히 인용되는 시간당 단가는 us-east-1 기준이고 **ap-northeast-2는 더 비싸다** → 런북에는 단가를 박지 말고 "실행 전 ap-northeast-2 RDS 요금 페이지 확인"으로 적는다. **`creating` 상태도 과금**되며, **삭제 요청이 아니라 삭제 완료(waiter 통과)** 까지 비용 종료로 보지 않는다. 삭제를 잊으면 상시 과금 → step 4 삭제 게이트. 커밋·PR은 사람([[never-auto-commit]]).

## 실행 단계

> 1~3 = 에이전트(문서 + 로컬 검증). 4 = 사람(드릴 실행: 복원·검증·측정·삭제). 5 = 사람 실측 제공 후 ADR 확정. terraform 변경 없음.

1. **복원 런북 작성** — `docs/runbooks/rds-restore.md` 신규. 절 구성(드릴 실행 순서 그대로):

   - **(a) 사전/프리플라이트** — 운영 가드레일 정의(§운영 가드레일 — 설정 변경 API 금지, `SELECT` + sentinel 최대 5건, 허용 부수효과 표), 드릴 식별자 `linkpulse-restore-drill`·클러스터 `linkpulse-prod-cluster` 고정, 비용·삭제 경고, **대기 상한값 표**(§가드레일). 모든 변경 명령 전 **`sts get-caller-identity`·리전(`ap-northeast-2`)·source/target 식별자·target 부재**를 확인. 원본에서 **서브넷 그룹·data SG·파라미터그룹 ARN/이름을 조회해 고정**. **실행자 권한 확인**(`iam:PassRole` + IAM/ECS/RDS/Secrets), **임시 role 이름·TD family의 기존 잔존/충돌 검사**, **이미지 digest 확인·기록**, `git status --short` 기준점 기록. **`list-clusters`로 대상 클러스터를 실측 확인**(레포 코드는 "이 서비스의 Terraform 관리 클러스터"까지만 증명한다 — r5). **셸↔AWS 시계 오차를 1회 측정해 기록**(`date -u` vs AWS 엔드포인트 응답 `Date` 헤더 — 근사값, §측정 정의 (ii)). **리소스 원장 표를 여기서 열고 예정값을 먼저 채운다** — 드릴 DB 식별자·임시 role 이름·TD family 2개·**드릴 토큰 `restore-drill-<YYYYMMDD-HHMMSS>`**(`--started-by`/로그 스트림 접두사 공용, 같은 날 재드릴 대비 시각 포함)·**시도별 client token 5개**·태그. 사후값은 생성 즉시 추가(§정리 게이트). **1차 출처 확인 체크리스트(r7 — 전부 확인하기 전에는 드릴을 시작하지 않는다)**: ⓐ ECS 태스크 **`DELETED` 상태**의 존재와 `describe-tasks` 노출 여부, ⓑ **이미 종결된 태스크에 `stop-task`를 호출했을 때의 동작**(오류인지 무해한 no-op인지 — **확인 못 해도 절차는 안전하다**: (가) 종결은 조치 없음이라 실제 문제 경로는 *관측과 호출 사이에 태스크가 종결로 넘어간 좁은 레이스*뿐이고, 그마저 "거부되면 30초 뒤 재조회(상한 3회)"가 흡수한다. **우선순위 낮음** — r9), ~~ⓒ~~ **`wait tasks-stopped` 동작 = r8에서 확인 완료**(6초×100회/exit 255, 매처가 `tasks[]` 기준 — 런북 작성 시 1회 재확인만), ⓓ **STOPPED 태스크 보관 창**, ⓔ **RDS 관리 시크릿이 인스턴스 삭제 시 즉시 삭제되는가 복구 창을 두고 예약되는가**(r8에서 범위 축소 — "삭제 예약 상태와 `DeletedDate` 판별 필드의 존재"는 확인됨. (A)/(B) 분류가 갈린다) + **`OwningService=rds` 시크릿을 사용자가 직접 삭제할 수 있는지**, ⓕ `--restore-time` 경계가 `<`인지 `≤`인지, ⓖ **ECS 멱등성 규칙 — client token TTL 수치**(로컬 `help`에 없음. **이 값이 24시간보다 짧아도 초 단위 토큰 결정은 유지**되므로 설계는 결과와 무관하다) **+ "토큰을 다른 요청에 재사용하지 않는다"의 정확한 문구**(같은 문서에서 한 번에 확보 — r10), **ⓗ ECS API의 결과적 일관성**(`run-task` 직후 조회 지연 — MISSING 판정의 근거. r8), **ⓘ `DeleteTaskDefinitions`에서 `failures[].reason=MISSING`이 반환되는 조건**(r10 — 이게 중요한 PASS 조건인데 AWS 문서는 `Failure` 구조만 설명한다. **확인 전에는 revision 부재를 `describe-task-definition`으로 직접 확인한 경우에만 종결 인정**), **ⓙ 이미 접수된 revision에 `delete-task-definitions`를 재전송했을 때의 응답**(r10 — 우선순위 4번의 재전송 규율. 어느 쪽이든 1~3 분기로 흡수되므로 절차는 안전). **ⓐ·ⓓ·ⓗ는 AWS 공식 문서로 확인 가능하다고 리뷰어가 확인했으므로**(codex-ide r8) 런북 작성 단계에서 **URL과 함께 확정**해, 드릴 당일 사람이 상태 머신의 의미를 다시 조사하지 않게 한다.
   - **(b) sentinel-A + baseline 캡처(T_baseline)** — 공개 API로 **sentinel-A 최대 3건**(고정 URL `https://example.com/`) 생성 → **응답의 `code`와 `created_at`(DB 시계)을 원장에 기록**(code는 지정 불가·서버 생성), `429`면 레이트리밋이므로 잠시 후 재시도 → 임시 TD-A(기존 실행 role, 운영 시크릿 `PGPASSWORD=<ARN>:password::`, 로그그룹 재사용, 스트림 접두사 `restore-drill-<YYYYMMDD-HHMMSS>`) 등록 → app 서브넷·`app` SG·`--platform-version 1.4.0`·**`--started-by <드릴 토큰>`·`--client-token <드릴 토큰>-tda-N`** 으로 run-task(**TD-A 총 시도 상한 2회 = 최초 1 + 재시도 1**, 시도마다 토큰을 바꾸고 응답의 `taskArn`·`failures[]`를 원장에 기록. 응답을 못 받은 재전송에만 같은 토큰 — §정리 게이트 (4)) → **`exitCode==0` 확인 후** 로그에서 **단일 SQL 출력**(`clock_timestamp()`·`count(*)`·`sum(clicks)`·`max(created_at)`·sentinel-A 존재)을 읽고 **DB 시각을 `T_baseline`으로 채택**(`url` 미출력). **TD-A가 실패해 재실행할 때 sentinel-A는 다시 만들지 않는다** — 이미 DB에 있으므로 상한 3건을 소모하지 않는다(r5).
   - **(c) 복원 지점 확보 + sentinel-B** — 원본 `LatestRestorableTime`이 `T_baseline`을 **여유 있게 지날 때까지 폴링**(측정 구간 밖 — `T0` 이전, **상한 30분** — 초과 시 `T_baseline`·최종 관측 `LatestRestorableTime`·경과 시간을 남기고 중단·정리 게이트) → **`T_baseline + 60초 ≤ restore point < 현재 LatestRestorableTime`** 을 만족하는 값으로 `restore point` 확정(하한 마진은 r5 신설 — 교차 시계 (i), 관측값을 그대로 쓰지 않는 것은 `before` 제약). → **확정 후 60초 이상 지난 뒤·T0 전에 sentinel-B 생성** → 응답 `created_at`(DB 시계)이 `restore point + 60초`보다 뒤인지 **T0 전에 검사**하고, 미달이면 마진을 늘려 **1회만 재생성**(B 상한 2건) → 채택한 B의 code·`created_at`을 원장에 기록.
   - **(d) PITR 복원(T0 기록 — 호출 직전)** — `restore-db-instance-to-point-in-time`: `--source-db-instance-identifier linkpulse-prod-pg`, `--target-db-instance-identifier linkpulse-restore-drill`, **`--restore-time <restore point>`**(UTC), `--db-subnet-group-name <data subnet group>`, `--vpc-security-group-ids <data SG id>`, **`--db-parameter-group-name <운영 PG>`**, **`--no-publicly-accessible`**, `--no-multi-az`, `--no-deletion-protection`, `--backup-retention-period 0`, `--tags Key=purpose,Value=rds-restore-drill`. **`--manage-master-user-password`는 넣지 않는다**(Oracle 전용). 응답의 **`DBInstance.InstanceCreateTime`(제어면 시계)을 원장에 기록**하고, T0 근처의 **`LatestRestorableTime`도 별도 관측·기록**한다(§측정 정의 — 지표 해석을 위해).
   - **(e) available 대기 + 구성 게이트(T_available)** — `aws rds wait db-instance-available` → `describe-db-instances`로 검사하되 **두 부류를 분리**한다(r3 정정):
     - **중단(abort) 조건 — 접속 전 즉시 중단, 정리 게이트로**: `PubliclyAccessible=true`, 기대와 다른 SG/서브넷 그룹, `StorageEncrypted=false`, 엔진/클래스 불일치, `DeletionProtection=true`. 노출·경로 문제라 검증을 진행할 이유가 없다. **abort는 T0 이후이므로 sentinel-A·B가 이미 존재한다** → 중단 기록에도 **sentinel code·`created_at`·`restore point`·확보한 시각들**을 남겨 실패한 드릴의 증거를 완결시킨다(r6).
     - **파라미터그룹 축 — 어느 쪽이든 중단하지 않는다**(r12 정정 — 이름 대조를 추가하면서 두 갈래로 나뉜다). 복원본은 이 분기에 올 때 이미 **비공개·암호화·기대 SG/서브넷 그룹**이므로 접속을 막을 안전상의 이유가 없다.
       - **교정(remediate)**: `DBParameterGroups[].ParameterApplyStatus`가 `in-sync`가 아님. 이 레포의 `rds.force_ssl`은 **`apply_method="pending-reboot"`로 명시**돼 있어(`rds.tf`) 복원본에서도 `pending-reboot`로 보고될 수 있다. 정답은 드릴 폐기가 아니라 **`reboot-db-instance` 후 재확인**이다. 재부팅 후에도 `in-sync`가 아니면 **검증은 계속하되 (a)는 FAIL로 확정**하고 한계를 기록한다 — r3까지는 "한계 기록 후 진행"이 최종 성공 판정과 충돌했다(r4 정정: 계속 진행 ≠ PASS).
       - **기록만((a) FAIL 확정)**: 붙은 그룹 **이름**이 운영 그룹이 아님. **재부팅으로 고쳐지지 않으므로 교정 분기로 가지 않고**, (a)만 FAIL로 확정한 뒤 (b)·(c) 재료를 확보한다.
       - **기대값(운영 그룹 이름)을 얻지 못한 경우는 FAIL이 아니라 판정 미완**이다 — "구성이 틀렸다"가 아니라 "판정할 재료가 없다"이므로 원장 값을 되살린 뒤 재확인한다.
       - **reboot 소요는 DB 복원 검증 소요시간에 포함**된다(그 지표는 `T_verified − T0` 단일 정의).
   - **(f) 자격증명 전환(T_creds_ready)** — 드릴 식별자에만 `modify-db-instance --manage-master-user-password --apply-immediately` → **드릴 인스턴스 조회로** `MasterUserSecret.SecretStatus=active` **그리고 인스턴스가 `modifying`→`available`로 복귀**했음을 함께 확인 → `SecretArn` 확보(§자격증명 흐름).
   - **(g) 검증 접속·쿼리(T_verified)** — 임시 role 생성(신뢰정책 `ecs-tasks.amazonaws.com` + 관리형 정책 attach + 드릴 시크릿 ARN 한정 인라인 정책) → 임시 TD-B 등록(`PGPASSWORD=<드릴 ARN>:password::`, 같은 로그그룹, 동일 digest 이미지) → `--platform-version 1.4.0`·**`--started-by <드릴 토큰>`·`--client-token <드릴 토큰>-tdb-N`** 으로 run-task(IAM 전파 지연 시 30초 간격 재시도, **최초 호출 포함 총 시도 상한 3회 = 최초 1 + 재시도 2** — **다음 시도에는 반드시 다음 토큰**을 쓴다. 같은 토큰이면 ECS가 최초 결과만 돌려줘 전파가 끝나도 새 태스크가 뜨지 않는다. 응답의 `taskArn`·`failures[]`를 **시도마다 원장에 기록**한다. 실패한 시도도 태스크를 만들 수 있어 정리 대상이다) → **`exitCode==0` 확인 후** 로그 판정: `count(*)`·`sum(clicks)`·`max(created_at)`·**sentinel-A 존재**·**sentinel-B 부재**·**`rds.force_ssl` 실효값**(`url` 미출력). **비밀번호를 command/override/로그/증거에 절대 넣지 않는다.**
     - **`rds.force_ssl` 실효값 조회(r4 추가 — 구성 동등성의 server-side 증거)**: `SELECT name, setting FROM pg_settings WHERE name = 'rds.force_ssl';` 로 읽어 `on`/`1`을 증거로 남긴다. `SHOW rds.force_ssl`은 GUC가 등록돼 있지 않으면 **에러로 SQL 배치를 중단**시켜 다른 검증 결과까지 잃을 수 있으므로 `pg_settings`(미등록이면 0행)를 쓴다. 이 값이 `on`이 아니면 성공 판정 **(a) 구성 동등성 FAIL**.
   - **(h) 판정·측정(축별 PASS/FAIL)** — **(b) 데이터 복원** = `count(*) ≥ baseline` ∧ `sum(clicks) ≥ baseline` ∧ **sentinel-A 전건 존재** ∧ **sentinel-B 부재**. **(a) 구성 동등성** = (e)의 abort 항목 전부 일치 ∧ **PG 이름 = 운영 그룹** ∧ PG `in-sync` ∧ `rds.force_ssl` 실효값 `on`. **(c) 측정** = **DB 복원 검증 소요시간 = T_verified − T0**, **복구 지점 지연 = T0 − restore point** 기록(§측정 정의). **sentinel-B가 존재하면** 복원 지점이 의도와 다르다는 신호 → (b) FAIL로 판정하고, **B의 `created_at` − restore point**를 함께 남겨 "미준수 vs 마진 부족"을 구분한다. 축을 섞어 "대체로 성공"으로 뭉뚱그리지 않는다.
   - **(i) 정리 게이트 — 멱등·재진입 가능(r4 전면 재작성)** — r3까지의 고정 순서는 **정상 종료 경로만** 처리했다: baseline 단계에서 중단하면 드릴 DB·TD-B·임시 role이 아직 없고, 복원·자격증명·검증 도중 중단하면 일부만 있거나 태스크가 `RUNNING`이다. 그 상태에서 첫 `delete-db-instance`의 NotFound나 "STOPPED 확인" 실패로 절차가 끊기면 **운영 마스터 비밀번호를 쥔 태스크나 과금 중인 복원본이 남는다** — 이 plan의 핵심 가드레일(비용·시크릿)이 정확히 그때 무너진다. 다음 규율로 바꾼다.
     - **리소스 원장(ledger) — 예정값을 생성 *전에* 적는다(r5 정정)**: r4는 "만드는 즉시 기록 + 원장이 정리 대상의 **유일한** 입력"이라고 했는데, 그러면 **요청은 수락됐지만 응답 유실·터미널 종료·기록 실패로 원장에 안 남은 리소스**를 영원히 못 찾는다 — 하필 그게 과금·시크릿 잔존의 원인이다. 두 층으로 바꾼다:
       - **(1) 예정값 선기록(생성 전)**: 값을 우리가 정하는 것들은 **호출 전에** 원장에 적는다 — 드릴 DB 식별자 `linkpulse-restore-drill`, 임시 role 이름, **TD family 2개**, **드릴 토큰**, **시도별 client token 5개**, 태그 `purpose=rds-restore-drill`. 호출이 어떻게 끝나든 이 값들로 되찾을 수 있다.
         - **드릴 토큰 = `restore-drill-<YYYYMMDD-HHMMSS>`**(`--started-by` 값과 로그 스트림 접두사 공용). **날짜만 쓰면 같은 날 재드릴 시 회수 키가 겹치므로** 시각까지 넣되(r6), **분이 아니라 초까지** 넣는다(r7 — 조기 실패 후 **같은 분 안에 재드릴**하면 `-tda-1` 같은 **시도별 client token까지 그대로 재사용**돼, ECS 멱등성 TTL — **개발자 가이드 기준 최대 24시간으로 알려져 있으나 로컬 `help`에 수치가 없어 미확인이다(체크리스트 ⓖ. r10 정정 — 같은 plan이 TD "최대 1시간"에는 이미 출처·미확인 헤지를 걸어 두었는데 이 숫자만 단정형이었다)** — 그 안에서는 새 태스크가 뜨지 않고 **이전 시도의 결과가 돌아온다**. **TTL이 24시간보다 짧더라도 초 단위 토큰 결정은 바뀌지 않는다**(같은 분 안의 재드릴을 막는 것이 목적). 길이는 `restore-drill-`(**14**) + `YYYYMMDD-HHMMSS`(15) = **29자**로, 시도 접미(`-tdb-3`)를 더해도 **35자**라 **client token 64자·`--started-by` 128자 제약 안**이다(r9 수치 정정 — r8이 접두사를 13자로 잘못 셌다. 결론은 동일). 무작위 nonce는 **불채택** — 사람이 같은 **초**에 두 드릴을 시작할 수 없고, 원장에 손으로 적는 값이라 짧을수록 오기(誤記)가 줄기 때문.
         - **시도별 client token**(≤64자, `run-task help` 실측): `<드릴 토큰>-tda-1`·`-tda-2`·`-tdb-1`·`-tdb-2`·`-tdb-3`. **논리적 시도마다 값이 다르다**(아래 (4)).
       - **(4) `--started-by`와 `--client-token`은 역할이 다르다(r6 정정 — r5의 서술이 틀렸다)**: 회수를 성립시키는 것은 **`--started-by` 하나**이고, `--client-token`은 도움말상 _"An identifier that you provide to ensure the idempotency of the request … Up to 64 characters"_ 로 **요청 멱등성** 장치다. r5는 둘을 묶어 "넣지 않으면 회수 경로가 성립하지 않는다"고 적어 목적을 뒤섞었다. 사용 규율:
         - **같은 논리 시도의 재전송**(응답 유실·5xx·타임아웃으로 결과를 모를 때) → **동일 토큰 재사용.** 중복 태스크가 생기지 않고 원응답을 되받는다(= 유실 응답 회수 경로).
         - **확인된 실패 뒤 새 태스크를 띄우는 다음 논리 시도**(IAM 전파 재시도 등) → **다음 토큰.** 같은 토큰을 다시 쓰면 ECS가 최초 결과만 돌려줘 **전파가 끝나도 새 태스크가 뜨지 않는다.**
         - 토큰을 **다른 요청에 재사용하지 않는다**(충돌 가능). 근거는 ECS 멱등성 개발자 가이드 — 로컬 help는 "Ensuring idempotency" 문서를 가리킬 뿐이므로 **런북 작성 시 1차 출처로 확인**하고 ADR 인용 목록에 넣는다(§가드레일 1차 출처 규율).
       - **(2) 사후값 기록(응답 직후)**: AWS가 만드는 값 — **`run-task` 응답의 `tasks[].taskArn`을 시도별로 전부**(+`failures[]`, 시각), TD revision ARN, 드릴 시크릿 ARN, sentinel code·`created_at`, `restore point`, `InstanceCreateTime`, 측정 4시각. **각 `taskArn`의 상태 관측 이력도 남긴다(r8 필수 — MISSING 규율의 입력이다)**: `lastStatus`를 확인한 **시각과 값**, `exitCode` 확인 여부. **TD는 revision ARN마다** `deregister` 성패와 `delete-task-definitions` 응답의 **`taskDefinitions[]` 포함 여부·`status`·`failures[]`의 `reason`** 을 **따로** 적는다(r9 — 배치 API라 요청 단위 성패로는 부분 실패를 못 가린다). **런북에는 원장 표의 빈 양식 예시를 그려 둔다(r10)** — TD 칸은 `revision ARN | deregister 성패 | taskDefinitions[] 포함 | status | failures[].reason | 재전송 횟수`, 다른 항목도 마찬가지로 빈 칸 형태를 보여 주면 드릴 당일 형식을 고민하지 않는다. 이 기록이 없으면 정리 게이트에서 MISSING을 만났을 때 **"보관 만료(종결)"와 "아직 못 본 태스크(상태 불명)"를 구분할 수 없어** 전부 (A)로 떨어진다. **측정값도 같은 표에 모아** step 5에서 ADR로 옮길 때 출처가 한 곳이 되게 한다.
       - **(3) 재탐색 대사(rediscovery — 정리 게이트 진입 시 필수)**: 원장만 읽지 않는다. **고정 식별자로 AWS 상태를 다시 조회해 원장과 대사**하고, **원장 ∪ 재탐색 결과**를 정리 대상으로 삼는다(원장에 없는데 잡힌 것도 정리하고 **"원장 누락"과 발견 시각을 증거에 기록**). 경로:
         - RDS: `describe-db-instances --db-instance-identifier linkpulse-restore-drill`(NotFound = 없음), 시크릿 ARN은 이 조회에서 도출.
         - IAM: `get-role --role-name <고정 이름>`.
         - **ECS 태스크는 두 경로로 나눠야 한다(r6 전면 정정 — r5의 명령은 실행 불가였다).** `list-tasks help` 실측: _"When you specify startedBy as the filter, **it must be the only filter that you use**."_ → r5가 쓴 `--started-by` + `--desired-status` 조합은 **API 제약 위반**이다. 또 _"you can filter results based on a desired status of `PENDING`, this doesn't return any results"_ 이므로 PENDING 필터는 무의미하다. 바른 절차:
           1. **비종결 태스크** — `aws ecs list-tasks --cluster linkpulse-prod-cluster --started-by <드릴 토큰>` **단독 호출**(다른 필터 없음 — **`--cluster`는 필터가 아니라 조회 범위(스코프)라 이 제약의 예외**다. r7: 규율 문장과 실제 명령이 겉보기에 충돌해 실행자가 당일 망설이지 않도록 못박는다). 기본 desired status가 `RUNNING`이라 아직 살아 있는 시도가 잡힌다.
           2. **종료 태스크** — `aws ecs list-tasks --cluster linkpulse-prod-cluster --desired-status STOPPED`를 **페이지 끝까지** 조회한 뒤, `describe-tasks`로 각 태스크의 **`startedBy`를 클라이언트에서 드릴 토큰과 대조**한다(두 필터를 함께 못 쓰므로 서버 필터링이 불가능하다). **한 흐름으로 적는다(r10 — 실행자 입장에서 끊기지 않게)**: `list-tasks`를 **페이지 끝까지** 돌려 ARN을 모으고 → 모은 ARN을 **100개씩 잘라** `describe-tasks`에 넘긴다(**요청당 ARN 100개 상한** — r9 codex-ide, r10 claude-ide 실측 _"A list of up to 100 task IDs or full ARN entries"_) → 각 응답의 `startedBy`를 드릴 토큰과 대조한다. 태스크 이력이 많은 계정에서도 절차가 그대로 동작한다.
           3. STOPPED 태스크는 ECS가 **일정 시간만 조회 가능하게 보관**하므로 **재탐색은 드릴 종료 직후 수행**한다(보관 창의 실제 길이는 로컬 `help`에 없다 → **1차 출처 확인 대상**이며 ADR 인용 목록에 넣는다. r7). 그 창을 놓쳐 회수하지 못한 STOPPED 태스크는 **이미 종결된 것이라 (A) 조치 필요 잔존이 아니다**(비용·권한 없음) — 증거 완결성만 손해다. 다만 **원장의 `taskArn`을 `describe-tasks`로 조회했을 때의 `failures[].reason=MISSING`은 곧바로 종결이 아니다(r8 정정)** — 보관 만료일 수도, **아직 전파되지 않은 신규 태스크**일 수도 있다. ②의 **MISSING 규율**(재조회 3회 → `--started-by` 대사 → **종결 관측 이력이 있을 때만** 종결, 없으면 "상태 불명"으로 (A))을 따른다.
         - **TD**: `--status`는 **단일 enum**이므로 파이프로 묶어 쓸 수 없다 → `list-task-definitions --family-prefix <family> --status <상태>`를 **`ACTIVE`·`INACTIVE`·`DELETE_IN_PROGRESS` 3회 따로** 호출한다(r6). 다만 help상 `INACTIVE` 조회는 _"as long as an active task or service still references them"_ 제약이 있어 **참조 태스크가 없는 INACTIVE TD는 이 경로로 안 잡힐 수 있다** → **원장의 family+revision으로 `describe-task-definition`을 병행**해 보강한다(INACTIVE revision도 조회된다). r5의 "경로는 전부 실측 확인됨"은 이 한 줄에서 과한 표현이었다. **이 조회 누락은 판정을 바꾸지 않는다(r8 정정 / r9·r10 재정정)**: 판정의 축은 **`delete-task-definitions` 응답의 revision별 결과**(그 ARN이 `taskDefinitions[]`에 있고 `status=DELETE_IN_PROGRESS`인지, `failures[]`에 있는지)이고, **`describe`/`list` 조회는 결과적 일관성을 감안한 확인 증거일 뿐 판정을 뒤집지 못한다**. 따라서 **응답상 삭제가 접수된 revision은 조회에 안 잡히든 `INACTIVE`로 보이든 (A)가 아니며**, **응답에서 실패했거나 재전송 상한까지 접수 증거를 못 얻은 revision은 (A)** 다(§TD 종결 2단계의 우선순위 1~4).
         - **`run-task`에는 `--started-by <드릴 토큰>`을 반드시 넣는다** — 이것이 회수 키다(없으면 1·2 경로 모두 드릴 태스크를 식별할 수 없다). `--client-token`의 역할은 위 (4).
     - **각 항목의 공통 형태**: `존재 조회 → 있을 때만 조치 → waiter → 결과 확인`. **NotFound·이미 종결 상태는 성공(멱등)으로 취급**한다. 어느 항목이 실패해도 **기록만 하고 다음 항목을 계속**하며(한 항목의 오류로 나머지를 포기하지 않는다), **마지막에 잔존 목록으로 (d)를 판정**한다. 절차 전체를 **몇 번을 다시 돌려도 같은 결과**여야 한다.
     - **순서(과금을 먼저 끊고, 대기는 뒤로 모은다)**: ⓪ **재탐색 대사**(위 (3)) → ① 드릴 DB가 존재하면 `delete-db-instance --skip-final-snapshot --delete-automated-backups` **호출만**(비동기 — 여기서 기다리지 않는다. 과금 시계를 가장 먼저 멈추기 위함) → ② ECS 태스크: **원장 ∪ 재탐색으로 모은 전 시도**를 순회해 **세 갈래로 분류한 뒤 조치한다(r7 정정 — r6의 "`STOPPED`가 아니면 전부 `stop-task`"는 이미 끝난 태스크에까지 정지를 시도해 **정리 게이트가 오류로 멈추거나 waiter가 상한까지 헛대기**한다)** → ③ TD-A·TD-B를 **revision ARN**으로 `deregister-task-definition` → **`delete-task-definitions`**(아래 2단계 종결) → ④ 임시 role: **인라인 정책 삭제 → 관리형 정책 `detach-role-policy` → `delete-role`** → 부재 확인(관리형을 떼지 않으면 `DeleteConflict`) → ⑤ **①의 `aws rds wait db-instance-deleted` 통과 확인**(매처가 **`length(DBInstances) == 0`** 이라 "조회에서 사라짐"이 성공 조건이다 — r9) — 여기서 비로소 **과금 종료로 판정**한다 → ⑥ 드릴 관리 시크릿 부재 확인(잔존 시 **운영 ARN과 대조 후에만** 삭제) → ⑦ **운영 `linkpulse-prod-pg` 무변경 확인** → ⑧ **잔존 목록 출력(2분류)**.
     - **②의 태스크 분류 — 위에서부터 먼저 맞는 것 하나로 확정한다(우선순위 규칙. r8 정정 — r7은 (나)를 `desiredStatus`+`lastStatus` 두 키로, (다)를 `lastStatus` 한 키로 갈라 **`desiredStatus=STOPPED` ∧ `lastStatus=RUNNING`**(= `stop-task`를 이미 냈고 아직 전이 전. `StopTask` 응답 예시가 보여 주는 정상 조합이고, waiter 상한을 넘겨 빠져나온 뒤 **재진입**하면 바로 이 상태다)이 (다)로 떨어져 **중복 `stop-task`를 부르는** gap이 있었다. 비종결의 분류 키는 **`desiredStatus` 하나**로 통일한다)**:
       - **(가) 종결 — 조치 없음**: `lastStatus`가 **`STOPPED`** 또는 **`DELETED`**. `describe-tasks`가 **`failures[].reason=MISSING`** 을 준 경우는 아래 별도 규율로 판정한다(무조건 종결이 아니다).
       - **(나) 비종결 · 이미 정지 요청됨 = `desiredStatus == STOPPED`인 것 전부**(`lastStatus`가 `RUNNING`이든 `DEACTIVATING`·`STOPPING`·`DEPROVISIONING`이든 **묻지 않는다**): **`stop-task`를 다시 호출하지 않고 `wait tasks-stopped`만** 건다(중복 정지 요청이 오류가 될 수 있고, 어차피 정지 중이다).
       - **(다) 비종결 · 정지 요청 없음 = `desiredStatus == RUNNING`**: **`stop-task` → `wait tasks-stopped`**. 단순 "STOPPED 확인"이 아니다 — **운영 마스터 비밀번호를 쥔 컨테이너를 방치하지 않기 위해 정지시키고 대기**한다. 실패한 시도의 태스크도 대상.
       - **MISSING은 종결 근거가 있을 때만 종결이다(r8 신설 — 이 규율이 없으면 (d)의 시크릿 가드레일이 조용히 뚫린다)**: ECS API는 **결과적 일관성(eventual consistency)** 이라 `run-task` 직후의 조회에는 새 태스크가 **아직 안 보일 수 있고**, `MISSING`의 의미는 "찾지 못함"일 뿐이라 **보관 만료 · 전파 지연 · 잘못된 클러스터/리전/ARN을 구분해 주지 않는다.** 응답만 받고 곧바로 중단해 정리 게이트로 들어온 태스크가 일시적으로 MISSING인데 이를 종결로 세면, **그 태스크가 뒤늦게 기동해 운영 마스터 비밀번호를 주입받은 채 실행**되는데도 (d)는 PASS로 기록된다. 절차:
         1. **30초 간격으로 `describe-tasks` 재조회(상한 3회)** — `stop-task` 거부 때와 같은 상한을 쓴다.
         2. 그래도 MISSING이면 **`list-tasks --started-by <드릴 토큰>` 단독 호출로 대사**한다(전파가 늦은 태스크가 여기서 잡힐 수 있다).
         3. 그 뒤에도 MISSING이면 **원장에 그 ARN의 종결 관측 이력(`STOPPED`/`DELETED`를 한 번이라도 본 기록, 또는 `exitCode` 확인 기록)이 있는 경우에만 (가) 종결**로 인정하고 발견 시각을 증거에 남긴다(보관 창이 지난 정상 케이스).
         4. **생성 응답만 있고 종결을 한 번도 관측하지 못한 ARN은 "상태 불명"으로 (A)에 남겨 (d) FAIL**로 판정한다. **상한 초과를 종결로 승격하지 않는다** — 모르는 것을 끝난 것으로 세지 않는다.
       - **`stop-task`가 전이 상태 때문에 거부되면** 오류를 기록하고 **30초 뒤 `describe-tasks`로 재조회해 다시 분류**한다(재조회 **상한 3회** — 초과하면 비종결로 두고 (A)에 남긴다. 무한 루프 금지, §가드레일 대기 상한 규율). **waiter가 상한(600초)을 넘겨도 절차를 끊지 않는다** — 마지막 `describe-tasks` 결과로 분류해 잔존 목록 판정에 넘긴다(어느 항목이 실패해도 계속 진행한다는 공통 형태와 같다).
       - **waiter는 태스크 ARN 하나씩 건다(r8 — claude-ide 실측)**: `wait tasks-stopped`의 매처는 `tasks[].lastStatus`가 **모든 원소**에 대해 STOPPED일 때만 성공이므로, 여러 ARN을 한 번에 넘기면 **하나가 MISSING이거나 늦게 내려가는 것만으로 배치 전체가 600초를 소진**한다(대상이 최대 5건이라 최악 50분). 하나씩 걸고, 실패하면 `describe-tasks`로 개별 재판정한다.
     - **TD 종결은 2단계다(r5 신설 — 3인 공통 지적)**: `delete-task-definitions`는 **비동기**로, revision을 `INACTIVE` → **`DELETE_IN_PROGRESS`** 로 보내고 연결 태스크·서비스가 전부 종료될 때까지 그 상태에 머문다(help 실측). **전용 waiter가 없고**, 개발자 가이드 기준 영구 삭제까지 최대 1시간이 걸릴 수 있다 → 드릴 창 안에서 물리적 부재를 기다리면 **정상 동작을 FAIL로 오판하거나 실행자가 무기한 대기**한다.
       - **판정 단위는 "요청"이 아니라 "revision ARN"이다(r9 정정 — 이게 없으면 부분 실패가 (d) PASS에 섞인다)**: `delete-task-definitions`는 **한 요청에 여러 revision(최대 10개)을 받는 배치 API**이고, **HTTP 200 응답 안에 성공한 `taskDefinitions[]`와 실패한 `failures[]`를 함께** 반환한다. 따라서 **CLI 종료 코드나 "요청이 성공했다"를 원장에 한 값으로 적으면**, TD-A는 삭제 전이되고 **TD-B는 `failures[]`에 있는데도 "삭제 호출 성공"으로 기록**돼 (A)=0으로 오판한다. **원장에는 revision ARN마다** ⓐ `taskDefinitions[]`에 포함됐는지와 그 `status`, ⓑ `failures[]`에 있는지와 `reason`을 **따로** 적는다.
       - **즉시 게이트(드릴 당일) — revision ARN마다 위에서부터 먼저 맞는 것 하나로 확정한다(r10 정정: 우선순위 단일 상태 머신).** r9는 "응답이 1차 증거"(즉시 게이트)와 "`INACTIVE`가 보이면 재조회 후 (A)"(대칭 규율)를 **나란히 적고 우선순위를 안 정했다** → **응답은 `DELETE_IN_PROGRESS`인데 결과적 일관성으로 후속 조회가 잠시 stale `INACTIVE`를 반환하는 같은 실행이 한쪽에선 PASS, 다른 쪽에선 FAIL**로 갈렸다. **판정의 축은 "삭제가 접수됐다는 응답 증거"이고, 조회는 확인 증거일 뿐 판정을 뒤집지 못한다:**
         1. **응답에서 그 ARN이 `taskDefinitions[]`에 있고 `status == DELETE_IN_PROGRESS`** → **종결 확정.** 이후 조회가 `INACTIVE`를 반환해도 **뒤집지 않는다**(stale 조회 — §확정 사실 ⓗ). 관측값은 증거에만 남긴다.
         2. **응답에서 그 ARN이 `failures[]`에 있고 `reason == MISSING`** → **멱등 성공(종결)**. 단 이 작업에서 `MISSING`이 반환되는 조건은 1차 출처로 확인되지 않았으므로(체크리스트 ⓘ), **확인 전에는 그 revision의 부재를 `describe-task-definition`으로 1회 직접 확인**한 경우에만 종결로 인정한다(r10 — codex-ide).
         3. **응답에서 `failures[]`에 있고 `reason`이 `MISSING`이 아님** → **비멱등 실패 = (A)**.
         4. **접수 여부가 불명**(응답 유실·타임아웃·응답의 어느 목록에도 그 ARN이 없음) → **조회만 반복하지 않는다.** 해당 revision에 **`delete-task-definitions`를 재전송**해(30초 간격, **최초 포함 총 3회**) revision별 응답을 다시 확보하고 1~3으로 판정한다. **끝까지 접수 증거를 못 얻으면 (A)** — 조회에서 `INACTIVE`로 보이든 아니든 마찬가지다. (이미 접수된 revision에 재전송했을 때의 응답은 미확인 → 체크리스트 ⓙ. 어느 쪽이든 1~3의 분기로 흡수된다.)
         - **`deregister` 자체가 실패한 revision은 삭제를 시도할 수 없으므로 (A)** 다.
         - 근거: **TD는 과금 대상도, 권한을 보유한 주체도 아니므로** 제어면의 비동기 지연을 기다릴 이유는 없지만, **삭제가 실제로 접수됐는지는 revision별로 확인해야** 한다. `INACTIVE` 관측 자체는 판정 근거가 아니다 — **접수 증거가 있으면 종결, 없으면 4번**(r8이 지적한 "`INACTIVE`가 어느 칸에도 없다"는 구멍은 4번이 닫는다).
       - **지연 확인(후속, 드릴 종료 뒤 — 권장 익일)**: `list-task-definitions --family-prefix <family> --status DELETE_IN_PROGRESS`가 **비었는지 1회 확인**해 증거에 남긴다. 미완이면 **(d) FAIL이 아니라 후속 추적 항목**으로 남기고(연결 태스크가 아직 살아 있다는 신호이므로 **태스크 STOPPED를 재확인**한다), 원인을 기록한다.
       - **증거에 시각 2개**: **삭제 요청 시각**과 **물리적 부재 확인 시각**을 따로 남겨, 즉시 차단(권한·과금)과 제어면 지연을 사후에 구분할 수 있게 한다.
     - **잔존 목록은 2분류로 출력한다 — 이 절이 (d) 판정의 단일 소스다(r7).** goal의 (d) 축 표·허용 부수효과 표·step 4 검증은 전부 **여기를 참조**하며, 같은 판정을 각자 다시 서술하지 않는다(r6까지 세 곳에 따로 적혀 있었고, 그중 (A)만 좁아 **미완 정리가 PASS로 출력되는** 구멍이 있었다).
       - **(A) 조치 필요 잔존 — 하나라도 있으면 (d) FAIL**: ① 드릴 DB(`wait db-instance-deleted` 미통과 포함) ② 임시 role(부재 확인 실패) ③ 드릴 시크릿이 **존재하고 삭제 예약도 아닌** 상태 ④ **드릴 태스크 중 ②의 (가) 종결로 확정되지 않은 것 전부** — `PROVISIONING`·`PENDING`·`ACTIVATING`·`RUNNING`·`DEACTIVATING`·`STOPPING`·`DEPROVISIONING`, **그리고 종결 관측 이력 없이 MISSING인 "상태 불명" ARN**(r8)(r7: `stop-task`나 waiter가 실패·상한 초과해 중간 상태에 머문 태스크가 (A)에 안 잡히면 **운영 마스터 비밀번호를 쥔 컨테이너가 아직 안 내려간 상태를 "정리 완료"로 기록**하게 된다 — 이 절이 지키려던 바로 그 가드레일). **`DEPROVISIONING`을 (A)에 두는 것은 의도적 보수성이다(r8)** — 이 단계는 컨테이너가 이미 멈추고 ENI를 회수하는 중이라 "비밀번호를 쥔 컨테이너"는 아니지만, 판정 기준을 상태별로 예외 처리하면 다시 갈라지므로 **종결 아니면 (A)** 한 줄로 유지한다(waiter 상한 600초를 감안하면 이 상태로 ⑧까지 오는 경우는 드물다). ⑤ **TD revision 중 §TD 종결 2단계의 즉시 게이트 1~2번으로 종결이 확정되지 않은 것 전부**(r10) — `deregister` 실패, `failures[]`에 **`MISSING` 이외의 사유**로 잡힌 revision, **재전송 상한 3회까지 접수 증거를 못 얻은** revision. **판정 단위는 요청이 아니라 revision ARN**이고, **판정 축은 삭제 응답이지 조회 상태가 아니다** — 응답으로 접수가 확인된 revision은 조회에 `INACTIVE`가 보여도 (A)가 아니며(stale), 접수 증거가 없으면 조회 상태와 무관하게 (A)다.
       - **(B) 정상 비동기 진행 — 잔존으로 세지 않으며 (d) PASS**: TD **`DELETE_IN_PROGRESS`**, 드릴 시크릿의 **삭제 예약(복구 창 대기)**. 지연 확인 대상으로만 남긴다.
       - 둘을 섞으면 정상 정리가 FAIL로 기록되고, 반대로 **(A)를 좁게 잡으면 미완 정리가 PASS로 기록된다.** 어느 쪽도 이 드릴의 결론을 오염시킨다.
     - 정리 명령 요약은 **런북 첫머리와 마지막 양쪽**에 둬 중단 시 바로 재진입할 수 있게 한다. **중단·대기 상한 초과·운영 알람 발생도 이 게이트로 들어오는 정상 경로**다.
   - **(j) 스냅샷 복원 절** — §복원 방식의 "보조: 스냅샷 복원"이 열거한 항목(스냅샷 식별자 선정, `SnapshotCreateTime` 기반 복구 지점, baseline/sentinel 판정 조건 변경, 복원 옵션, 자격증명 전환, 구성/정리 게이트 참조)을 **온콜이 따라 할 수 있는 수준**으로 기술한다. 이번 드릴에서 실행하지는 않는다. **판정 조건 반전은 서술이 아니라 PITR/스냅샷 2열 대조표로 적는다(r5)** — 온콜이 실제 장애 중에 읽는 문서라 "뒤집힌다"는 문장만으로는 오독하기 쉽다. 최소 행: 복구 지점의 출처(`--restore-time` 지정값 / `SnapshotCreateTime` 관측값), baseline 기준 시각, sentinel-A 조건, sentinel-B 조건, 집계 부등식 방향.
   - **(k) 진단 절** — 태스크가 `exitCode != 0`이거나 기동 실패일 때 볼 것(`stoppedReason`, 컨테이너 `reason`, 흔한 원인: 로그그룹 미존재·시크릿 키 누락·플랫폼 버전·IAM 전파 지연).

   모든 명령에 "이 명령은 사람이 실행"·과금/삭제 주의 표기. 마지막에 `docs/runbooks/alarm-response.md`의 RDS 관련 절에서 이 런북으로 가는 **링크 한 줄 추가**(온콜 발견 가능성).
   → 검증: 런북이 sentinel-A/baseline→복원지점/sentinel-B→복원→구성게이트→자격증명→검증→측정→정리완료 전 구간을 담고, 쿼리가 `schema.sql`의 `links` 컬럼과 정합(`url` 미출력)하며 `pg_settings`의 `rds.force_ssl` 조회를 포함, TD 사양에 `PGPASSWORD`+`:password::`·`--platform-version 1.4.0`·digest 고정이 전부 있고, 명령이 `aws rds/ecs/iam/secretsmanager` 구문상 유효(에이전트는 실행 안 함, `--help` 참조). **정리 게이트 추가 검증(r4)**: ① 리소스 원장 표가 (a)에 있고 각 생성 단계가 원장 기입을 지시, ② 모든 정리 항목이 `존재 조회 → 있을 때만 조치` 형태이고 **NotFound를 성공으로 명시**, ③ ECS 항목이 `stop-task` + `wait tasks-stopped`를 포함(단순 확인이 아님), ④ role 순서가 **인라인 삭제 → 관리형 detach → delete-role**, ⑤ 항목 실패 시 **계속 진행 + 잔존 목록 판정** 규칙 명시, ⑥ 정리 절차가 첫머리·말미 양쪽에 존재, ⑦ 성공 판정 4축(a~d) 표가 런북에도 있어 실행자가 축별로 기록. **r5 추가 검증**: ⑧ 원장이 **예정값(생성 전)** 과 **사후값** 두 층이고, ⑨ 정리 게이트 ⓪에 **재탐색 대사**가 있으며 `run-task`에 **`--started-by`·`--client-token`** 이 들어가 있음, ⑩ `run-task` 응답의 **`taskArn`을 시도별 목록**으로 기록하고 정리가 전건을 순회, ⑪ TD 종결이 **2단계**(즉시 인정 + 지연 확인)이고 `DELETE_IN_PROGRESS`가 잔존이 아님을 명시, ⑫ 잔존 목록이 **(A) 조치 필요 / (B) 정상 비동기** 2분류, ⑬ 교차 시계 **3개 비교 표**와 `restore point ≥ T_baseline + 60초`가 있음, ⑭ (j)에 **PITR/스냅샷 2열 대조표**가 있음. **r6 추가 검증(실행 가능성)**: ⑮ ECS 태스크 재탐색이 **두 경로로 분리**돼 있고(`--started-by` **단독** / `--desired-status STOPPED` 전체 조회 + `describe-tasks`로 `startedBy` 클라이언트 대조) **`--started-by`와 다른 필터를 결합한 명령이 하나도 없음**, ⑯ `list-task-definitions --status`가 **상태별 개별 호출**로 적혀 있고 파이프 표기가 없음, ⑰ **시도별 client token**이 원장 예정값에 있고 "재전송=동일 토큰 / 다음 시도=다음 토큰" 규율이 명시, ⑱ 태스크 정리가 **우선순위 3분류**(① `lastStatus` 기준 종결 → 조치 없음 / ② `desiredStatus==STOPPED` → waiter만 / ③ `desiredStatus==RUNNING` → `stop-task`+waiter)로 적혀 있고, **비종결의 분기 키가 `desiredStatus` 하나**이며(r8 — 두 키를 섞으면 `desiredStatus=STOPPED`∧`lastStatus=RUNNING`이 중복 `stop-task`를 부른다), **잔존 목록 (A)도 같은 종결 정의를 참조**함(r7). 런북에는 **조건 우선순위 표 + 대표 조합**(`STOPPED/RUNNING`·`STOPPED/STOPPING`·`RUNNING/RUNNING`)을 함께 적어 상태명이 늘어도 분기를 놓치지 않게 한다(r8 — codex-cli 개선), ⑲ 재시도 상한이 런북 전체에서 **"최초 포함 총 N회"** 한 가지 표현이고 **상한 = 원장의 client token 개수**가 일치함(TD-A 2 = `-tda-1·2` / TD-B 3 = `-tdb-1·2·3` — 상한을 바꾸면 토큰 목록도 함께 바꾼다. r7). **r7 추가 검증**: ⑳ **(d) 판정이 잔존 2분류 한 곳에서만 정의**되고 축 표·부수효과 표·step 4는 그것을 **참조**만 함, ㉑ (A)에 **TD `ACTIVE`/`INACTIVE`·삭제 호출 실패**가, (B)에 **드릴 시크릿 삭제 예약**이 들어 있음, ㉒ 드릴 토큰이 **초 단위**(`YYYYMMDD-HHMMSS`)이고 로그 스트림 접두사·`--started-by`·client token 접두가 **한 값에서 파생**됨, ㉓ 프리플라이트에 **1차 출처 확인 체크리스트 ⓐ~ⓗ**가 있고 "확인 전 드릴 금지"가 명시됨. **r8 추가 검증**: ㉔ **MISSING 규율**(재조회 3회 → `--started-by` 대사 → **종결 관측 이력 있을 때만** 종결 / 없으면 "상태 불명"으로 (A) FAIL, **상한 초과를 종결로 승격 금지**)이 정리 게이트와 잔존 목록 양쪽에 있음, ㉕ **`wait tasks-stopped`를 ARN 하나씩** 걸라고 적혀 있고 **600초/exit 255** 숫자가 대기 상한 표에 있음, ㉖ TD 종결 판정이 **revision ARN 단위**이고(요청 단위 성패 아님), **우선순위 1~4의 단일 상태 머신**으로 적혀 있으며(① 응답 `DELETE_IN_PROGRESS`=종결 확정·**후속 stale 조회가 뒤집지 못함** ② `failures[].reason=MISSING`+부재 확인=멱등 성공 ③ 그 밖의 `failures[]`=(A) ④ 접수 불명=**삭제 재전송 3회** 후 증거 없으면 (A)), **`INACTIVE` 관측을 독립 판정 근거로 쓰는 문장이 없음**(r10), ㉗ 시크릿 ③에 **`OwningService` 확인 + 거부 시 에스컬레이션**이 있음. **r9 추가 검증**: ㉘ 원장 TD 칸이 **revision ARN마다 `taskDefinitions[]` 포함·`status`·`failures[].reason`** 을 따로 적게 돼 있음, ㉙ **`INACTIVE` 재조회 규율**(30초×3회 → 안 바뀌면 (A))이 MISSING 규율과 **대칭**으로 적혀 있음, ㉚ 대기 상한 표에 **`db-instance-available`·`db-instance-deleted` = 30초×60회=1800초/exit 255**와 **`db-instance-deleted` 매처 `length(DBInstances)==0`** 이 있음, ㉛ 종료 태스크 대사가 **"`list-tasks` 페이지 끝까지 → 모은 ARN을 100개씩 잘라 `describe-tasks`"** 한 흐름으로 적혀 있음. **r10 추가 검증**: ㉜ TD 판정에서 **조회 상태가 응답 증거를 뒤집는 문장이 없음**(stale `INACTIVE` PASS/FAIL 이중해석 제거), ㉝ **원장 표에 TD 칸의 빈 양식 예시**(revision ARN / `taskDefinitions[]` 포함 / `status` / `failures[].reason`)가 그려져 있고 다른 원장 항목도 빈 칸 예시가 있음, ㉞ 런북의 `delete-task-definitions` 명령 예시 옆에 **"최대 10 revision" 상한**이 적혀 있음(이번 드릴은 2개지만 재드릴·타 상황 오용 방지), ㉟ 미확인 수치를 본문에서 **단정형으로 쓴 문장이 없음**(ⓖ TTL·ⓘ MISSING 조건·ⓙ 재전송 응답은 전부 "확인 대상" 표기와 함께 등장).

2. **ADR 0005 작성(DR 태세) — 실측 수치는 플레이스홀더** — `docs/adr/0005-backup-restore-dr.md` 신규(맥락·결정·트레이드오프). 결정: Single-AZ + 자동백업 7일 PITR + 관리 비밀번호를 **현 태세로 채택**(비용 대비 이 규모 서비스에 복원-시간 RTO 수용), Multi-AZ·크로스리전은 범위 밖(향후). **1차 출처 URL 인용**(확인 후): PITR/`LatestRestorableTime`·트랜잭션 로그 업로드 주기(복구 지점 근거), **복원 시 관리 비밀번호 옵션의 Oracle 전용 제약**과 `modify-db-instance` 전환 흐름, 관리 시크릿 수명주기, PITR 복원본의 기본 파라미터그룹 승계, **ECS `valueFrom`의 `:json-key::` 접미 문법(r4 추가)** — 이 문법은 `aws ecs register-task-definition help`에 나오지 않고("full ARN of the secret"까지만 기술) 근거가 **ECS 개발자 가이드**이며 실증은 운영 `ecs.tf:55`가 그 형태로 동작 중이라는 사실이므로, 1차 출처 규율상 인용 목록에 넣는다. **ECS 태스크 정의 삭제 수명주기(r5 추가)** — `DELETE_IN_PROGRESS` 전이는 로컬 help로 실측했으나 **"영구 삭제까지 최대 1시간"** 은 help에 없고 개발자 가이드 근거이므로 URL을 인용하고, ADR 한계 절에 **"드릴 종료 시점에 TD 물리 삭제는 미완일 수 있다(과금·권한과 무관)"** 를 한 줄 남긴다. **ECS 멱등성 규칙(r6 추가)** — `--client-token` 재사용 시 "새 태스크 없이 최초 결과 반환", 다른 요청에 재사용 시 충돌은 **개발자 가이드(Ensuring idempotency)** 근거다(로컬 help는 그 문서를 가리키기만 한다) → URL 인용 + 런북 작성 시 확인. **ECS 태스크 수명주기·보관(r7 추가)** — `STOPPED` 뒤의 **`DELETED` 전이**, `describe-tasks`의 **`MISSING`** 반환, **STOPPED 태스크 조회 보관 창**은 로컬 help로 확인하지 못했고 **ECS 개발자 가이드(태스크 수명주기)** 근거다 → URL 인용 + 런북 작성 **전** 확인. **RDS 관리 시크릿 삭제 수명주기(r7 추가)** — 인스턴스 삭제 시 **즉시 삭제인지 복구 창을 둔 예약 삭제인지**를 확정해 인용한다(잔존 판정 (A)/(B)가 갈린다). **`DeleteTaskDefinitions` API(r10 추가)** — **최대 10 revision 입력**, **HTTP 200 안의 `taskDefinitions[]`·`failures[]` 혼재**, **성공 revision의 `DELETE_IN_PROGRESS` 상태**를 한 문서가 뒷받침하므로 **TD 삭제 판정의 1차 출처로 직접 인용**한다(codex-cli r10). **ECS 멱등성 TTL(r10 추가)** — `--client-token`의 유효 기간과 "다른 요청에 재사용 금지"의 정확한 문구를 인용한다(체크리스트 ⓖ. plan 본문의 "최대 24시간"은 확인 전까지 미확정 표기). **ECS API의 결과적 일관성(r8 추가)** — `run-task` 직후 `describe-tasks`가 태스크를 아직 못 볼 수 있다는 AWS 지침을 인용한다. 이것이 **MISSING을 종결로 세지 않는 근거**이고, ADR 한계 절에 **"정리 판정에서 `MISSING`은 종결 관측 이력이 있을 때만 종결로 인정했다"** 를 한 줄 남긴다(드릴 결론을 읽는 사람이 판정 보수성을 알 수 있게). **측정 지표 라벨을 ADR에서 고정한다(r6, r11에서 ⓪ 추가)**: ⓪ **"DB 복원 검증 소요시간" = `T_verified − T0`**(**서비스 RTO가 아니다** — cutover·서비스 검증이 범위 밖이라 끝점이 측정되지 않는다. §측정 정의), ① **"선택한 복원 지점의 나이" = `T0 − restore point`**(드릴 절차상 60초 마진 + 사람 조작 시간 포함), ② **"백업 시스템 복구 지점 지연(참고)" = `T0 − LatestRestorableTime(T0 시점)`**. 세 라벨을 step 5에서 바꾸지 않는다. **한계 절**에 "파라미터그룹은 복원에 자동 승계되지 않아 실제 DR 시 PG 재지정이 별도 단계"를 명시. **DB 복원 검증 소요시간·복구 지점 절은 플레이스홀더 + 증거 체크리스트**(단정 금지 — step 4 실측으로 step 5에서 확정, [[infra-plan-review-and-first-source]]). **이 절은 서비스 RTO를 주장하지 않는다(r11)** — 산식이 cutover·서비스 검증을 포함하지 않기 때문이며, ADR 본문에도 그 한계를 명시한다. **복구 지점 지연(시각 지표)과 sentinel 브래킷(행 수준)은 서로 다른 측정으로 분리해 기록**한다. **드릴 verdict도 4축(구성 동등성·데이터 복원·측정·정리)을 각각 PASS/FAIL로 적는다(r4)** — 부분 성공을 "성공"으로 뭉개지 않는다.
   → 검증: ADR에 맥락/결정/트레이드오프 + 1차 출처 URL, 측정 절이 **플레이스홀더+증거 체크리스트** 형태이고 **verdict 4축 표**를 포함, 범위 밖(Multi-AZ·크로스리전)·한계(PG 미승계) 명시, 런북과 상호 링크(깨진 링크 0).

3. **로컬 종합 검증** — 변경이 **문서뿐**(`.tf` 무변경)임을 확인하고, 검증 쿼리를 `schema.sql`에 대조. (선택) `docs/plans/0007-.../fixtures/`에 검증 쿼리·기대 대조표 스냅샷.
   → 검증(r3 정정 — 전역 diff 기준은 성립하지 않는다): 작업 시작 시점에 이미 `scripts/full-destroy-prod.sh`에 **무관한 기존 수정**이 있으므로 "`git diff --stat`이 `docs/…`만"은 문서가 옳아도 실패한다. 대신 ① 시작 시 `git status --short`를 기준점으로 기록하고, ② **이 task가 만든 변경**(`docs/runbooks/rds-restore.md`·`docs/adr/0005-*.md`·`docs/runbooks/alarm-response.md`·plan 디렉터리)만 diff로 검사하며, ③ **사전 존재 비문서 변경이 그대로인지**(추가 수정 0) 별도 확인한다. 그 밖에: 런북 쿼리가 스키마와 일치, 런북↔ADR↔`alarm-response.md` 상호 링크 깨짐 0.

4. **[사람] 복원 드릴 실행** (모든 `aws` CLI — 에이전트 금지, 과금 발생):

   - **원장 개시(예정값 선기록: 드릴 DB 식별자·role 이름·TD family·드릴 토큰 `restore-drill-<YYYYMMDD-HHMMSS>`·시도별 client token 5개·태그)** + `list-clusters`·셸↔AWS 시계 오차 측정 → **sentinel-A 생성(code·`created_at` 기록) → baseline 캡처(`T_baseline`, DB `clock_timestamp()`)** → `LatestRestorableTime` 폴링(**상한 30분**) → **복원 지점 확정**(`T_baseline + 60초 ≤ restore point < LatestRestorableTime`) → **60초 마진 후 sentinel-B 생성 + `created_at` 마진 검사**(미달 시 1회 재생성).
   - **`T0` 기록(`date -u`) → 그 직후 PITR 복원 호출**(`--restore-time`·운영 PG·`--no-publicly-accessible`) — **순서를 뒤집지 않는다**(T0는 호출 **직전** 시각이다. r5 — 요약이 "복원 → T0" 순으로 읽히던 표현 교정) → 응답의 `InstanceCreateTime`·T0 근처 `LatestRestorableTime` 기록 → available(**T_available**) → **구성 게이트**(abort/remediate 분리, PG는 필요 시 reboot 후 재확인).
   - **자격증명 전환**(`modify-db-instance --manage-master-user-password`) → 드릴 시크릿 `active`(**상한 15분**) + 인스턴스 `available` 복귀(**T_creds_ready**).
   - **검증 접속·쿼리**(임시 role + TD-B, `exitCode==0` 확인 후 로그 판정): 집계를 baseline과 대조하고 **sentinel-A 존재·sentinel-B 부재·`rds.force_ssl` 실효값**을 확인(**T_verified**) → **DB 복원 검증 소요시간·복구 지점 지연 기록**, 이상 유무 기록.
   - **정리 게이트(멱등·재진입 — 중단·상한 초과·운영 알람 시에도 동일 진입)**: ⓪ **재탐색 대사**(`describe-db-instances` / **`list-tasks --started-by <드릴 토큰>` 단독**[활성] + **`list-tasks --desired-status STOPPED` 전체 페이지 → `describe-tasks`로 `startedBy` 대조**[종료] / `list-task-definitions --family-prefix`를 **상태별 3회** + 원장 revision으로 `describe-task-definition` / `get-role`)로 **원장 ∪ 실제 상태**를 확정 → ① 드릴 DB `delete-db-instance` **호출** → ② **모든 시도의 태스크**를 순회해 **우선순위 3분류**(① `lastStatus` `STOPPED`·`DELETED` → 종결·조치 없음 / ② `desiredStatus==STOPPED` → **`wait tasks-stopped`만** / ③ `desiredStatus==RUNNING` → **`stop-task` → `wait tasks-stopped`**. **waiter는 ARN 하나씩**, 상한 600초. **MISSING은 재조회 3회 → `--started-by` 대사 → 종결 관측 이력이 있을 때만 종결, 없으면 "상태 불명"으로 (A)**) → ③ TD **deregister → delete**(**revision ARN마다** 응답의 `taskDefinitions[]`·`status`·`failures[]`를 원장에 기록. 판정은 **응답 기준 우선순위 1~4**: 응답 `DELETE_IN_PROGRESS`=**종결 확정**[후속 조회의 `INACTIVE`가 뒤집지 못함] / `failures[].reason=MISSING`+부재 확인=멱등 성공 / 그 밖의 실패=(A) / **접수 불명이면 삭제 재전송 30초×3회** 후에도 증거 없으면 (A) — r10) → ④ role **인라인 삭제 → 관리형 detach → delete-role** → ⑤ **`wait db-instance-deleted` 통과(= 과금 종료 판정)** → ⑥ 드릴 시크릿 부재 확인 → ⑦ **운영 `linkpulse-prod-pg` 무변경 확인**(설정·파라미터그룹·시크릿 ARN 불변) → ⑧ **잔존 목록 2분류 출력**. 각 항목은 존재 조회 후에만 조치하고 NotFound는 성공, 실패해도 다음 항목을 계속한다.
   - **[후속·권장 익일] TD 지연 확인** — `list-task-definitions --family-prefix <family> --status DELETE_IN_PROGRESS`가 비었는지 1회 확인해 증거에 기록. 미완이면 (d) FAIL이 아니라 후속 추적(태스크 STOPPED 재확인).
     → 검증: **4축 PASS/FAIL을 각각 기록** — (a) 구성 동등성(abort 항목 일치 ∧ **PG 이름 = 운영 그룹** ∧ PG `in-sync` ∧ `rds.force_ssl=on`), (b) 데이터 복원(sentinel-A 존재 ∧ sentinel-B 부재 ∧ 집계 ≥ baseline), (c) DB 복원 검증 소요시간·복구 지점 지연 숫자 확보(+ `InstanceCreateTime` 기반 제어면 단독값 병기), (d) 삭제 **완료**로 과금 0 + **§정리 게이트 「잔존 목록 2분류」 기준 (A) = 0**(r7 — 이 판정의 정의는 그 절 하나뿐이다. 태스크는 (가) 종결로 **확정되지 않으면 전부 (A)** — **종결 관측 이력 없는 MISSING = "상태 불명"도 (A)** 다(r8). (B)인 TD `DELETE_IN_PROGRESS`·시크릿 삭제 예약은 세지 않는다). 넷 전부 PASS일 때만 "드릴 성공"이며, 그 외는 부분 성공/실패로 기록한다.

5. **[사람→에이전트] ADR 확정 + 증거** — step 4 실측(4개 시각·DB 복원 검증 소요시간·복구 지점 지연·sentinel 판정·이슈)을 사람이 제공 → **ADR 0005의 측정 절을 플레이스홀더에서 실측 확정**으로 갱신, 드릴 원본 출력(복원 로그·쿼리 결과, **`url`·비밀번호 제외**)을 `docs/postmortems/evidence/rds-restore-<날짜>/`(gitignore·로컬)에 스냅샷. (선택) 짧은 드릴 기록.
   → 검증: ADR 측정 절에 **실측 숫자**가 실리고, DR 태세가 실증 위에서 마감 = goal #2·#3 종결.

## 리스크/롤백

- **운영 오조작(가장 큰 리스크)**: 복원/삭제 명령을 운영 식별자에 잘못 실행 → **드릴 식별자 `linkpulse-restore-drill` 고정**, 검증은 **읽기 전용 쿼리**만, `delete-db-instance`는 **드릴 식별자에만**(런북에 운영 식별자 금지 경고). 운영은 `deletion_protection=true`라 실수 삭제도 방어.
- **운영 시크릿 오삭제(지연 장애형 — r2 추가)**: RDS 관리 시크릿은 `rds!db-<resource-id>` 형태라 운영·드릴이 육안으로 거의 구분되지 않는다. 운영 시크릿을 지우면 **다음 태스크 기동 시 `DB_PASSWORD` 주입이 실패**해(`ecs.tf`의 `secrets`) 드릴이 끝난 한참 뒤 배포·재기동 시점에 터진다 → ① 삭제 대상 ARN은 **반드시 드릴 인스턴스 조회로 도출**, ② 운영 ARN을 미리 출력해 **금지 목록으로 대조**, ③ 대조 통과 후에만 삭제(그마저도 인스턴스 삭제로 자동 정리되면 불필요).
- **비용 방치**: 복원본 삭제를 잊으면 상시 과금 → step 4 **정리 게이트** + 런북 첫머리·말미 양쪽에 정리 명령. 드릴 인스턴스는 `--no-deletion-protection`·`--skip-final-snapshot`·`--backup-retention-period 0`으로 즉시 삭제 가능하게. **삭제는 비동기** → `wait db-instance-deleted` 통과 전에는 과금 종료로 판정하지 않는다.
- **중간 중단 시 정리가 끊기는 위험(r4 추가 — 가장 현실적인 실패 경로)**: 드릴은 baseline·복원·자격증명·검증 어디서든 멈출 수 있고, 그때는 리소스가 **일부만** 존재하거나 태스크가 `RUNNING`이다. 고정 순서 정리는 첫 항목의 NotFound·상태 불일치에서 그대로 끊겨 **과금 중인 복원본이나 운영 비밀번호를 쥔 컨테이너를 남긴다.** 완화: **리소스 원장 + 항목별 존재 조회 + NotFound=성공 + 실패해도 계속 + 잔존 목록 판정**의 멱등 정리(§정리 게이트 (i)). 정리 절차는 **몇 번 다시 돌려도 안전**해야 하며, 이것이 (d) 축의 PASS 조건이다.
- **원장 자체가 비는 위험(r5 추가 — 위 완화책의 사각지대)**: 요청이 수락된 직후 **응답 유실·터미널 종료·기록 실패**가 나면 리소스는 존재하는데 원장에는 없다. r4의 "원장이 정리 대상의 **유일한** 입력"은 바로 그 구간을 못 덮었고, 특히 **`run-task`처럼 ARN이 사후 생성되는 호출**이 취약하다(고정 식별자가 없어 되찾을 단서가 없다). 완화: **예정값 선기록**(생성 전) + **`--started-by <드릴 토큰>` = 회수 키**(원장이 비어도 이 값으로 되찾는다) + **정리 진입 시 재탐색 대사**(`list-tasks --started-by` 등). **`--client-token`은 이 완화책에 넣지 않는다(r7)** — 회수 수단이 아니라 **같은 논리 시도의 재전송을 위한 요청 멱등 장치**이고, 역할을 섞으면 §정리 게이트 (4)의 "재전송=동일 토큰 / 다음 시도=다음 토큰" 규율이 흐려진다. 원장에 없는데 잡힌 리소스도 정리하고 **"원장 누락"으로 기록**해 절차 결함을 사후에 고친다.
- **임시 IAM role 잔존**: 드릴용 role·인라인 정책을 지우지 않으면 드릴 시크릿을 읽는 권한이 남는다(시크릿이 함께 사라지면 무해하지만 위생상 잔재) → 정리 체크리스트 항목으로 고정. **관리형 정책을 detach하지 않으면 `delete-role`이 `DeleteConflict`로 실패**하므로 순서를 지킨다.
- **검증 컨테이너를 통한 자격증명 반출(r3 추가)**: TD-A는 **운영 마스터 비밀번호**를 외부 이미지 컨테이너에 주입하고 `app` SG에는 `0.0.0.0/0:443` egress가 열려 있다 → 부동 태그가 다른 이미지를 가리키거나 공급망이 침해되면 반출 경로가 성립한다. 완화: **불변 digest 고정**(TD-A·TD-B 공통, 폴백도 동일 digest), digest를 증거에 기록, 태스크는 1회성·즉시 종료. 잔여 위험(공식 이미지 자체의 신뢰)은 드릴 1회 규모에서 수용하되 ADR 한계에 한 줄 남긴다.
- **복원 지점 경계값 거부(r3 추가)**: `--restore-time`이 `LatestRestorableTime`과 같거나 이후면 요청이 거부될 수 있다(strictly before) → baseline을 이미 캡처한 뒤 막히는 상황을 피하려고 **여유를 두고 폴링 후 관측값보다 앞선 시각**을 쓴다. 그래도 거부되면 몇십 초 뒤 값을 다시 계산해 재시도(baseline 재캡처는 불필요 — `T_baseline`만 넘으면 된다).
- **검증 접속 실패**(SG/시크릿/이미지) → 폴백: (a) CloudShell VPC 환경 psql, (b) 최소한 RDS 레벨 복원 실증(available·`LatestRestorableTime`·`describe`)만이라도 기록. 끝내 불가하면 접속 설계를 런북에서 교정 후 재드릴.
- **복구 지점 지연이 예상보다 큼**: 지연이 크게 나오면 이는 **실패가 아니라 실측 발견**(ADR에 기록, 필요 시 백업/로그 설정 재검토를 후속 백로그로).
- **측정 자체가 무효화될 위험(r2 추가)**: 쓰기 트래픽이 없어 `max(created_at)` 기반 델타가 항상 0으로 나오던 r1 산식이 그 사례다 → 1차 지표를 **트래픽과 무관한 시각 기반**(`T0 − restore point`)으로 두고, 행 수준 실증은 **sentinel 계측**으로 보강한다(§측정 정의).
- **롤백**: 이 plan은 **인프라 변경이 없다**(문서만). "롤백"은 드릴 리소스 삭제(=정리 게이트)뿐이고, 운영은 전 과정에서 §운영 가드레일을 지킨다(운영 서비스·RDS 설정 무변경, 데이터 쓰기는 sentinel 최대 5건뿐 — 이 행들과 드릴 로그 스트림은 정리 대상이 아니라 **증거로 남는다**). 문서는 revert로 원복.

## 검토 반영 로그

<!-- /plan-merge가 라운드별로 기록. 형식: [rN] 리뷰어#번호 지적요약 → 반영|기각 — 사유 -->

### r1 (2026-07-27) — 3인 전원 request-changes, revision 1 대상

**결함(진행 차단)**

- [r1] codex-cli#1 / codex-ide#1 복원 명령의 `--manage-master-user-password`는 **Oracle 전용** → PostgreSQL 자격증명 흐름 성립 불가 → **반영**. 로컬 `aws rds ... help` 실측으로 확인(`Applies to RDS for Oracle only`; `modify-db-instance`는 제약 없음). §복원본 자격증명 흐름 신설, 복원 후 드릴 식별자에만 `modify-db-instance --manage-master-user-password` → `SecretStatus=active` 대기 → 드릴 인스턴스 조회로 ARN 도출. 전환 시간은 `T_creds_ready`로 RTO에 포함.
- [r1] **상충 판정**: claude-ide는 사전 확인에서 해당 플래그가 "전부 존재 ✔"라 했으나 이는 **플래그 존재만 확인**한 것이고 Constraints의 엔진 제약을 놓쳤다 → codex 2인 채택. 1차 출처 규율 사례로 §가드레일에 기록.
- [r1] codex-ide#1(후반) 관리 시크릿을 `delete-secret` 필수 단계로 둔 수명주기 오류 → **반영**. 인스턴스 삭제 시 RDS가 함께 정리하는 전제로 바꾸고 **부재 확인**을 기본, 잔존 시에만 ARN 대조 후 수동 삭제.
- [r1] codex-cli#2 / codex-ide#3 비밀번호를 run-task env override로 넘기면 `DescribeTasks`·CloudTrail·셸에 평문 잔존 → **반영**. "role 우회" 폐기, 임시 태스크정의의 `secrets`로 ARN 참조 + **드릴 시크릿 ARN 한정 임시 실행 role**(정리 게이트에서 삭제). CloudShell VPC는 1순위 폴백으로 유지.
- [r1] codex-cli#3 / codex-ide#2 / claude-ide#1 `max(created_at)` 델타는 RPO를 측정하지 못함(클릭은 타임스탬프 없음, 쓰기 트래픽 0이면 항상 0) → **반영**. §측정 정의 신설: 1차 지표 = **`T0 − restore point`**, 복원은 `--restore-time` 명시(`--use-latest-restorable-time`은 대안으로 강등), baseline은 **T0 직전 고정**, 행 수준 실증은 **sentinel 링크**, 대조는 `count/sum ≥ baseline`(복원 후 운영 재조회 금지).
- [r1] codex-ide#2(말미) "운영 쓰기를 금지한다면 손실 실증 성공판정을 낮춰라" → **기각(부분)** — sentinel 생성을 **공개 API 정상 경로**로 허용해 실증을 유지한다. 대신 "운영 무접촉"의 정의를 **구성·데이터 무변경(SELECT + 공개 API sentinel 허용)** 으로 못박아(codex-cli 개선제안·claude-ide#2와 동일 취지) 모호성을 제거.
- [r1] claude-ide#2 운영 baseline을 읽을 경로가 plan에 없음(운영도 비공개, 전역 집계 API 없음) → **반영**. baseline도 one-off run-task 1회(TD-A + 기존 실행 role)로 수행하는 단계를 실행 단계 1(b)·4에 명시.
- [r1] claude-ide#3 run-task 전제 미확정(이미지·`executionRoleArn`·**로그그룹**) → **반영**. public `postgres:16`+rate limit 폴백, TD-A는 기존 실행 role 재사용, **`logs:CreateLogGroup` 부재로 `awslogs-create-group` 사용 시 기동 실패** → 기존 `/ecs/linkpulse-prod-app` 재사용 명시.
- [r1] claude-ide#4 정리 게이트에 운영 시크릿 오삭제 방어 없음(지연 장애형) → **반영**. 리스크 절에 항목 신설(ARN 드릴 조회 도출 → 운영 ARN 금지목록 대조 → 그 후에만 삭제).
- [r1] codex-cli#4 / codex-ide#4 / claude-ide#5 복원본이 운영 파라미터그룹(`rds.force_ssl=1`)을 승계하지 않음 → **반영**. `--db-parameter-group-name`·`--no-publicly-accessible` 명시 + available 직후 **구성 게이트**(공개여부·SG/서브넷·PG `in-sync`·암호화·엔진/클래스·`DeletionProtection`) 신설, ADR 한계 절에 "PG 미승계 → 실제 DR 시 재지정 별도 단계" 기록.
- [r1] codex-ide#5 삭제 요청만으로 과금 종료 판정 → **반영**. `wait db-instance-deleted` 통과 + 최종 체크리스트(시크릿·태스크·TD·임시 role) + 정리 절차를 런북 첫머리·말미 양쪽 배치(중단 시 재진입).
- [r1] claude-ide#6 / codex-ide 개선 본문 문장 절단·마크업 파손(24~25행) → **반영**. 문장 복구·볼드 정상화.

**개선 제안**

- [r1] codex-cli / codex-ide / claude-ide RTO 시작점 표현 불일치("T0→available" vs "available에서 시작") → **반영**. 시각 4개(`T0`/`T_available`/`T_creds_ready`/`T_verified`) 기록 + **RTO = T_verified − T0** 단일 정의.
- [r1] codex-cli 프리플라이트(`sts get-caller-identity`·리전·target 부재)·드릴 태그 → **반영**(1(a), 복원 명령 `--tags`).
- [r1] codex-cli 증거에서 `url` 제외(토큰·개인정보) → **반영**. 검증 쿼리·로그·evidence 전반에 적용.
- [r1] claude-ide 비용 수치가 자체 1차 출처 규율 위반(us-east-1 단가·gp3 일할 오차) → **반영**. 단가 삭제, "추정 — 실행 전 ap-northeast-2 요금 페이지 확인" + `creating`도 과금·삭제 완료 전 종료 아님(codex-ide 개선과 동일 취지).
- [r1] claude-ide `--backup-retention-period 0`·드릴 태그 → **반영**(복원 명령에 포함).
- [r1] claude-ide 발견 가능성(`alarm-response.md`에서 링크) → **반영**(step 1 말미).
- [r1] claude-ide step 1 검증이 아직 없는 ADR 0005 링크를 확인 대상으로 삼음 → **반영**. 링크 정합 검증을 step 2·3으로 이동.
- [r1] claude-ide 시크릿 평문 노출면을 런북에 한 줄 → **반영(상위 흡수)**. 평문 주입 설계 자체를 폐기했으므로 "비밀번호를 command/override/로그/증거에 넣지 않는다"는 가드로 대체.

### r2 (2026-07-27) — 3인 전원 request-changes, revision 2 대상

> r1 결함은 3인 모두 해소로 확인(claude-ide는 표로 6건 전건 해소 명시, codex 2인도 총평에서 확인). r2 지적은 **전부 r2에서 새로 도입한 설계**에 대한 것이다. 병합 전 4건을 1차 출처로 실측했다.

**결함(진행 차단)**

- [r2] codex-cli#1 / codex-ide#3(일부) TD의 `secrets`에 JSON 키를 지정하지 않아 **SecretString 전체**가 주입돼 psql 인증 실패 → **반영**. `ecs.tf:55`가 `:password::`를 쓰는 이유가 이것임을 확인. TD-A·TD-B 모두 `name=PGPASSWORD`, `valueFrom=<ARN>:password::`로 못박고 나머지 접속정보는 비민감 env로 분리.
- [r2] codex-cli#1 / codex-ide#3 JSON 키 주입에 **Fargate platform 1.4.0 이상** 필요 → **반영**. `run-task --platform-version 1.4.0` 명시.
- [r2] codex-ide#1 부동 태그 `postgres:16` 컨테이너에 **운영 마스터 비밀번호**를 주입 + `app` SG에 443 egress → 공급망 침해 시 반출 → **반영**. `security_groups.tf`의 `app_https`(0.0.0.0/0:443) 실측 확인. TD-A·TD-B 모두 **불변 digest 고정**, 폴백도 동일 digest 보존, digest를 증거에 기록, 리스크 절 항목 신설. **ECR 미러링은 선택으로 강등** — 드릴 1회 규모에 digest 고정이면 충분하다고 판단(잔여 위험은 ADR 한계에 기록).
- [r2] codex-cli#3 임시 role 삭제 시 **관리형 정책 detach 누락 → `DeleteConflict`**, `deregister`는 TD를 지우지 않고 `INACTIVE`로 남김 → **반영**. `aws ecs delete-task-definitions` 도움말의 "You must deregister … before you delete it" 실측 확인. 정리 순서를 ①DB 삭제 waiter →②태스크 STOPPED →③TD revision ARN deregister(+필요 시 delete) →④인라인 삭제→관리형 detach→delete-role →⑤시크릿 부재 →⑥운영 무변경으로 명시하고, **성공 판정을 택한 종결 상태와 일치**시킴.
- [r2] claude-ide#1 `--restore-time`에 관측된 `LatestRestorableTime`을 그대로 넣으면 **strictly before 제약**에 걸려 거부 가능 → **반영**. help 129행 `Must be before the latest restorable time` 실측 확인. 복원 지점을 `T_baseline < restore point < 현재 LatestRestorableTime`으로 재정의, 리스크 절에 재시도 절차 추가.
- [r2] codex-ide#2 / claude-ide#3 sentinel이 복원 지점보다 앞서 **하한만 확인** → "행 수준 손실 경계" 주장이 근거보다 강함 → **반영(양방향 채택)**. 두 리뷰어가 제시한 (b)안으로 수렴: **sentinel-A(복원 지점 이전 → 존재)** + **sentinel-B(복원 지점 확정 후·T0 이전 → 부재)** 브래킷. 부수 효과로 **PITR이 지정한 restore point를 실제로 지켰는지** 검증된다(B 존재 = 실패 신호). codex-ide가 제시한 (a)안(주장 하향)은 **불채택** — API 호출 1건·무해한 행 1건 비용으로 목표를 지킬 수 있으므로 goal #3을 낮출 이유가 없다. 복구 지점 지연과 sentinel 판정은 **서로 다른 측정**으로 ADR에 분리 기록.
- [r2] codex-cli#2 / codex-ide 개선 `T_baseline`의 시계·스냅샷 일관성 미정의 → 거짓 실패 가능 → **반영**. sentinel-A 생성 후 TD-A에서 **단일 SQL**로 `clock_timestamp()`+집계+sentinel 존재를 함께 출력하고 그 **DB 시각을 `T_baseline`으로 정의**, 전 시각 UTC 통일.
- [r2] codex-ide#4 / codex-cli 개선 "운영 데이터 무변경"과 sentinel 쓰기가 **논리적으로 충돌** → **반영**. §운영 가드레일을 신설해 금지(설정 변경 API 전면·기존 행 UPDATE/DELETE·스키마)와 허용(`SELECT` + sentinel 최대 4건)을 분리 정의하고, sentinel의 **URL 형식·개수 상한·식별 표식·영구 잔존**을 사람 사전 승인 항목으로 올림. plan 전체에서 **"무접촉"이라는 표현을 폐기**.
- [r2] claude-ide#2 구성 게이트가 **교정 가능한 상태(PG `ParameterApplyStatus`)까지 전면 중단**으로 처리 → 과잉 → **반영**. 게이트를 **abort**(공개 노출·SG/서브넷·암호화·엔진/클래스·삭제보호)와 **remediate**(PG는 `reboot-db-instance` 후 재확인, 그래도 아니면 한계 기록 후 진행)로 분리. 근거로 이 레포의 `rds.force_ssl`이 `apply_method="pending-reboot"`임을 인용 — **r2가 "복원본은 신규 부팅이라 적용된다"고 단정한 것이 낙관적이었음을 인정**. reboot 소요는 RTO(단일 정의)에 포함.
- [r2] codex-ide#5 목표에 있는 **스냅샷 복원 절이 "한 줄"뿐** → 실행 불가 → **반영(절충)**. 목표에서 빼지 않는다 — 대신 §복원 방식에 스냅샷 절이 담을 항목을 명시(식별자 선정, `SnapshotCreateTime` 기반 복구 지점, **baseline/sentinel 판정 조건이 뒤집힌다**는 점, 복원 옵션, 자격증명 전환)하고 **PITR과 공통인 부분은 참조로 처리**해 분량 폭증 없이 실행 가능하게 함. goal #1에 "이번 드릴에서 실행하는 것은 PITR 하나"를 명시.
- [r2] codex-cli#4 / codex-ide 개선 step 3의 `git diff --stat`이 docs만 → **기존 `scripts/full-destroy-prod.sh` 수정 때문에 처음부터 실패** → **반영**. 실측으로 해당 M 상태 확인. 시작 시 `git status --short` 기준점 기록 → **이 task가 만든 경로만** diff 검사 → 사전 존재 변경의 불변 여부 별도 확인으로 교체.
- [r2] codex-ide#3 임시 role의 **trust policy·`iam:PassRole`·이름 충돌/잔존 검사·삭제 순서**, task role 미부여 경계 → **반영**(§태스크 기동 전제 + 프리플라이트 + 정리 순서).

**개선 제안**

- [r2] codex-ide `describe-tasks.containers[].exitCode == 0` 확인 후에만 로그 판정, 실패 시 `stoppedReason` 진단 → **반영**. TD-A·TD-B 양쪽 판정 순서에 명시하고 런북에 **(k) 진단 절** 신설.
- [r2] claude-ide `modify-db-instance` 직후 인스턴스가 `modifying`→`available`로 복귀했는지도 확인 → **반영**(`T_creds_ready` 정의 명확화).
- [r2] claude-ide IAM 전파 지연 → 첫 `run-task` 실패 시 수십 초 후 1회 재시도 → **반영**.
- [r2] claude-ide 로그 스트림 접두사를 `restore-drill-YYYYMMDD`로 고정 → **반영**.
- [r2] claude-ide / codex-cli sentinel URL을 무해한 고정값(`https://example.com/`)으로 → **반영**.
- [r2] claude-ide 임시 role 신뢰정책 JSON을 런북에 통째로 박기 → **반영**(codex-ide#3과 동일 취지, 결함으로 처리).
- [r2] codex-cli 이미지 digest 기록 → **반영**(codex-ide#1로 상향 처리).

### r3 (2026-07-27) — codex-ide `approve` / codex-cli·claude-ide `request-changes`, revision 3 대상

> r2 결함은 3인 모두 해소로 확인(claude-ide는 표로 전건, codex 2인은 총평에서). r3 지적 **10건 전부 반영, 기각 0건**. 병합 전 아래 6건을 레포·`help`로 직접 실측했다(다른 에이전트 주장 액면 수용 금지 — [[infra-plan-review-and-first-source]]): code 서버 생성(`httpapi/links.go:22-25`·`links/service.go:36-49`) ✔, 클러스터 `linkpulse-prod-cluster`(`ecs.tf:1`+`locals.tf:2`) ✔, `created_at`은 `RETURNING`(`links/postgres.go:22-26`) ✔, 알람 차원(`monitoring.tf:160-203·225-279`) ✔, 레이트리밋 20/min·burst 10(`ratelimit.go:18-19`) ✔, `aws ecs stop-task`·`wait tasks-stopped` 존재 ✔.

**결함(진행 차단)**

- [r3] codex-cli#1 [높음] 정리 게이트가 "실패·중단 시에도 동일 진입"을 충족하지 못함 — 고정 순서는 **정상 종료만** 처리하고, 중단 시 첫 `delete-db-instance`의 NotFound나 "STOPPED 확인" 실패로 절차가 끊겨 **과금 중인 복원본·운영 시크릿을 쥔 태스크가 잔존** → **반영**. §(i)를 **멱등·재진입 절차로 전면 재작성**: **리소스 원장**(DB 식별자·태스크 ARN·TD revision ARN·role 이름·시크릿 ARN·sentinel code) 신설, 모든 항목을 `존재 조회 → 있을 때만 조치 → waiter → 확인` 형태로, **NotFound=성공(멱등)**, **한 항목 실패해도 계속 진행 후 잔존 목록으로 (d) 판정**. ECS는 "확인"이 아니라 `RUNNING`/`PENDING`이면 **`stop-task` → `wait tasks-stopped`**(실측). 리스크 절에 "중간 중단 시 정리가 끊기는 위험" 항목 신설. **단, 순서는 지적 그대로 두지 않고 조정했다(메인 코더 판단)** — `delete-db-instance`는 **호출만 먼저**(비동기, 과금 시계 우선 정지) 하고 **waiter는 IAM 정리 뒤 ⑤로 미룬다.** 삭제 대기(수 분) 동안 운영 비밀번호를 쥔 태스크를 방치하지 않으면서 과금 시계도 가장 먼저 멈추는, 두 순서 모두보다 나은 배치라고 판단.
- [r3] codex-cli#2 [중간] PG가 끝내 `in-sync`가 아닐 때 "기록 후 진행"과 성공 조건이 충돌 → 미충족 드릴을 성공으로 오분류 → **반영**. 성공 판정을 **4축 PASS/FAIL 표**(구성 동등성·데이터 복원·측정·정리)로 분해하고 **전체 verdict = 넷 전부 PASS**로 못박음. `in-sync` 실패 시 **검증은 계속하되 (a)는 FAIL 확정**, 결과는 "데이터 복원 PASS / 구성 동등성 FAIL"의 **부분 성공**으로 ADR에 기록. 지적대로 **`in-sync`가 아니면 `rds.force_ssl` 적용이 미입증이므로 구성 동등성 PASS로 처리하지 않는다.**
- [r3] claude-ide#1 [중간] goal #4 "운영 인프라를 일절 바꾸지 않는다"가 여전히 과장 — 실제로는 임시 TD 2개(`INACTIVE` 잔존)·임시 IAM role·**운영 클러스터에서의 1회성 태스크 2회**·운영 로그그룹 스트림을 만든다. 게다가 **`run-task --cluster`가 plan 어디에도 없다** → **반영**. goal #4를 "**운영 서비스·RDS 설정 무변경** + 드릴이 만드는 운영 계정 리소스는 열거 항목으로 한정·전량 정리"로 낮추고, §운영 가드레일에 **허용되는 운영 계정 부수효과 표**(부수효과 → 대상 → 정리 게이트 종결 상태)를 신설. 클러스터가 `linkpulse-prod-cluster` **하나뿐이라 운영 클러스터를 쓸 수밖에 없다**는 사실을 실측해 확정 사실·표·프리플라이트에 명시. (r2에서 RDS 축만 고친 것과 같은 종류의 과장이 한 층 위에 남아 있었다는 지적이 정확했다.)
- [r3] claude-ide#2 [중간] sentinel-B "부재" 판정에 최소 마진·시각 기록 규율이 없어 **정상 복원을 실패로 오판** 가능(restore point와 B 생성 사이가 몇 초면 DB 시계 vs RDS 제어면 시계 오차만으로 뒤집힌다) → **반영**. codex-ide 개선("B의 `created_at`이 restore point보다 **엄격히 뒤**인지 T0 전 검사")과 **통합**해 하나로 확정: **restore point 확정 후 60초 이상 뒤에 B 생성 → 응답 `created_at`(DB 시계)이 `restore point + 60초`보다 뒤인지 T0 전에 검사 → 미달이면 1회 재생성**. B가 존재했을 때 **`created_at` − restore point를 함께 기록**해 "복원 지점 미준수 vs 마진 부족"을 증거로 구분. 재생성 여유 때문에 **sentinel 상한을 4건 → 5건(A≤3 + B≤2)으로 상향**했다(사람 사전 승인 항목이라 명시적으로 변경).

**개선 제안**

- [r3] codex-cli / codex-ide / claude-ide "code 접두 등 식별 표식"은 불가 — 공개 DTO가 `url`만 받고 code는 서버가 무작위 생성 → **반영**(3인 공통, 실측 확인). 식별 기준을 **응답 code 목록 + A/B 구분 + 응답 `created_at`** 으로 교체.
- [r3] codex-cli 폴링·재시도에 상한이 없어 절차가 무기한 멈출 수 있음 → **반영**. §가드레일에 **대기 상한 규율** 신설(`LatestRestorableTime` 30분 / 시크릿 `active` 15분 / IAM 전파 30초×3회 / waiter 초과 시 `describe`로 직접 판단), **초과 시 중단 → 정리 게이트 진입이 정상 경로**임을 명시.
- [r3] codex-cli 시각의 시계·UTC 직렬화 형식 미고정 → **반영**. §측정 정의에 **시각 직렬화·시계 출처 고정** 신설(`YYYY-MM-DDTHH:MM:SSZ`, 항목별 시계 출처 명시, 섞이는 유일한 비교는 B의 60초 마진으로 흡수, 기록에 시계 종류 병기).
- [r3] codex-ide `sslmode=require`는 "이 연결이 TLS였다"의 증거일 뿐 "서버가 비TLS를 거부한다"의 증거가 아니므로 **`rds.force_ssl` 실효값**을 증거로 → **반영(구현만 변형)**. 검증 SQL에 추가하되 **`SHOW`가 아니라 `SELECT … FROM pg_settings WHERE name='rds.force_ssl'`** 을 쓴다 — `SHOW`는 GUC 미등록 시 **에러로 배치를 중단**시켜 다른 검증 결과까지 잃기 때문(미등록이면 `pg_settings`는 0행). 이 값을 성공 판정 (a)의 조건으로 승격.
- [r3] claude-ide 알람 간섭 없음을 근거로 문서화 + **역방향 규칙** → **반영**. 실측(ECS=`ClusterName`+`ServiceName`, RDS=운영 식별자)을 확정 사실·가드레일에 기록하고, **드릴 중 실제 알람이 울리면 드릴 중단 → 정리 게이트 → 알람 대응 우선**(운영 사고 > 드릴)을 명시.
- [r3] claude-ide sentinel 생성 시 `429` 대비 한 줄 → **반영**(20/min·burst 10 실측 인용, "앱 장애가 아니라 레이트리밋" 명시).
- [r3] claude-ide `:password::` 접미 문법은 `register-task-definition help`에 없고 근거가 ECS 개발자 가이드 → **반영**. ADR 1차 출처 인용 목록에 항목 추가(실증은 운영 `ecs.tf:55`).
- [r3] claude-ide TD 종결 상태를 실행자가 당일 고르지 말고 기본값 하나로 고정 → **반영**. **기본 종결 = `delete-task-definitions`까지 삭제**로 고정(드릴 잔재를 남기지 않는 쪽), 성공 판정 (d)와 부수효과 표에 반영.
- [r3] claude-ide sentinel `code`·응답 `created_at`을 드릴 기록 **필수 항목**으로 → **반영**(#2와 묶어 원장 기입 항목으로 승격).

### r4 (2026-07-27) — 3인 전원 request-changes, revision 4 대상

> claude-ide가 r3 결함 2건·개선 5건 **전건 해소**를 표로 확인. r4 지적은 **전부 r4에서 새로 도입한 규율(원장·태스크 상한·TD 종결·시계 고정)의 마감 문제**이고, 그중 셋은 **내가 r3 병합에서 리뷰어 처방을 벗어나 판단을 얹었던 지점**에 정확히 꽂혔다. **지적 10건 전부 반영, 기각 0.** 병합 전 아래 5건을 로컬 `help`로 직접 실측: `run-task --started-by`(≤128자)·`--client-token` 존재 ✔, `list-tasks --started-by/--family/--desired-status` 필터 존재 ✔, `delete-task-definitions`가 _"immediately transitions from the INACTIVE to `DELETE_IN_PROGRESS`"_ + _"stay … until all the associated tasks and services have been terminated"_ ✔(전용 waiter 없음), `list-task-definitions --status`에 `DELETE_IN_PROGRESS` 유효값 포함 ✔, `restore-db-instance-to-point-in-time` 응답에 `DBInstance.InstanceCreateTime`(help 1268행) ✔.

**결함(진행 차단)**

- [r4] codex-cli#1 [높음] 원장을 "정리 대상의 **유일한** 입력"으로 두면 **요청 수락 직후 응답 유실·터미널 종료·기록 실패** 구간을 복구하지 못한다 — 특히 ARN이 사후 생성되는 `run-task`는 되찾을 단서가 없어 과금·자격증명 경로가 남는다 → **반영**. r4의 그 문장이 과했음을 인정하고 원장을 **2층 + 재탐색**으로 재설계: **(1) 예정값 선기록**(생성 **전**에 드릴 DB 식별자·role 이름·TD family·**드릴 토큰**·태그) **(2) 사후값 기록**(시도별 `taskArn`·`failures[]`·revision ARN·시크릿 ARN·측정값) **(3) 정리 진입 시 재탐색 대사** — `list-tasks --cluster … --started-by <드릴 토큰>` 등 고정 식별자로 AWS를 재조회해 **원장 ∪ 실제 상태**를 정리 대상으로 삼고, 원장에 없는데 잡힌 것도 정리하며 "원장 누락"으로 기록. 이를 성립시키려고 **`run-task`에 `--started-by`·`--client-token`을 필수화**했다(넣지 않으면 회수 경로 자체가 없다). 리스크 절에 "원장 자체가 비는 위험" 항목 신설.
- [r4] codex-cli#2 / codex-ide#2 [중간] 허용 부수효과의 **"태스크 실행 2회"** 와 **"최대 3회 재시도"** 가 충돌 — 실패 시도도 태스크·로그 스트림을 만들 수 있는데 원장은 TD별 ARN 한 칸뿐이라 정리·검증에서 누락된다 → **반영**. ① 재시도 횟수의 의미를 **"최초 호출을 포함한 총 시도 상한"** 으로 고정(**TD-A 2회 · TD-B 3회**), ② 허용 부수효과 표·goal #4를 **"1회성 태스크 실행 최대 5회"** 로 동기화(사람 사전 승인 값이므로 명시적으로 변경), ③ 원장을 **시도별 목록**(`taskArn`·`failures[]`·시각)으로 바꾸고 ④ 정리 게이트가 **성공·실패 무관 전건을 순회해 STOPPED 확인**하도록 수정.
- [r4] codex-cli#3 / codex-ide#1 / claude-ide#2 [중간·중간·낮음] **TD 삭제는 비동기**(`DELETE_IN_PROGRESS`, 전용 waiter 없음, 가이드상 최대 1시간)인데 r4는 "기본 종결 = 삭제"·"잔존 0"으로만 판정해 **정상 정리를 (d) FAIL로 오판하거나 실행자를 무기한 대기**시킨다 → **반영(3인 통합)**. **§TD 종결 2단계** 신설: **즉시 게이트**(`deregister` 성공 ∧ `delete` 호출 성공 ∧ 상태가 `DELETE_IN_PROGRESS` 또는 조회 불가 → **종결 인정**. 근거 = **TD는 과금 대상도 권한 보유 주체도 아니므로** 비용·시크릿 가드레일과 무관하다) + **지연 확인**(권장 익일 `list-task-definitions --status DELETE_IN_PROGRESS` 1회 확인, 미완이면 **(d) FAIL이 아니라 후속 추적** — 연결 태스크 생존 신호이므로 STOPPED 재확인). **잔존 목록을 (A) 조치 필요 / (B) 정상 비동기 2분류**로 출력하도록 (d) 축 재정의. codex-cli가 요구한 "상한 있는 폴링"은 **불채택** — 1시간 폴링은 드릴 창을 넘겨 실행을 막고, codex-ide가 제시한 2단계가 같은 목적(오판·무기한 대기 방지)을 비용 없이 달성하므로 그쪽으로 수렴. 증거에 **삭제 요청 시각 + 물리적 부재 확인 시각**을 분리 기록(codex-ide 개선).
- [r4] claude-ide#1 [중간] r4의 **"시계 축이 섞이는 유일한 비교는 sentinel-B"** 단정이 틀렸다 — 실제로는 **셋**이고, 그중 **(i) `T_baseline`(DB) vs `restore point`(제어면)** 은 **성공 판정 (b)의 근거 자체**인데 정성 표현("여유 있게")만 있어 DB 시계가 앞서면 **정상 복원을 (b) FAIL로 오판**한다(B에는 숫자를 박고 정작 판정 근거 쪽에는 없었다) → **반영**. ① §측정 정의에 **교차 시계 3개 비교 표**를 만들어 각각의 흡수 방법을 명시, ② **(i)에 `restore point ≥ T_baseline + 60초` 숫자 마진** 부여(실행 단계 (c)·step 4 동기화), ③ **(ii)** 는 프리플라이트에서 **셸↔AWS 시계 오차 1회 측정·기록** + 복원 응답 **`InstanceCreateTime`(제어면)** 으로 **제어면 단독 산출값 병기**(claude-ide가 제시한 두 옵션을 모두 채택 — 1차 지표가 ADR에 실릴 숫자라 증거를 두껍게).

**개선 제안**

- [r4] codex-ide "계정 유일 클러스터"는 레포 Terraform만으로 **AWS 계정 전체의 유일성을 증명할 수 없다** → **반영**. 표현을 "이 서비스의 **Terraform 관리 운영 클러스터**"로 좁히고 **프리플라이트 `list-clusters` 실측**을 근거로 추가(r3 병합에서 내가 단정한 문구를 1차 출처 규율대로 교정).
- [r4] codex-cli 사람 실행 요약이 "PITR 복원 → T0 기록" 순으로 읽혀 측정자가 **호출 뒤에 시각을 잡을 수 있다** → **반영**. step 4를 **"`T0` 기록 → 그 직후 복원 호출"** 로 통일하고 순서를 뒤집지 말라고 명시.
- [r4] codex-cli 60초 마진 때문에 `T0 − restore point`에 **의도적 대기가 최소 60초 포함** → 백업 시스템의 능력치로 오독될 수 있다 → **반영**. 이 값을 **"이번 드릴에서 선택한 복원 지점의 나이"** 로 표기하고, 시스템 능력치는 **T0 근처 `LatestRestorableTime`** 을 별도 관측·기록해 구분(ADR에 과소평가된 숫자가 박히는 것을 막는다).
- [r4] claude-ide TD-A 실패 재실행 시 **sentinel-A를 다시 만들 필요가 없다**(상한 3건 미소모) → **반영**(실행 단계 (b) 한 줄).
- [r4] claude-ide `LatestRestorableTime` 폴링 상한 초과는 **의미 있는 DR 발견**이므로 기록 형식을 고정 → **반영**. `T_baseline`·최종 관측값·경과 시간을 남기도록 대기 상한 규율과 (c)에 명시(실패한 드릴에서도 ADR에 쓸 숫자가 나온다).
- [r4] claude-ide 원장에 `restore point`·`InstanceCreateTime`·측정 4시각도 모으기 → **반영**(사후값 층에 포함 — step 5에서 ADR로 옮길 때 출처 단일화).
- [r4] claude-ide (j) 스냅샷 절의 판정 반전을 **PITR/스냅샷 2열 대조표**로 → **반영**(온콜이 장애 중에 읽는 문서라 서술만으로는 오독 위험 — 최소 행까지 지정).

### r5 (2026-07-27) — 3인 전원 request-changes, revision 5 대상

> **판정 오류를 먼저 기록한다.** r4 병합에서 나는 "r5 수정은 리뷰어 처방의 직접 구현이라 신규 결함 위험이 낮다"고 보고 `status: approved`로 전환했다. **틀렸다.** r5에서 내가 직접 작문한 **재탐색 명령**이 ECS API 제약을 위반해 **실행 자체가 불가능**했고, 3인이 전원 이를 잡아냈다. 교훈: 리뷰어 처방을 구현하더라도 **내가 새로 조합한 CLI 절차는 반드시 1차 출처로 검증**해야 하며, 그 검증 전에 승인하지 않는다([[infra-plan-review-and-first-source]]). claude-ide는 r4 지적 2건·개선 4건 전건 해소를 확인했다. **지적 9건 전부 반영, 기각 0.**
>
> 병합 전 실측(로컬 `help`): `list-tasks` **80-81행 _"When you specify startedBy as the filter, it must be the only filter that you use."_** ✔ / **96-100행 _"you can filter results based on a desired status of `PENDING`, this doesn't return any results … only a task's `lastStatus` may have a value of `PENDING`"_** ✔ / `run-task --client-token` _"ensure the **idempotency** of the request … Up to **64** characters"_ ✔ / `list-task-definitions --status`는 **단일 enum**이고 `INACTIVE` 조회에 _"as long as an active task or service still references them"_ 제약 ✔.

**결함(진행 차단)**

- [r5] codex-ide#1 [높음] / codex-cli#1 [중간] **`--started-by`와 `--desired-status`를 결합한 재탐색 명령이 ECS 제약 위반**이라 정리 게이트 ⓪이 실패한다. `desiredStatus=PENDING`은 항상 빈 결과다 → **반영(전면 재작성)**. §정리 게이트 (3)의 ECS 경로를 **두 갈래로 분리**: ① 비종결 태스크 = **`list-tasks --cluster … --started-by <드릴 토큰>` 단독 호출**(기본 desired status가 RUNNING) ② 종료 태스크 = **`list-tasks --desired-status STOPPED` 페이지 끝까지 조회 → `describe-tasks`로 `startedBy`를 클라이언트에서 대조**(두 필터를 함께 못 쓰므로 서버 필터링 불가). 아울러 codex-ide 지적대로 **정지 대상을 `RUNNING`/`PENDING` 두 상태가 아니라 `lastStatus`가 `STOPPED`가 아닌 전 상태**(`PROVISIONING`·`ACTIVATING` 등 수명주기 중간 포함)로 확장. 확정 사실의 "토큰 하나로 모든 시도 회수 가능"도 삭제·정정.
- [r5] codex-cli#2 [중간] / codex-ide#2 [중간] / claude-ide#2 [낮음] **`--client-token`의 시도별 값·재사용 규칙 부재** — 같은 토큰으로 "재시도"하면 ECS가 최초 결과만 반환해 **IAM 전파가 끝나도 새 태스크가 뜨지 않는다** → **반영**. §정리 게이트에 **(4) 토큰 규율** 신설: **`--started-by` = 회수 키 / `--client-token` = 요청 멱등**으로 역할 분리(r5가 둘을 묶어 "회수 경로"라 한 서술은 오류 — claude-ide 지적), **논리적 시도별 고유 토큰 5개(`-tda-1`·`-tda-2`·`-tdb-1~3`, ≤64자)를 호출 전 원장에 기록**, **같은 논리 시도의 재전송(응답 유실·5xx)에만 동일 토큰 재사용**(= 유실 응답 회수 경로), **확인된 실패 후 새 태스크를 띄우는 다음 시도에는 다음 토큰**. 세 리뷰어의 표현이 조금씩 달랐는데(claude-ide "재시도에 동일 토큰" vs codex 2인 "재전송과 새 시도를 구분"), **codex 쪽이 정확**하다 — claude-ide 문구를 그대로 쓰면 IAM 전파 재시도가 무효화된다. codex 형태로 확정.
- [r5] codex-cli#3 [중간] / codex-ide#2(말미) **TD-B 재시도 상한이 plan 안에서 충돌** — §태스크 기동 전제만 "최대 3회 재시도"(=총 4회)로 남아 부수효과 상한 5회를 넘었다 → **반영**. 해당 문장을 **"최초 호출 포함 총 3회(= 재시도 최대 2회)"** 로 통일. 검증 항목 ⑲로 런북까지 전파되게 고정.
- [r5] claude-ide#1 [낮음] **§복원 방식만 r3판 옛 규칙**(`T_baseline < restore point`)이라 restore point 규칙이 plan 안에 두 개 → **반영**. `T_baseline + 60초 ≤ restore point < 현재 LatestRestorableTime`으로 통일하고 하한 근거를 교차 시계 (i)로 링크.

**개선 제안**

- [r5] codex-cli / codex-ide `list-task-definitions --status`는 **단일 enum**이라 `ACTIVE|INACTIVE|DELETE_IN_PROGRESS` 표기가 파이프 리터럴로 오독된다 → **반영**(상태별 3회 개별 호출로 풀어 씀 + 검증 항목 ⑯).
- [r5] claude-ide `--status INACTIVE` 조회는 _"참조하는 활성 태스크·서비스가 있는 동안"_ 제약이 있어 **참조가 끊긴 INACTIVE TD는 누락될 수 있다**("경로는 전부 실측 확인됨"이 과한 표현) → **반영**. 표현을 낮추고 **원장 family+revision으로 `describe-task-definition` 병행**을 보강책으로 추가. 결과 영향 없음(INACTIVE TD는 과금·권한 없음)도 함께 기록.
- [r5] codex-cli §검증 접속의 "태스크는 실행 후 소멸해 지속 인프라가 남지 않는다"가 r5의 STOPPED 조회·TD 지연 확인과 모순 → **반영**. "**컴퓨팅은 종료(과금·권한 없음), 제어면 기록은 일정 시간 잔존**"으로 교정.
- [r5] codex-ide STOPPED 태스크 조회는 **보존 기간에 의존**하므로 재탐색을 드릴 종료 직후 수행하고 **원장 누락 발견 시각도 증거에** → **반영**. 창을 놓쳐 회수 못 한 STOPPED 태스크는 **이미 종결이라 (A) 조치 필요 잔존이 아님**(증거 완결성만 손해)도 명시.
- [r5] claude-ide 드릴 토큰이 `restore-drill-YYYYMMDD`면 **같은 날 재드릴 시 회수 키가 겹친다** → **반영**. 토큰 형식을 **`restore-drill-<YYYYMMDD-HHMMSS>`** 으로 바꾸고 본문 전체(로그 스트림 접두사 포함) 동기화(`--started-by` 128자라 여유 충분).
- [r5] claude-ide 복구 지점 지연의 **ADR 라벨 2개를 지금 확정** → **반영**. ① "선택한 복원 지점의 나이" = `T0 − restore point` ② "백업 시스템 복구 지점 지연(참고)" = `T0 − LatestRestorableTime(T0 시점)` — step 5에서 바꾸지 않는다.
- [r5] claude-ide **abort 경로에도 sentinel이 이미 존재**하므로 중단 기록에 code·`created_at`을 남길 것 → **반영**(구성 게이트 abort 절에 한 줄).

### r6 (2026-07-27) — 3인 전원 request-changes, revision 6 대상

> r5 결함은 3인 모두 해소로 확인(claude-ide는 표로 결함 2건·개선 4건 전건, codex 2인은 총평에서). **r6 지적 3건·개선 6건 전부 반영, 기각 0**(nonce만 부분 불채택 — 아래).
>
> **이번 병합은 실측을 하지 못했다.** codex-cli#1의 신규 주장(ECS `DELETED` 상태·`stop-task`의 종결 태스크 거부·`wait tasks-stopped`의 MISSING 처리)을 로컬 `aws ecs ... help`로 확인하려 했으나 **CLI 실행이 승인되지 않았다.** [[infra-plan-review-and-first-source]] 규율상 **다른 에이전트의 주장을 검증 없이 plan의 확정 사실로 올리지 않으므로**, 세 주장은 §확정 사실에 **"미실측"으로 명시**하고 프리플라이트 **1차 출처 체크리스트 ⓐ~ⓖ**(확인 전 드릴 금지)로 넘겼다. 절차 자체는 **확인 전에도 안전한 방향**(종결 집합을 넓게 인정 + 이미 정지 요청된 태스크는 waiter만)으로 설계했다. **r5의 교훈("내가 새로 조합한 CLI 절차는 1차 출처 검증 전에 승인하지 않는다")을 그대로 적용해 이번에도 `status: approved`로 올리지 않는다** — ②의 3분류는 리뷰어 처방이 아니라 **내가 새로 조합한 상태 머신**이다.

**결함(진행 차단)**

- [r6] codex-cli#2 / codex-ide#1 / claude-ide#1 [중간·중간·낮음] **3인 공통** — r6이 정지 대상을 "`STOPPED`가 아닌 전 상태"로 넓혔는데 **잔존 목록 (A)만 `RUNNING`/`PENDING`으로 남아** (d)가 거짓 PASS할 수 있다(`stop-task`·waiter가 실패해 `DEACTIVATING`에 머문 태스크가 (A)에 안 잡힌다 → **운영 마스터 비밀번호를 쥔 컨테이너가 안 내려간 상태를 "정리 완료"로 기록**) → **반영(3인 통합)**. ① **잔존 2분류 절을 (d) 판정의 단일 소스로 승격**하고 축 표(goal)·부수효과 표·step 4 검증은 **참조만** 하게 바꿨다(claude-ide 개선 #3의 구조 처방 채택 — 같은 판정이 세 곳에 각각 서술된 것이 이 결함의 뿌리다), ② (A)에 **종결 집합에 들지 않는 태스크 전부**와 **TD `ACTIVE`/`INACTIVE`·삭제 호출 실패**(codex-cli#2)를 넣고, ③ 검증 항목 ⑱을 "잔존 목록도 같은 정의를 쓴다"로 확장(claude-ide 처방).
- [r6] codex-cli#1 [중간] **종결 판정·정지 명령이 상태 수명주기를 반영하지 않는다** — `STOPPED`만 종결로 보면 `DELETED`·MISSING 태스크에 `stop-task`를 다시 호출해 오류가 나고, `wait tasks-stopped`는 `lastStatus==STOPPED`만 성공으로 봐 이미 사라진 태스크에서 상한까지 대기한다 → **반영(단, 근거는 미실측으로 명시)**. §정리 게이트 ②를 **3분류 상태 머신**으로 재작성: **(가) 종결**(`STOPPED`·`DELETED`·MISSING) = 조치 없음 / **(나) 이미 정지 요청됨**(`desiredStatus==STOPPED`) = **waiter만** / **(다) 정지 요청 없음** = `stop-task`+waiter. `stop-task` 거부 시 **30초 뒤 재조회(상한 3회)**, **waiter 상한 초과도 절차를 끊지 않고** 마지막 `describe-tasks` 결과로 분류해 (A)/(B)에 넘긴다. 지적의 사실 주장 3건은 확인하지 못했으므로 **§확정 사실에 "미실측"으로 적고** 프리플라이트 체크리스트로 넘겼다(위 서두).
- [r6] claude-ide#2 [낮음] **잔존 2분류에 "드릴 시크릿 삭제 예약"을 담을 칸이 없다** — plan 스스로 관리 시크릿 수명주기를 1차 출처 확인 대상으로 열어 뒀는데, 복구 창을 둔 예약 삭제라면 ⑥의 "부재 확인"에서 **존재하되 삭제 예약됨**으로 보여 (A)로 떨어져 **(d) 거짓 FAIL** → **반영**. §자격증명 흐름 5를 **3결과 판정**(부재 / 삭제 예약 → **(B)** / 존재·예약 아님 → 대조 후 삭제)으로 바꾸고 (B) 분류에 시크릿 삭제 예약을 추가했다. 예약된 시크릿은 값 조회가 거부되고 임시 role도 이미 삭제돼 **권한·과금이 없다** — TD `DELETE_IN_PROGRESS`와 같은 성질이라 (B)가 맞다.

**개선 제안**

- [r6] codex-cli / codex-ide **드릴 토큰이 `-HHMM`이면 같은 분 안에 재드릴 시 시도별 client token까지 재사용**된다(멱등 TTL 최대 24시간 → 새 태스크가 안 뜨고 이전 결과 반환) → **반영**. 토큰을 **`restore-drill-<YYYYMMDD-HHMMSS>`**(초 단위)로 바꾸고 본문 7곳을 동기화. 길이 34자로 client token 64자 제약 안. **무작위 nonce는 불채택** — 사람이 같은 **초**에 두 드릴을 시작할 수 없고, 원장에 손으로 적는 값이라 짧을수록 오기가 준다(초 단위로 목적은 이미 달성).
- [r6] codex-cli / codex-ide 리스크 절(원장 유실)이 `--started-by`와 `--client-token`을 **함께 "되찾을 단서"** 로 서술해 본문의 역할 분리와 어긋남 → **반영**. 완화책을 **`--started-by` = 회수 키**로 한정하고 `--client-token`을 **명시적으로 제외**(요청 멱등 장치). r5 개선에서 낮추기로 한 "경로 전부 실측 확인"이 이 문장에만 남아 있던 것도 함께 삭제.
- [r6] claude-ide **`--cluster`가 "단독 필터" 제약의 예외인 이유를 한 줄** → **반영**(재탐색 경로 1에 괄호로 명시 — 규율 문장과 명령이 겉보기에 충돌해 실행자가 당일 망설일 수 있다).
- [r6] claude-ide **STOPPED 태스크 보관 창을 1차 출처 항목으로** → **반영**. 재탐색 3에 "보관 창 길이는 미확인"을 적고 ADR 인용 목록·프리플라이트 체크리스트 ⓓ에 추가.
- [r6] claude-ide **(d) 판정 산출물을 한 곳으로 수렴**(잔존 2분류를 단일 소스로, 축 표는 참조) → **반영**. 위 결함 #1의 구조적 처방으로 채택 — 이번 라운드 결함의 뿌리가 "같은 판정을 세 곳에 각각 서술"이라는 진단이 정확하다.
- [r6] claude-ide **⑲에 "상한 = client token 개수" 일치 검사** → **반영**(검증 항목 ⑲ 확장 — 이후 상한이 바뀔 때 토큰 목록이 함께 안 바뀌는 것을 막는다).

### r7 (2026-07-28) — 3인 전원 request-changes, revision 7 대상

> claude-ide가 r6 결함 2건·개선 4건 **전건 해소**를 표로 확인. **r7 지적 3건·개선 6건 전부 반영, 기각 0.** 이번 라운드 지적은 **전부 r7에서 내가 새로 작문한 상태 머신(②의 3분류·MISSING 처리)** 에 꽂혔다 — r6 병합 보고에서 "이건 리뷰어 처방이 아니라 내가 새로 조합한 것이라 승인하지 않는다"고 판단한 지점이 정확히 결함으로 확인됐다. **`status: approved`로 올리지 않은 판단이 옳았다.**
>
> **판정: `status: approved`.** r5에서 "리뷰어 처방의 직접 구현이라 안전하다"며 승인했다가 틀린 전례가 있어 근거를 명시한다 — **r5·r7과 이번의 결정적 차이는 "내가 새로 작문한 절차가 있는가"** 다. r5는 재탐색 명령을, r7은 태스크 상태 머신을 **내가 새로 조합**했고 둘 다 결함이 나왔다. r8의 변경은 **3인이 문장 단위로 제시한 처방의 직접 채택**(분류 키 통일·MISSING 규율·TD 판정 일원화)이고, 새로 조합한 절차는 없다. 남은 미실측(ⓐⓑⓓⓔ**ⓕ**ⓖⓗ — r9 정합: 이 문장이 ⓕ를 빠뜨렸다)은 **plan이 스스로 프리플라이트 게이트("확인 전 드릴 금지")로 처리**했고, 위험한 실행은 step 4(사람)이며 step 1~3은 문서 작성이라 진행을 막지 않는다. **(r9에서 이 판정은 뒤집혔다 — 아래 r8 섹션 참조.)**
>
> **이번에도 메인 코더의 CLI 실측은 하지 못했다**(실행 미승인). 대신 **claude-ide가 로컬 `help`로 ⓒ(`wait tasks-stopped` = 6초×100회/exit 255, 매처가 `tasks[]` 기준)와 ⓔ 절반(`DeletedDate` 필드 실재)을 실측**해 왔고, 이는 plan이 미실측으로 남겨 둔 항목에 대한 **직접 인용 가능한 근거**라 채택하되 **출처를 "claude-ide r7 실측, 메인 코더 미재현 → 런북 작성 시 1회 재확인"으로 명시**했다([[infra-plan-review-and-first-source]] — 액면 수용과 근거 채택을 구분한다).

**결함(진행 차단)**

- [r7] codex-cli#1 [중간] / claude-ide#1 [낮음] **(나)/(다)의 분류 키가 서로 달라 `desiredStatus=STOPPED` ∧ `lastStatus=RUNNING`이 (다)로 떨어져 중복 `stop-task`를 부른다** — (나)는 `desiredStatus`+`lastStatus` 두 키, (다)는 `lastStatus` 한 키였다. codex-cli는 **`StopTask` API 응답 예시가 바로 그 조합을 보여 준다**는 근거를, claude-ide는 **waiter 상한 초과 후 재진입하면 항상 이 상태**라는 시나리오를 댔다 → **반영(2인 동일 처방 채택)**. 비종결의 분기 키를 **`desiredStatus` 하나로 통일**하고 분류를 **우선순위 규칙**(① `lastStatus` 종결 → ② `desiredStatus==STOPPED`면 waiter만 → ③ `desiredStatus==RUNNING`이면 `stop-task`+waiter)으로 못박았다. 검증 항목 ⑱에 "**단일 키**로 갈린다"와 런북 **조건 우선순위 표 + 대표 조합**(codex-cli 개선)을 함께 넣어 재발을 막는다.
- [r7] codex-ide#1 [높음] **`MISSING`을 즉시 종결로 간주하면 결과적 일관성 구간의 살아 있는 태스크를 "정리 완료"로 오판한다** — ECS API는 eventual consistency라 `run-task` 직후 조회에 태스크가 아직 안 보일 수 있고, `MISSING`의 의미는 "찾지 못함"일 뿐 **보관 만료·전파 지연·잘못된 ARN을 구분하지 않는다.** 응답만 받고 곧바로 중단해 정리 게이트로 들어온 태스크가 일시적 MISSING이면 (d)는 PASS인데 **그 태스크가 뒤늦게 기동해 운영 마스터 비밀번호를 주입받는다** → **반영**. r7이 "MISSING = 보관 창이 지난 것"이라고 **단정**한 것이 근거 없는 비약이었다(1차 출처 규율을 스스로 어긴 문장이다). **MISSING 규율** 신설: ① 30초 간격 재조회 **상한 3회** → ② `list-tasks --started-by` 대사 → ③ **원장에 종결 관측 이력(`STOPPED`/`DELETED` 관측 또는 `exitCode` 확인)이 있는 ARN만 종결 인정** → ④ 생성 응답만 있고 종결을 한 번도 못 본 ARN은 **"상태 불명"으로 (A) → (d) FAIL**, **상한 초과를 종결로 승격하지 않는다.** 재탐색 3·잔존 목록 (A)④·(d) 축 표·step 4 요약까지 같은 정의로 동기화. 지수 백오프는 **채택하지 않고 기존 30초×3회 고정**을 재사용했다 — 사람이 손으로 실행하는 절차라 백오프 계산을 얹을 실익이 없고, 지적의 핵심("유한 재조회 후에도 종결로 승격 금지")은 그대로 충족된다.
- [r7] codex-cli#2 [낮음] **TD `INACTIVE`의 (d) 판정이 plan 안에서 정반대** — 재탐색 절은 "INACTIVE TD는 (A) 잔존이 아니다", 단일 소스인 잔존 2분류 (A)⑤는 "`ACTIVE`/`INACTIVE`면 (A)"였고, 즉시 게이트 요약의 **"호출 성공 + 비-`ACTIVE` 상태 확인"** 도 정확한 조건보다 넓어 `INACTIVE`를 종결로 오독하게 했다 → **반영**. 판정 기준을 **"`delete-task-definitions` 호출의 성패"** 로 일원화: **호출 성공이면** `INACTIVE`로 보이거나 조회에 안 잡혀도 **(A)가 아니고**, **호출 실패로 남았으면 (A)** 다. 재탐색 절의 옛 문구를 교정하고, 즉시 게이트 요약을 **"삭제 호출 성공 + `DELETE_IN_PROGRESS`/조회 불가"** 로 정확히 맞췄다(검증 항목 ㉖).

**개선 제안**

- [r7] claude-ide **`wait tasks-stopped` 숫자를 대기 상한 표에 박을 수 있게 됐다**(6초×100회=600초, exit 255) → **반영**. 정성 표현이던 waiter 항목에 숫자를 넣고, 다른 waiter도 **런북 작성 시 각 `wait ... help`에서 숫자를 뽑아 표에 박도록** 지시를 남겼다.
- [r7] claude-ide **waiter를 태스크 ARN 하나씩 호출할 것** — 매처가 `tasks[]`의 **모든 원소**를 보므로 여러 ARN을 한 번에 넘기면 하나가 MISSING·지연되는 것만으로 **배치 전체가 600초를 소진**한다(대상 최대 5건 → 최악 50분) → **반영**(§정리 게이트 ② + 검증 항목 ㉕).
- [r7] codex-ide **수동 `delete-secret` 전에 `OwningService=rds` 확인** — 서비스 연결 시크릿은 직접 삭제가 허용되지 않을 수 있으므로, 거부되면 반복하지 말고 **RDS 측 잔존 이상으로 기록·에스컬레이션** → **반영**(시크릿 정리 ③).
- [r7] claude-ide **ⓔ의 범위를 좁혀 적기** — "삭제 예약 상태와 `DeletedDate` 판별 필드의 존재"는 실측으로 확인됐으니 남은 미지는 **"RDS가 즉시 지우는가 예약하는가"** 하나 → **반영**(체크리스트 ⓔ 축소 + 근거 인용).
- [r7] codex-ide **ⓐ·MISSING 사유·STOPPED 보관 창은 공식 문서로 확인 가능하니 런북 작성 단계에서 URL과 함께 확정** → **반영**(체크리스트에 명시 + ADR 인용 목록에 ECS 결과적 일관성 항목 추가). 체크리스트에 **ⓗ(결과적 일관성)** 를 신설했다.
- [r7] **메인 코더 자체 보강(지적 아님)**: MISSING 규율이 "종결 관측 이력"을 입력으로 요구하는데 **원장에 그 항목이 없어 규율이 실행 불가**였다 → 원장 사후값 층에 **각 `taskArn`의 상태 관측 이력**(`lastStatus` 확인 시각·값, `exitCode` 확인 여부, TD 삭제 호출 성패)을 필수 항목으로 추가. 리뷰어 처방의 필연적 귀결이라 새 절차 조합이 아니다.
- [r7] claude-ide **(A)④의 `DEPROVISIONING`은 의도 확인만** — 컨테이너가 이미 멈춘 단계라 "비밀번호를 쥔 컨테이너"는 아니다 → **반영(의도 명시)**. 상태별 예외를 두면 판정이 다시 갈라지므로 **"종결 아니면 (A)"** 한 줄을 유지하되, **의도적 보수성임을 근거와 함께** 적었다.

### r8 (2026-07-28) — codex-ide `approve` / codex-cli·claude-ide `request-changes`, revision 8 대상

> **또 승인 판정이 틀렸다. `status`를 `approved` → `in-review`로 되돌린다.** r8 병합에서 나는 "변경이 전부 3인 처방의 직접 채택이고 **새로 조합한 절차는 없다**"를 근거로 승인했는데, **실제로는 (A)⑤에 판정 문장 하나를 새로 써 넣었고**(_"호출이 성공했다면 `INACTIVE`가 보이더라도 (A)가 아니다"_) 그것이 같은 라운드에 내가 좁혀 놓은 즉시 게이트와 정면으로 어긋났다. claude-ide가 그 자기진단 불일치까지 정확히 짚었다. **패턴이 확정됐다: 내가 반영하면서 새로 쓰는 판정 문장은 매번 다음 라운드에 결함으로 잡힌다.** 앞으로 승인 근거로 "새로 조합한 절차 없음"을 쓸 때는 **그 라운드에 추가·수정한 문장을 하나씩 지목해 확인한 뒤**에만 쓴다([[infra-plan-review-and-first-source]]).
>
> claude-ide가 r7 결함 1건·개선 3건 전건 해소를 표로 확인했고, **codex-ide는 approve로 전환**(MISSING 규율이 목표 (d) 축·원장·재탐색·상태 머신·잔존 분류·사람 요약에 일관 반영됐음을 확인). **r8 지적 2건·개선 6건 전부 반영, 기각 0.**
>
> 이번에도 메인 코더의 CLI 실측은 못 했고(실행 미승인), **claude-ide가 RDS waiter 2종을 로컬 `help`로 실측**해 제공했다(`db-instance-available`·`db-instance-deleted` = 30초×60회=1800초/exit 255, 후자의 매처는 `length(DBInstances)==0`). ⓒ 때와 같은 규율로 **출처를 밝히고 채택 + 런북 작성 시 1회 재확인**.

**결함(진행 차단)**

- [r8] codex-cli#1 [중간] **TD 삭제의 "호출 성공"을 revision별로 정의하지 않아 부분 실패가 (d) PASS에 섞인다** — `DeleteTaskDefinitions`는 **한 요청에 최대 10 revision을 받는 배치 API**이고 **HTTP 200 안에 성공한 `taskDefinitions[]`와 실패한 `failures[]`를 함께** 반환한다. CLI 종료 코드나 요청 단위 성패만 원장에 적으면 **TD-A는 전이되고 TD-B는 `failures[]`에 있는데도 "삭제 호출 성공"** 으로 남아 (A)=0으로 오판한다 → **반영**. 판정 단위를 **revision ARN**으로 바꾸고, 원장에 ARN마다 **`taskDefinitions[]` 포함 여부·`status`·`failures[].reason`** 을 따로 기록하게 했다. 종결 = **그 ARN이 `taskDefinitions[]`에 있고 `status=DELETE_IN_PROGRESS`, 또는 부재, 또는 `failures[].reason=MISSING`**(멱등 성공은 MISSING **하나로 한정**). 그 밖의 실패·응답 누락은 종결이 아니다.
- [r8] claude-ide#1 [낮음] **"삭제 호출은 성공했는데 `INACTIVE`로 보이는" 상태가 (A)·(B)·종결 어디에도 속하지 않는다** — 즉시 게이트 기준으로는 종결이 아니고, (A)⑤ 괄호는 "(A) 아님"이라 하고, (B) 목록엔 없다. **(d) 판정의 단일 소스가 잔존 2분류이므로 (A)가 아니면 실무적으로 PASS로 흘러간다** → 즉 내가 r8에서 넣은 괄호 문장 하나가 `INACTIVE`를 사실상 종결로 만들었고, 이는 같은 라운드의 정정·검증 ㉖과 정면으로 걸린다. ECS가 결과적 일관성임을 plan 스스로 ⓗ로 인정했으므로 **삭제 직후 `INACTIVE` 관측은 드문 일도 아니다** → **반영(처방 그대로)**. ① **괄호 문장 삭제** ② **MISSING과 대칭인 재조회 규율 신설**: `INACTIVE` 관측 시 30초 간격 **재조회 상한 3회** → `DELETE_IN_PROGRESS`/부재로 바뀌면 종결, **끝까지 `INACTIVE`면 (A)**. 이로써 이 상태가 **2분류 중 정확히 한 칸**에 들어간다.

**개선 제안**

- [r8] claude-ide **RDS waiter 숫자를 지금 표에 확정**(`db-instance-available`·`db-instance-deleted` 둘 다 30초×60회=1800초/exit 255) → **반영**. 대기 상한 규율에서 정성 표현이 사라졌다.
- [r8] claude-ide **`db-instance-deleted`의 매처가 `length(DBInstances)==0`** 이라는 사실을 정리 게이트 ⑤ 옆에 → **반영**. "삭제 요청이 아니라 조회에서 사라짐이 과금 종료 판정"이라는 규율의 직접 근거다.
- [r8] codex-ide **TD 삭제 판정의 1차 증거를 응답(성공 목록 포함 + `failures[]` 부재)으로 고정하고, 후속 `describe`는 결과적 일관성을 감안한 확인 증거로 분리** → **반영**(codex-cli#1과 같은 방향이라 통합. 재탐색 절의 판정 문구도 이 구분으로 교체).
- [r8] codex-ide **`DescribeTasks`는 요청당 ARN 100개 상한** → 종료 태스크 대사에서 **100개씩 배치 호출**을 런북에 명시 → **반영**(태스크 이력이 많은 계정에서도 절차가 그대로 동작).
- [r8] codex-cli **드릴 토큰 길이 계산 오류**(`restore-drill-`은 13자가 아니라 **14자** → 토큰 29자, client token 35자) → **반영**(결론은 동일하나 증거 수치를 바로잡음).
- [r8] claude-ide **병합 로그의 미실측 목록에 ⓕ 누락** → **반영**(로그 문장 정합). 아울러 **ⓑ의 우선순위를 낮춤** — (가) 종결은 조치 없음이라 실제 문제 경로는 *관측과 호출 사이의 좁은 레이스*뿐이고 그마저 "거부 시 30초×3회 재조회"가 흡수하므로, 체크리스트에 **"확인 못 해도 절차는 안전"** 메모를 붙여 프리플라이트 부담을 줄였다.

### r9 (2026-07-28) — codex-cli `approve` / codex-ide·claude-ide `request-changes`, revision 9 대상

> **예고한 패턴이 그대로 재현됐다.** r8 병합 보고에서 "병합하며 새로 쓰는 판정 문장은 매번 다음 라운드에 잡힌다"고 적고 승인을 보류했는데, 이번 결함도 정확히 **내가 r9에서 새로 쓴 부분** — codex-cli 처방(응답=1차 증거)과 claude-ide 처방(`INACTIVE` 재조회)을 나란히 반영하면서 **둘의 우선순위를 정하지 않은 것** — 에서 나왔다. 두 처방 각각은 옳았고, **합치는 순간 내가 만든 공백**이다. 교훈을 한 겹 더 구체화한다: **서로 다른 리뷰어의 처방을 같은 절에 합칠 때는 "둘이 충돌하는 입력이 무엇인지"를 먼저 적고, 그 입력에서의 우선순위를 명시한 뒤 반영한다.**
>
> **판정: `status: approved`(revision 10).** r8에서 세운 규율대로, 승인 전에 **이번 라운드에 추가·수정한 문장을 하나씩 지목해 점검**했다. ① 즉시 게이트 우선순위 1~4 = codex-ide 처방 ①②의 정리이고, **내가 새로 정한 것은 ④의 재전송 상한 3회 숫자 하나**인데 이는 plan 전체의 재시도 관례(30초×3회)와 같다 ② (A)⑤·재탐색 TD 절·(d) 축 표·step 4 = ①과의 동기화 ③ TTL 확신도·체크리스트 ⓖⓘⓙ·재탐색 한 흐름·ADR 인용·원장 양식 = 리뷰어 처방의 직접 채택 ④ 검증 ㉖·㉛·㉜~㉟ = 위 반영의 검증 대응. **판정이 갈릴 수 있는 입력**(stale `INACTIVE`, 접수 불명)은 전부 우선순위 규칙에 귀속됐고, 남은 미확인 ⓘ·ⓙ는 **"확인 전에는 더 보수적으로 판정"**(부재를 직접 조회로 확인해야 종결 인정)으로 흡수돼 **확인 결과와 무관하게 안전**하다. 아울러 잔여 쟁점의 성격이 r8 이후 **설계 결함 → 문장 정합성**으로 바뀌었고, 이 plan의 산출물인 런북은 작성 후 **코드 교차검토(§10)** 로 한 번 더 걸러진다.
>
> **codex-cli는 approve로 전환**(TD 배치 삭제의 부분 실패 문제 해소 확인). claude-ide가 r8 결함 1건·개선 4건 전건 해소를 표로 확인하고, **r9의 수치 주장 3건을 로컬 `help`로 실측 검증**했다(`delete-task-definitions` _"up to 10"_ + 응답 `taskDefinitions`/`failures` 혼재 ✔ / `describe-tasks` _"up to 100"_ ✔ / 토큰 길이 29·35자 ✔). **r9 지적 2건·개선 6건 전부 반영, 기각 0.**

**결함(진행 차단)**

- [r9] codex-ide#1 [중간] **revision별 삭제 응답과 후속 `INACTIVE` 조회의 판정 우선순위가 모순** — 즉시 게이트·재탐색 절은 "응답이 1차 증거, 접수된 revision은 조회에 안 잡혀도 (A) 아님"인데, 바로 아래 대칭 규율은 "삭제 호출이 성공했더라도 `INACTIVE`가 보이면 재조회 3회 후 (A)"였다. ⇒ **응답은 `DELETE_IN_PROGRESS`인데 결과적 일관성으로 후속 `describe`가 잠시 stale `INACTIVE`를 반환하는 동일 실행이 한쪽에선 PASS, 다른 쪽에선 FAIL** → **반영(처방 그대로)**. 즉시 게이트를 **우선순위 1~4의 단일 상태 머신**으로 재작성: ① 응답의 그 ARN이 `taskDefinitions[]`에 있고 `status=DELETE_IN_PROGRESS` → **종결 확정, 이후 stale 조회가 뒤집지 못함** ② `failures[].reason=MISSING` → 멱등 성공(단 ⓘ 확인 전에는 **부재를 직접 조회로 확인한 경우만**) ③ 그 밖의 `failures[]` → (A) ④ **접수 여부 불명**(응답 유실·타임아웃·어느 목록에도 없음) → **조회 반복이 아니라 삭제 요청 재전송**(30초 간격, 최초 포함 총 3회) 후 1~3으로 판정, 끝까지 증거 없으면 (A). **`INACTIVE` 관측 자체는 판정 근거에서 빠졌다** — r8이 지적한 "`INACTIVE`가 어느 칸에도 없다"는 구멍은 ④가 닫는다.
- [r9] claude-ide#1 [낮음] **스스로 미확인(ⓖ)으로 분류한 숫자를 본문에서 단정** — 드릴 토큰을 초 단위로 하는 근거로 "ECS 멱등성 TTL(**최대 24시간**)"을 단정형으로 썼는데, 로컬 `help`에는 TTL 수치가 없고 체크리스트 ⓖ는 미확인으로 남아 있다. **같은 plan이 TD "최대 1시간"에는 출처·미확인 헤지를 정확히 걸어 두었으므로 형식이 어긋난 것**이다(r1의 Oracle 전용 플래그에서 시작해 9라운드 지켜 온 규율) → **반영**. 문장의 확신도를 낮추고(개발자 가이드 기준·ⓖ에서 확인) **ADR 인용 목록에 추가**했으며, **"TTL이 더 짧아도 초 단위 토큰 결정은 유지된다"** 를 명시해 확인 결과와 무관하게 설계가 흔들리지 않음을 드러냈다.

**개선 제안**

- [r9] codex-ide **목표 표의 (d) 요약이 TD 종결을 "`DELETE_IN_PROGRESS` 또는 조회 불가(삭제 호출 성공)"로 여전히 별도 서술** → 요청 단위 "호출 성공"이 다시 판정 기준으로 읽힌다 → **반영**. 축 표를 **§TD 종결 2단계 참조**로 바꿔 단일 소스 원칙을 TD에도 관철.
- [r9] codex-ide **`failures[].reason=MISSING`의 반환 조건이 AWS 문서에 명시돼 있지 않다**(중요한 PASS 조건인데) → **반영**. 체크리스트 **ⓘ** 신설 + **확인 전에는 revision 부재를 `describe-task-definition`으로 직접 확인한 경우에만 종결 인정**. 재전송 응답 미확인분은 **ⓙ**로 함께 신설.
- [r9] codex-cli **`DeleteTaskDefinitions` API를 TD 삭제 판정의 1차 출처로 런북에 직접 인용**(10개 상한·응답 혼재·`DELETE_IN_PROGRESS`를 한 문서가 뒷받침) → **반영**(ADR 인용 목록).
- [r9] claude-ide **`delete-task-definitions`의 10개 상한을 런북 명령 예시 옆에도** — 이번 드릴은 TD 2개지만 런북은 재드릴·타 상황에서도 쓰인다 → **반영**(검증 항목 ㉞).
- [r9] claude-ide **`list-tasks` 페이지네이션과 `describe-tasks` 100개 배치를 한 문장으로** — 실행자에게는 한 흐름이다 → **반영**(재탐색 경로 2 + 검증 ㉛).
- [r9] claude-ide **원장 표의 빈 양식 예시를 런북에** → **반영**(TD 칸 컬럼까지 지정 + 검증 ㉝). ⓖ 확인 시 **"다른 요청 재사용 금지"의 정확한 문구도 함께 확보**하라는 제안도 ⓖ 항목에 반영(체크리스트 한 번으로 두 문장 확정).

### r11 (2026-07-29) — 코드 리뷰 round-6의 plan 이탈 지적 반영, 사용자 지시로 동기화

> plan 리뷰 라운드가 아니라 **코드 교차 검토(round-6)에서 3인이 공통으로 올린 절차 지적**을 사용자가 (A)안으로 결정해 반영한 revision이다. 설계 결정은 하나도 바뀌지 않았고 **측정 지표의 이름만** 문서 전체에서 일치시켰다. 대상 산식(`T_verified − T0`)과 측정 절차는 revision 10과 동일하다.

- [코드 r6] claude-ide#4 / codex-cli 개선#1 / codex-ide 개선 **승인된 plan은 `RTO = T_verified − T0`인데 런북·ADR은 r3~r5를 거치며 같은 산식을 "DB 복원 검증 소요시간"으로 바꿔 부르고 있다. 어느 resolution도 이를 plan 이탈로 기록하지 않았다** → **반영(사용자 결정 = plan을 문서에 맞춘다)**. 이유: `T_verified`는 **격리 복원본에서 쿼리가 나온 시점**이고, 서비스가 복원본을 쓰도록 전환(cutover)·데이터 역복사·애플리케이션 헬스 확인·롤백은 **이 plan의 범위 밖**이라 서비스 복구의 끝점이 측정되지 않는다. 이 숫자를 RTO로 적으면 ADR을 읽는 사람이 "장애 시 이만큼이면 복구된다"로 오독한다. 문서(런북 §1·§9, ADR 0005) 쪽이 기술적으로 옳으므로 plan을 그쪽으로 맞췄다.
  - 바꾼 곳: goal #3, 성공 축 (c), §측정 정의(정의 + 라벨 3개 확정 + 서비스 RTO 실측 조건), §복원본 자격증명 흐름 4, §복원 방식(폴링은 "측정 구간 밖"), 실행 단계 (c)·(e)·(h), step 2의 ADR 라벨 고정 목록(⓪ 추가), step 4·5.
  - **바꾸지 않은 곳**: ① §범위 밖의 "Multi-AZ 전환 — RTO를 자동 failover로 단축", ② step 2의 "복원-시간 RTO 수용" — 둘 다 **진짜 서비스 RTO 태세**를 말하는 문장이라 라벨 교체 대상이 아니다. ③ **r1~r9 검토 반영 로그의 `RTO` 표기** — 그 시점의 판정을 그대로 남기는 기록이라 소급 수정하지 않는다(고쳐 쓰면 "언제 무엇이 바뀌었는지"를 잃는다).
- [코드 r6] claude-ide#3 **step 1 산출물인 `alarm-response.md` 링크가 r5에서 제거돼 온콜 발견 경로가 0이 됐다** → **코드 쪽에서 이미 반영 완료**(round-6 resolution). `alarm-response.md`에 **[참고 — 복구 도구 아님]** 문단으로 복구했고 런북 머리말에 역방향 링크를 넣었다. step 1·2·3의 "런북↔ADR↔`alarm-response.md` 상호 링크" 검증 기준은 그대로 유효하며 현재 충족된다. **plan 문구 변경 없음.**

**status**: `approved` 유지. 승인의 근거가 된 설계·절차·검증 기준이 그대로이고, 이번 변경은 승인 시점에 이미 구현돼 있던 문서와의 **명칭 정합**이기 때문이다. 다만 revision이 올랐으므로 다음 코드 리뷰 라운드의 리뷰어는 `reviewed-revision: 11` 기준으로 plan을 참조한다.

### revision 12 — `rds.force_ssl` 기본값 사실 정정 + 파라미터그룹 이름 대조 반영 (2026-08-07)

**계기**: 코드 교차 검토 **round 33 codex-cli#3**이 "기본 파라미터그룹이면 `force_ssl`이 빠진다"는 전제가 PostgreSQL 16에서 거짓임을 지적했다. 런북·ADR은 r33에서 정정했으나 **plan 본문은 코드 라운드가 수정할 수 없는 범위**라(`docs/plans/README.md`) 옛 전제가 남았고, r33~r35에서 리뷰어들이 라운드마다 "범위 밖 미해결"로 재확인했다(r33 codex-cli, r34 claude-ide, r35 codex-ide·claude-ide). 여기서 plan 프로토콜로 닫는다.

**1차 출처**(에이전트가 직접 확인): [Using SSL with a PostgreSQL DB instance](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/PostgreSQL.Concepts.General.SSL.html) — _"The `rds.force_ssl` parameter default value is **1 (on) for RDS for PostgreSQL version 15 and later**. For all other RDS for PostgreSQL major versions 14 and older, the default value of this parameter is 0 (off)."_ 대상은 PostgreSQL **16**(`infra/prod/variables.tf`의 `postgres_version` 기본값)이다.

**따라오는 결론 — 검사는 없애지 않고 근거와 형태를 바꾼다.**

1. `--db-parameter-group-name`을 명시하는 이유는 **TLS 노출 방지가 아니라 구성 동등성**이다(그리고 앞으로 늘어날 커스텀 파라미터의 보존).
2. **`pg_settings`의 `rds.force_ssl` 조회는 그룹 누락을 잡아 주지 못한다** — 기본 그룹에서도 `on`이 나오기 때문이다. 그래서 `ParameterApplyStatus == in-sync`만 보던 사후 확인에 **붙은 그룹 이름 대조**를 더한다. 이름 없이 `in-sync`만 보면 기본 그룹도 통과해 **(a)가 거짓 PASS**가 된다.
3. 이름 불일치는 **재부팅으로 고쳐지지 않으므로 교정 대상이 아니고**, 그렇다고 abort(접속 전 중단)도 아니다 — 복원본은 여전히 비공개·암호화·기대 SG이므로 **(a)만 FAIL로 확정하고 검증은 계속한다.**

**바꾼 곳**: 성공 축 표 (a) / 전체 verdict 문단 / §배경·제약의 파라미터그룹 항목 / 실행 단계 (e) 구성 게이트 / 실행 단계 (h) 판정 / step 4 검증 기준.

**바꾸지 않은 곳**:

- **r1~r11 검토 반영 로그의 옛 표현**(예: r1의 _"복원본이 운영 파라미터그룹(`rds.force_ssl=1`)을 승계하지 않음"_) — 그 시점의 판정을 그대로 남기는 기록이라 소급 수정하지 않는다(r11이 세운 규율과 동일 — 고쳐 쓰면 "언제 무엇이 왜 바뀌었는지"를 잃는다). 승계하지 않는다는 사실 자체는 지금도 참이다.
- **step 2의 ADR 작성 지시** — ADR 0005는 r33에서 이미 이 사실과 인용을 반영했고, 그 절의 지시 항목("PITR 복원본의 기본 파라미터그룹 승계"·"한계 절에 PG 재지정은 별도 단계")은 여전히 유효하다.
- **`rds.force_ssl` 실효값 조회 자체** — 그룹 누락은 못 잡지만 **server-side 강제가 실제로 켜져 있다는 증거**로서의 가치는 그대로다((a)의 독립 재료로 유지).

**status**: `approved` 유지. 이 변경은 새 설계가 아니라 **이미 3인 리뷰어가 round 33~35에서 approve한 런북·ADR의 사실관계를 plan에 들여오는 정합 작업**이고, 승인의 근거가 된 절차·범위·측정 정의는 그대로다. 다음 라운드의 리뷰어는 `reviewed-revision: 12` 기준으로 참조한다.
