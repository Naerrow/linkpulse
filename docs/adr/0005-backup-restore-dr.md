# ADR 0005 — 백업 복원 검증 태세 (Single-AZ + 자동백업 7일 PITR + 관리 마스터 비밀번호)

- 상태: 채택 (2026-07-28). **측정 절(§DB 복원 검증 소요시간·복구 지점)은 플레이스홀더** — 복원 드릴 실측으로 확정한다([`plan.md`](../plans/0007-p4-rds-restore-drill/plan.md) step 4~5).
- 관련: AGENTS.md 가드레일 #1(인프라 변경은 사람 승인)·#2(비밀값), Phase P4(하드닝), 절차는 [Runbook — RDS 백업 복원 검증](../runbooks/rds-restore.md).

## 맥락

현재 RDS 태세(`infra/prod/rds.tf`·`variables.tf` 실측): **Single-AZ**(자동 failover 없음) + **자동 백업 7일 보관**(`db_backup_retention=7` → PITR 창 7일) + **RDS 관리 마스터 비밀번호**(`manage_master_user_password=true`, Secrets Manager) + gp3 20GB **암호화** + `deletion_protection=true`.

백업은 **켜져 있지만 한 번도 복원해 본 적이 없다.** 별도 DB로 복원되는지, 데이터가 조회되기까지 얼마나 걸리는지, 지정한 복구 지점이 지켜지는지 검증한 적이 없다 — **복원해 보지 않은 백업은 백업이 아니다.** 이 ADR은 현 태세를 명시적으로 채택하면서, 그 태세가 실제로 동작함을 **비파괴 드릴**(운영과 분리된 별도 인스턴스로 복원 → 검증 → 삭제)로 실증하고 수치를 남기는 것을 결정한다.

이 결정은 손상 데이터의 역복사, 쓰기 동결, 복원본 cutover, 서비스 헬스 확인, 롤백, IaC 상태 정합을 포함하지 않는다. 따라서 산출값은 **DB 복원 검증 소요시간**이며 서비스 RTO나 사고 복구 절차가 아니다.

## 결정

### 1. 현 태세를 그대로 채택한다 — Single-AZ + 자동백업 7일 + 관리 비밀번호

- **Single-AZ 유지.** Multi-AZ는 비용이 약 2배이고 자동 failover로 가용성을 높이지만, 이 서비스는 1인 운영·실사용 트래픽이 작아 현재 비용을 우선한다. 이 드릴은 서비스 중단·복구 시간을 측정하지 않으며 Multi-AZ는 P5+ 업그레이드 후보로 남긴다.
- **자동 백업 7일 = PITR 창 7일.** 보관 연장의 비용 대비 이득을 아직 입증하지 못했으므로 현 설정을 유지하고, 이번 드릴은 이 7일 창 안에서 복원 가능성만 검증한다.
- **RDS 관리 마스터 비밀번호 유지.** 코드·state에 평문이 없고 로테이션을 RDS가 맡는다(가드레일 #2와 정합).

### 2. 복원 절차를 문서로 고정한다 (PITR 주 / 스냅샷 보조)

- **주: PITR.** 자동 백업 + 트랜잭션 로그 체인의 **복원 가능성**을 검증한다. 드릴은 **`--restore-time` 명시**를 기본안으로 한다 — `--use-latest-restorable-time`은 편하지만 **복원 지점이 사후에만 확정**돼 baseline과의 대조 기준이 흐려진다.
- **보조: 스냅샷 복원.** 이번 드릴에서 실행하지 않지만 별도 검증 때 운영자가 따라 할 수 있는 수준으로 런북에 기술한다(§11). **판정 조건이 뒤집히는 지점**(스냅샷이 baseline보다 앞서면 집계 부등식이 성립하지 않는다)은 2열 대조표로 못박았다. 두 모드는 **모드별로 세우는 공통 읽기 전용 플래그 2개**(`SENTINEL_BOUNDARY_OK`·`BASELINE_COMPARABLE_OK`)로만 검증 단계에 이어진다 — 검증 태스크의 전제를 PITR 전용 값에 묶으면 스냅샷으로 만든 **유료 복원본이 검증에 도달하지 못한 채 폐기**되기 때문이다. 삭제할 수 없는 sentinel-B POST는 이 둘과 분리된 일회성 `SENTINEL_B_CREATE_ALLOWED`가 PITR·스냅샷의 각 사전조건을 통과했을 때만 허용한다. 허용값은 POST 전에 `SENTINEL_B_CREATE_STATE=dispatched`로 소비하며, 첫 유효 응답의 60초 마진 미달이 확인된 경우에만 두 번째 시도를 연다. 응답 불명은 해당 시도를 소비한다.
- **복원본은 원본 구성을 자동 승계하지 않는다** — 아래 §한계 1.
- **복원 API 응답이 불명확하면 무한 재시도하지 않는다.** 같은 토큰 태그의 대상이 3회 연속 명시적 부재일 때만 1회 창을 열고, 최초 호출 직전 봉인한 mode/source/baseline task tuple과 완전한 baseline 집계 증거·sentinel 경계를 다시 검증한 뒤 같은 요청을 재호출한다. 호출 직전 `dispatched`로 소진해 블록 재실행도 두 번째 전송으로 이어지지 않게 한다.

### 3. 검증 접속은 one-off ECS run-task (Bastion·ECS Exec 아님)

RDS는 격리 data 서브넷·`publicly_accessible=false`·data SG(`app` SG에서만 5432 inbound)로 보호되고 ECS Exec은 비활성이다. 검증은 **app 서브넷·`app` SG를 단 일회성 Fargate 태스크**(psql 이미지)로 하고 결과는 CloudWatch 로그로 받는다. 컴퓨팅은 실행 후 종료돼 **지속 인프라(과금·권한)가 남지 않는다**(제어면 기록은 일정 시간 남지만 비용도 권한도 없다). Bastion·SSM 포워딩은 드릴용으로 과하고, CloudShell(VPC 환경) psql은 **1순위 폴백**으로만 문서화한다.

**비밀번호는 평문으로 흐르지 않는다.** 값을 꺼내 태스크 env/override로 주입하면 `RunTask` 요청 파라미터·`DescribeTasks` 응답·CloudTrail·셸 히스토리에 평문이 남는다 → **태스크 정의의 `secrets`로 시크릿 ARN을 참조**해 ECS 에이전트가 주입하게 한다(운영 `ecs.tf`와 같은 경계).

**복원본 자격증명은 `modify-db-instance --manage-master-user-password`로 사후 전환한다(런북 §7).** 복원 계열 API의 같은 옵션이 Oracle 전용이라는 제약(§1차 출처) 때문이기도 하지만, 근본 이유는 **운영 자격증명을 드릴 경로에 끌어들이지 않기 위해서**다.

복원본은 원본의 마스터 자격증명을 승계하지만, 운영 자격증명을 검증 태스크에 사용하지 않기 위해 이 전환을 **드릴 전용 단계**로 둔다.

**데이터 손상·행 오삭제·인스턴스 소실의 실제 복구는 모두 현재 지원 범위 밖이다.** retained backup 식별과 데이터 역복사 또는 cutover, 쓰기 정합, 서비스 검증, 롤백을 구현하고 끝점까지 실측하기 전에는 이 ADR의 숫자를 사고 복구 수치로 사용하지 않는다.

### 4. 드릴은 "무접촉"이 아니다 — 허용 부수효과를 열거로 고정한다

운영 인스턴스의 **설정 변경 API는 전면 금지**하고 기존 행도 건드리지 않지만, 드릴은 **운영 계정·운영 클러스터에 부수 리소스를 만든다**: 임시 TD 2개, 임시 IAM role, 1회성 태스크 **최대 5회**, 운영 로그그룹의 드릴 로그 스트림, sentinel 링크 행 **최대 5건**(공개 API로 생성 — 삭제 API가 없어 영구 잔존). **이 열거가 허용의 전부**이며, 로그 스트림·sentinel 행을 뺀 전부는 정리 게이트에서 **종결 상태까지 확인**해 과금과 권한을 끊는다(런북 §2.2·§10).

**이 부수효과들의 전제는 "동시에 도는 드릴이 없다"는 것이다 — 단일 변경 작업 락을 필수 사전조건으로 채택한다.** 위 상한(sentinel 5건·태스크 5회)은 **한 드릴 기준**이라, 두 세션이 동시에 돌면 각자 상한을 지키면서도 합계로 넘긴다. 특히 sentinel 행은 삭제 API가 없어 되돌릴 수 없다. 난수 `drill-token` 태그는 **이미 만들어진** 리소스의 소유권을 가를 뿐 상호 배제 락이 아니며, 고정 식별자 부재 조회는 두 세션이 동시에 통과할 수 있다. 그래서 드릴은 조직의 변경 관리 기록에서 **단일 활성 작업**(운영자 1명·`$TOKEN` 1개, 획득·만료 시각 기록)을 배정받은 뒤에만 시작한다.

이 전제는 문장이 아니라 게이트로 강제한다: 런북 §2.5 ⓪이 **세션마다** `DRILL_MUTATION_OK`를 다시 도출하고, §3~§8·§11의 **비정리 상태 변경 13개가 각자 그 값을 직접 소비**한다(재진입 세션도 예외가 아니다 — 읽기 전용 재도출로 되살아나는 플래그만으로는 sentinel POST·reboot·자격증명 전환·IAM 생성·검증 태스크 실행이 열리지 않는다). **반대로 §10 정리 8개는 락을 요구하지 않는다**: 락을 잡지 못한 세션이 과금을 끊지 못하면 유료 인스턴스가 남기 때문이다.

**그 자격은 여섯 조건이며, 전부 "세션마다 달라지는 사실"이라는 하나의 기준으로 고른 것이다** — 셸(Bash), 레포·작업 디렉터리 재구성, 필수 로컬 도구(`python3`) 실행, **잔여 유효기간이 남은 락**, **원장과 정확히 같은 AWS 주체 + 이 세션에서 확인된 권한 4그룹**, **쓰려는 dispatch 원장이 이번 드릴의 것**. 뒤 세 조건을 "드릴 1회당 사실"로 두면 재진입에서 각각 이렇게 깨진다.

- **락을 불리언으로만 캐시하면**: 이 드릴은 30분급 대기가 두 번 이상 이어지므로, 대기 중 락이 만료되고 다른 운영자가 새 락을 얻어도 기존 세션이 모든 변경 게이트를 계속 통과한다 → 위 상한 근거가 무너진다. 그래서 원장에는 **만료 시각**을 적고 플래그는 매번 그 시각에서 다시 도출한다. **다만 그것만으로는 부족하다** — 자격은 프리플라이트 **실행 시점**의 사실이라 그 뒤 흐른 벽시계(자리 비움, 시크릿 폴링 + 수동 진단)를 모르고, "긴 대기 뒤 재실행"이라는 규율은 사람의 기억에 의존한다. 그래서 **비정리 변경 13개가 각자 호출 직전에 현재 시각과 만료 epoch를 비교**한다: 자격은 "시작해도 되는가", 직전 비교는 "지금도 유효한가"를 답한다. 만료 뒤에는 프리플라이트를 다시 통과하기 전까지 어떤 변경도 열리지 않는다.

  **재개 자격은 삭제 자격보다 좁다.** 태그 3요소(`OWNED`)는 "지워도 되는가"의 기준이지 "**새 권한을 부여해도 되는가**"의 기준이 아니다 — 같은 태그를 달았어도 신뢰 정책이 바뀐 role에 드릴 DB 시크릿 읽기 권한을 붙이면 의도하지 않은 주체가 비밀번호를 읽는 창이 열린다. 그래서 재개는 **소유(태그) ∧ 구성(신뢰 정책 문서 전체 일치)** 이 모두 맞을 때만 열고, 이어 붙일 정책의 현재 상태는 **성공적으로 조회했을 때만** 판정한다(조회 실패는 `unknown`이며 그 상태에서는 아무것도 바꾸지 않는다 — 실패를 "없음"으로 읽으면 그것이 곧 앞에서 금지한 멱등성 가정이다).

  **비교는 필드를 골라 보지 않고 문서·목록을 통째로 한다.** 신뢰 정책의 첫 Statement에서 `Effect·Principal.Service·Action`만 뽑거나 기대 정책 ARN만 필터하는 **선택 투영은 고르지 않은 것을 구조적으로 볼 수 없다** — 같은 Statement에 `Principal.AWS`를 얹거나 `AdministratorAccess`를 덧붙여도 투영값은 기대와 똑같이 나온다(런북 검토 중 셸 스텁으로 재현했다 — 두 경우 모두 재개 자격이 성립했다). 이 드릴의 role은 정확히 하나의 형태로만 만들어지므로 옳은 기준은 **허용 목록과의 전체 구조 정확 일치**다: 신뢰 정책 문서 전체 ∧ 연결된 관리형 정책 **목록 전체** ∧ 인라인 정책 **이름 목록 전체** ∧ 인라인 정책 **문서 전체**. 넷 중 인라인 문서만 `put`으로 교정하고, 목록이 예상 밖이면 교정 대상이 아니므로 **변경하지 않고 멈춘다.**

  **막는 것만으로는 부족하고, 막힌 뒤 이어 갈 길을 함께 둔다.** 만료 재확인이 성공적으로 개입하면 **부분 완료 상태**가 남는다 — sentinel-A는 예약분 일부만 전송된 채, IAM은 role만 있고 정책이 없는 채. 이때 ① 보내지 않은 것이 **코드로 증명되는** 건수는 선차감 예산에서 **환급**하고(소각하면 상한이 남았는데도 재개가 막혀 드릴이 정지한다) ② 부분 생성된 리소스는 **이번 드릴 토큰 소유로 확인될 때만** 남은 호출부터 이어 간다(롤백은 락 없이 하지 않지만, 락을 갱신한 뒤 이어 가는 것은 롤백이 아니라 정상 변경이다). 재개 경로가 없으면 안전 게이트가 곧 **유료 복원본을 띄운 채 검증을 포기하게 만드는 장치**가 된다.

  그 비교는 **게이트마다 인라인으로 쓰지 않고 단일 함수(`lock_live`)로 모은다.** 인라인은 양성 게이트(`&& [ … -lt … ]`)와 음성 게이트(`|| [ … -ge … ]`)의 **실패 의미를 갈라 놓는다** — 현재 시각을 얻지 못해 `[` 가 오류(status 2)를 내면 양성은 닫히지만 음성은 거짓이 되어 **변경 분기로 내려간다**. 함수는 시각 획득과 숫자 검증을 한 곳에서 하고 성공했을 때만 0을 반환하므로, 미정의·시각 오류·만료가 **양쪽 게이트에서 같은 방향으로** 닫힌다. 또한 게이트와 실제 호출 사이에 다른 API 호출이나 대기가 끼는 자리(IAM 3개, TD 등록 2개)는 **호출 직전에 한 번 더** 호출한다 — 블록 입구 검사 하나를 공유하면 그 사이 만료가 새어 든다.
- **권한을 최초 세션의 사실로 두면**: 새 터미널의 다른 `AWS_PROFILE`·바뀐 세션 정책으로도 읽기 전용 reconciliation은 통과하므로, `CreateRole`은 허용되고 `DeleteRole`은 거부되는 **부분 권한**으로 리소스를 만든 뒤 정리하지 못한다. 권한 프리플라이트가 생성 권한만이 아니라 **정리 권한까지** T0 전에 확인하려고 있는 것이라, 재진입이 그것을 우회하면 목적 자체가 사라진다. 이때 **원장의 주체 기록이 비어 있는 상태를 "일치"로 읽지 않는 것**이 결정의 일부다 — 빈 값은 일치가 아니라 확인 불가이며, 신규 세션도 첫 프리플라이트가 출력한 주체를 원장에 적고 다시 통과해야 자격이 선다. 그 한 단계가 "권한표를 확인한 주체"와 "실제로 변경하는 주체"를 같게 만드는 유일한 지점이다.
- **작업 디렉터리를 존재 확인만 하면**: 재진입은 `DRILL_DIR`를 손으로 붙여 넣으므로 다른 드릴의 원장이나 그것을 가리키는 symlink도 통과한다. 그 자격으로 TD 등록·`run-task` 예약이 남의 `td-pending`·`task-pending`에 쓰고 성공하면 지운다 → 정리 단계 방어(§10.3 앵커)보다 **먼저** 남의 증거가 오염·종결된다.

트레이드오프는 재진입·긴 대기마다 확인 절차가 한 단계 늘어난다는 것이고(권한표는 세션마다 다시 채운다), 되돌릴 수 없는 영구 쓰기와 정리 불능 리스크를 감안해 이 비용을 받아들인다.

### 5. 측정 지표의 라벨을 여기서 고정한다 (step 5에서 바꾸지 않는다)

| 라벨 | 산식 | 읽는 법 |
| ---- | ---- | ------- |
| **DB 복원 검증 소요시간** | `T_verified − T0` | 복원 호출부터 격리 복원본에서 데이터가 쿼리되기까지. 구성 게이트·필요 시 reboot·드릴 전용 자격증명 전환을 **전부 포함**하며 서비스 RTO가 아니다 |
| **선택한 복원 지점의 나이** | `T0 − restore point` | **이번 드릴에서 고른 지점**의 나이. 절차상 sentinel-B 60초 마진 + 사람 조작 시간이 **최소 60초 이상** 포함돼 있다 |
| **백업 시스템 복구 지점 지연(참고)** | `T0 − LatestRestorableTime(T0 시점 관측)` | 백업 시스템 자체의 능력치. 두 값을 구분하지 않으면 ADR에 **시스템을 과소평가한 숫자**가 박힌다 |

**시각 지표(위 표)와 행 수준 실증(sentinel 브래킷)은 서로 다른 측정이며 따로 기록한다.**

- `RPO = 운영 max(created_at) − 복원본 max(created_at)` 류의 산식은 **무효**다: `created_at`은 신규 행에만 기록되고 클릭은 `clicks`만 갱신하며(타임스탬프 없음), 이 서비스는 실사용 쓰기가 거의 없어 드릴 창 동안 신규 행이 0건일 공산이 크다 → **"손실 없음"이 아니라 "측정 실패"인 0**이 나온다.
- 그래서 행 수준은 **sentinel 양방향 브래킷**으로 실증한다: **A**(복원 지점 이전 생성 → **존재해야** 함) / **B**(복원 지점 이후·T0 이전 생성 → **부재해야** 함). B가 존재하면 **PITR이 지정한 `--restore-time`을 지키지 않았다는 신호**다.

## DB 복원 검증 소요시간·복구 지점 (플레이스홀더 — 드릴 실측으로 확정)

> ⚠️ 아래 표는 **아직 실측되지 않았다.** 숫자를 단정하지 않으며 드릴(런북 §3~§9) 후 step 5에서 채운다.

| 항목 | 값 | 비고 |
| ---- | -- | ---- |
| 드릴 일시 (UTC) | `TBD` | |
| `T0` / `T_available` / **`T_config_ok`** / `T_creds_ready` / `T_verified` | `TBD` | 실행자 셸 `date -u`; `T_verified`는 **드릴 전체에서 한 번만** 기록하고(첫 유효 결과 판독 시), 이후 TD-B 시도는 판독 상태(`not-read → read`)만 갱신하며 그 tuple을 재사용한다. tuple 재사용은 `T_VERIFIED_TOKEN`이 이번 드릴 `TOKEN`과 같고 taskArn이 이번 계정·리전 형식이며 `T0 ≤ T_verified`일 때만 허용 |
| **DB 복원 검증 소요시간 = `T_verified − T0`** | `TBD` | **서비스 RTO 아님** |
| **중간 구간 4개**(`T_available−T0` / `T_config_ok−T_available` / `T_creds_ready−T_config_ok` / `T_verified−T_creds_ready`) | `TBD` | **필수 기록** — 복원 검증 병목 분석 |
| `restore point` (제어면) | `TBD` | `--restore-time` 지정값 |
| **선택한 복원 지점의 나이 = `T0 − restore point`** | `TBD` | ≥60초 의도적 대기 포함 |
| **백업 시스템 복구 지점 지연(참고)** | `TBD` | `T0 − LatestRestorableTime(T0)` |
| 제어면 단독 산출값 | `TBD` | `InstanceCreateTime` 기반 |
| baseline `count(*)` / `sum(clicks)` → 복원본 | `TBD` | 부등식 방향: 복원본 ≥ baseline |
| sentinel-A 존재 / sentinel-B 부재 | `TBD` | code·`created_at`(DB 시계) |

**증거 체크리스트**(step 5에서 전부 채워져야 이 ADR이 마감된다):

- [ ] 시각 5개(`T0`·`T_available`·`T_config_ok`·`T_creds_ready`·`T_verified`) + `restore point` + `LatestRestorableTime`(T0 근처) + `InstanceCreateTime`
- [ ] baseline 단일 SQL tuple(`clock_timestamp()`·`count(*)`·`sum(clicks)`·`max(created_at)`·sentinel-A expected/hits·`BASELINE_CAPTURED_AT`·source TD-A taskArn·`TDA_RESULT_READ_STATE`). 재진입의 `not-read`는 빈 tuple의 최초 판독만 허용하고, `read`는 보존 tuple과 현재 로그가 전부 같을 때만 재사용한다
- [ ] 복원본 검증 SQL 출력(파싱한 `row_count`·`clicks_sum` + sentinel-A/B + `pg_settings`의 `rds.force_ssl`)
- [ ] `rds.force_ssl` 실효값 + `FORCE_SSL_OBTAINED` + 근거 TD-B taskArn(데이터 결과 플래그와 분리)
- [ ] 최초 셸 `T_verified` 값·source·TD-B taskArn·**`T_VERIFIED_TOKEN`**(드릴 전체 1회 tuple)과, 그와 **별도 행**인 시도별 `TDB_RESULT_READ_STATE`. 재진입의 `not-read`는 tuple이 비었을 때 최초 판독을 허용하고, 완전한 tuple이 있으면 계보 확인 후 보존한다. `read` 상태의 시각 유실·계보 불일치·`unknown`이면 `t_verify_db`를 별도 DB 시계 증거로만 기록
- [ ] 최초 복원 호출 직전 봉인한 sentinel-B(`RESTORE_PRECHECK_B_CODE`·`RESTORE_PRECHECK_B_CREATED_AT`)와 판정에 쓴 B의 정확 일치 여부
- [ ] 구성 게이트 `describe-db-instances` 출력(비공개·암호화·SG·서브넷그룹·엔진/버전/클래스 · **붙은 파라미터그룹 이름 = 운영 `$PROD_PG`** ∧ 그 적용이 `in-sync`). **이름과 적용 상태를 함께 적는다** — `in-sync`만으로는 기본 그룹도 통과한다
- [ ] 정리 게이트 잔존 목록 2분류 출력 + 삭제 요청 시각·부재 확인 시각
- [ ] **단일 변경 작업 락 기록** — 변경 관리 기록 ID·owner·`$TOKEN`·획득/**만료(`LOCK_EXPIRES_AT`, UTC ISO8601)**/갱신 이력/해제 시각(런북 §2.6 원장 행). 락 없이 시작한 드릴은 위 §4의 상한 근거가 성립하지 않으므로 이 항목이 비면 (d)를 포함한 어떤 축도 마감하지 않는다. **만료 시각이 비면 락 잔여 TTL 게이트가 도출될 수 없으므로 같은 취급이다**
- [ ] **세션별 AWS 실행 주체** — 원장에 고정한 `LEDGER_ACCOUNT_ID`·`LEDGER_CALLER_ARN`과, **모든 세션**(신규 2회차·재진입·긴 대기 후 재도출)이 그것과 정확 일치를 확인한 기록 + 그 세션의 권한 4그룹 확인 결과·시각(런북 §2.5 ⑤ 표의 세션 열). 주체가 바뀐 채 진행한 구간이 있으면 그 사실을 남긴다 — 만든 리소스의 정리 권한이 달랐을 수 있다
- [ ] 셸↔AWS 시계 오차 측정값, 이미지 digest, **실행 셸(`bash <버전>` — 신규·재진입·정리 세션 각각)**, **세션별 변경 자격 6요소(`TOOLS_OK`·`EXCLUSIVE_DRILL_LOCK_OK`+잔여초·`SESSION_AWS_OK`·`MUTATION_CONTEXT_OK`·`DRILL_MUTATION_OK`, 그리고 `SHELL_OK`)**. 셸 종류는 ARN 경계·단어 분할을 통해 **소유권 대조와 정리 판정의 전제**이고, `python3`는 시각 파싱의 유일한 수단이라 부재 시 되돌릴 수 없는 쓰기 뒤에 "형식 오류"로 오진단된다 — 둘 다 시계 오차와 같은 층의 실행 환경 증거다(런북 머리말·§2.1 ⓪·§2.5 ⓪·§10.3)
- [ ] 최초 복원 호출 `RESTORE_PRECHECK_*` tuple, 복원 재시도 상태, sentinel-B 시도별 `dispatched`·응답·마진 판정
- [ ] 원본 출력 스냅샷은 `docs/postmortems/evidence/rds-restore-<날짜>/`(gitignore·로컬), **`url`·비밀번호 제외**

### 드릴 verdict (4축 — 부분 성공을 "성공"으로 뭉개지 않는다)

각 축의 판정 상태는 **`PASS` / `FAIL` / `미완`** 셋 중 하나다(런북 §1과 같은 3치). **`미완` = 판정 재료를 얻지 못한 판정 유보 상태**이며 승인된 plan의 최종 성공 verdict를 대신하지 않는다. 축별·전체 합성은 **확인된 FAIL > 미완 > PASS** 순서다. FAIL 재료가 하나라도 있으면 다른 재료가 미획득이어도 FAIL이고, FAIL 없이 재료만 부족할 때 미완이며, 모든 조건을 확인해야 PASS다.

| 축 | 판정 | 근거 | 미완이면 그 신호 |
| -- | ---- | ---- | ---------------- |
| (a) 구성 동등성 | `TBD` | 비공개·암호화·SG/서브넷그룹·엔진/버전/클래스 ∧ **PG 이름 = `$PROD_PG`** ∧ PG `in-sync` ∧ `FORCE_SSL_OBTAINED=1` ∧ `rds.force_ssl` = `on`(또는 `1`) | `SAFE_STATE=unknown`(구성 조회 실패 **또는 기대값 미확보**) / `CONFIG_STATE=unknown`(PG 상태 조회 실패 또는 기대 이름 미확보) / `FORCE_SSL_OBTAINED=0` |
| (b) 데이터 복원 | `TBD` | sentinel-A 존재 ∧ sentinel-B 부재 ∧ 비교 가능한 경우 파싱한 복원본 집계 ≥ 완전한 baseline tuple | `BASELINE_EVIDENCE_OK=0` / `VERIFY_RESULT_OK=0` |
| (c) 측정 | `TBD` | `VERIFY_RESULT_OK=1` ∧ 최초 셸 `T_verified` 원장값으로 DB 복원 검증 소요시간·복구 지점 지연 숫자 확보 | `VERIFY_RESULT_OK=0` / `T_VERIFIED_OK=0` |
| (d) 정리 | `TBD` | 런북 §10.9 (A) 조치 필요 잔존 = 0 | 없음(§10.9가 상태 불명을 (A)로 흡수해 2치로 닫는다) |

**전체 = 넷 전부 PASS일 때만 "드릴 성공".** 확인된 FAIL 축이 하나라도 있으면 다른 축이 미완이어도 전체 FAIL이고, FAIL 없이 미완만 있으면 전체는 판정 유보다. 둘 다 승인된 plan의 성공 조건은 충족하지 못한다. (a) FAIL·(b) PASS라면 **"데이터 복원 PASS / 구성 동등성 FAIL"**의 부분 성공/실패로 기록하고, 미완은 오른쪽 열의 신호와 원인을 남긴 뒤 재진입·재드릴한다.

## 1차 출처 (이 설계가 성립하는 근거)

**RDS — 복원**

- **트랜잭션 로그 업로드 주기 = 복구 지점의 구조적 하한**: [Restoring a DB instance to a specified time](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_PIT.html) — _"RDS uploads transaction logs for DB instances to Amazon S3 **every five minutes**."_ / _"To see the latest restorable time … look at the value returned in the `LatestRestorableTime` field."_ ⇒ 복구 지점 지연이 분 단위로 나오는 것은 **정상**이며 튜닝 대상이 아니다.
- **복원본은 원본 구성을 자동 승계하지 않는다**: 같은 문서 — _"Restored DB instances are automatically associated with the **default DB parameter and option groups**. However, you can apply a custom parameter group and option group by specifying them during a restore using the AWS CLI or RDS API."_ / _"you can choose the default virtual private cloud (VPC) security group. Or you can apply a custom VPC security group."_ 로컬 `aws rds restore-db-instance-to-point-in-time help`도 동일 — _"The target database is created with most of the original configuration, but … with the **default security group, the default subnet group, and the default DB parameter group**."_
- **`--restore-time` 제약**: 로컬 help — _"Must be a time in Universal Coordinated Time (UTC) format."_ / _"Must be **before** the latest restorable time for the DB instance."_ / `--use-latest-restorable-time`과 동시 지정 불가.
- **복원 명령의 관리 비밀번호는 Oracle 전용**: 로컬 `restore-db-instance-to-point-in-time help` · `restore-db-instance-from-db-snapshot help` 모두 Constraints에 _"Applies to RDS for Oracle only."_ [Password management with Amazon RDS and AWS Secrets Manager](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/rds-secrets-manager.html)도 같은 취지(복원 계열은 "RDS for Oracle only"로 열거). ⇒ **PostgreSQL 복원본은 `modify-db-instance --manage-master-user-password`로 사후 전환**한다(이 명령에는 엔진 제약이 없다 — 로컬 help 실측).
- **관리 시크릿의 수명주기**: 같은 문서 — _"If you delete a DB instance that manages a secret in Secrets Manager, the secret and its associated metadata are **also deleted**."_ **즉시 삭제인지 복구 창을 둔 예약 삭제인지는 문서에 명시가 없다** → 판별은 `describe-secret`의 `DeletedDate` 유무로 한다(로컬 help — _"The date the secret is scheduled for deletion. If it is not scheduled for deletion, this field is omitted … recovery window of at least 7 days"_). 런북 §10.8은 **부재 / 삭제 예약 / 예약도 아님**의 3분기로 어느 쪽이든 판정이 성립하게 설계했다. 다만 _"also deleted"_ 가 **비동기**라는 점 때문에, DB 삭제 직후의 `present`는 곧바로 잔존으로 확정하지 않고 **30초 간격 총 3회 재조회**한 뒤에만 사용자 주도 삭제를 시도한다 — 정상적인 전파 지연에 `DeleteSecret`을 쏘면 서비스 관리 시크릿이라 거부되고, 정상 동작이 에스컬레이션으로 기록된다.
- **`MasterUserSecret.SecretStatus`** 값: `creating` → `active` → (`rotating`/`impaired`) — 로컬 `describe-db-instances help` 및 위 RDS 문서.
- **waiter 상한**(로컬 help): `db-instance-available`·`db-instance-deleted` = **30초 × 60회, 초과 시 exit 255**. `db-instance-deleted`의 매처는 **`length(DBInstances) == 0`** ⇒ "조회에서 사라짐"이 성공 조건이고, 이것이 **"삭제 요청이 아니라 삭제 완료를 과금 종료로 본다"** 는 규율과 대응한다.

**ECS — 검증 태스크와 정리**

- **`valueFrom`의 `:json-key::` 접미**: `aws ecs register-task-definition help`는 "full ARN of the secret"까지만 기술하고 이 문법을 설명하지 않는다. 근거는 ECS 개발자 가이드([Passing sensitive data to a container](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/specifying-sensitive-data-secrets.html))이며, **실증은 운영 `ecs.tf:55`가 그 형태로 동작 중**이라는 사실이다. 키를 생략하면 SecretString 전체가 주입되므로 반드시 `:password::`를 쓴다.
- **결과적 일관성**: [RunTask API](https://docs.aws.amazon.com/AmazonECS/latest/APIReference/API_RunTask.html) — _"The Amazon ECS API follows an **eventual consistency** model … the result of an API command you run that affects your Amazon ECS resources **might not be immediately visible** to all subsequent commands you run."_ ⇒ **`MISSING`을 "이미 끝났다"로 읽지 않는다**(아래 §한계 3).
- **멱등성(client token)**: [Ensuring idempotency](https://docs.aws.amazon.com/AmazonECS/latest/APIReference/ECS_Idempotency.html) — _"The time to live (TTL) for the `RunTask` client token is **24 hours**."_ / _"You should not reuse the same client token for different requests."_ / 같은 토큰 + 같은 파라미터 재시도는 **원응답 반환**, 파라미터가 다르면 **`ConflictException`**(응답 `resourceIds`에 그 토큰에 묶인 taskArn이 들어온다). 멱등 범위는 **클러스터 단위**. ⇒ 드릴 토큰을 **초 단위**로 만든 이유가 이 TTL이다(같은 분 안의 재드릴이 시도별 토큰까지 재사용하면 새 태스크가 뜨지 않는다).
- **태스크 수명주기**: [Amazon ECS task lifecycle](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/task-lifecycle-explanation.html) — _"**DELETED**: This is a transition state when a task stops. This state is not displayed in the console, but is displayed in `describe-tasks`."_ ⇒ 종결 집합은 `STOPPED` ∪ `DELETED`.
- **`MISSING`의 의미**: [API failure reasons](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/api_failures_messages.html) — `DescribeTasks`의 `MISSING`은 _"The specified task wasn't found. Verify the correct cluster or Region is specified and that both the task ARN or ID is valid."_ 즉 **"찾지 못함"일 뿐** 보관 만료·전파 지연·잘못된 클러스터를 구분해 주지 않는다. **같은 문서가 `DeleteTaskDefinitions`의 `MISSING`은 열거하지 않는다** → 그 작업의 `MISSING` 조건은 **미확인**으로 두고 런북이 보수적으로 처리한다.
- **STOPPED 태스크 보관**: [Viewing stopped task errors](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/stopped-task-errors.html) — _"Stopped tasks only appear in the console for 1 hour."_ **콘솔 기준만 문서화돼 있고 API 조회 보관 창은 명시가 없다** → 재탐색은 드릴 종료 직후 수행한다.
- **태스크 정의 삭제**: [DeleteTaskDefinitions](https://docs.aws.amazon.com/AmazonECS/latest/APIReference/API_DeleteTaskDefinitions.html) — _"You must deregister a task definition revision before you delete it."_ / _"it is immediately transitions from the `INACTIVE` to `DELETE_IN_PROGRESS`."_ / _"A task definition revision will stay in `DELETE_IN_PROGRESS` status until all the associated tasks and services have been terminated."_ / _"You can specify up to **10** task definitions as a comma separated list."_ **응답(HTTP 200) 안에 `taskDefinitions[]`와 `failures[]`가 함께 온다** ⇒ 삭제 판정 단위는 요청이 아니라 **revision ARN**이다.
- **`wait tasks-stopped`**(로컬 help) — _"Wait until JMESPath query `tasks[].lastStatus` returns STOPPED for **all elements** … poll every **6 seconds** … exit with a return code of **255 after 100 failed checks**."_ ⇒ 상한 600초, 매처가 `tasks[]`만 보므로 **`failures[]`의 MISSING은 성공 조건을 영원히 못 만족**하고, "all elements"라 **ARN 하나씩** 건다.
- **`list-tasks` 필터 제약**(로컬 help) — _"When you specify startedBy as the filter, **it must be the only filter that you use**."_ / _"Although you can filter results based on a desired status of `PENDING`, this doesn't return any results."_ ⇒ 활성 회수와 종료 회수를 **두 경로로 분리**한다. `describe-tasks`는 _"A list of up to **100** task IDs or full ARN entries."_

## 결과 / 트레이드오프 (운영자가 알아야 할 함정)

### 한계 1 — 파라미터그룹·SG·서브넷그룹은 복원에 자동 승계되지 않는다

복원 명령에 `--db-parameter-group-name`·`--vpc-security-group-ids`·`--db-subnet-group-name`을 **빼먹으면** 복원본이 **엔진 기본 그룹·기본 SG**로 떠 운영과 구성이 달라지고 네트워크 경계도 달라진다. 그래서 이 프로젝트에서 **"복원 = 별도 단계가 붙는 작업"** 이다.

**단, "기본 그룹이면 TLS가 풀린다"는 이 엔진에서는 사실이 아니다.** AWS는 _"The `rds.force_ssl` parameter default value is **1 (on) for RDS for PostgreSQL version 15 and later**. For all other RDS for PostgreSQL major versions 14 and older, the default value of this parameter is 0 (off)."_ 라고 명시한다([Using SSL with a PostgreSQL DB instance](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/PostgreSQL.Concepts.General.SSL.html)) — 대상은 PostgreSQL **16**이므로 기본 그룹에서도 비TLS 접속은 거부된다. 따라서 파라미터그룹을 명시하고 사후 확인하는 근거는 TLS 노출이 아니라 **구성 동등성 자체**(드릴의 (a) 축이 측정하려는 것)와 앞으로 추가될 커스텀 파라미터의 보존이다.

런북 §5가 이 옵션들을 필수로 못박고, **§6 ④가 파라미터그룹 축을 판정한다** — "붙은 그룹 이름이 운영 것과 같은가"와 "그 적용이 끝났는가(`ParameterApplyStatus=in-sync`)"를 **둘 다** 본다. 적용 상태만 보면 기본 그룹도 `in-sync`라 옵션 누락이 그대로 통과하기 때문이다. **어느 쪽이 어긋나도 드릴을 중단하지는 않는다**: 복원본은 여전히 비공개·암호화·기대 SG이므로 접속을 막을 이유가 없고, (a)만 FAIL로 확정한 뒤 (b)·(c) 재료를 확보하는 편이 낫다.

또 이 레포의 `rds.force_ssl`은 `apply_method="pending-reboot"`이라 복원본에서 `pending-reboot`로 보고될 수 있다 → **재부팅 후 `in-sync` 재확인**이 절차에 포함되고, reboot 소요는 **DB 복원 검증 소요시간**에 포함된다.

### 한계 2 — 드릴 종료 시점에 태스크 정의의 물리 삭제는 미완일 수 있다

`DELETE_IN_PROGRESS`는 연결 태스크·서비스가 전부 종료될 때까지 유지되고 **전용 waiter가 없다.** 차단 리소스가 사라진 뒤에도 시간이 걸린다: [Deleting an Amazon ECS task definition revision](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/delete-task-definition-v2.html) — _"Amazon ECS tasks - The task definition deletion can take **up to 1 hour** to complete after the task is stopped."_ (서비스 배포·task set은 최대 24시간). 드릴 창 안에서 물리적 부재를 기다리면 **정상 동작을 FAIL로 오판하거나 무기한 대기**하게 된다. **TD는 과금 대상도, 권한을 보유한 주체도 아니므로** 이 잔재는 (B) 정상 비동기 진행으로 분류하고 익일 1회 지연 확인만 한다.

### 한계 3 — 정리 판정에서 `MISSING`은 종결 관측 이력이 있을 때만 종결로 인정했다

ECS API의 결과적 일관성 때문에 `MISSING`은 "보관 만료(끝남)"일 수도, "아직 전파되지 않은 신규 태스크"일 수도 있다. 후자를 종결로 세면 **뒤늦게 기동해 운영 마스터 비밀번호를 주입받은 태스크**가 있는데도 정리가 PASS로 기록된다. 그래서 드릴은 **모르는 것을 끝난 것으로 세지 않는다** — 종결 관측 이력이 없는 MISSING은 "상태 불명"으로 **(d) FAIL**이다. 이 보수성 때문에 **정상 드릴이 FAIL로 기록될 여지**가 있고, 그 경우 원인은 "정리 실패"가 아니라 "관측 기록 부족"이므로 증거에 그렇게 남긴다.

### 한계 4 — 검증 컨테이너를 통한 자격증명 반출 경로

baseline 태스크(TD-A)는 **운영 마스터 비밀번호**를 외부 공개 이미지(`postgres:16`) 컨테이너에 주입하고, `app` SG에는 `0.0.0.0/0:443` egress가 열려 있다. 부동 태그가 다른 이미지를 가리키거나 공급망이 침해되면 반출 경로가 성립한다. 완화: **불변 digest 고정**(TD-A·TD-B 공통, Docker Hub rate limit 폴백도 같은 digest 보존), digest를 증거에 기록, 태스크는 1회성·즉시 종료, **task role 미부여**. 잔여 위험(공식 이미지 자체의 신뢰)은 드릴 1회 규모에서 **수용**한다. 더 강한 경계가 필요하면 검증한 이미지를 ECR에 미러링해 쓴다.

### 한계 5 — sentinel 링크 행은 영구 잔존한다

앱에 링크 삭제 API가 없어 드릴이 만든 최대 5건(고정 URL `https://example.com/`)은 운영 DB에 남는다. **정리 대상이 아니라 증거**로 취급하고 code·`created_at`을 기록한다. 재드릴을 반복하면 누적되므로, 링크 삭제 경로가 생기면 그때 정리 항목으로 승격한다.

### 운영 오조작이 가장 큰 리스크

- **복원/삭제 명령을 운영 식별자에 잘못 실행** → 드릴 식별자 `linkpulse-restore-drill` 고정, 검증은 읽기 전용 쿼리만, `delete-db-instance`는 드릴 식별자에만. 운영은 `deletion_protection=true`라 실수 삭제도 방어된다.
- **운영 시크릿 오삭제(지연 장애형)** — RDS 관리 시크릿은 `rds!db-<resource-id>` 형태라 운영·드릴이 육안으로 거의 구분되지 않는다. 운영 시크릿을 지우면 **다음 태스크 기동 시 `DB_PASSWORD` 주입이 실패**해 드릴이 끝난 한참 뒤 배포 시점에 터진다 → 삭제 대상 ARN은 **반드시 드릴 인스턴스 조회로 도출**하고, 운영 ARN을 금지 목록으로 미리 출력해 대조한 뒤에만 삭제한다. 게다가 그 조회 대상은 **고정 식별자**라 다른 실행의 드릴 DB일 수 있으므로, ARN 채택 자체를 **이번 복원 호출의 소유권**(정확한 DB ARN + `purpose`·`drill-token` 태그) 위에서만 허용하고, 확인된 `(DB ARN, secret ARN)` 튜플을 원장에 남겨 **DB가 지워진 뒤의 정리도 그 튜플로만** 판단한다.
- **비용 방치** — `creating` 상태도 과금되며, **삭제 요청이 아니라 `wait db-instance-deleted` 통과**를 비용 종료로 본다. 단가는 ap-northeast-2 요금 페이지를 실행 전 확인한다(흔히 인용되는 시간당 단가는 us-east-1 기준이다).

## 범위 밖 (향후 / 별도 ADR)

- **Multi-AZ 전환** — 비용 약 2배, 자동 failover로 가용성 개선. P5+ 업그레이드 후보.
- **크로스리전 백업 복사** — 리전 장애 DR. 현재 태세는 **단일 리전 장애를 커버하지 못한다.**
- **백업 보관 연장**(7일 초과), AWS Backup 도입.
- P4(d)의 나머지: **Secrets 로테이션**(별도 plan 0008), **IAM 최소권한**(0009).
