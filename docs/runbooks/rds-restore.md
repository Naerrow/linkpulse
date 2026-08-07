# Runbook — RDS 백업 복원 검증(PITR·스냅샷) / 복원 가능성 리허설

운영 DB `linkpulse-prod-pg`의 백업에서 **격리된 별도 인스턴스로 복원**해 데이터가 조회되는지와 복원 지점을 검증한 뒤 **드릴 리소스를 전량 정리**하는 절차다. 이 문서는 **백업 복원 가능성·DB 검증 소요시간을 측정하는 드릴**이며, 손상된 운영 데이터의 역복사, 쓰기 동결, 복원본 cutover, 애플리케이션 헬스 확인, 롤백, Terraform 상태 정합을 수행하지 않는다. 따라서 **사고 복구 런북도 서비스 RTO 측정 절차도 아니다.**

- 관련: [ADR 0005 — 백업·복원 검증 태세](../adr/0005-backup-restore-dr.md) · plan [`0007-p4-rds-restore-drill`](../plans/0007-p4-rds-restore-drill/plan.md) · 알람 대응은 [alarm-response.md](alarm-response.md)가 단일 소스다(이 문서는 **알람 대응 절차가 아니다**)
- **표기**: 이 문서의 **모든 `aws` 명령은 사람이 실행**한다(AGENTS.md 가드레일 #1 — 에이전트 자율 실행 금지). **💸** = 과금 발생, **⚠️** = 상태 변경(되돌리기 어려움).
- **실행 셸은 Bash로 고정한다 — 재진입·정리 세션도 마찬가지다.** macOS 기본 셸은 zsh이고, 차이는 **둘**이다.
  - **① 히스토리 수정자**: zsh는 중괄호 없는 `$VAR:` 뒤의 한 글자를 수정자로 해석한다(`:r`·`:t`·`:c`·`:s`·`:h`·`:e`·`:a`·`:A`·`:l`·`:q`·`:u`). `$ACCOUNT_ID:role/...`이 `…012ole/...`로, `$ACCOUNT_ID:task/...`가 `…012ask/...`로 **조용히 변형된다.** 이 문서는 리터럴 콜론 앞 변수를 전부 `${VAR}:`로 감싸 이 차이를 없앴다.
  - **② 단어 분할**: zsh는 따옴표 없는 **스칼라 확장을 공백으로 나누지 않는다**(`for a in $ARNS`가 목록 전체를 **1회** 순회하고, `set -- $PENDING`이 인자 1개를 만든다). `${VAR}:` 정규화로는 닫히지 않는 차이이며, 이쪽이 더 위험하다 — **TD family 점유 검사가 조용히 통과**해 중복 revision을 만들고, **§10 정리의 재탐색·일괄 삭제가 통째로 어긋나 유료 인스턴스와 임시 IAM role이 잔존**한다.
  - 그래서 **셸 확인은 경고가 아니라 게이트다.** §2.1 ⓪(신규·재진입)과 §10.3(정리)이 `SHELL_OK`를 각각 다시 도출하고, **상태를 바꾸는 21개 호출이 전부** 그 값을 소비한다. 소비 형태는 목적에 따라 둘로 갈린다.
    - **비정리 변경 13개**(TD 등록 2·`run-task` 2·복원 2·sentinel-A/B POST 2·§6 reboot 1·§7 modify 1·§8.1 IAM 3)는 §2.5 ⓪이 **세션마다 다시 도출**하는 단일 변경 자격 **`DRILL_MUTATION_OK`**(= `SHELL_OK` ∧ `REPO_OK` ∧ **잔여 TTL이 남은** 단일 변경 작업 락 ∧ 필수 로컬 도구 ∧ **원장과 같은 AWS 주체·확인된 권한** ∧ **이번 드릴 소유의 dispatch 원장**)를 **각 게이트에서 직접** 소비하며, 같은 게이트가 **호출 직전 락 미만료**(`date -u +%s` < `LOCK_EXPIRES_EPOCH`)를 한 번 더 본다 — 자격은 ⓪ **실행 시점**의 사실이라 그 뒤 흐른 벽시계를 모르기 때문이다. 여섯 조건은 전부 **세션마다 달라지는 사실**이라 원장에서 되살아나지 않는다. **재진입은 `VALUES_OK` 생산 블록(§2.5 ⑧)을 거치지 않는 것이 정상 경로**라, 재진입에서 읽기 전용 reconciliation만으로 되살아나는 §6~§8 경로를 `VALUES_OK` 계보로는 닫을 수 없다 — 그래서 계보가 아니라 이 자격을 전 게이트가 직접 요구한다.
    - **§10.4~§10.8 정리 8개**는 §10.3이 도출한 `SHELL_OK`만 소비하고 **락을 요구하지 않는다.** 락을 못 잡은 세션이 과금을 끊지 못하면 유료 인스턴스가 남기 때문이다(§0·§10.2).
    - `bash -n`/`zsh -n`은 문법만 보므로 두 차이를 다 놓친다.
- **이 드릴은 운영 계정·운영 클러스터에 부수 리소스를 만든다.** "인프라 무변경"이 아니다 — 무엇이 허용인지는 §2.2가 전부다.
- 전 구간에서 **비밀번호를 명령줄·override·로그·증거에 넣지 않는다.** 시크릿은 태스크 정의의 `secrets`로만 참조한다.

---

## 0. 정리 명령 요약 — **중단 시 여기부터** (상세는 §10)

드릴이 어디서 멈추든(오류·상한 초과·운영 알람 발생·판단 중단) **먼저 정리 게이트로 들어온다.** 절차는 멱등이라 몇 번을 다시 돌려도 안전하고, 없는 리소스의 NotFound는 성공으로 친다.

```bash
# 이 요약은 중단 직후 **새 터미널**에서 붙여 넣는 자리다 — macOS 기본 셸이 zsh라 정리 세션이
# 특히 위험하다(ARN 변형·단어 분할 → 소유권 대조 실패 → 유료 인스턴스 잔존). 셸부터 확인한다.
# 형태는 §2.1 안전 가드 규약을 따른다(`|| echo` 로 경고만 내지 않는다). 이 요약의 조회는 읽기 전용이고,
# 상태를 바꾸는 §10.4~§10.8은 §10.3이 다시 도출하는 `SHELL_OK`를 게이트로 소비한다.
if [ -n "${BASH_VERSION:-}" ]; then
  echo "bash $BASH_VERSION"
else
  echo "STOP: Bash가 아니다 — 'bash' 로 들어간 뒤 다시 시작한다(§2.1 ⓪ · §10.3)"
fi

export AWS_REGION=ap-northeast-2 AWS_PAGER=""
DRILL_DB=linkpulse-restore-drill
DRILL_ROLE=linkpulse-restore-drill-exec
CLUSTER=linkpulse-prod-cluster
TD_A=linkpulse-restore-drill-a
TD_B=linkpulse-restore-drill-b
LOG_GROUP=/ecs/linkpulse-prod-app
SRC_DB=linkpulse-prod-pg                # 운영 — 삭제 명령에 절대 이 값이 들어가면 안 된다
TOKEN="restore-drill-YYYYMMDD-HHMMSS"    # ← 원장의 드릴 토큰으로 치환(따옴표 안에)
# **`DRILL_DIR`는 §10의 앵커다.** 위험 호출(run-task·register-task-definition)의 미대사 상태는 사람이
# 옮겨 적는 값이 아니라 이 디렉터리 안의 `dispatch/` 파일 원장에 있고(§2.6), 그 파일은 **AWS 호출 전에**
# 쓰이므로 터미널이 죽어도 남는다. 경로가 틀리면 §10.3이 탐색을 unknown으로 닫는다.
DRILL_DIR="$HOME/linkpulse-evidence/rds-restore-$TOKEN"   # ← 원장의 작업 디렉터리(§2.1에서 만든 경로)

# **원장 입력 — §10이 스스로 재도출할 수 없는 값은 전부 여기서 붙여 넣는다.**
# 정리 세션은 §2.1을 거치지 않고 여기서 §10.3으로 바로 들어오므로(§10.3 ⓪-0), 이 자리가 §10의 **유일한 원장 입력구**다.
# 비워 두면 §10.3의 형식·소유권 대조가 통째로 어긋난다. 특히 태스크 원장 3개(`TASK_ARNS_LEDGER`·`TDA_TASK_ARN`·
# `TDB_TASK_ARN`)가 비면 **보관 만료·응답 유실 태스크가 합집합에서 빠진 채** 잔존 0건으로 보여 (d)가 PASS로 기록된다.
ACCOUNT_ID=""              # ← 원장. task/TD ARN 형식 대조의 기준값 — 비면 전건이 형식 불일치가 된다
PROD_SECRET=""             # ← 원장. 운영 시크릿 ARN(삭제 대상에서 배제하는 기준값)
RESTORE_EXPECTED_ARN=""    # ← 원장. 비어 있어도 되지만 값이 있으면 소유권 ⑤로 쓴다(§2.7)
TD_A_ARN=""                # ← 원장. §10.3 `REVS_LEDGER` 입력(정확한 revision ARN)
TD_B_ARN=""
TASK_ARNS_LEDGER=""        # ← 원장 §2.6 시도별 표의 taskArn **전건**(공백 구분)
TDA_TASK_ARN=""            # ← 채택한 TD-A taskArn
TDB_TASK_ARN=""            # ← 채택한 TD-B taskArn
# **미대사 시도는 여기에 붙여 넣지 않는다** — `$DRILL_DIR/dispatch/`(파일 원장)가 단일 소스다.
# 사람이 세어 옮기는 값으로 두면 ⓐ 셸 산술이 빈 값을 0으로 만들고 ⓑ 같은 client token 재전송이
# 이중 계상되며 ⓒ 터미널이 죽는 순간 값이 사라진다 — 셋 다 이 값이 막으려던 바로 그 창이다.
DRILL_SECRET=""            # ← 원장. DB가 이미 지워졌다면 §10.8이 쓸 수 있는 유일한 근거다
SECRET_LIFECYCLE=""        # ← 원장 값(not-created | create-requested | arn-known). 빈 값은 §10.8이 (A)로 막는다
SECRET_OWNER_DB_ARN=""     # ← 원장. §7에서 소유권이 확인된 시점의 드릴 DB ARN
SECRET_PROVENANCE_OK=""    # ← 원장 값 `0`|`1`. DB가 이미 삭제된 경로에서 시크릿 소유권의 유일한 근거이므로
                           #   실행 가능한 기본값(0)을 두지 않는다 — 빈 값은 §10.8이 (A)로 막는다
# 아래 4개는 **붙여 넣지 않는다** — 각 §10.x가 이번 세션에서 다시 도출한다. 다만 미설정이
# "잔존 0건"으로 새지 않도록 §2.1과 같은 fail-closed 초기값을 둔다(§10.9 (A)).
TD_DISCOVERY_STATE=unknown
TASK_DISCOVERY_STATE=unknown
ROLE_STATE=unknown
SECRET_STATE=unknown

# ⓪ 재탐색 대사(§10.3) — 이 요약에서 실행하는 것은 **읽기 전용 조회뿐**이다
aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" \
  --query 'DBInstances[0].[DBInstanceStatus,MasterUserSecret.SecretArn]' --output text   # NotFound면 없음 = 정상
aws ecs list-tasks --cluster "$CLUSTER" --started-by "$TOKEN"   # 다른 필터와 함께 쓰지 않는다

# ① 과금부터 끊는다 = **§10.4를 그대로 실행한다** — 삭제 명령은 여기에 싣지 않는다.
#    §10.4의 게이트(부재/조회실패/존재 3갈래 + 고정 식별자 + purpose·drill-token 태그 **값** + 이번 세션의 복원 소유권)를 거치지 않은
#    delete-db-instance 는, 변수가 오염된 재진입 세션에서 **다른 인스턴스를 지울 수 있다.**
#    같은 게이트를 여기에 복제하면 두 벌이 갈라지므로 요약은 §10.4를 가리키기만 한다.
# ② 태스크: 살아 있는 시도를 반드시 내린다(운영 마스터 비밀번호를 쥔 컨테이너) — §10.5
# ③ TD: revision마다 deregister → 성공분만 delete(§10.6)   ④ role 인라인→detach→delete(§10.7)
# ⑤ aws rds wait db-instance-deleted --db-instance-identifier "$DRILL_DB"   ← 여기 통과가 과금 종료(§10.8)
# ⑥ 드릴 시크릿 부재 확인  ⑦ 운영 무변경 확인  ⑧ 잔존 목록 2분류 출력(§10.8·§10.9)
```

**전체 순서와 판정 기준은 §10이 단일 소스다.** 위 요약만 보고 판정하지 않으며, **상태를 바꾸는 명령은 §10의 해당 절에서만 실행한다**(요약을 붙여 넣는 것만으로 리소스가 지워지면 안 된다).

---

## 1. 성공 판정 4축 (넷을 각각 **PASS / FAIL / 미완(판정 유보)**으로 기록)

**세 번째 상태 「미완」이 있는 이유**: 조회·파싱이 실패해 판정 재료를 **얻지 못한** 상태를 실제 결함이 확인된 FAIL과 구분하기 위해서다. 미완은 승인된 plan의 최종 성공 verdict를 대신하는 값이 아니라, 원인 해소 후 재진입·재드릴해야 하는 **판정 유보 상태**다. 각 축은 **확인된 FAIL > 미완 > PASS** 순으로 합성한다. 즉 FAIL 재료가 하나라도 있으면 다른 재료가 미획득이어도 그 축은 FAIL이고, FAIL 없이 재료만 부족할 때 미완이며, 모든 PASS 조건을 확인했을 때만 PASS다.

| 축 | PASS 조건 | 미완(판정 불가) 신호 |
| -- | --------- | -------------------- |
| **(a) 구성 동등성** | 복원본이 available ∧ `describe`상 **비공개·암호화·기대 SG/서브넷 그룹·엔진/버전/클래스** 일치 ∧ **파라미터그룹 이름 = 운영 `$PROD_PG` ∧ 그 적용이 `in-sync`**(이름 없이 `in-sync`만 보면 기본 그룹도 통과한다) ∧ 복원본에서 조회한 **`rds.force_ssl` 실효값 = `on`(또는 `1`)** | `SAFE_STATE=unknown`(§6 구성·태그 조회 실패) 또는 `CONFIG_STATE=unknown`(파라미터그룹 상태 조회 실패 **또는 기대 이름 `$PROD_PG` 미확보**) 또는 `FORCE_SSL_OBTAINED=0`(§8.3 실효값 미획득) |
| **(b) 데이터 복원** | **sentinel-A 전건 존재 ∧ sentinel-B 부재** ∧ (복구 지점이 `T_baseline`보다 뒤일 때 = `BASELINE_COMPARABLE_OK=1`) **복원본 `row_count`·`clicks_sum` ≥ 원장 baseline tuple** — PITR은 항상 성립하고, §11 스냅샷 모드는 성립하지 않을 수 있어 그때는 숫자만 기록한다 | `BASELINE_EVIDENCE_OK=0`(시각·집계·A expected/hits·source task tuple 불완전, §4.3) 또는 `VERIFY_RESULT_OK=0`(복원본 시각·집계·A/B hits 미획득, §8.3) |
| **(c) 측정** | DB 복원 검증 소요시간·복원 지점 지연이 숫자로 기록됨 | `VERIFY_RESULT_OK=0` 또는 `T_VERIFIED_OK=0` → 최초 셸 시계 `T_verified`가 없다(현재 시각·추정값으로 대체하지 않는다) |
| **(d) 정리** | **§10.9 「잔존 목록 2분류」의 (A) 조치 필요 잔존 = 0** — 판정 정의는 **§10.9 하나뿐**이고 이 표는 참조만 한다 | 없음 — §10.9는 상태 불명을 **(A)로 흡수**해 2치로 닫는다(그래서 (d)에는 미완이 없다) |

**전체 판정 상태도 `FAIL > 미완 > PASS` 순서다.** 확인된 FAIL 축이 하나라도 있으면 다른 축이 미완이어도 전체는 FAIL이고, FAIL 없이 미완만 있으면 전체는 미완, 넷 전부 PASS일 때만 전체 PASS다. 승인된 plan의 최종 verdict는 **넷 전부 PASS일 때만 "드릴 성공"**이라는 2치 기준을 그대로 따른다. 따라서 FAIL·미완은 모두 성공 조건 미충족이며, 축별 상태와 원인을 병기한 **부분 성공/실패 또는 판정 유보**로 남긴다. (a) FAIL·(b) PASS라면 **"데이터 복원 PASS / 구성 동등성 FAIL"**로 기록한다.

### 1.1 측정 정의 — 무엇을 어느 시계로 적는가

기록할 시각(전부 **UTC ISO-8601 초 단위** `YYYY-MM-DDTHH:MM:SSZ`) — `T0`와 `T_verified`는 각각 **최초 유효 시점에 한 번만** 기록하고 재진입에서 새로 찍지 않는다:

| 시각 | 정의 | 시계 출처 |
| ---- | ---- | --------- |
| `T0` | 복원 API 호출 **직전** | 실행자 셸 `date -u` |
| `T_available` | `wait db-instance-available` 통과 | 실행자 셸 `date -u` |
| `T_config_ok` | **구성 게이트 통과**(필요했다면 reboot·재대기까지 끝난 시각) | 실행자 셸 `date -u` |
| `T_creds_ready` | 드릴 시크릿 `active` + 인스턴스 `available` 복귀 | 실행자 셸 `date -u` |
| `T_verified` | 검증 쿼리 결과 확보 | 실행자 셸 `date -u` |
| `T_baseline` | baseline 집계 스냅샷 시각 | **DB 시계**(`clock_timestamp()`) |
| sentinel `created_at` | 링크 생성 시각 | **DB 시계**(`INSERT … RETURNING`) |
| `restore point` · `LatestRestorableTime` · `SnapshotCreateTime` · `InstanceCreateTime` | 복원 지점·백업 지표 | **RDS 제어면 시계** |

- **DB 복원 검증 소요시간 = `T_verified − T0`**. **중간 구간 4개(`T_available − T0` / `T_config_ok − T_available` / `T_creds_ready − T_config_ok` / `T_verified − T_creds_ready`)도 필수 기록**이다.
- §8.3 결과 블록을 다시 읽을 때는 현재 TD-B taskArn에 묶인 원장 tuple(`TDB_RESULT_READ_STATE`·`T_verified`·source·taskArn)을 쓴다. 재진입이어도 상태가 `not-read`이고 **기존 tuple이 아예 없으면** 그 세션의 최초 유효 판독 시각을 기록한다. `not-read`인데 **완전한 기존 tuple이 있으면**(후속 시도) 그 시각을 보존한 채 현재 taskArn만 `read`로 올린다 — 최초 측정값은 새 시도로 바뀌지 않는다. `read`인데 원장 시각이 유실됐거나 상태가 `unknown`이면 `t_verify_db`(DB 시계)는 **별도 진단 증거**로만 남기고, 셸 시계 `T_verified`를 현재 시각이나 DB 시각으로 대체하지 않는다. 이 경우 (b)는 판정할 수 있어도 (c)는 미완이다.
- 이 값에는 §6 구성 확인·필요 시 reboot와 §7의 **드릴 전용 자격증명 전환**이 모두 들어간다. 운영 서비스가 복원본을 사용하도록 전환하거나 정상 응답하는 시점은 측정하지 않으므로 **서비스 RTO로 환산하거나 사고 복구 약속에 사용하지 않는다.**
- `T_config_ok`는 구성 확인 구간과 자격증명 전환 구간을 분리해 병목을 찾기 위한 측정점일 뿐, 서비스 복구 시간을 계산하기 위한 차감 기준이 아니다.
- **인스턴스 소실·데이터 손상 사고 복구는 모두 현재 범위 밖이다.** retained backup 식별·데이터 역복사 또는 cutover·쓰기 정합·서비스 검증·롤백 절차를 별도 구현하고 끝점까지 실측하기 전에는 이 런북을 사고 대응에 사용하지 않는다.
- **복구 지점 지연(1차 지표) = `T0 − restore point`** → ADR 라벨은 **"선택한 복원 지점의 나이"**. 이 값에는 절차상 **의도적 대기가 최소 60초 포함**된다(sentinel-B 마진 + 사람 조작 시간) → **백업 시스템의 능력치로 읽지 않는다.**
- 시스템 능력치는 **별도 지표**로 남긴다: **"백업 시스템 복구 지점 지연(참고)" = `T0 − LatestRestorableTime(T0 시점 관측값)`**.
- CloudWatch 로그 타임스탬프는 참고용이며 측정에 쓰지 않는다. **드릴 도중 시계 출처를 바꾸지 않는다.**

**교차 시계 비교는 셋이다** — 각각의 흡수 방법을 지킨다:

| # | 비교 | 어디에 쓰이나 | 흡수 방법 |
| - | ---- | ------------- | --------- |
| (i) | `T_baseline`(DB) vs `restore point`(제어면) | **(b) 판정 근거** — "복원 지점 > `T_baseline`"이라야 `count(*) ≥ baseline` 단조성 논증이 성립 | **`restore point ≥ T_baseline + 60초`** 숫자 마진(§4) |
| (ii) | `T0`(셸) vs `restore point`(제어면) | **1차 지표** 복구 지점 지연 | ① 프리플라이트에서 **셸↔AWS 시계 오차 1회 측정·기록**(근사값) ② 복원 응답의 **`InstanceCreateTime`(제어면)** 병기 → 제어면 단독 산출값도 남긴다 |
| (iii) | sentinel-B `created_at`(DB) vs `restore point`(제어면) | sentinel-B 부재 판정 | **60초 마진**(§4) |

- **집계 대조**: 복원본은 `count(*) ≥ baseline` ∧ `sum(clicks) ≥ baseline`이어야 한다(복원 지점 > `T_baseline`이고, 라우터에 DELETE 경로가 없어 `clicks`는 증가만 하므로 단조 증가). **미만이면 실패 신호.** 복원 후 운영을 다시 읽어 비교하지 않는다(서로 다른 시점 비교가 된다).
- **증거에서 `url`은 제외**한다(토큰·개인정보 유입 가능) — `code`·`clicks`·`created_at`만 남긴다.

---

## 2. (a) 프리플라이트 — 여기서 막히면 드릴을 시작하지 않는다

### 2.1 환경 고정

이 런북의 모든 명령은 아래 환경을 전제한다(리전 오조작 방지 — 명령마다 `--region`을 반복하지 않는다).

> **안전 가드의 형태는 문서 전체에서 하나다.** 상태를 바꾸는 명령 앞의 확인은 전부
> **`if <조건 전부 참>; then <명령> else echo "STOP: …" fi`** 로 쓴다. **`STOP` 이후의 상태 변경 명령은 실행하지 않는다.**
> 여러 API를 순서대로 호출하는 상태 머신에서 뒤 단계가 실패하면 앞 단계의 성공분은 이미 존재할 수 있으므로, 원장에 남기고 §10에서 정리한다.
> `… || echo "STOP"` 처럼 메시지만 내고 다음 줄이 실행되는 형태는 쓰지 않는다(경고가 아니라 게이트여야 한다).
> 대기 루프도 같은 원칙으로 **성공 플래그**(`OK=1`)를 세우고, 플래그가 서지 않으면 **시각 기록·후속 명령을 건너뛴다.**

```bash
# ⓪ 셸 확인 — zsh면 여기서 멈춘다(위 표기 절: ARN의 `${VAR}:` 뒤 한 글자가 히스토리 수정자로 먹힌다).
SHELL_OK=0
if [ -n "${BASH_VERSION:-}" ]; then
  SHELL_OK=1
  echo "SHELL_OK=1 bash $BASH_VERSION"
else
  echo "STOP: Bash가 아니다(zsh 등) — 'bash' 를 실행해 새 셸로 들어간 뒤 이 블록부터 다시 시작한다"
  echo "      이 세션에서는 상태를 바꾸는 명령을 하나도 실행하지 않는다"
fi

export AWS_REGION=ap-northeast-2
export AWS_PAGER=""
aws sts get-caller-identity            # 계정 ID·주체 확인 → 원장에 기록
echo "$AWS_REGION"                     # ap-northeast-2 인지 눈으로 확인

REPO_DIR="$HOME/mini-project/linkpulse"   # 레포 경로(§2.2의 git 기준점은 항상 여기서 찍는다)
REPO_OK=0
# ⓪의 `SHELL_OK`를 **여기서 소비한다.** 경고로 두면 zsh 세션이 그대로 TOKEN·작업 디렉터리를 만들고
# `REPO_OK` → `DRILL_MUTATION_OK`(§2.5 ⓪) → §3~§8·§11의 변경 게이트를 전부 통과한다. `VALUES_OK` 계보
# (§3.2·§3.3·§5·§11)든 자격을 직접 보는 경로(§6·§7·§8)든 `SHELL_OK`이 밑에 깔려 있어 여기가 유일한 관문이다.
# 멈춰야 하는 지점은 토큰 생성 **앞**이다.
if [ "${SHELL_OK:-0}" = 1 ] && REPO_ROOT=$(git -C "$REPO_DIR" rev-parse --show-toplevel 2>&1); then
  REPO_DIR="$REPO_ROOT"
  REPO_OK=1
  echo "REPO_DIR=$REPO_DIR"
elif [ "${SHELL_OK:-0}" != 1 ]; then
  echo "STOP: SHELL_OK=${SHELL_OK:-미설정} — TOKEN·작업 디렉터리도 만들지 않는다(⓪부터 bash로 다시 시작)"
else
  echo "STOP: REPO_DIR이 git 레포가 아니다 — clone 경로가 다르면 위 값을 고친다: $REPO_ROOT"
fi
SRC_DB=linkpulse-prod-pg
DRILL_DB=linkpulse-restore-drill
DRILL_ROLE=linkpulse-restore-drill-exec
CLUSTER=linkpulse-prod-cluster
LOG_GROUP=/ecs/linkpulse-prod-app
TD_A=linkpulse-restore-drill-a
TD_B=linkpulse-restore-drill-b
TOKEN=""
DRILL_DIR=""
RESTORE_CALL_STATE=not-started
RESTORE_T0=""
RESTORE_REQUEST_KIND=""
RESTORE_REQUEST_SOURCE=""
RESTORE_EXPECTED_ARN=""
RESTORE_INSTANCE_CREATED=""
RESTORE_RETRY_STATE=not-used   # not-used | authorized | dispatched — 소진은 재호출 직전에 기록한다(§5)
RESTORE_RETRY_ALLOWED=0
RESTORE_PRECHECK_STATE=not-ready   # not-ready | ready — 최초 호출의 전체 사전조건을 원장에 고정한다
RESTORE_PRECHECK_KIND=""
RESTORE_PRECHECK_SOURCE=""
RESTORE_PRECHECK_BASELINE_TASK_ARN=""
RESTORE_PRECHECK_B_CODE=""         # 최초 호출 직전 채택돼 있던 sentinel-B code(정확 일치로만 대조)
RESTORE_PRECHECK_B_CREATED_AT=""   # 그 B의 created_at(같은 값 = 같은 B라는 증거)
SENTINEL_A_CREATE_STATE=not-used   # not-used | dispatched — §3.1 영구 행 쓰기(이번 예약분 `A_POST_COUNT`건)의 일회성 소비 상태
A_POST_COUNT=1                     # 이번에 보낼 sentinel-A 건수. §3.1은 이 값을 **보존**한다(리셋 금지).
                                   # 1로 시작하는 이유: 선차감이라 3으로 예약했다가 첫 요청이 429·전송 실패면
                                   # 남은 한도 3건이 한 번에 소진된다. (b) 판정은 **1건이면 성립**한다
A_CONSUMED_COUNT=0                 # 누적 소비 건수(상한 3) — §3.1이 남은 한도를 여기서 도출한다.
                                   # **예약 시 증가하고, 같은 예약 안에서 `curl` 미호출이 코드로 증명된
                                   # 잔여분만 §3.1 `break`가 감소시킨다**(그 외 감소 없음 — 사람이 줄이지 않는다)
SENTINEL_B_CREATE_STATE=not-used   # not-used | dispatched | accepted | ambiguous | retry-authorized
SENTINEL_B_TRY=1                   # 첫 시도 1, 확인된 마진 미달 뒤에만 2
SESSION_MODE=new               # new | reentry — 최초 결과 판독 source 라벨에만 쓴다
TDA_TRY=1                      # client token 시도 번호(상한 2). §3.3 블록은 이 값을 리셋하지 않는다
TDB_TRY=1                      # 같은 규율(상한 3) — §8.3. 다음 논리 시도로 넘어갈 때만 올린다
TDA_TASK_ARN=""                # §3.3 응답의 taskArn — §3.4가 읽기 전용으로 재도출할 때 쓴다
TDB_TASK_ARN=""                # §8.3 응답의 taskArn — §8.3 결과 게이트가 쓴다
TDA_RESULT_READ_STATE=not-read # not-read | read | unknown — 현재 TDA_TASK_ARN의 baseline tuple 판독 상태
TDB_RESULT_READ_STATE=not-read # not-read | read | unknown — 현재 TDB_TASK_ARN의 유효 결과 판독 상태
A_EXPECTED_COUNT=""            # 응답으로 code를 확인해 원장에 적은 sentinel-A 개수(1~3)
BASELINE_CAPTURED_AT=""        # §3.4 성공 확인 시각(셸) — 복원 후 baseline 증거 재구성용
BASELINE_ROW_COUNT=""
BASELINE_CLICKS_SUM=""
BASELINE_MAX_CREATED_AT=""
BASELINE_A_HITS=""
BASELINE_SOURCE_TASK_ARN=""
T_VERIFIED=""                  # 최초 유효 검증 결과 확보 시각(셸) — 한 번 기록한 뒤 보존
T_VERIFIED_SOURCE=""           # shell-first-read | shell-first-read-reentry  (코드가 만드는 값은 이 둘뿐 — 손으로 적지 않는다)
T_VERIFIED_TASK_ARN=""         # 최초 유효 결과를 읽은 정확한 TD-B taskArn
T_VERIFIED_TOKEN=""            # 그 시각을 만든 드릴의 TOKEN(계보 키 — 다른 드릴 tuple 배제)
SNAPSHOT_ID=""
A_CREATED_MAX=""
SECRET_LIFECYCLE=not-created
SECRET_OWNER_DB_ARN=""
SECRET_PROVENANCE_OK=0
TOOLS_OK=0                     # §2.5 ⓪-1 로컬 도구(python3) **실행** 실측 뒤에만 1
EXCLUSIVE_DRILL_LOCK_OK=0      # §2.5 ⓪-2 사람이 관리하는 단일 변경 작업 락 — 보유 + **잔여 TTL** 확인 뒤에만 1
SESSION_AWS_OK=0               # §2.5 ⓪-3 현재 AWS 주체가 원장과 같고 권한 4그룹이 이 세션에서 확인됐을 때만 1
MUTATION_CONTEXT_OK=0          # §2.5 ⓪-4 쓰려는 dispatch 원장이 **이번 드릴의 것**임을 확인했을 때만 1
DRILL_MUTATION_OK=0            # §2.5 ⓪-5가 세션마다 재도출하는 **비정리 변경 자격**. 손으로 1을 치지 않는다
# §10의 상태 변수는 **미설정이 (d) PASS로 새지 않도록** 여기서 fail-closed로 시작한다.
# 각 §10.x 블록이 실행되면 덮어쓴다. 실행되지 않았다면 아래 값 그대로 §10.9 (A)에 걸린다.
TD_DISCOVERY_STATE=unknown
TASK_DISCOVERY_STATE=unknown
ROLE_STATE=unknown
SECRET_STATE=unknown
TASK_ARNS_LEDGER=""            # §2.6 시도별 표의 taskArn 전건(공백 구분) — §10.3 합집합의 원장 입력
# 위험 호출의 미대사 상태는 셸 변수가 아니라 `$DRILL_DIR/dispatch/` 파일 원장이 든다(§2.6·§3.3·§10.3).
if [ "$REPO_OK" = 1 ]; then
  # 드릴 토큰 = 초 단위 시각 + **난수 6자리**. 아래 3용도 공용.
  # 난수가 없으면 같은 UTC 초에 시작한 두 실행이 **완전히 같은 토큰**을 갖게 되고,
  # 그 순간 정리(§10.4·§10.6)의 drill-token 태그 대조가 "남의 실행"을 "내 것"으로 통과시킨다.
  TOKEN_RAND=$(od -An -N3 -tx1 /dev/urandom 2>/dev/null | tr -d ' \n')
  case "$TOKEN_RAND" in
    [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f])
      TOKEN="restore-drill-$(date -u +%Y%m%d-%H%M%S)-$TOKEN_RAND"
      echo "$TOKEN" ;;                              # ← 원장에 즉시 적는다
    *) echo "STOP: 토큰 난수를 얻지 못했다 — 토큰 충돌을 배제할 수 없으므로 시작하지 않는다"; REPO_OK=0 ;;
  esac
fi
if [ "$REPO_OK" = 1 ]; then
  # 작업 디렉터리 — 드릴이 만드는 파일(TD JSON·정책 JSON·ARN 목록)은 **전부 여기 안에만** 만든다.
  # 기본값은 레포 **밖**이라 git이 애초에 보지 않는다(§2.2 git 규칙). step 5 증거 보관과도 한 곳이 된다.
  # 레포 안(<레포>/docs/postmortems/evidence/…)에 두는 대안을 고르면 그 경로가 .gitignore 대상인지 먼저 확인한다.
  DRILL_DIR="$HOME/linkpulse-evidence/rds-restore-$TOKEN"   # 레포 안에 두려면 <레포>/docs/postmortems/evidence/rds-restore-<날짜>
  # **`dispatch/manifest`를 여기서 봉인한다.** §10은 `$DRILL_DIR`만 보고 원장을 읽는데, 경로가 존재하기만
  # 하면 **다른 드릴의 디렉터리나 빈 `$HOME`도 앵커로 통과**한다. manifest에 이 드릴의 `TOKEN`을
  # 적어 두면 §10.3이 "이 원장이 이 드릴 것인가"를 확인할 수 있다. 네 원장 경로도 **미리 만들어**
  # "아직 호출 안 함"(빈 디렉터리·빈 파일)과 "원장을 잃음"(경로 자체가 없음)을 구분한다 —
  # §10.3이 네 경로의 형태를 전부 확인하므로 하나라도 사라지면 부분 손실로 잡힌다.
  if mkdir -p "$DRILL_DIR/dispatch/task-pending" "$DRILL_DIR/dispatch/td-pending" \
     && : > "$DRILL_DIR/dispatch/task-arns" && : > "$DRILL_DIR/dispatch/td-arns" \
     && printf 'token=%s\n' "$TOKEN" > "$DRILL_DIR/dispatch/manifest" \
     && [ -s "$DRILL_DIR/dispatch/manifest" ] && cd "$DRILL_DIR"; then pwd; cat dispatch/manifest
  # manifest에 `ACCOUNT_ID`는 적지 않는다 — 이 블록 시점에는 아직 §2.5 ⑧을 거치지 않아 빈 값이고,
  # 빈 값을 적으면 "확인했는데 비었다"와 "아직 모른다"가 섞인다. 토큰만으로 충분하다(난수 6자리 포함).
  else echo "STOP: 증거 디렉터리·위험 호출 원장 준비 실패 — 드릴을 시작하지 않는다"; REPO_OK=0; fi
else
  echo "STOP: REPO_OK=0 — 환경·작업 디렉터리를 준비하지 않는다"
fi
```

> **재진입할 때 위 초기화 블록을 다시 실행하지 않는다.** 위 블록은 새 `TOKEN`과 디렉터리를 만드는 **신규 드릴 전용**이다. 새 터미널에서는 아래 전용 블록의 빈 문자열에 원장 값을 붙여 넣고, 이후 각 절의 **읽기 전용 reconciliation 블록**으로 플래그를 다시 도출한다.

```bash
# ⓪ 셸 확인 — **재진입 세션이 가장 위험하다**(새 터미널 = macOS 기본 zsh). 신규 블록과 같은 3줄을
#    여기서 다시 도출한다. 플래그를 블록 간에 물려받지 않는 것이 이 문서의 관용구다.
SHELL_OK=0
if [ -n "${BASH_VERSION:-}" ]; then
  SHELL_OK=1
  echo "SHELL_OK=1 bash $BASH_VERSION"
else
  echo "STOP: Bash가 아니다(zsh 등) — 'bash' 를 실행해 새 셸로 들어간 뒤 이 블록부터 다시 시작한다"
  echo "      이 세션에서는 상태를 바꾸는 명령이 전부 게이트에 막힌다(REPO_OK=0 + 각 변경 게이트가 SHELL_OK를 직접 본다)"
  echo "      읽기 전용 reconciliation은 계속 돌려도 되지만, 그 결과로 §6~§8이 열리지는 않는다"
fi

export AWS_REGION=ap-northeast-2 AWS_PAGER=""
REPO_DIR="$HOME/mini-project/linkpulse"
SRC_DB=linkpulse-prod-pg
DRILL_DB=linkpulse-restore-drill
DRILL_ROLE=linkpulse-restore-drill-exec
CLUSTER=linkpulse-prod-cluster
LOG_GROUP=/ecs/linkpulse-prod-app
TD_A=linkpulse-restore-drill-a
TD_B=linkpulse-restore-drill-b

# 아래 값은 모두 원장의 **기존 값**으로 치환한다. 새 값 생성 금지.
TOKEN=""
DRILL_DIR=""
PROD_SECRET=""
DRILL_SECRET=""
DIGEST=""
A_CODES=""
ACCOUNT_ID=""
PROD_ADDR=""
PROD_USER=""
PROD_DB=""
SUBNET_GROUP=""
DATA_SG=""
PROD_PG=""
EXPECTED_ENGINE=""
EXPECTED_ENGINE_VERSION=""
EXPECTED_CLASS=""
APP_SUBNETS=""
APP_SG=""
T_BASELINE=""
LRT=""
RESTORE_POINT=""
B_CODE=""
B_CREATED_AT=""
RESTORE_CALL_STATE=""       # not-started | accepted | ambiguous | rejected
RESTORE_T0=""
RESTORE_REQUEST_KIND=""     # pitr | snapshot
RESTORE_REQUEST_SOURCE=""   # PITR restore point 또는 snapshot ID
RESTORE_EXPECTED_ARN=""
RESTORE_INSTANCE_CREATED=""
RESTORE_RETRY_STATE=""      # not-used | authorized | dispatched — **원장 값 그대로** 붙여 넣는다(§5)
                            #   authorized = 창만 열리고 재호출 전에 세션이 끊긴 상태 → 다시 열 수 있다
                            #   dispatched = 재호출을 실제로 보냈다(전송 여부 불명 포함) → 다시 열지 않는다
RESTORE_RETRY_ALLOWED=0     # 재시도 창은 §5 reconciliation만 연다. 손으로 1을 치지 않는다
RESTORE_PRECHECK_STATE=""   # not-ready | ready — 최초 호출 직전 원장 값
RESTORE_PRECHECK_KIND=""
RESTORE_PRECHECK_SOURCE=""
RESTORE_PRECHECK_BASELINE_TASK_ARN=""
RESTORE_PRECHECK_B_CODE=""         # 최초 호출 직전 채택돼 있던 sentinel-B code(정확 일치로만 대조)
RESTORE_PRECHECK_B_CREATED_AT=""   # 그 B의 created_at(같은 값 = 같은 B라는 증거)
SENTINEL_A_CREATE_STATE=""  # not-used | dispatched — **원장 값 그대로**. 빈 값은 §3.1이 STOP으로 막는다
A_POST_COUNT=""             # 이번에 보낼 건수(보충일 때만). 빈 값은 §3.1이 STOP으로 막는다 — 3으로 되돌리지 않는다
A_CONSUMED_COUNT=""         # **원장의 누적 소비 건수**(0~3). §3.1이 `3 − 이 값`을 남은 한도로 쓴다.
                            #   원장에는 §3.1 (1-b)의 **마지막 출력값**을 적는다 — 중단 없이 끝났으면 선차감 값,
                            #   락 만료로 중단했으면 **환급 후 값**이다. 큰 쪽을 택하지 않는다
SENTINEL_B_CREATE_STATE=""  # not-used | dispatched | accepted | ambiguous | retry-authorized — 원장 값
SENTINEL_B_TRY=""           # 1 | 2 — 원장 값. 빈 값을 1로 보정하지 않는다
SESSION_MODE=reentry        # 최초 결과 판독이면 source를 shell-first-read-reentry로 남기는 데만 쓴다
CLOCK_SKEW_SECONDS=""       # §2.5 ⑥ 실측(셸 − AWS, 초, 부호 유지) — §5 흡수폭 산출에 쓴다
SNAPSHOT_ID=""              # §11 스냅샷 모드에서만. **원장 값을 그대로** 붙여 넣는다(§5 공통 reconciliation이 대조한다)
A_CREATED_MAX=""            # sentinel-A 중 가장 늦은 created_at(DB 시계) — §11 경계 판정용
TD_A_ARN=""
TD_B_ARN=""
TDA_TRY=""                  # 원장의 시도 번호. 응답 불명은 **같은 번호**, 확인된 실패 뒤 새 논리 시도만 +1
TDB_TRY=""                  # 빈 값이면 §3.3·§8.3이 STOP한다 — 조용히 1로 되돌리지 않는다
TDA_TASK_ARN=""             # §3.3 응답 taskArn — 이 값이 있으면 §3.4를 다시 돌려 baseline을 재도출한다
TDB_TASK_ARN=""             # §8.3 응답 taskArn
TDA_RESULT_READ_STATE=""    # not-read | read | unknown — **현재 TDA_TASK_ARN과 같은 원장 행**의 값
A_EXPECTED_COUNT=""         # 응답으로 code를 확인해 원장에 적은 sentinel-A 개수(1~3)
BASELINE_CAPTURED_AT=""     # §3.4 성공 확인 시각(셸, 원장 값)
BASELINE_ROW_COUNT=""
BASELINE_CLICKS_SUM=""
BASELINE_MAX_CREATED_AT=""
BASELINE_A_HITS=""
BASELINE_SOURCE_TASK_ARN=""
TDB_RESULT_READ_STATE=""    # not-read | read | unknown — **현재 TDB_TASK_ARN과 같은 원장 행**의 값
T_VERIFIED=""               # 최초 유효 검증 결과 확보 시각(셸, 원장 값)
T_VERIFIED_SOURCE=""        # shell-first-read | shell-first-read-reentry  (코드가 만드는 값은 이 둘뿐 — 손으로 적지 않는다)
T_VERIFIED_TASK_ARN=""      # 위 시각을 만든 정확한 TD-B taskArn
T_VERIFIED_TOKEN=""         # 그 시각을 만든 드릴의 TOKEN(계보 키)
SECRET_LIFECYCLE=""         # not-created | create-requested | arn-known
SECRET_OWNER_DB_ARN=""      # 시크릿 소유권을 확인한 시점의 **드릴 DB ARN**(§7 — DB 삭제 후 §10.8이 쓴다)
SECRET_PROVENANCE_OK=""     # 원장 값 `0`|`1`. §0과 같은 이유로 실행 가능한 기본값(0)을 두지 않는다
                            #   — 확인된 소유권 1을 실수로 덮으면 §10.8이 드릴 시크릿을 정리하지 못한다
TOOLS_OK=0                   # 재진입 세션의 로컬 도구도 이 세션에서 다시 실측한다(§2.5 ⓪-1)
EXCLUSIVE_DRILL_LOCK_OK=0    # 재진입 때도 락 보유와 **잔여 TTL**을 다시 확인하기 전에는 생성 경로를 열지 않는다(⓪-2)
SESSION_AWS_OK=0             # 재진입의 AWS 주체·권한은 원장에서 붙여 넣을 수 없다 — 이 세션에서 실측한다(⓪-3)
MUTATION_CONTEXT_OK=0        # 붙여 넣은 DRILL_DIR가 **이번 드릴의 원장**인지도 이 세션에서 확인한다(⓪-4)
DRILL_MUTATION_OK=0          # **§2.5 ⓪ 블록을 이 세션에서 다시 실행해야** 1이 된다 — 원장에서 붙여 넣지 않는다.
                             #   그 전에는 §3~§8·§11의 비정리 변경이 전부 닫히고, §10 정리만 SHELL_OK로 진행한다
TASK_ARNS_LEDGER=""         # §2.6 시도별 표(tda-1~2·tdb-1~3)의 taskArn **전건**을 공백으로 이어 붙인다.
                            #   채택한 ARN 2개(TDA/TDB)만으로는 실패한 시도의 태스크가 정리에서 빠진다(§10.3)
                            # 미대사 시도는 여기에 적지 않는다 — `$DRILL_DIR/dispatch/` 파일 원장이 든다(§2.6)
# 아래 4개는 **붙여 넣지 않는다.** 각 §10.x 블록이 이번 세션에서 다시 도출한다.
# 다만 미설정으로 두면 §10.9 (A)의 판정이 "값이 없음"을 잔존 없음으로 읽으므로 unknown에서 시작한다.
TD_DISCOVERY_STATE=unknown  # §10.3이 ok로 올린다. unknown이면 (A)⑥
TASK_DISCOVERY_STATE=unknown # §10.3 태스크 재탐색이 ok로 올린다. unknown이면 (A)⑥(§10.5도 닫힌다)
ROLE_STATE=unknown          # §10.7이 absent/present로 올린다. absent가 아니면 (A)②
SECRET_STATE=unknown        # §10.8이 absent/scheduled/present로 올린다. absent·scheduled가 아니면 (A)③

REPO_OK=0
if [ "${SHELL_OK:-0}" = 1 ] \
   && REPO_ROOT=$(git -C "$REPO_DIR" rev-parse --show-toplevel 2>&1) \
   && [ -n "$TOKEN" ] && [ -n "$DRILL_DIR" ] && [ -d "$DRILL_DIR" ]; then
  REPO_DIR="$REPO_ROOT"
  REPO_OK=1
  cd "$DRILL_DIR" || REPO_OK=0
else
  echo "STOP: 셸이 Bash가 아니거나(SHELL_OK=${SHELL_OK:-미설정}) 원장의 TOKEN/DRILL_DIR·레포 경로를 재구성하지 못했다"
fi
echo "REPO_OK=$REPO_OK SHELL_OK=${SHELL_OK:-미설정} TOKEN=$TOKEN DRILL_DIR=$DRILL_DIR"
```

**플래그(`SHELL_OK`·`TOOLS_OK`·`SESSION_AWS_OK`·`MUTATION_CONTEXT_OK`·`DRILL_MUTATION_OK`·`VALUES_OK`·`TARGET_ABSENT_OK`·`RESTORE_POINT_OK`·`SENTINEL_A_SEND_ALLOWED`·`SENTINEL_B_CREATE_ALLOWED`·`SENTINEL_B_CREATE_MODE`·`SENTINEL_B_SEND_ALLOWED`·`B_MARGIN_OK`·`SENTINEL_BOUNDARY_OK`·`BASELINE_COMPARABLE_OK`·`BASELINE_RESULT_OK`·`BASELINE_LEDGER_OK`·`BASELINE_EVIDENCE_OK`·`VERIFY_RESULT_OK`·`FORCE_SSL_OBTAINED`·`T_VERIFIED_OK`·`RESTORE_OK`·`AVAILABLE_OK`·`SAFE_OK`·`TARGET_OK`·`CREDS_OK`·`ROLE_READY`·`TD_A_READY`·`TD_B_READY`)는 원장에서 붙여 넣지 않는다.** `SENTINEL_B_CREATE_MODE`도 모드별 생산 블록이 매번 재도출하는 파생값이지 원장 입력이 아니다. **`SHELL_OK`은 §2.1 ⓪(신규·재진입)과 §10.3(정리)이 세션마다 다시 도출한다** — 손으로 `SHELL_OK=1`을 치면 그 순간 게이트가 확인이 아니라 선언이 된다(§7의 `TARGET_OK`와 같은 규율). **`TOOLS_OK`·`SESSION_AWS_OK`·`MUTATION_CONTEXT_OK`·`DRILL_MUTATION_OK`도 같은 규율이며 §2.5 ⓪이 신규·재진입 세션마다 다시 도출한다** — 이 자격 없이는 §3~§8·§11의 비정리 변경이 하나도 열리지 않으므로(머리말), 재진입은 §2.5 ⓪을 먼저 실행한다. **`EXCLUSIVE_DRILL_LOCK_OK`·`LOCK_EXPIRES_EPOCH`도 같다** — 원장에는 락의 **만료 시각**(`LOCK_EXPIRES_AT`)을 적고 플래그와 epoch는 매번 그 시각으로 다시 도출한다(만료된 락에서 1을 물려받으면 그 순간 게이트가 사라진다). `LOCK_EXPIRES_EPOCH`는 손으로 적지 않는다 — ⓪-2가 계산하며, 미설정은 0으로 읽혀 **모든 변경 게이트가 만료로 판정**한다. 반대로 **§10 정리는 이 자격을 요구하지 않는다.** **`BASELINE_RESULT_OK`의 재도출 경로는 §3.4다** — 원장의 `TDA_TASK_ARN`과 같은 행의 `TDA_RESULT_READ_STATE`를 붙여 넣고 그 읽기 전용 블록을 다시 돌린다. `not-read`는 baseline tuple 전 필드가 비어 있을 때만 최초 판독 시각을 만들고, `read`는 보존된 tuple과 로그가 전부 일치할 때만 플래그를 재도출한다. 태스크 보관이 만료된 복원 후 재진입은 §5 reconciliation 뒤 §4.3의 원장 tuple 게이트로 `BASELINE_LEDGER_OK`·`BASELINE_EVIDENCE_OK`를 다시 세운다. `BASELINE_EVIDENCE_OK`는 이미 만들어진 복원본 검증용이고 최초 유료 호출 자격이 아니다. `ambiguous + authorized` 재시도만 §5에 최초 호출 직전 고정한 `RESTORE_PRECHECK_*` tuple과 `BASELINE_LEDGER_OK`를 함께 소비한다. **`T_VERIFIED`·`T_VERIFIED_SOURCE`·`T_VERIFIED_TASK_ARN`·`T_VERIFIED_TOKEN`은 드릴 전체에서 한 번만 만들어지는 **최초 측정 tuple**이고, `TDB_RESULT_READ_STATE`는 **현재 `TDB_TASK_ARN` 행마다** 따로 움직이는 값이다 — 둘은 수명주기가 다르니 원장에서도 별도 행으로 두고 함께 붙여 넣는다. 보존 경로는 `T_VERIFIED_TOKEN`이 현재 `TOKEN`과 같고 taskArn이 이번 계정·리전 형식이며 `RESTORE_T0 ≤ T_verified`일 때만 열린다(§8.3). 상태가 `read`인데 시각이 없거나 상태가 `unknown`이면 현재 시각으로 보충하지 않는다. **`SECRET_PROVENANCE_OK`·`SECRET_OWNER_DB_ARN`은 예외로 붙여 넣는다** — 드릴 DB가 삭제된 뒤에는 재도출할 대상 자체가 없으므로, 소유권이 **확인됐던 시점**(§7)의 기록이 §10.8이 쓸 수 있는 유일한 근거다. 값이 아니라 “방금 검증했다”는 사실이므로 해당 읽기 전용 블록으로 재도출한다. 특히 `RESTORE_CALL_STATE`·`RESTORE_T0`·`RESTORE_REQUEST_SOURCE`는 최초 요청 원장을 그대로 보존하며, `AlreadyExists` 뒤 새 `T0`를 만들거나 생성 명령을 재실행하지 않는다. **단 하나의 예외는 §5가 `ambiguous` + 3회 연속 `DBInstanceNotFound`에서 여는 1회 재호출 창**이다 — 그때도 `RESTORE_CALL_STATE`·`RESTORE_T0`·source는 **그대로 보존**하고 호출 자격만 연다(최초 요청의 미접수는 증명할 수 없다). 재시도 시각은 원장에 병기만 한다. **`RESTORE_RETRY_STATE`도 원장 값을 그대로 붙여 넣는다** — 이 값이 "창을 열었을 뿐"(`authorized`)과 "재호출을 보냈다"(`dispatched`)를 가르므로, 재도출 대상 플래그(`RESTORE_RETRY_ALLOWED`)와 달리 세션을 넘어 보존돼야 한다.

**드릴 토큰 하나에서 세 가지가 파생된다**: ① `run-task --started-by`(회수 키) ② 로그 스트림 접두사 ③ 시도별 client token 접두(`<TOKEN>-tda-1` 등). **초 단위**인 이유는 조기 실패 후 같은 분 안에 재드릴하면 시도별 client token까지 재사용돼(멱등 TTL **24시간** — §2.3 ⓖ) **새 태스크가 뜨지 않고 이전 결과가 돌아오기** 때문이다. **뒤의 난수 6자리는 그것만으로 부족하기 때문이다** — 초 단위는 "같은 초에 시작한 다른 실행"을 구분하지 못하는데, 이 토큰은 §10.4·§10.6·§10.8에서 **"이 리소스가 내 것인가"의 판정 근거**로 쓰인다. 토큰이 겹치면 정리가 **다른 실행의 과금 중인 인스턴스와 TD를 자기 것으로 오인해 삭제**할 수 있다. 길이는 `restore-drill-`(14) + `YYYYMMDD-HHMMSS`(15) + `-`(1) + 난수(6) = 36자, 시도 접미(`-tdb-3`)까지 42자로 **client token 64자·`--started-by` 128자 제약 안**이다.

### 2.2 운영 가드레일 — 허용은 이 절이 전부

**금지**(하나라도 필요하면 드릴을 멈추고 재설계):

- 운영 인스턴스 `linkpulse-prod-pg`에 대한 `modify`/`restore`/`delete`/`reboot` 등 **설정 변경 계열 API 전면 금지**
- 기존 행의 `UPDATE`/`DELETE` 금지, 스키마 변경 금지
- 운영 ECS 서비스·운영 TD·ALB·IaC(`.tf`) 변경 금지, 기존 실행 role의 정책 변경 금지
- **드릴 종료 전 `$DRILL_DIR`를 지우지 않는다** — 위험 호출(run-task·register)의 **미대사 원장이 여기에만** 있다(`dispatch/`). 기본 경로가 `$HOME/linkpulse-evidence/...`라 홈 정리에 걸리기 쉬운데, 지우면 응답을 잃은 태스크를 되찾을 근거가 사라지고 §10.3이 그 사실조차 볼 수 없다(같은 디렉터리의 `scan-*`·`task-stop/`·`td-delete/`도 함께 보호된다).
- **드릴은 레포를 바꾸지 않는다** — 드릴 전후 `git -C "$REPO_DIR" status --short`(항상 레포 경로에서)가 같아야 한다. 그래서 드릴이 만드는 모든 파일(TD JSON·정책 JSON·ARN 목록)은 **§2.1의 `$DRILL_DIR` 안에만** 만든다(레포 안에 둘 때는 gitignore된 `docs/postmortems/evidence/` 아래). 시작 시 그 출력을 원장에 기준점으로 붙여 둔다.

**허용되는 운영 데이터 쓰기**: `SELECT` 읽기 + **공개 `POST /api/links`로 만드는 sentinel 링크 최대 5건**(A 최대 3 + B 최대 2). URL은 무해한 고정값 `https://example.com/`. **삭제 API가 없어 이 행들은 영구 잔존**한다(정리 대상이 아니라 증거). code는 **클라이언트가 지정할 수 없다**(서버가 무작위 생성) → 식별 기준은 **응답 code + A/B 구분 + 응답 `created_at`**. 생성이 `429`면 앱 장애가 아니라 레이트리밋(`POST /api/links` = per-IP 20/min·burst 10)이므로 잠시 후 재시도한다.

**동시 실행 금지 — 사람의 단일 변경 작업 락이 필수다.** 난수 `drill-token`은 이미 만들어진 리소스의 소유권을 가를 뿐 상호 배제 락이 아니다. 두 세션은 고정 식별자 부재 조회를 동시에 통과하고 서로 다른 태그의 TD revision을 모두 등록할 수 있으므로, RDS의 `AlreadyExists`가 한쪽을 막기 전에 양쪽이 삭제할 수 없는 sentinel 행을 쓸 수 있다. 승인자는 조직의 변경 관리 기록에서 이 드릴의 **단일 활성 작업**을 한 운영자·한 `$TOKEN`에 배정하고, 시작 시각·만료 시각을 아래 원장에 남긴다. **락은 보유 여부와 잔여 유효기간이 함께 게이트다** — §2.5 ⓪-2가 원장의 `LOCK_EXPIRES_AT`을 현재 시각과 비교해 다음 변경을 마칠 TTL(45분)이 남았을 때만 `EXCLUSIVE_DRILL_LOCK_OK=1`을 세운다. 만료가 가까우면 **갱신 전에는 변경 자격이 열리지 않는다**: 이 드릴은 30분급 대기가 두 번 이상 이어지므로, 한 번 세운 비트를 세션 내내 물려받으면 락이 만료된 뒤 다른 운영자가 새 락을 얻어도 이쪽이 계속 변경을 통과한다. **그리고 자격이 선 뒤에도 각 변경 호출 직전에 미만료를 다시 확인한다** — ⓪ 실행과 실제 호출 사이에 흐른 시간(대기·자리 비움·수동 진단)은 자격 비트가 알 수 없기 때문이다. §10 정리 게이트 완료 또는 중단 인계 전에는 락을 해제하지 않으며, 같은 고정 식별자를 쓰는 다른 세션을 시작하지 않는다. 고정 리소스 부재 재조회와 난수 태그는 방어층이지만 이 사람 락을 대체하지 않는다.

**이 락은 산문이 아니라 게이트다.** §2.5 ⓪이 셸·레포·로컬 도구·**잔여 TTL이 남은 락**·**원장과 같은 AWS 주체와 확인된 권한**·**이번 드릴 소유의 dispatch 원장**에서 **`DRILL_MUTATION_OK`** 를 도출하고, §3~§8·§11의 **비정리 변경 13개가 각자 그 값을 직접 소비**한다(머리말의 소비 표). 재진입도 그 블록을 다시 실행하기 전에는 어떤 변경도 열리지 않는다 — 읽기 전용 reconciliation으로 되살아나는 플래그만으로는 sentinel-B POST·reboot·자격증명 전환·IAM 생성·TD-B 실행이 열리지 않는다. 반대로 **§10 정리 8개는 락 없이 `SHELL_OK`만 요구한다**: 락을 잡지 못한 세션이 과금을 끊지 못하면 유료 인스턴스가 남기 때문이다.

**허용되는 운영 계정 부수효과** — **이 표에 없는 운영 계정 리소스 생성·변경은 금지**:

| 부수효과 | 대상 | 정리 게이트 종결 상태 |
| -------- | ---- | --------------------- |
| 임시 태스크정의 TD-A·TD-B | 새 family 2개(`linkpulse-restore-drill-a/-b`, 운영 TD 무관) | `deregister` → `delete-task-definitions` → **§10.6 즉시 게이트 통과 = 종결** |
| 임시 IAM role + 인라인 정책 | `linkpulse-restore-drill-exec` | 인라인 삭제 → 관리형 detach → `delete-role` → **부재 확인** |
| 1회성 태스크 실행 **최대 5회**(TD-A ≤2 · TD-B ≤3, 최초 호출 포함한 총 시도) | **운영 클러스터 `linkpulse-prod-cluster`** | **전 시도가 §10.5 (가) 종결로 확정** |
| 드릴 로그 스트림 | 운영 로그그룹 `/ecs/linkpulse-prod-app`, 접두사 `<TOKEN>` | **잔존(정리하지 않음)** — 증거로 남긴다 |
| sentinel 링크 행 최대 5건 | 운영 DB `links` | **잔존(삭제 API 없음)** — code·`created_at` 기록 |

**알람 간섭 없음**: ECS 알람 3개는 차원이 `ClusterName`+`ServiceName`이라 서비스에 속하지 않는 드릴 태스크를 세지 않고, RDS 알람 4개는 차원이 `DBInstanceIdentifier=linkpulse-prod-pg`라 드릴 인스턴스를 보지 않는다. `/ecs/linkpulse-prod-app`에는 메트릭 필터가 없어 드릴 로그가 섞여도 오탐이 없다. **역방향 규칙: 드릴 도중 실제 알람이 울리면 드릴을 중단하고 §10 정리 게이트로 진입한 뒤 알람 대응을 우선한다(운영 사고 > 드릴).**

### 2.3 1차 출처 확인 체크리스트 — **전부 처리하기 전에는 드릴을 시작하지 않는다**

| # | 항목 | 상태 | 근거 |
| - | ---- | ---- | ---- |
| ⓐ | ECS 태스크 **`DELETED` 상태**의 존재·`describe-tasks` 노출 | ✅ **확정** | [Amazon ECS task lifecycle](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/task-lifecycle-explanation.html) — _"DELETED: This is a transition state when a task stops. This state is not displayed in the console, but is displayed in `describe-tasks`."_ |
| ⓑ | **이미 종결된 태스크에 `stop-task` 호출 시 동작**(오류 vs no-op) | ⚠️ **미확인(우선순위 낮음)** | `stop-task help` 첫 줄이 _"Stops a **running** task."_ 라 대상이 running임은 분명. 절차가 (가)/(나)/(다) 분기로 중복 호출을 피하므로 실제 노출 경로는 *관측과 호출 사이의 좁은 레이스*뿐이고, "거부되면 30초 뒤 재조회(총 3회)"가 흡수한다 |
| ⓒ | `wait tasks-stopped` 동작 | ✅ **확정(재확인 완료)** | 로컬 `aws ecs wait tasks-stopped help` — _"Wait until JMESPath query **`tasks[].lastStatus`** returns STOPPED for **all elements** … poll **every 6 seconds** … exit with a return code of **255 after 100 failed checks**."_ ⇒ 상한 600초, 매처가 `tasks[]`만 보므로 **`failures[]`의 MISSING은 영원히 성공 조건을 못 만족**, "all elements"라 **ARN 하나씩** 걸어야 한다 |
| ⓓ | **STOPPED 태스크 보관 창** | 🔶 **부분 확정** | [Viewing stopped task errors](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/stopped-task-errors.html) — _"Stopped tasks only appear in the console for 1 hour."_ **콘솔** 기준만 문서화돼 있고 **API 조회 보관 창은 명시가 없다.** 인접 근거로 [Ensuring idempotency](https://docs.aws.amazon.com/AmazonECS/latest/APIReference/ECS_Idempotency.html) — _"Tasks that have been stopped for under an hour only include the task ARN, last status, and desired status."_ ⇒ **재탐색은 드릴 종료 직후 수행**하고, 보관 만료 판정은 §10.5 MISSING 규율(종결 관측 이력)로만 한다 |
| ⓔ | RDS 관리 시크릿이 인스턴스 삭제 시 **즉시 삭제인지 복구 창 예약인지** + `OwningService=rds` 시크릿을 사용자가 직접 지울 수 있는지 | 🔶 **부분 확정** | [Password management with RDS and Secrets Manager](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/rds-secrets-manager.html) — _"If you delete a DB instance that manages a secret in Secrets Manager, the secret and its associated metadata are also deleted."_ **"삭제된다"까지만 확정이고 즉시/예약 구분은 문서에 없다.** 판별 필드는 확정 — `describe-secret help`의 `DeletedDate`: _"The date the secret is scheduled for deletion. If it is not scheduled for deletion, this field is omitted … recovery window of at least 7 days"_ ⇒ **§10.8의 상태 머신(부재 / `DeletedDate` 있음=(B) / 예약도 아님 / 조회 실패=unknown)을 그대로 적용**하면 어느 쪽이든 보수적으로 판정한다. `OwningService` 직접 삭제 가능 여부는 미확인 → 거부 시 재시도 금지·에스컬레이션 |
| ⓕ | `--restore-time` 경계가 `<`인지 `≤`인지 | 🔶 **문구 확정, 경계 미실측** | 로컬 `restore-db-instance-to-point-in-time help` — _"Must be **before** the latest restorable time for the DB instance."_ ⇒ 절차는 관측값보다 **앞선 값**을 쓰므로 어느 쪽이든 안전. 거부되면 §12 진단 |
| ⓖ | ECS 멱등성 — client token TTL·재사용 규칙 | ✅ **확정** | [Ensuring idempotency](https://docs.aws.amazon.com/AmazonECS/latest/APIReference/ECS_Idempotency.html) — _"The time to live (TTL) for the `RunTask` client token is **24 hours**. You should not reuse the same client token for different requests."_ / _"If you retry an API request with the same client token and the same request parameters after it has completed successfully, **the result of the original request is returned**. If you retry a successful request using the same client token, but one or more of the parameters are different … the retry fails with a **`ConflictException`**."_ / 멱등 범위는 **클러스터 단위** |
| ⓗ | ECS API의 **결과적 일관성** | ✅ **확정** | [RunTask API](https://docs.aws.amazon.com/AmazonECS/latest/APIReference/API_RunTask.html) — _"The Amazon ECS API follows an **eventual consistency** model … the result of an API command you run that affects your Amazon ECS resources **might not be immediately visible** to all subsequent commands you run."_ + _"Run the DescribeTasks command using an exponential backoff algorithm …"_ ⇒ **MISSING을 "이미 끝났다"로 읽으면 안 된다**(§10.5) |
| ⓘ | `DeleteTaskDefinitions`에서 `failures[].reason=MISSING`이 반환되는 조건 | ⚠️ **미확인** | [API failure reasons](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/api_failures_messages.html)가 `MISSING`을 열거하는 작업은 `DescribeClusters`·`DescribeInstances`·`DescribeServices`·`DescribeTasks`·`GetTaskProtection`·`StartTask`·`UpdateTaskProtection`이고 **`DeleteTaskDefinitions`는 목록에 없다.** ⇒ **확인 전에는 §10.6 2번을 좁게 적용**(revision 부재를 `describe-task-definition`으로 직접 확인한 경우에만 종결 인정) |
| ⓙ | 이미 접수된 revision에 `delete-task-definitions` 재전송 시 응답 | ⚠️ **미확인** | 어느 쪽이든 §10.6의 1~3 분기로 흡수되므로 절차는 안전 |
| ⓚ | **태그 필드명이 서비스마다 다르다 — ECS는 소문자, RDS·IAM·SecretsManager는 대문자** | ✅ **확정(실측)** | botocore 서비스 모델의 `Tag` shape 멤버: `ecs` = **`key`·`value`** / `rds`·`iam`·`secretsmanager` = **`Key`·`Value`**. CLI 실측도 같다 — `aws ecs register-task-definition --tags Key=…,Value=…` → **rc=252(파싱 거부)**, `key=…,value=…` → rc=0. `aws rds add-tags-to-resource`는 정반대. JMESPath는 대소문자를 구분하므로 `tags[?Key==…]`를 ECS 응답에 쓰면 **빈 배열 → `None`** 이다. ⇒ **ECS 태그를 쓰는 모든 자리(`--tags`, `tags[?…]`)는 소문자, RDS·IAM(`TagList[?Key==…]`, `--tags Key=…`)은 대문자.** 한쪽 표기를 다른 쪽에 복사하지 않는다 |

| ⓛ | **이미 연결된 관리형 정책에 `AttachRolePolicy` 재호출 시 동작**(no-op vs 오류) | ⚠️ **미확인** | 널리 멱등으로 알려져 있으나 이 저장소에서 1차 출처로 확인하지 않았다. ⇒ **가정하지 않는다** — §8.1 재개 경로는 `list-attached-role-policies`로 **현재 상태를 조회해 없을 때만** 호출한다(조회는 읽기 전용이라 안전). `PutRolePolicy`는 API 이름과 문서 문구(_"creates or updates"_)로 덮어쓰기가 확정이지만, 같은 이유로 인라인 정책도 **드릴 시크릿 ARN 일치까지 확인한 뒤** 건너뛴다 |

| ⓜ | **IAM이 `get-role`/`get-role-policy`로 돌려주는 정책 문서가 저장한 것과 바이트 단위로 같은가**(정규화·필드 추가 여부) | ⚠️ **미확인** | §8.1은 trust·인라인 문서를 **JSON 구조 전체**로 기대값과 대조한다(선택 투영은 고르지 않은 것을 못 본다 — ⓛ 참조). AWS가 저장 시 `Sid` 삽입·`Principal` 형태 변환 등 정규화를 하면 정확 일치가 깨질 수 있다. ⇒ **불일치는 fail-closed**(변경하지 않고 §10으로) 이므로 절차는 안전하지만, **드릴 당일 첫 `get-role` 응답 원문을 증거로 남겨 이 항목을 닫는다.** 정규화가 실재하면 기대값을 그 형태로 고친다 |

> **ⓚ는 실제로 밟은 함정이다.** ECS 태그 조회 5곳이 대문자로 쓰여 있어 `TD_A_READY`·`TD_B_READY`가 영구히 0이고(드릴이 §3.2를 못 넘음) §10.6이 전 revision을 (A)로 떨어뜨리는 상태가 21라운드 동안 남아 있었다 — **바뀐 코드만 보는 리뷰와 문자열 존재만 보는 회귀 검사로는 "처음부터 틀린 것"이 안 잡힌다.** 그래서 이 항목은 `code-review/regression-ledger.sh`가 기계로 검사한다.

> ⓑ·ⓘ·ⓙ는 **절차가 미확인 상태에서도 안전하도록 설계**돼 있다(보수적 분기). ⓔ는 (A)/(B) 분류가 갈리는 항목이므로 **드릴 당일 실제 관측값을 반드시 증거에 남긴다.**

### 2.4 대기 상한 — 무기한 대기·무한 재시도 금지

초과하면 **드릴을 중단하고 §10 정리 게이트로 진입**한다(중단도 정상 종료 경로다).

| 대기 | 상한 | 초과 시 |
| ---- | ---- | ------- |
| `LatestRestorableTime` 폴링 | **30분** | `T_baseline`·최종 관측 `LatestRestorableTime`·경과 시간을 남기고 중단. **이건 단순 실패가 아니라 복구 지점 지연의 하한을 알려주는 DR 발견**이므로 반드시 기록 |
| `aws rds wait db-instance-available` | **30초 × 60회 = 1800초, 초과 시 exit 255** | `describe-db-instances`로 상태 직접 확인 → 재호출/중단 판단 |
| 드릴 시크릿 `SecretStatus=active` 폴링 | **15분** | 중단 |
| `run-task` 재시도 | **TD-A 총 2회 / TD-B 총 3회**(최초 호출 포함, 30초 간격) | CloudShell 폴백(§8 대안) 또는 중단 |
| `aws ecs wait tasks-stopped` | **6초 × 100회 = 600초, 초과 시 exit 255**. 매처 `tasks[].lastStatus` 전원 STOPPED → **ARN 하나씩** | 절차를 끊지 않고 마지막 `describe-tasks` 결과로 §10.5 재분류 |
| `describe-tasks` MISSING 재조회 / `stop-task` 거부 후 재조회 / `delete-task-definitions` 재전송 | **각각 30초 간격, 최초 포함 총 3회** | 각 절의 규율대로 (A)로 남긴다 |
| `aws rds wait db-instance-deleted` | **30초 × 60회 = 1800초, 초과 시 exit 255**. 매처는 **`length(DBInstances) == 0`**(= 조회에서 사라져야 성공) | `describe`로 직접 확인 → 재호출/중단. **이 waiter 통과가 과금 종료 판정** |
| TD 물리 삭제 | **폴링하지 않는다** | §10.6 즉시 게이트로 판정하고 지연 확인은 익일 |

> **긴 대기를 지났으면 §2.5 ⓪을 다시 실행한 뒤 변경을 재개한다.** 위 표의 `LatestRestorableTime` 폴링·`db-instance-available`·`db-instance-deleted` waiter는 각각 최대 30분이고, 그 뒤에도 §6 reboot·§7 modify·§8.1 IAM·§8.2 TD-B 등록이 이어진다. `DRILL_MUTATION_OK`는 **대기 전에 계산된 캐시 값**이라 그 사이 락이 만료돼도 값 자체는 1로 남는다.
>
> **이 규율은 산문만이 아니다.** 표에 없는 지연 — 운영자가 자리를 비우거나, 15분 시크릿 폴링에 수동 진단·승인 시간이 얹히는 경우 — 까지 덮으려면 사람의 기억에 기댈 수 없다. 그래서 **비정리 변경 13개가 각자 호출 직전에 `lock_live`로 미만료를 다시 확인**한다(§2.5 ⓪-2가 그 epoch를 만든다). 만료 뒤에는 ⓪을 다시 실행하기 전까지 어떤 변경도 열리지 않고, 만료 자체가 STOP 출력에 찍힌다. ⓪은 읽기 전용이라 몇 번을 다시 돌려도 안전하다. **§10 정리는 이 자격도 이 재확인도 요구하지 않으므로 대기 뒤에도, 락이 만료된 뒤에도 그대로 열려 있다**(과금을 끊는 경로는 절대 닫지 않는다).

### 2.5 실행 전 실측 확인

**⓪ 변경 자격 — 신규·재진입 세션이 각각 이 블록을 다시 실행한다.** `SHELL_OK`과 같은 규율이다(§2.1 ⓪). 여기서 나오는 **`DRILL_MUTATION_OK`가 §3~§8·§11의 비정리 변경 13개가 직접 소비하는 단일 자격**이며, 원장에서 붙여 넣거나 손으로 1을 치면 그 순간 게이트가 확인이 아니라 선언이 된다. **§10 정리는 이 자격을 요구하지 않는다** — 락을 잡지 못한 세션도 과금을 끊을 수 있어야 한다.

**신규 세션의 실행 순서는 ⓪ → ⑤ → ⓪이다.** 첫 실행이 실행 주체를 출력하고 STOP하는 것은 **정상**이다. 두 번째 실행 전에 이 블록에서 손으로 채울 값은 **넷**이다 — `LOCK_HOLDER_CONFIRMED=1`·`LOCK_EXPIRES_AT`(⓪-2)·`LEDGER_ACCOUNT_ID`·`LEDGER_CALLER_ARN`(⓪-3) — 그리고 ⑤ 권한표를 그 주체 기준으로 채운 뒤 `PERMISSIONS_OK=1`까지 다섯이다. **블록을 통째로 다시 붙여 넣으면 넷이 다시 0/빈 값으로 초기화되므로**(그것이 이 블록의 fail-closed 규율이다) 값을 채운 사본을 손에 두고 실행한다. ⓪은 읽기 전용이라 몇 번을 다시 돌려도 안전하다. 재진입은 원장의 같은 다섯 값을 붙여 넣고 권한표만 다시 확인하면 한 번으로 끝난다.

**신규 세션도 `LEDGER_*` 없이는 자격이 서지 않는다.** 빈 원장을 "일치"로 읽으면 ⑤ 권한표를 확인한 주체와 실제로 변경하는 주체가 달라도 통과한다(그 사이 `AWS_PROFILE`이 바뀌는 것을 아무도 못 본다). 첫 실행이 출력한 값을 **원장과 이 블록 양쪽에** 적는 한 단계가 그 창을 닫는다.

**⓪-1~⓪-4는 전부 "세션마다 달라지는 사실"이고, ⓪-5가 그것을 하나로 접는다.** 원장에서 되살아나는 값이 아니라 **지금 이 셸·이 사람·이 자격증명·이 디렉터리**의 사실이라, 재진입에서 읽기 전용 reconciliation만으로는 하나도 서지 않는다. **긴 대기 뒤에도 다시 실행한다** — §2.4의 30분급 대기를 지나면 락 잔여 시간이 그만큼 줄고, `DRILL_MUTATION_OK`는 대기 **전에** 계산된 캐시 값이다. 다시 도출하지 않으면 만료된 락으로 §6 reboot·§7 modify·§8.1 IAM·§8.2 TD-B 등록을 계속 통과한다(그 사이 다른 운영자가 새 락을 얻었을 수 있다).

```bash
# ⓪-0 **이전 자격을 먼저 폐기한다 — 이 블록의 첫 실행문이어야 한다.**
# 아래 검사들은 각자 자기 플래그를 0으로 시작하지만, 그 대입은 **검사 직전**에 있다. 그래서 같은 셸에서
# ⓪을 다시 실행하다 `python3`나 STS 조회에서 중단하면(붙여넣기 끊김·Ctrl-C·명령 hang) **직전 실행의
# `DRILL_MUTATION_OK=1`과 아직 미래인 `LOCK_EXPIRES_EPOCH`이 그대로 남아** 이후 변경 게이트가 열린다
# (r28 codex-ide 실측). 재검증을 시작하는 순간 이전 자격은 무효다 — 어느 지점에서 끊겨도 그렇다.
DRILL_MUTATION_OK=0
LOCK_EXPIRES_EPOCH=0        # `lock_live`가 곧바로 실패하게 만든다(두 겹으로 닫는다)
EXCLUSIVE_DRILL_LOCK_OK=0
SESSION_AWS_OK=0
MUTATION_CONTEXT_OK=0
TOOLS_OK=0
echo "이전 변경 자격 폐기 — 이 블록을 끝까지 실행해야 다시 선다"

# ⓪-1 로컬 도구 실측 — **존재가 아니라 실행을 확인한다.** `python3`는 §4·§5·§11의 시각 파싱 유일 수단인데,
# 첫 사용 지점(§4.1 LatestRestorableTime 폴링)이 **sentinel-A POST·TD-A 등록·run-task 뒤**이고 호출이 전부
# `2>/dev/null`이라, 여기서 막지 않으면 되돌릴 수 없는 쓰기를 다 끝낸 자리에서 "치환/형식 오류"로 **오진단**된다
# (완벽히 유효한 값을 보여주면서 형식 오류라고 말한다). macOS의 `/usr/bin/python3`는 Xcode CLT가 없으면
# **존재하지만 실행이 실패**하므로 `command -v`로는 부족하다 → 실제 파싱 왕복을 돌려 본다.
TOOLS_OK=0
PY_SMOKE=$(python3 -c 'import datetime as d,json,re
s="2026-01-02T03:04:05Z"
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s)
x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
assert x.utcoffset() is not None
assert d.datetime.fromtimestamp(int(x.timestamp()),d.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")==s
assert json.loads("{\"code\":\"a1\"}")["code"]=="a1"
print("ok")' 2>&1)
PY_SMOKE_RC=$?
if [ "$PY_SMOKE_RC" -eq 0 ] && [ "$PY_SMOKE" = ok ]; then
  TOOLS_OK=1
else
  echo "STOP: python3 실측 실패(rc=$PY_SMOKE_RC) — 시각·JSON 파싱이 전부 빈 값이 되어 §4.1 이후가 '형식 오류'로 오진단된다"
  echo "      출력: $PY_SMOKE"
fi
echo "TOOLS_OK=$TOOLS_OK"

# ⓪-2 단일 변경 작업 락 — 보유뿐 아니라 **남은 유효기간**까지 이 세션에서 다시 확인한다.
# 불리언 한 번으로 끝내면, 30분급 대기(§2.4 — LatestRestorableTime 폴링·available/deleted waiter) 뒤
# 락이 만료돼 **다른 운영자가 새 락을 얻은 뒤에도** 이 세션이 reboot·modify·IAM·TD-B를 계속 통과한다.
# 그러면 이 락이 막으려던 동시 드릴과 삭제 불가능한 sentinel 상한 초과가 그대로 가능해진다.
LOCK_HOLDER_CONFIRMED=0     # ← 승인 기록에서 활성 작업이 현재 운영자·$TOKEN 하나뿐임을 확인한 뒤에만 1
LOCK_EXPIRES_AT=""          # ← 그 기록의 **만료 시각**을 UTC ISO8601로(예: 2026-08-04T09:30:00Z). §2.6 원장 행
LOCK_MIN_TTL_SECONDS=2700   # 45분 = 최장 단일 대기 30분 + 그 뒤에도 이어지는 변경(reboot·modify·IAM·TD-B)
EXCLUSIVE_DRILL_LOCK_OK=0
LOCK_TTL_OUT=""             # 같은 셸에서 ⓪을 다시 돌릴 때 **직전 실행의 잔여 초가 남아 출력되지 않도록** 비운다
# **`LOCK_EXPIRES_EPOCH`가 이 절의 진짜 산출물이다.** 위 플래그는 ⓪ 실행 **시점**의 사실이라, 그 뒤
# 벽시계가 얼마나 흘렀는지는 모른다(운영자가 46분 자리를 비우거나 15분 시크릿 폴링 + 수동 진단이
# 겹치면 §2.4 표에 없는 경로로 만료를 넘는다 — r26 codex-cli). 그래서 **비정리 변경 13개가 각자
# 호출 직전에 `date -u +%s`와 이 값을 비교**한다. 0으로 시작해 fail-closed다(미설정·파싱 실패 = 만료).
LOCK_EXPIRES_EPOCH=0
if [ "${TOOLS_OK:-0}" != 1 ]; then
  echo "STOP: ⓪-1이 서지 않아 락 만료를 계산할 수 없다 — 잔여 시간을 모르는 락은 자격으로 쓰지 않는다"
elif [ "$LOCK_HOLDER_CONFIRMED" != 1 ]; then
  echo "STOP: 단일 변경 작업 락 보유가 확인되지 않았다 — 승인 기록을 먼저 확인한다(손으로 1을 치는 것은 확인이 아니다)"
  echo "      정리만 수행하는 세션은 이대로 두어도 된다(§10은 이 자격을 요구하지 않는다)"
else
  # 잔여 초와 만료 epoch를 **한 번에** 뽑는다(두 번 파싱하면 두 값이 어긋날 수 있다).
  LOCK_TTL_OUT=$(python3 -c 'import sys,re,datetime as d
s,need=sys.argv[1],int(sys.argv[2])
if not re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s):
    print("bad-format"); sys.exit(2)
exp=d.datetime.fromisoformat(s.replace("Z","+00:00"))
remain=int((exp-d.datetime.now(d.timezone.utc)).total_seconds())
print(remain, int(exp.timestamp())); sys.exit(0 if remain>=need else 1)' "$LOCK_EXPIRES_AT" "$LOCK_MIN_TTL_SECONDS" 2>&1)
  LOCK_TTL_RC=$?
  if [ "$LOCK_TTL_RC" -eq 0 ]; then
    EXCLUSIVE_DRILL_LOCK_OK=1
    LOCK_EXPIRES_EPOCH=$(printf %s "$LOCK_TTL_OUT" | awk '{print $2}')
    case "$LOCK_EXPIRES_EPOCH" in ''|*[!0-9]*) LOCK_EXPIRES_EPOCH=0; EXCLUSIVE_DRILL_LOCK_OK=0
      echo "STOP: 만료 epoch를 얻지 못했다 — 변경 직전 재확인의 기준값이 없으므로 자격을 열지 않는다" ;;
    esac
  elif [ "$LOCK_TTL_RC" -eq 1 ]; then
    echo "STOP: 락 잔여 $(printf %s "$LOCK_TTL_OUT" | awk '{print $1}')초 < 최소 ${LOCK_MIN_TTL_SECONDS}초 — **갱신 전에는 변경 자격을 열지 않는다**"
    echo "      (음수면 이미 만료됐다. 갱신 뒤 이 ⓪ 블록을 처음부터 다시 실행한다)"
  else
    echo "STOP: 락 만료 시각을 파싱하지 못했다(LOCK_EXPIRES_AT='$LOCK_EXPIRES_AT' out=$LOCK_TTL_OUT) — UTC ISO8601로 적는다"
  fi
fi
echo "EXCLUSIVE_DRILL_LOCK_OK=$EXCLUSIVE_DRILL_LOCK_OK 잔여=$(printf %s "${LOCK_TTL_OUT:-미산출}" | awk '{print $1}')초 만료epoch=$LOCK_EXPIRES_EPOCH"

# **호출 직전 미만료 판정은 이 함수 하나로만 한다.** 게이트마다 `[ "$(date -u +%s)" -lt … ]`를 인라인으로
# 쓰면 **양성·음성 게이트의 실패 의미가 갈린다**: `date`가 비숫자를 뱉으면 `[` 는 `integer expression
# expected`로 status 2를 내는데, 양성(`&& [ -lt ]`)은 닫히지만 음성(`|| [ -ge ]`)은 **거짓이 되어 변경
# 분기로 내려간다**(r27 codex-cli 실측 — 인라인을 택한 r26의 실수다). 함수로 모으면 현재 시각 획득과
# 숫자 검증을 한 곳에서 하고, **성공했을 때만 0을 반환**하므로 양쪽 게이트가 같은 방향으로 닫힌다.
#   - 양성 게이트: `… && lock_live`      → 미정의(rc=127)·date 실패·만료 = 전부 닫힘
#   - 음성 게이트: `… || ! lock_live`    → 같은 세 경우가 전부 STOP
# §10.3의 `reconcile_td_dispatch`와 달리 **`unset -f` 하지 않는다** — 세션이 사는 동안 모든 변경 게이트가
# 쓰는 값이고, ⓪을 다시 실행하면 정의도 함께 갱신된다. ⓪을 건너뛴 세션에서는 미정의라 전부 닫힌다.
lock_live() {
  local now
  now=$(date -u +%s 2>/dev/null) || return 1
  case "$now" in ''|*[!0-9]*) return 1 ;; esac
  case "${LOCK_EXPIRES_EPOCH:-0}" in ''|*[!0-9]*) return 1 ;; esac
  [ "$now" -lt "${LOCK_EXPIRES_EPOCH:-0}" ]
}
lock_live && echo "lock_live: 유효(만료까지 $(( LOCK_EXPIRES_EPOCH - $(date -u +%s) ))초)" \
          || echo "lock_live: **만료/미확정** — 이 세션의 비정리 변경은 전부 닫힌다"

# ⓪-3 이 세션의 **AWS 실행 주체·권한**. 권한은 드릴 1회당 사실이 아니라 **지금 이 자격증명의 사실**이다.
# 재진입은 원장의 `ACCOUNT_ID`를 문자열로 붙여 넣을 뿐이라, 다른 `AWS_PROFILE`·다른 role·바뀐 세션 정책으로도
# 읽기 전용 reconciliation은 그대로 통과한다. 그 상태에서 `CreateRole`은 허용되고 `DeleteRole`은 거부되는
# **부분 권한**이면 §8.1이 role을 만든 뒤 §10 정리가 실패한다 — 정리 권한까지 T0 전에 확인하려고 둔
# 프리플라이트를 재진입 정상 경로가 우회하는 것이다. 그래서 주체를 원장과 정확히 결속하고 권한을 세션마다 다시 세운다.
LEDGER_ACCOUNT_ID=""    # ← 원장 §2.6 "계정 ID / 리전 / 실행 주체" 행의 계정. **신규 세션도 첫 실행 뒤 여기에 적는다**
LEDGER_CALLER_ARN=""    # ← 같은 행의 주체 ARN(`sts get-caller-identity`의 `Arn` 그대로)
PERMISSIONS_OK=0        # ← 아래 ⑤ 권한표 네 행이 **이 세션의 주체 기준으로** 모두 allow일 때만 1
SESSION_AWS_OK=0
CALLER_OUT=$(aws sts get-caller-identity --query '[Account,Arn]' --output text 2>&1)
CALLER_RC=$?
CUR_ACCOUNT=$(printf %s "$CALLER_OUT" | awk '{print $1}')
CUR_ARN=$(printf %s "$CALLER_OUT" | awk '{print $2}')
case "$CUR_ACCOUNT" in ''|*[!0-9]*) CUR_ACCOUNT="" ;; esac
# 신규 세션 안내는 **판정과 분리해 먼저** 낸다 — 권한이 아직 안 선 신규 세션도 적을 값을 봐야 한다.
if [ -z "$LEDGER_ACCOUNT_ID" ] && [ -n "$CUR_ACCOUNT" ]; then
  echo "이 세션의 실행 주체 — §2.6 원장과 **위 LEDGER_* 두 줄 양쪽에** 적고 이 블록을 다시 실행한다:"
  echo "  LEDGER_ACCOUNT_ID=$CUR_ACCOUNT"
  echo "  LEDGER_CALLER_ARN=$CUR_ARN"
fi
# **빈 원장을 '일치'로 읽지 않는다.** 두 값이 모두 있을 때만 비교하면, 하나라도 비었을 때 비교 분기 자체를
# 건너뛰고 아래 else로 내려가 **다른 주체에도 자격이 발급된다**(r26 codex-cli·codex-ide 실측). 빈 값은
# "일치"가 아니라 "확인 불가"이므로 독립 분기로 먼저 막는다. 신규 세션의 첫 실행이 여기서 STOP하는 것이 정상이다 —
# 그 STOP이 ⑤ 권한표 확인과 실제 변경 사이에 **주체가 바뀌지 않았음**을 강제하는 유일한 지점이다.
if [ "$CALLER_RC" -ne 0 ] || [ -z "$CUR_ACCOUNT" ] || [ -z "$CUR_ARN" ]; then
  echo "STOP: 현재 AWS 실행 주체를 확인하지 못했다(rc=$CALLER_RC) — 누구로 실행 중인지 모르면 변경하지 않는다: $CALLER_OUT"
elif [ -z "$LEDGER_ACCOUNT_ID" ] || [ -z "$LEDGER_CALLER_ARN" ]; then
  echo "STOP: 원장 주체가 비었다(LEDGER_ACCOUNT_ID='${LEDGER_ACCOUNT_ID}' LEDGER_CALLER_ARN='${LEDGER_CALLER_ARN}')"
  echo "      위에 출력된 두 값을 §2.6 원장과 이 블록에 적고 다시 실행한다. **한쪽만 채우는 것도 미기입이다**"
elif [ "$CUR_ACCOUNT" != "$LEDGER_ACCOUNT_ID" ] || [ "$CUR_ARN" != "$LEDGER_CALLER_ARN" ]; then
  echo "STOP: 이 세션의 AWS 주체가 원장과 다르다 — 만들 권한과 **정리할 권한**이 함께 달라진다"
  echo "      원장: $LEDGER_ACCOUNT_ID / $LEDGER_CALLER_ARN"
  echo "      현재: $CUR_ACCOUNT / $CUR_ARN"
  echo "      의도한 전환이면 원장 주체로 되돌리거나, 새 주체로 ⑤ 권한표를 전부 다시 확인하고 원장을 갱신한다"
elif [ "$PERMISSIONS_OK" != 1 ]; then
  echo "STOP: 이 세션의 권한 4그룹이 확인되지 않았다 — 아래 ⑤ 권한표를 **이 주체 기준으로** 확인하고 위 PERMISSIONS_OK를 1로 친다"
else
  SESSION_AWS_OK=1
fi
echo "SESSION_AWS_OK=$SESSION_AWS_OK account=${CUR_ACCOUNT:-미확인} arn=${CUR_ARN:-미확인} PERMISSIONS_OK=$PERMISSIONS_OK"

# ⓪-4 작업 디렉터리 원장의 **소유권·형태** — §10.3 dispatch 앵커와 대칭인 읽기 전용 검사다.
# 재진입의 `REPO_OK`는 `$DRILL_DIR`가 디렉터리인지만 본다. 그래서 **다른 드릴의 원장**이나 그 디렉터리를
# 가리키는 **symlink**를 붙여 넣어도 변경 자격이 서고, TD 등록·run-task 예약이 그 원장의
# `td-pending/<family>`·`task-pending/*`에 쓰고 성공하면 지운다 — 남의 미대사 증거를 오염·종결시킨다.
# §10.3이 정리 단계에서 같은 검사를 하지만 **변경 단계가 먼저 오염하면 그때는 이미 늦다**(codex-cli r25 실측).
MUTATION_CONTEXT_OK=0
MC_DISPATCH="$DRILL_DIR/dispatch"
if [ -z "${DRILL_DIR:-}" ] || [ ! -d "$DRILL_DIR" ] || [ -L "$DRILL_DIR" ] \
   || [ ! -d "$MC_DISPATCH" ] || [ -L "$MC_DISPATCH" ]; then
  echo "STOP: DRILL_DIR/dispatch가 없거나 symlink다('${DRILL_DIR:-미설정}') — 이 세션에는 쓸 원장이 없다"
elif [ -L "$MC_DISPATCH/manifest" ] || [ ! -s "$MC_DISPATCH/manifest" ] || [ ! -r "$MC_DISPATCH/manifest" ]; then
  echo "STOP: dispatch/manifest가 없거나 읽을 수 없거나 symlink다 — 이 디렉터리가 이번 드릴 원장이라는 증거가 없다"
elif [ "$(awk -F= '$1=="token"{print $2}' "$MC_DISPATCH/manifest")" != "$TOKEN" ]; then
  echo "STOP: manifest의 토큰이 이번 드릴과 다르다 — **다른 드릴의 원장에 쓰려 하고 있다**"
  awk -F= '$1=="token"{print "      manifest="$2}' "$MC_DISPATCH/manifest"; echo "      TOKEN=$TOKEN"
elif ! { [ -d "$MC_DISPATCH/task-pending" ] && [ ! -L "$MC_DISPATCH/task-pending" ] \
         && [ -r "$MC_DISPATCH/task-pending" ] && [ -x "$MC_DISPATCH/task-pending" ] \
         && [ -d "$MC_DISPATCH/td-pending" ] && [ ! -L "$MC_DISPATCH/td-pending" ] \
         && [ -r "$MC_DISPATCH/td-pending" ] && [ -x "$MC_DISPATCH/td-pending" ] \
         && [ -f "$MC_DISPATCH/task-arns" ] && [ -f "$MC_DISPATCH/td-arns" ] \
         && [ ! -L "$MC_DISPATCH/task-arns" ] && [ ! -L "$MC_DISPATCH/td-arns" ] \
         && [ -r "$MC_DISPATCH/task-arns" ] && [ -r "$MC_DISPATCH/td-arns" ]; }; then
  echo "STOP: dispatch 원장 네 경로 중 일부가 없거나 읽을 수 없다 — 부분 손실이다(§2.1이 넷을 전부 미리 만든다)"
  ls -la "$MC_DISPATCH" 2>&1 | sed 's/^/      /'
else
  MUTATION_CONTEXT_OK=1
fi
echo "MUTATION_CONTEXT_OK=$MUTATION_CONTEXT_OK"

# ⓪-5 이 세션의 **비정리 변경 자격**. §3~§8·§11의 변경 게이트가 전부 이 값 하나를 직접 본다.
# 여섯 조건은 모두 **세션마다 달라지는 사실**이다 — 셸 종류, 레포/작업 디렉터리 재구성, 이 머신에서
# 도구가 도는가, 지금 이 사람이 **유효기간이 남은** 락을 쥐고 있는가, 지금 이 자격증명이 **원장과 같은
# 주체이며 필요한 권한을 갖는가**, 쓰려는 원장이 **이번 드릴의 것인가**.
DRILL_MUTATION_OK=0
if [ "${SHELL_OK:-0}" = 1 ] && [ "${REPO_OK:-0}" = 1 ] \
   && [ "${EXCLUSIVE_DRILL_LOCK_OK:-0}" = 1 ] && [ "${TOOLS_OK:-0}" = 1 ] \
   && [ "${SESSION_AWS_OK:-0}" = 1 ] && [ "${MUTATION_CONTEXT_OK:-0}" = 1 ]; then
  DRILL_MUTATION_OK=1
else
  echo "STOP: 이 세션은 비정리 변경 자격이 없다 — SHELL_OK=${SHELL_OK:-미설정} REPO_OK=${REPO_OK:-미설정} EXCLUSIVE_DRILL_LOCK_OK=${EXCLUSIVE_DRILL_LOCK_OK:-미설정} TOOLS_OK=${TOOLS_OK:-미설정} SESSION_AWS_OK=${SESSION_AWS_OK:-미설정} MUTATION_CONTEXT_OK=${MUTATION_CONTEXT_OK:-미설정}"
  echo "      §10 정리는 이 자격 없이도 진행한다(§10.3이 SHELL_OK를 따로 도출한다)"
fi
echo "DRILL_MUTATION_OK=$DRILL_MUTATION_OK"
```

아래 ①~⑧은 신규 세션의 실측이다(재진입은 §2.1 재진입 블록 + 위 ⓪ + 필요한 절의 읽기 전용 재도출만 실행한다). **예외는 ⑤ 권한 확인 표 하나다** — 그 표는 위 ⓪-3의 `PERMISSIONS_OK`가 소비하므로 **모든 세션이 자기 주체 기준으로 다시 확인한다**(주체가 같아도 세션 정책·연결 정책은 그 사이 바뀔 수 있다). 표만 확인하면 되고 ⑤의 조회 명령을 다시 돌릴 필요는 없다.

```bash
# ① 원본 구성 조회 → 복원 명령에 그대로 쓸 값을 고정한다(수기 조립 금지)
aws rds describe-db-instances --db-instance-identifier "$SRC_DB" --query 'DBInstances[0].{
  status:DBInstanceStatus, engine:Engine, ev:EngineVersion, class:DBInstanceClass,
  subnetGroup:DBSubnetGroup.DBSubnetGroupName, sgs:VpcSecurityGroups[].[VpcSecurityGroupId,Status],
  pg:DBParameterGroups[].DBParameterGroupName, enc:StorageEncrypted, pub:PubliclyAccessible,
  addr:Endpoint.Address, port:Endpoint.Port, user:MasterUsername, db:DBName,
  secret:MasterUserSecret.SecretArn, retention:BackupRetentionPeriod,
  latest:LatestRestorableTime}' --output json
# → 서브넷그룹(linkpulse-prod-db)·data SG id·파라미터그룹 이름·운영 시크릿 ARN을 원장에 고정.
#   운영 시크릿 ARN은 **금지 목록**이다(§10.8 ⑥에서 대조).

# ② 대상 식별자가 비어 있는지 — DBInstanceNotFound만 "없음"이다.
#    권한·리전·네트워크 오류를 없음으로 읽으면 이전 드릴 DB를 이번 복원본으로 오인할 수 있다.
TARGET_ABSENT_OK=0
TARGET_STATE=unknown
TARGET_OUT=$(aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" \
             --query 'DBInstances[0].DBInstanceArn' --output text 2>&1)
TARGET_RC=$?
if [ "$TARGET_RC" -eq 0 ]; then
  TARGET_STATE=present
elif printf %s "$TARGET_OUT" | grep -q "DBInstanceNotFound"; then
  TARGET_STATE=absent
  TARGET_ABSENT_OK=1
fi
echo "TARGET_STATE=$TARGET_STATE TARGET_ABSENT_OK=$TARGET_ABSENT_OK rc=$TARGET_RC $TARGET_OUT"

# ③ 클러스터 실측(레포 코드는 "이 서비스의 Terraform 관리 클러스터"까지만 증명한다)
aws ecs list-clusters

# ④ 이름 충돌·잔존 검사 — role과 두 TD family가 모두 "없음"이어야 한다.
aws iam get-role --role-name "$DRILL_ROLE" 2>&1 | tail -1                    # NoSuchEntity 기대

# 계정 ID는 **여기서 한 번만** 뽑아 아래 두 비교에 재사용한다(⑧도 자체적으로 다시 고정한다).
# 비교식 안에서 인라인 호출하면 그 호출이 실패했을 때 계정 자리가 빈 문자열이 되어 **실재하는 revision과도
# 불일치** → `TD_A_EXACT` 공백 → family를 'absent'로 잘못 표시한다(호출 수도 revision당 2회에서 1회로 준다).
ACCOUNT_ID_PRE=$(aws sts get-caller-identity --query Account --output text 2>/dev/null)
case "$ACCOUNT_ID_PRE" in ""|None|*[!0-9]*) ACCOUNT_ID_PRE=""; echo "STOP: 계정 ID를 얻지 못했다 — 아래 family 판정은 unknown으로 남는다" ;; esac

TD_A_FAMILY_STATE=unknown
TD_A_FAMILY_OK=0
TD_A_LIST=$(aws ecs list-task-definitions --family-prefix "$TD_A" --status ACTIVE \
  --query 'taskDefinitionArns' --output text 2>&1)
TD_A_LIST_RC=$?
if [ "$TD_A_LIST_RC" -eq 0 ] && [ -n "$ACCOUNT_ID_PRE" ]; then
  TD_A_EXACT=""
  for arn in $TD_A_LIST; do
    [ "${arn%:*}" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID_PRE}:task-definition/$TD_A" ] \
      && TD_A_EXACT="$TD_A_EXACT $arn"
  done
  if [ -z "$TD_A_EXACT" ]; then TD_A_FAMILY_STATE=absent; TD_A_FAMILY_OK=1
  else TD_A_FAMILY_STATE=present; fi
fi
echo "TD_A_FAMILY_STATE=$TD_A_FAMILY_STATE TD_A_FAMILY_OK=$TD_A_FAMILY_OK exact=$TD_A_EXACT"

TD_B_FAMILY_STATE=unknown
TD_B_FAMILY_OK=0
TD_B_LIST=$(aws ecs list-task-definitions --family-prefix "$TD_B" --status ACTIVE \
  --query 'taskDefinitionArns' --output text 2>&1)
TD_B_LIST_RC=$?
if [ "$TD_B_LIST_RC" -eq 0 ] && [ -n "$ACCOUNT_ID_PRE" ]; then
  TD_B_EXACT=""
  for arn in $TD_B_LIST; do
    [ "${arn%:*}" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID_PRE}:task-definition/$TD_B" ] \
      && TD_B_EXACT="$TD_B_EXACT $arn"
  done
  if [ -z "$TD_B_EXACT" ]; then TD_B_FAMILY_STATE=absent; TD_B_FAMILY_OK=1
  else TD_B_FAMILY_STATE=present; fi
fi
echo "TD_B_FAMILY_STATE=$TD_B_FAMILY_STATE TD_B_FAMILY_OK=$TD_B_FAMILY_OK exact=$TD_B_EXACT"

# ⑤ 실행자 권한 — 관리자 권한을 전제하지 않는다. **이 목록은 런북이 실제로 호출하는 API 전수다**
#    (누락이 있으면 프리플라이트가 통과시킨 실패가 T0 이후·과금 중에 터진다):
#    rds:RestoreDBInstanceToPointInTime, rds:ModifyDBInstance, rds:DescribeDBInstances,
#      rds:RebootDBInstance(§6 교정 경로 — force_ssl이 pending-reboot라 밟힐 가능성이 높다),
#      rds:DeleteDBInstance, rds:AddTagsToResource(--tags), rds:ListTagsForResource(§10.4 삭제 안전 확인),
#      rds:DescribeDBSnapshots·rds:RestoreDBInstanceFromDBSnapshot(§11 스냅샷 절 — 실행 가능한 절이다)
#    §7의 관리 시크릿 전환이 요구하는 종속 권한(AWS 문서 "Permissions required for Secrets Manager integration"):
#      kms:DescribeKey, secretsmanager:CreateSecret, secretsmanager:TagResource
#    ecs:RegisterTaskDefinition/TagResource/ListTagsForResource/RunTask/DescribeTasks/ListTasks/StopTask/
#      DeregisterTaskDefinition/DeleteTaskDefinitions, ecs:ListClusters(③),
#      ecs:ListTaskDefinitions·ecs:DescribeTaskDefinition(④·§10.3)
#    ec2:DescribeSubnets, ec2:DescribeSecurityGroups (§3.3 네트워크 값 조회)
#    iam:CreateRole/TagRole/PutRolePolicy/AttachRolePolicy/DeleteRolePolicy/DetachRolePolicy/DeleteRole/GetRole/
#      ListAttachedRolePolicies/GetRolePolicy/GetUser,
#      **iam:ListRolePolicies**(§8.1이 인라인 정책 **이름 목록 전체**를 기대와 대조한다 — 기대 이름만
#      조회하면 다른 Sid로 얹은 정책을 못 본다. 없으면 재개·readiness가 `unknown`으로 닫힌다)
#    iam:PassRole (실행 role 2개를 태스크에 넘긴다 — 없으면 run-task가 막힌다)
#    secretsmanager:DescribeSecret, secretsmanager:DeleteSecret(§10.8 케이스 ③ 수동 삭제)
#    logs:GetLogEvents/FilterLogEvents, sts:GetCallerIdentity
aws iam get-user 2>/dev/null || aws sts get-caller-identity   # 주체 확인용
#    ※ `PERMISSIONS_OK`는 여기서 선언하지 않는다 — **⓪-3이 세션마다 다시 세우는 값**이다(그 블록의 변수).
#      드릴 1회당 사실로 두면 재진입이 다른 주체·부분 권한으로 리소스를 만든 뒤 정리하지 못한다.

# ⑥ 셸 ↔ AWS 시계 오차 1회 측정(근사값 — 정밀 동기화를 주장하지 않는다)
date -u +%Y-%m-%dT%H:%M:%SZ
curl -sSI "https://rds.$AWS_REGION.amazonaws.com" | awk -F': ' 'tolower($1)=="date"{print $2}' | tr -d '\r'
# → 두 값의 차(셸 − AWS)를 **초 단위 정수**로 계산해 원장과 아래 변수에 적는다(§5가 흡수폭 산출에 쓴다).
#   부호를 유지한다: 셸이 앞서면 양수, 뒤처지면 음수. 측정하지 않으면 §5는 기본 300초를 쓴다.
CLOCK_SKEW_SECONDS=""   # 예: 3  /  -12

# ⑦ 이미지 digest 고정 — 부동 태그 금지(TD-A에 운영 마스터 비밀번호가 주입되고 app SG에 0.0.0.0/0:443 egress가 있다)
docker buildx imagetools inspect postgres:16 | head -20      # Name/Digest(멀티아치 인덱스) + Platforms 확인
# Docker가 없는 환경(CloudShell 폴백 경로가 그렇다)에서는 레지스트리 API로 같은 값을 얻는다:
TOK=$(curl -s "https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/postgres:pull" \
      | sed -E 's/.*"token":"([^"]+)".*/\1/')
curl -sI -H "Authorization: Bearer $TOK" \
  -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json" \
  https://registry-1.docker.io/v2/library/postgres/manifests/16 \
  | awk 'tolower($1)=="docker-content-digest:"{print $2}' | tr -d '\r'
# ※ `tr -d '\r'`: HTTP 헤더 값 끝의 CR를 지운다. 그대로 옮겨 적으면 §2.5 ⑧·§8.2의 hex 검사가
#   fail-closed로 잡기는 하지만 메시지가 "16진수가 아닌 문자"라 원인(개행 문자)을 지목하지 못한다.
# → postgres:16@sha256:<DIGEST> 를 원장에 기록하고 TD-A·TD-B 모두 같은 digest를 쓴다.
#   인덱스 digest면 arch 선택은 Fargate가 하고, 단일 아치 manifest digest를 쓴다면
#   TD의 runtimePlatform.cpuArchitecture를 그 아치와 반드시 일치시킨다.
#   Docker Hub rate limit 폴백(ECR pull-through/수동 push)도 **같은 digest를 보존**한다.
```

**권한 확인 원장 — `T0` 전에, 그리고 세션마다 실행자가 각 행의 결과와 확인 시각을 기입한다.** 단순히 API 이름을 읽은 것은 확인이 아니다. 연결된 정책 검토 또는 `simulate-principal-policy` 등 조직에서 허용한 읽기 전용 방법으로 확인하고, 네 행이 모두 `allow`일 때만 **⓪-3의 `PERMISSIONS_OK=1`** 로 치환한다. **이 표는 생성 권한만이 아니라 정리 권한(`DeleteRole`·`DeleteDBInstance`·`DeleteTaskDefinitions`·`DeleteSecret`)까지 함께 본다** — `CreateRole`은 허용되고 `DeleteRole`은 거부되는 부분 권한이면 리소스를 만든 뒤 §10에서 막힌다.

| 권한 그룹 | 확인 결과(`allow`만 통과) | 확인 시각/근거 | 세션(신규/재진입 N차) |
| --------- | ------------------------- | -------------- | --------------------- |
| RDS 복원·조회·수정·삭제·태그 | ` ` | ` ` | ` ` |
| ECS 등록·태그·실행·조회·정리 | ` ` | ` ` | ` ` |
| IAM role 생성·정책 조회/변경·PassRole | ` ` | ` ` | ` ` |
| Secrets Manager·KMS·Logs·EC2 조회 | ` ` | ` ` | ` ` |

**⑧ 핵심 값을 셸 변수로 고정하고 빈 값이면 중단한다** — 이 값들은 §5(복원)·§3.2/§8.2(TD)에서 **운영 식별자·네트워크 경계**로 쓰이므로 수기 치환 오타가 곧 오조작이다.

```bash
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
PROD_ADDR=$(aws rds describe-db-instances --db-instance-identifier "$SRC_DB" --query 'DBInstances[0].Endpoint.Address' --output text)
PROD_USER=$(aws rds describe-db-instances --db-instance-identifier "$SRC_DB" --query 'DBInstances[0].MasterUsername' --output text)
PROD_DB=$(aws rds describe-db-instances --db-instance-identifier "$SRC_DB"   --query 'DBInstances[0].DBName' --output text)
PROD_SECRET=$(aws rds describe-db-instances --db-instance-identifier "$SRC_DB" --query 'DBInstances[0].MasterUserSecret.SecretArn' --output text)
SUBNET_GROUP=$(aws rds describe-db-instances --db-instance-identifier "$SRC_DB" --query 'DBInstances[0].DBSubnetGroup.DBSubnetGroupName' --output text)
PROD_PG=$(aws rds describe-db-instances --db-instance-identifier "$SRC_DB" --query 'DBInstances[0].DBParameterGroups[0].DBParameterGroupName' --output text)
EXPECTED_ENGINE=$(aws rds describe-db-instances --db-instance-identifier "$SRC_DB" --query 'DBInstances[0].Engine' --output text)
EXPECTED_ENGINE_VERSION=$(aws rds describe-db-instances --db-instance-identifier "$SRC_DB" --query 'DBInstances[0].EngineVersion' --output text)
EXPECTED_CLASS=$(aws rds describe-db-instances --db-instance-identifier "$SRC_DB" --query 'DBInstances[0].DBInstanceClass' --output text)
APP_SUBNETS=$(aws ec2 describe-subnets --filters "Name=tag:Name,Values=linkpulse-prod-app-*" --query 'Subnets[].SubnetId' --output text | tr '\t' ',')
APP_SG=$(aws ec2 describe-security-groups --filters "Name=tag:Name,Values=linkpulse-prod-app-sg" --query 'SecurityGroups[0].GroupId' --output text)
DIGEST="sha256:0000000000000000000000000000000000000000000000000000000000000000"  # ← 위 ⑦ 값으로 치환

# data SG는 "하나뿐"을 확인하고 집는다 — SG가 늘면 [0]이 조용히 다른 값을 잡는다(현재 rds.tf는 1개)
SGS=$(aws rds describe-db-instances --db-instance-identifier "$SRC_DB" \
      --query 'DBInstances[0].VpcSecurityGroups[?Status==`active`].VpcSecurityGroupId' --output text)
if [ "$(printf %s "$SGS" | wc -w)" -eq 1 ]; then DATA_SG="$SGS"; else
  DATA_SG=""; echo "STOP: 운영 SG가 1개가 아니다($SGS) — 어느 것이 data SG인지 사람이 확인해 직접 지정"; fi

VALUES_OK=0
# `SHELL_OK`를 `REPO_OK`와 **함께** 본다 — ⓪을 건너뛰고 이 블록만 붙여 넣은 세션에서도 fail-closed여야 한다
# (미설정은 0으로 읽는다). `DRILL_MUTATION_OK`는 ⓪-5가 도출한 **이 세션의 변경 자격**이라, ⓪을 건너뛰면
# 미설정 → 0이 되어 값이 다 맞아도 VALUES_OK가 서지 않는다. 여기가 ⓐ 계보의 마지막 관문이고,
# 비계보 경로(§4.3·§6·§7·§8.1·§8.3)는 같은 `DRILL_MUTATION_OK`를 각자 직접 본다(머리말).
if [ "${SHELL_OK:-0}" = 1 ] && [ "$REPO_OK" = 1 ] && [ "$PERMISSIONS_OK" = 1 ] \
   && [ "${DRILL_MUTATION_OK:-0}" = 1 ]; then
  VALUES_OK=1
else
  echo "STOP: SHELL_OK=${SHELL_OK:-미설정} REPO_OK=$REPO_OK PERMISSIONS_OK=$PERMISSIONS_OK DRILL_MUTATION_OK=${DRILL_MUTATION_OK:-미설정}(락=${EXCLUSIVE_DRILL_LOCK_OK:-미설정} 도구=${TOOLS_OK:-미설정} 주체=${SESSION_AWS_OK:-미설정} 원장=${MUTATION_CONTEXT_OK:-미설정})"
fi
for v in REPO_DIR TOKEN DRILL_DIR ACCOUNT_ID PROD_ADDR PROD_USER PROD_DB PROD_SECRET SUBNET_GROUP DATA_SG PROD_PG \
         EXPECTED_ENGINE EXPECTED_ENGINE_VERSION EXPECTED_CLASS APP_SUBNETS APP_SG DIGEST; do
  eval "val=\$$v"
  if [ -z "$val" ] || [ "$val" = "None" ]; then echo "STOP: $v 가 비었다"; VALUES_OK=0; continue; fi
  echo "$v=$val"
done   # ← break 하지 않고 끝까지 돌려 **빈 값을 한 번에 전부** 드러낸다.

# 플레이스홀더 digest는 "비어 있지 않다"를 통과하지만 실재하지 않는 이미지다 →
# 그대로 두면 TD 등록은 성공하고 **태스크 기동 때 CannotPullContainerError**로 늦게 터진다.
case "$DIGEST" in
  sha256:0000000000000000000000000000000000000000000000000000000000000000)
    echo "STOP: DIGEST가 플레이스홀더 그대로다 — ⑦의 실측값으로 치환"; VALUES_OK=0 ;;
esac
echo "VALUES_OK=$VALUES_OK"
```

**`VALUES_OK=1`이 아니면 드릴을 시작하지 않는다** — 이 플래그의 실제 소비처는 **네 곳**이다: §3.2(TD-A 등록)·§3.3(TD-A run-task)·§5(PITR 복원)·§11(스냅샷 복원). 넷 다 **ⓐ 계보**(신규 드릴이 처음 여는 유료·영구 경로)이고, 재진입에서 이 절들로 되돌아가려면 §3.1이 안내하는 대로 ⓪과 ⑧(읽기 전용)을 다시 돌린다. 반대로 **§8.2 TD-B 등록은 이 플래그를 요구하지 않는다** — 이미 만들어진 복원본의 검증 재개는 재진입 정상 경로(⓪만 실행)에서 열려야 하므로, §8.1·§8.3과 같이 `DRILL_MUTATION_OK` + 자기 인자 검증으로 닫는다(§2.1 가드 규약).

### 2.6 리소스 원장 — 예정값을 **생성 전에** 적는다

원장은 두 층이다. **(1) 예정값**은 우리가 값을 정하는 것들이라 **호출 전에** 적는다(응답 유실·터미널 종료로 기록이 비어도 이 값으로 되찾는다). **(2) 사후값**은 AWS가 만드는 값이라 **응답 직후** 적는다. 정리 게이트는 **원장 ∪ 재탐색 결과**를 대상으로 삼는다(§10.3).

**(1) 예정값(생성 전 기입)**

| 항목 | 값 |
| ---- | -- |
| 계정 ID / 리전 / **실행 주체 ARN** | ` ` / `ap-northeast-2` / ` ` (§2.5 ⓪-3이 신규 세션에서 출력하는 `LEDGER_ACCOUNT_ID`·`LEDGER_CALLER_ARN` — **재진입은 이 값을 붙여 넣어 현재 주체와 정확 일치를 확인한다**) |
| 드릴 DB 식별자 | `linkpulse-restore-drill` |
| 임시 role 이름 | `linkpulse-restore-drill-exec` |
| TD family | `linkpulse-restore-drill-a` / `linkpulse-restore-drill-b` |
| 드릴 토큰(`--started-by`·로그 접두사) | `restore-drill-________-______-______`(끝 6자리는 §2.1이 만드는 **난수** — 손으로 짓지 않는다) |
| 시도별 client token 5개 | `<TOKEN>-tda-1` `-tda-2` / `<TOKEN>-tdb-1` `-tdb-2` `-tdb-3` |
| 태그 | `purpose=rds-restore-drill` + `drill-token=<TOKEN>` |
| 이미지 digest | `postgres:16@sha256:` ` ` |
| 운영 시크릿 ARN(**삭제 금지 목록**) | ` ` |
| 권한 확인 4그룹 결과·시각 (**세션마다** — §2.5 ⑤ 표) | 신규 ` ` / 재진입 ` ` |
| 단일 변경 작업 락 | 변경 기록 ` ` / owner ` ` / `$TOKEN` ` ` / 획득 ` ` / **만료 `LOCK_EXPIRES_AT`(UTC ISO8601, 예 `2026-08-04T09:30:00Z`)** ` ` / 갱신 이력 ` ` / 해제 ` ` |
| ↳ **락은 드릴 총 소요보다 길게 잡는다** | `LOCK_MIN_TTL_SECONDS=2700`(45분)은 **다음 변경까지 완주할 최소치**이지 드릴 전체 길이가 아니다(최장 단일 대기 30분 + 그 뒤 reboot·modify·IAM·TD-B). 만료를 45분으로 끊어 발급하면 §4.1 폴링 한 번에 재갱신이 필요하다 → 승인자는 **여유 있게 발급하고, 넘칠 것 같으면 갱신 후 위 행과 §2.5 ⓪을 함께 갱신**한다 |
| ↳ **만료 시각의 시계 출처**(§1.1 규율 — 값과 함께 어느 시계인지 적는다) | ` `(예: 변경 관리 시스템 UTC / 승인자 노트북 UTC). 게이트는 이 값을 **실행자 셸의 `date -u +%s`** 와 비교하므로, 두 시계가 어긋나면 만료 판정이 이르거나 늦게 닫힌다. §2.5 ⑥에서 잰 셸↔AWS 오차(`CLOCK_SKEW_SECONDS`)가 크면 그만큼 여유를 두고 발급한다 |
| `git status --short` 기준점 | ` ` |
| 셸↔AWS 시계 오차 `CLOCK_SKEW_SECONDS`(셸 − AWS, 초, 부호 유지) | ` ` |
| 실행 셸(§2.1 ⓪·§10.3이 세션마다 출력하는 `bash <버전>`) | 신규 ` ` / 재진입 ` ` / 정리 ` ` |
| 변경 자격(§2.5 ⓪이 세션마다 출력하는 `TOOLS_OK`·`EXCLUSIVE_DRILL_LOCK_OK`+잔여초·`SESSION_AWS_OK`·`MUTATION_CONTEXT_OK`·`DRILL_MUTATION_OK`) | 신규 ` ` / 재진입 ` ` / **긴 대기 후 재도출 ` `**(§2.4 — 30분급 대기를 지날 때마다 한 줄 추가) (정리 세션은 도출하지 않는다) |
| 복원 호출 상태 | `not-started` → `accepted` / `ambiguous` / `rejected` |
| 최초 `T0` / 요청 종류·source | ` ` |
| 복원 응답 ARN / `InstanceCreateTime` | ` ` |
| 복원 재시도 상태(`ambiguous`+부재 확정, **1회 한정**) | `RESTORE_RETRY_STATE=not-used` → `authorized`(창 개방) → `dispatched`(재호출 직전) / 재시도 시각 ` ` |
| 최초 복원 호출 사전조건 tuple | `RESTORE_PRECHECK_STATE=not-ready` → `ready` / kind ` ` / source ` ` / baseline taskArn ` ` / **B code** ` ` / **B `created_at`** ` ` |
| sentinel-A 생성 상태·건수 | `SENTINEL_A_CREATE_STATE=not-used` → `dispatched` / `A_POST_COUNT=` ` `(이번에 보낼 건수) / **`A_CONSUMED_COUNT=` ` `**(누적 소비한 영구 행 수, 상한 3 — §3.1이 `3 − 이 값`을 남은 한도로 도출한다). **예약 시 증가하고, `curl` 미호출이 코드로 증명된 잔여분만 §3.1 `break`가 감소시킨다** — 그 경우 원장에는 **환급 후 출력값**을 적는다(선차감 값을 적으면 예산이 소진돼 재개가 막힌다) |
| sentinel-B 생성 상태·시도 | `SENTINEL_B_CREATE_STATE=not-used` → `dispatched` → `accepted` / `ambiguous` / `retry-authorized` · `SENTINEL_B_TRY=1`→`2` |
| sentinel-A 중 가장 늦은 `created_at`(§11 스냅샷 모드 경계 판정용) | ` ` |
| 드릴 시크릿 수명주기 | `not-created` → `create-requested` → `arn-known` |
| 드릴 시크릿 소유권 튜플(§7에서 확인된 시점 기록) | `SECRET_OWNER_DB_ARN=` ` ` / `SECRET_PROVENANCE_OK=0`→`1` |

**시도별 client token 규율**(⓸): **재전송 = 동일 토큰 / 다음 논리 시도 = 다음 토큰.**

- **같은 논리 시도의 재전송**(응답 유실·5xx·타임아웃으로 결과를 모를 때) → **동일 토큰**. 중복 태스크가 생기지 않고 원응답을 되받는다(= 유실 응답 회수 경로).
- **확인된 실패 뒤 새 태스크를 띄우는 다음 시도**(IAM 전파 재시도 등) → **다음 토큰**. 같은 토큰이면 최초 결과만 돌아와 **전파가 끝나도 새 태스크가 뜨지 않는다.**
- **토큰을 다른 요청에 재사용하지 않는다** — 파라미터가 하나라도 다르면 `ConflictException`이며, 그 응답의 `resourceIds`에 **해당 토큰에 이미 묶인 taskArn**이 들어 있다(회수 단서로 원장에 옮겨 적는다).
- **상한 = 원장의 토큰 개수**(TD-A 2 = `-tda-1·2` / TD-B 3 = `-tdb-1·2·3`). 상한을 바꾸면 토큰 목록도 함께 바꾼다.

**(2) 사후값(응답 직후 기입)** — 빈 양식 그대로 쓴다.

태스크(시도마다 한 줄, **성공·실패 무관 전건**):

| 시도 | client token | taskArn | `failures[]` | 관측 시각 → `lastStatus`/`desiredStatus` | `exitCode` / 유효 결과 판독 상태 |
| ---- | ------------ | ------- | ------------ | ---------------------------------------- | ----------------------------- |
| tda-1 | | | | | |
| tda-2 | | | | | |
| tdb-1 | | | | | |
| tdb-2 | | | | | |
| tdb-3 | | | | | |

> **위 표의 `taskArn` 전건은 `TASK_ARNS_LEDGER`(공백 구분)로도 함께 적는다.** §10.3이 정리 대상 합집합을 만들 때 쓰는 원장 입력이며, **채택한 2개(`TDA_TASK_ARN`·`TDB_TASK_ARN`)만으로는 실패한 시도의 태스크가 정리에서 빠진다**(그 태스크도 운영 마스터 비밀번호를 주입받았을 수 있다). **정리 세션은 §2.1을 거치지 않으므로 §0 요약의 원장 입력구에 같은 값을 붙여 넣는다.**
>
> **미대사 시도는 이 표에 세어 적지 않는다 — `$DRILL_DIR/dispatch/`가 단일 소스다.** ECS는 결과적 일관성이라 응답을 잃은 시도는 원장에도 재탐색 목록에도 없을 수 있고, **"성공한 빈 조회"가 완결로 읽히면 뒤늦게 뜬 태스크가 (d) PASS 밑에 숨는다**(§10.3). 그 상태를 **사람이 세어 옮기는 숫자**로 두면 세 방향으로 깨진다: ⓐ 셸 산술이 빈 값을 조용히 0으로 만들고 ⓑ **같은 client token 재전송**(§2.6 ⓸ — 새 시도가 아니라 원응답 회수다)이 이중 계상되며 ⓒ 터미널이 죽는 순간 값 자체가 사라진다. 셋 다 이 값이 막으려던 바로 그 창이라, 대신 **AWS 호출 전에 파일을 만들고 대사 뒤에 지운다**:
>
> | 경로 | 언제 | 무엇 |
> | ---- | ---- | ---- |
> | `dispatch/task-pending/<client token>` | `run-task` **호출 전** 생성 → 정확한 `taskArn` 또는 확정 `failures[]` 대사 후 삭제 | 파일 하나 = 미대사 시도 하나. **같은 토큰 재전송은 같은 파일명**이라 두 번 세지 않는다 |
> | `dispatch/task-arns` | 회수한 `taskArn` 한 줄씩(중복 제거) | §10.3 합집합의 입력 |
> | `dispatch/td-pending/<family>` | `register-task-definition` **호출 전** 생성 → 정확한 revision ARN 대사 후 삭제 | family 단위(등록은 family당 1회) |
> | `dispatch/td-arns` | 회수한 revision ARN 한 줄씩 | §10.3 `REVS_UNION`의 입력 |
>
> **`$DRILL_DIR`가 앵커다.** 디렉터리가 없거나 경로가 틀리면 §10.3이 "원장을 못 읽었다"로 보고 탐색을 `unknown`으로 닫는다. 사람이 옮겨 적을 것은 `DRILL_DIR` 경로 하나뿐이고, 그것은 이미 §0 입력구에 있다.
>
> **채택한 시도의 `taskArn`은 `TDA_TASK_ARN`·`TDB_TASK_ARN`으로 따로 적어 둔다.** 재진입에서 §3.4와 §8.3 결과 블록을 다시 읽는 입력이다(플래그는 붙여 넣지 않는다 — §2.1). TD-A·TD-B 각 행에는 같은 taskArn의 `TDA_RESULT_READ_STATE=not-read|read|unknown`·`TDB_RESULT_READ_STATE=not-read|read|unknown`도 함께 둔다. 새 taskArn을 채택하면 그 행은 `not-read`로 시작하고, 응답 불명은 같은 번호·같은 토큰을 유지한다. **확인된 실패 뒤 새 논리 시도로 넘어갈 때만** `TDA_TRY`/`TDB_TRY`를 1 올린 뒤 그 값을 원장에 남긴다.

> **상태 관측 이력(시각 + 값)을 반드시 남긴다.** 이 기록이 없으면 정리 게이트에서 MISSING을 만났을 때 "보관 만료(종결)"와 "아직 못 본 태스크(상태 불명)"를 구분할 수 없어 **전부 (A)로 떨어진다**(§10.5).

TD(**revision ARN마다** 한 줄 — 배치 API라 요청 단위 성패로는 부분 실패를 못 가린다). 등록의 미대사 상태도 위 `dispatch/td-pending/<family>` 파일이 들며 이 표에 세어 적지 않는다(등록 응답을 잃으면 서비스에는 revision이 생겼는데 원장은 비고, INACTIVE 목록은 참조가 끊기면 누락되기까지 한다):

| revision ARN | `deregister` 성패 | `taskDefinitions[]` 포함 | `status` | `failures[].reason` | 재전송 횟수 |
| ------------ | ----------------- | ------------------------ | -------- | ------------------- | ----------- |
| | | | | | |

측정·증거(**측정값도 같은 표에 모아** ADR로 옮길 때 출처가 한 곳이 되게 한다):

| 항목 | 값 |
| ---- | -- |
| sentinel-A code / `created_at`(DB) | ` ` |
| sentinel-B 시도 1 | state ` ` / HTTP·curl 결과 ` ` / code ` ` / `created_at`(DB) ` ` / 채택·마진 판정 ` ` |
| sentinel-B 시도 2(승인된 마진 미달 때만) | state ` ` / HTTP·curl 결과 ` ` / code ` ` / `created_at`(DB) ` ` / 채택·마진 판정 ` ` |
| baseline tuple | `TDA_RESULT_READ_STATE=not-read|read|unknown` / `T_BASELINE=` ` ` / `BASELINE_ROW_COUNT=` ` ` / `BASELINE_CLICKS_SUM=` ` ` / `BASELINE_MAX_CREATED_AT=` ` ` / `A_EXPECTED_COUNT=` ` ` / `BASELINE_A_HITS=` ` ` / `BASELINE_CAPTURED_AT=` ` ` / `BASELINE_SOURCE_TASK_ARN=` ` ` |
| `restore point`(제어면) | ` ` |
| `LatestRestorableTime`(T0 근처 관측) | ` ` |
| `T0` / `T_available` / `T_config_ok` / `T_creds_ready` | ` ` |
| **최초 측정 tuple**(드릴 전체 1회) | `T_VERIFIED=` ` ` / `T_VERIFIED_SOURCE=`(`shell-first-read` \| `shell-first-read-reentry`) ` ` / `T_VERIFIED_TASK_ARN=` ` ` / `T_VERIFIED_TOKEN=` ` ` |
| **TD-B 시도별 판독 상태**(행마다) | `TDB_TASK_ARN=` ` ` / `TDB_RESULT_READ_STATE=not-read\|read\|unknown` ` ` |
| `t_verify_db`(DB, 셸 `T_verified` 유실 시 별도 진단값) | ` ` |
| 복원본 `row_count` / `clicks_sum` / `A_HITS_V` / `B_HITS_V` / 근거 `TDB_TASK_ARN` | ` ` |
| `rds.force_ssl` 실효값 / `FORCE_SSL_OBTAINED` / 근거 `TDB_TASK_ARN` | ` ` |
| `InstanceCreateTime`(복원 응답, 제어면) | ` ` |
| 복원 호출 상태·요청 source·정확한 ARN | ` ` |
| 드릴 시크릿 수명주기 / ARN | `not-created` / ` ` |
| TD revision ARN 2개 | ` ` |
| 삭제 요청 시각 / 물리적 부재 확인 시각 | ` ` |
| 4축 판정 (a)(b)(c)(d) | ` ` |

### 2.7 「이번 드릴 소유」의 단일 정의 — §10.3·§10.4가 그대로 쓰고, §5·§10.8은 여기에 목적별 조건을 더한다

정리 절마다 소유권을 따로 정의하면 **한 절은 "우리 것"이라 시크릿을 지우고 다른 절은 "남의 것"이라 인스턴스를 못 지우는** 어긋남이 생긴다. 정의는 여기 하나뿐이고 각 절은 이것을 참조한다.

> **OWNED(리소스) =**
> ① 고정 드릴 식별자(`linkpulse-restore-drill` / `linkpulse-restore-drill-{a,b}` / `linkpulse-restore-drill-exec`) ∧
> ② **태그 조회 자체가 성공**(실패는 불일치가 아니라 `unknown` → 30초×3 재조회) ∧
> ③ `purpose = rds-restore-drill` ∧
> ④ **`$TOKEN`이 비어 있지 않다** ∧ **`drill-token = $TOKEN`** ∧
> ⑤ 원장 `RESTORE_EXPECTED_ARN`이 **비어 있지 않다면** 관측 ARN과 일치

- **④가 1차 소유권 근거다.** `drill-token`은 §2.1에서 **난수 6자리를 포함**하므로, 다른 실행이 고정 식별자를 먼저 점유했을 때 그 리소스를 이번 드릴 것으로 오인하지 않게 한다. **그래서 `$TOKEN`이 비어 있으면 ④는 성립하지 않는다** — 재진입 템플릿(§2.1)은 `TOKEN=""`로 시작하고, `purpose`만 달리고 `drill-token`이 없는 리소스에서는 관측값도 빈 문자열이라 **`"" = ""`로 1차 근거가 공허하게 참**이 된다. **소유권을 판정하는 비교는 예외 없이 가드를 앞에 둔다.** 빈 관측값이 나오는 경로가 둘이라 쿼리 형태로는 안전을 보장할 수 없다.

- **태그 누락** — `TagList[?Key==\`purpose\`||Key==\`drill-token\`].[Key,Value]`처럼 **행을 걸러 오는** 형태는 태그가 없으면 행 자체가 없어 `awk`가 빈 문자열을 뽑는다(§5·§6 ③·§10.3·§10.4 **네 곳**).
- **태그 값이 빈 문자열** — `Tags[?Key==\`drill-token\`].Value|[0]` 같은 **고정 arity 투영**은 누락은 `None`으로 렌더링해 막아 주지만, [IAM](https://docs.aws.amazon.com/IAM/latest/APIReference/API_Tag.html)·[ECS](https://docs.aws.amazon.com/AmazonECS/latest/APIReference/API_Tag.html) 태그 API는 **값의 최소 길이가 0**이라 `drill-token=""`인 리소스가 존재할 수 있다. 그때 `--output text`의 그 자리는 빈 필드가 되고, `awk`의 기본 필드 분할이 연속 공백을 하나로 합치면서 **뒤 필드가 밀려** 결국 빈 문자열이 된다. 즉 `None` 렌더링은 **누락만** 막는다.

그래서 **§10의 소유권 비교는 투영형이라도 전부 가드한다**(§10.3 TD 재탐색·§10.3 task 소유권·§10.5 `stop-task`·§10.6 ×3·§10.7 `delete-role`). §10은 **락도 `VALUES_OK`도 요구하지 않고**(과금을 끊는 경로라 의도적으로 열어 둔다) 고정 식별자를 점유한 **남의 리소스를 대상으로 삼을 수 있다고 이 문서가 이미 전제**하므로, 빈 토큰을 막아 줄 상위 게이트가 없다.

**§10 밖의 readiness 비교**(§3.2 TD-A·§8.1 create 확인·§8.2 TD-B·§8.3 계보)는 별도 가드를 두지 않는다 — 소유권이 아니라 "쓸 준비가 됐나"를 묻는 비교이고, **빈 `$TOKEN` 세션에서는 그 결과와 무관하게 변경이 열리지 않는다.** `REPO_OK`가 `[ -n "$TOKEN" ]`을 요구하고(§2.1 ⓪) 그것이 `DRILL_MUTATION_OK`의 구성요소라, §3~§8·§11의 **비정리 변경 13개가 전부 닫히기** 때문이다. **§10만 그 자격을 요구하지 않으므로**(과금을 끊는 경로라 의도적으로 열어 둔다) 거기서는 소유권 비교 자체가 마지막 방어선이고, 그래서 §10.3 dispatch 앵커까지 포함해 전수 가드한다. 다만 태그는 이미 생긴 리소스를 구분할 뿐 동시 실행을 막지 않는다. 상호 배제는 §2.2·§2.5의 사람이 관리하는 단일 변경 작업 락이 담당한다.
- **⑤는 있을 때만 쓰는 강화 조건**이다. 응답을 잃어 원장 ARN이 비어 있는 것은 정상 경로이며(§5 `ambiguous`), 그때 소유권을 부정하면 과금 중인 리소스를 정리할 수 없다.
- **`RESTORE_CALL_STATE`는 소유권 조건이 아니다.** 그것은 "우리 요청이 어떻게 끝났나"이지 "이 리소스가 누구 것인가"가 아니다. 전송 불명·뒤늦은 접수·재시도에서 둘은 갈라지고, 그때 호출 상태로 소유권을 부정하면 **우리가 만든 유료 인스턴스를 아무도 지우지 못한다**(§10.2의 "과금 시계를 가장 먼저 멈춘다"가 무너지는 지점).
- **소유권이 서면 원장을 채운다.** `RESTORE_EXPECTED_ARN`이 비어 있었다면 관측 ARN으로 채우고, 시크릿 쪽은 `(SECRET_OWNER_DB_ARN, DRILL_SECRET)` 튜플과 `SECRET_PROVENANCE_OK=1`을 함께 기록한다. 이후 절은 **튜플의 내부 정합**(`SECRET_OWNER_DB_ARN = RESTORE_EXPECTED_ARN`)까지 다시 확인한다.
- **OWNED를 그대로 쓰는 곳과 조건을 더하는 곳을 구분한다**(목적이 다르기 때문이다 — 아래 두 절만 더한다).
  - **§10.3·§10.4**: OWNED 그대로. 질문이 _"이 리소스를 정리해도 되는가"_ 이므로 ⑤가 비어 있어도 정리할 수 있어야 한다.
  - **§5**: OWNED **+ 요청 계보**(`RESTORE_CALL_STATE ∈ {accepted, ambiguous}` ∧ source 원장 일치, `accepted`는 exact ARN·`InstanceCreateTime`까지). 질문이 _"드릴을 계속해도 되는가"_ 이므로 "우리 것"만으로는 부족하고 **어느 요청의 결과인지**가 서야 한다.
  - **§10.8**: OWNED **+ 튜플 내부 정합**(`SECRET_OWNER_DB_ARN = RESTORE_EXPECTED_ARN`, 그래서 ARN이 **비어 있지 않을 것**을 요구한다). 여기서 ⑤가 사실상 필수가 되는 것은 위 "있을 때만"과 모순이 아니다 — **DB가 이미 삭제돼 관측으로 소유권을 다시 세울 수 없어** 유일한 근거가 튜플뿐인데, **튜플이 기록됐다는 것 자체가 ARN이 이미 채워졌다는 뜻**이기 때문이다: §10.3은 소유권이 설 때 비어 있던 `RESTORE_EXPECTED_ARN`을 관측 ARN으로 채운 뒤 튜플을 적고, §7은 관측 DB ARN이 `RESTORE_EXPECTED_ARN`과 **일치할 때만** 튜플을 적는다(빈 값은 어떤 실제 ARN과도 일치하지 않는다).

---

## 3. (b) sentinel-A + baseline 캡처 → `T_baseline`

### 3.1 sentinel-A 생성 (최대 3건)

**sentinel-B(§4.3)와 같은 2단 일회성 가드를 쓴다.** 이 문서의 블록은 붙여넣기로 재실행되는 것이 정상 경로인데, A는 1회 붙여넣기당 **영구 행을 `A_POST_COUNT`건(최대 3)** 소비하므로 가드가 없으면 재진입 한 번으로 §2.2가 사전 승인한 상한(A 3 + B 2 = **5건**, 삭제 API 없음)을 넘긴다. A와 B는 같은 상한을 공유하므로 대비도 같아야 한다.

**(1-a) 전송 예약** — 허용값을 소비하고 영속 상태를 `dispatched`로 바꾼다. 출력된 상태·건수를 원장에 적은 다음에만 (1-b)를 실행한다.

```bash
SENTINEL_A_SEND_ALLOWED=0
# **하드코딩 대입을 쓰지 않는다**(§3.3 `TDA_TRY`와 같은 관용구). `A_POST_COUNT=3`으로 적으면 보충하려고
# 남은 한도를 정해 둬도 **이 블록을 다시 붙여 넣는 순간 3으로 리셋**돼, 삭제 API가 없는 영구 행이
# §2.2 사전 승인 상한(A 3 + B 2 = 5)을 넘긴다. **신규 드릴의 시작값은 §2.1의 `A_POST_COUNT=1`·`A_CONSUMED_COUNT=0`이다**
# — 3이 아니다. 선차감이라 3으로 예약했다가 첫 요청이 429·전송 실패면 남은 한도 3건이 한 번에 사라지고,
# (b) 판정은 1건이면 성립하기 때문이다(§2.1의 같은 주석). **보충한다고 3으로 되돌리지 않는다.**
A_POST_COUNT="${A_POST_COUNT:-}"          # 이번에 보낼 건수
A_CONSUMED_COUNT="${A_CONSUMED_COUNT:-}"  # 원장의 **누적 소비 건수** — 남은 한도는 이 값에서 도출한다.
                                          # 예약 시 증가 / (1-b) `break`의 증명된 미전송분만 감소(§3.1)
A_REMAINING=-1
case "$A_CONSUMED_COUNT" in
  0|1|2|3) A_REMAINING=$((3 - A_CONSUMED_COUNT)) ;;
  *) echo "STOP: A_CONSUMED_COUNT는 0~3이다(원장의 누적 소비 건수) — 현재 '${A_CONSUMED_COUNT:-미설정}'" ;;
esac
A_COUNT_OK=0
case "$A_POST_COUNT" in
  1|2|3) if [ "$A_REMAINING" -ge 1 ] && [ "$A_POST_COUNT" -le "$A_REMAINING" ]; then A_COUNT_OK=1; fi ;;
esac
[ "$A_COUNT_OK" = 1 ] || echo "STOP: A_POST_COUNT는 1~남은 한도($A_REMAINING)여야 한다 — 현재 '${A_POST_COUNT:-미설정}'"
# 영구 쓰기라 프리플라이트가 선 세션에서만 연다(§2.5 ⑧ `VALUES_OK`는 §3.2·§3.3·§5·§11의 전제이기도 하다).
# **보충 상한은 사람이 아니라 코드가 도출한다** — 사람이 `SENTINEL_A_CREATE_STATE`를 `not-used`로 되돌려도
# `A_POST_COUNT ≤ 3 − A_CONSUMED_COUNT`를 여기서 검사하므로 리셋이 선언이 아니라 확인이 된다(§2.1 `TARGET_OK` 규율).
# 재진입 보충 절차: §2.5 ⓪(락·도구 재확인)과 ⑧(읽기 전용)을 다시 돌려 `DRILL_MUTATION_OK`·`VALUES_OK`를 세우고,
# 원장의 `A_CONSUMED_COUNT`(= (1-b)의 **마지막 출력값** — 중단했으면 환급 후 값)와 이번에 보낼
# `A_POST_COUNT`를 적은 뒤 `SENTINEL_A_CREATE_STATE=not-used`로 되돌린다.
if [ "${SHELL_OK:-0}" = 1 ] && [ "${DRILL_MUTATION_OK:-0}" = 1 ] && [ "${VALUES_OK:-0}" = 1 ] \
   && lock_live \
   && [ "$SENTINEL_A_CREATE_STATE" = not-used ] && [ "$A_COUNT_OK" = 1 ]; then
  SENTINEL_A_CREATE_STATE=dispatched
  # **되돌릴 수 없는 호출 앞에서 코드가 누적값을 선차감한다**(§4.3 `SENTINEL_B_CREATE_STATE=dispatched`,
  # §5 `RESTORE_RETRY_STATE=dispatched`, §7 `SECRET_LIFECYCLE=create-requested`와 같은 관용구).
  # POST **뒤에** 올리면 전송 도중 셸이 끊긴 바로 그 창에서 값이 0에 머물고, 보충 때 그 stale 0 위에서
  # 다시 3건이 허용돼 삭제 API가 없는 영구 행이 상한을 넘는다. 보수적으로 먼저 차감한다.
  A_CONSUMED_COUNT=$((A_CONSUMED_COUNT + A_POST_COUNT))
  SENTINEL_A_SEND_ALLOWED=1
  echo "SENTINEL_A_CREATE_STATE=$SENTINEL_A_CREATE_STATE count=$A_POST_COUNT A_CONSUMED_COUNT=$A_CONSUMED_COUNT(선차감) ← **POST 전에 원장 기입**"
else
  # 만료도 함께 찍는다 — `mutation`·`락`은 ⓪ **실행 시점**의 값이라 만료 뒤에도 1로 남는다.
  # 그것만 출력하면 "전부 정상인데 막혔다"가 되어 원인을 부정하는 메시지가 된다(r27 claude-ide).
  echo "STOP: sentinel-A 생성 안 함 — shell=${SHELL_OK:-미설정} mutation=${DRILL_MUTATION_OK:-미설정} values=${VALUES_OK:-미설정} state=${SENTINEL_A_CREATE_STATE:-미설정} count_ok=$A_COUNT_OK consumed=${A_CONSUMED_COUNT:-미설정} remaining=$A_REMAINING 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)"
fi
```

**(1-b) 전송** — 같은 셸의 일회성 허용값만 소비한다. POST 전에 0으로 닫으므로 이 블록을 두 번 붙여 넣어도 두 번째 실행은 없다.

```bash
if [ "${DRILL_MUTATION_OK:-0}" = 1 ] && lock_live \
   && [ "${SENTINEL_A_SEND_ALLOWED:-0}" = 1 ] && [ "$SENTINEL_A_CREATE_STATE" = dispatched ]; then
  SENTINEL_A_SEND_ALLOWED=0
  A_POST_I=1
  # **반복 안에서도 매번 `lock_live`를 본다.** 이 블록은 한 번의 게이트 통과로 **최대 3건의 영구 행**을
  # 쓰는 유일한 자리다(`A_POST_COUNT`=1~3, 각 `curl` 최대 10초). 진입 시 한 번만 확인하면 첫 요청 뒤
  # 락이 만료돼 새 운영자가 인수해도 나머지가 계속 나간다(r28 codex-ide 실측: lock_checks=1 post_calls=3).
  # sentinel 행은 **삭제 API가 없어 되돌릴 수 없으므로** 상한 관리가 곧 이 락의 존재 이유다.
  #
  # **중단 시 미전송분은 환급한다.** 선차감(`:1020`)의 근거는 _"'반영 안 됐다'를 증명할 수 없으니
  # 보수적으로 소비로 센다"_ 인데, 그것은 **응답 불명**(요청은 나갔는데 결과를 모름)에 대한 논거다.
  # 아래 `break` 시점의 잔여분은 `curl`이 **한 번도 실행되지 않았음이 코드로 증명되는** 건수다
  # (`A_POST_I` 증가 전에 끊긴다). 증명 가능한 미전송분까지 소각하면 **0건 전송 + 예산 3건 소진**이
  # 되어 `A_REMAINING=0`으로 (1-a) 재개가 막히고, §3.2가 `A_EXPECTED_COUNT ∈ {1,2,3}`을 요구하므로
  # **TD-A 등록조차 열리지 않는다**(r29 claude-ide 실측 — 드릴이 영구 정지한다).
  # 이미 보낸 `A_POST_I - 1`건만 소비로 남으므로 상한은 그대로 지켜진다.
  while [ "$A_POST_I" -le "$A_POST_COUNT" ]; do
    if ! lock_live; then
      A_SENT=$((A_POST_I - 1))
      A_REFUND=$((A_POST_COUNT - A_SENT))
      A_CONSUMED_COUNT=$((A_CONSUMED_COUNT - A_REFUND))
      echo "STOP: $A_POST_I번째 전송 직전 락 만료 — 남은 전송을 중단한다(만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s))"
      echo "      전송 $A_SENT건(영구 행) / 미전송 $A_REFUND건 **환급** → A_CONSUMED_COUNT=$A_CONSUMED_COUNT ← **이 값을 원장에 적는다**"
      echo "      A_EXPECTED_COUNT에는 응답으로 code를 확인한 개수($A_SENT건 이하)만 적는다."
      echo "      재개: 락 갱신 → §2.5 ⓪ → SENTINEL_A_CREATE_STATE=not-used로 되돌리고 A_POST_COUNT를 다시 정해 (1-a)부터"
      break
    fi
    curl -sS --max-time 10 -X POST https://lpulse.live/api/links \
      -H 'content-type: application/json' -d '{"url":"https://example.com/"}'
    echo
    A_POST_I=$((A_POST_I + 1))
  done
else
  echo "STOP: sentinel-A 전송 안 함 — mutation=${DRILL_MUTATION_OK:-미설정} allowed=${SENTINEL_A_SEND_ALLOWED:-미설정} state=${SENTINEL_A_CREATE_STATE:-미설정} 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)"
fi
# 응답: {"code":"...","short_url":"...","url":"...","clicks":0,"created_at":"...Z"}
# → code 와 created_at(DB 시계)만 원장에 옮겨 적는다.
# → 응답으로 code를 확인한 개수를 A_EXPECTED_COUNT=1~3으로 함께 고정한다.
#    `A_SENT=0`(첫 `curl` 전에 만료)이면 **A_EXPECTED_COUNT를 적을 수 없고 §3.2도 열리지 않는다** —
#    이것이 오류가 아니라 **정상적인 재개 대기 상태**다. 0을 적거나 게이트를 우회하지 않는다.
# → **원장에 적을 A_CONSUMED_COUNT는 이 블록의 마지막 출력값이다.** 두 경우로 갈린다:
#    ① 중단 없이 끝났으면 (1-a)가 출력한 **선차감 값** 그대로(사람이 다시 올리지 않는다).
#    ② 락 만료로 `break` 했으면 **환급 후 출력된 값**(`← 이 값을 원장에 적는다` 줄).
#    ②에서 ①의 선차감 값을 적으면 보내지도 않은 건수가 예산에서 사라져 재개가 막힌다(r29에서 코드로
#    고친 데드엔드를 원장에서 되살리는 것이다 — r30 3인 전원 지적). **화면의 마지막 값을 그대로 옮긴다.**
```

**응답을 못 받은 요청은 재시도하지 않는다(타임아웃·연결 끊김).** 서버에 반영됐는지 알 수 없어 무작정 재시도하면 **사전 승인된 영구 행 상한 5건**을 넘길 수 있다(§2.2 — sentinel 행은 삭제 API가 없다).

- **응답 불명 요청도 상한을 1건 소비한 것으로 센다.** code를 모르니 `sentinel_a_hits`로는 확인되지 않고(그 필터는 **아는 code**만 센다), 확인 가능한 것은 baseline의 **`row_count`가 예상보다 큰지**뿐이다 — 즉 "반영 안 됐다"를 증명할 수 없으므로 **보수적으로 소비**로 계산한다. 그래서 `A_CONSUMED_COUNT`는 응답을 받은 건수가 아니라 **보내기로 예약한 건수**를 (1-a)가 미리 차감한다.
- 중단하고 기록한 뒤, 남은 한도 안에서만 보충한다 — **남은 한도는 (1-a)가 `3 − A_CONSUMED_COUNT`로 도출**하므로 사람이 상한을 판단하지 않는다. sentinel-A는 3건 전부 필요하지 않다 — **1건만 있어도 (b)의 "A 전건 존재" 판정은 성립**하므로(전건 = 원장에 적은 code 전부), 무리해서 채우지 않는다.
- `429`(레이트리밋)는 서버가 명시적으로 거절한 것이라 **행이 늘지 않지만, 선차감분은 환급하지 않는다.** 이 블록은 HTTP 상태를 코드로 판별하지 않으므로 "429였다"는 사람의 눈으로만 아는 사실이고, 그것을 근거로 `A_CONSUMED_COUNT`를 되돌리면 상한 강제가 다시 사람의 기억에 의존하게 된다. 429가 섞이면 남은 한도가 그만큼 줄어드는 것을 감수한다 — **1건만 있어도 (b) 판정이 성립**하므로 실질 손해가 없다. 재시도가 필요하면 남은 한도 안에서 (1-a)부터 다시 예약한다.
- **`A_EXPECTED_COUNT`는 요청 횟수가 아니라 원장에 code를 적은 개수다.** 1~3이 아니면 baseline을 채택하지 않는다. TD-A SQL도 같은 `A_CODES`에서 기대 개수를 계산하고 §3.4가 hits와 일치하는지 확인하므로, 중복 code·누락·0 hits가 영구 쓰기와 유료 복원 호출로 이어지지 않는다.

> **TD-A가 실패해 재실행할 때 sentinel-A는 다시 만들지 않는다** — 이미 DB에 있으므로 상한 3건을 소모하지 않는다.

### 3.2 임시 TD-A 등록 (운영 baseline 읽기용)

운영 DB도 비공개(격리 data 서브넷·`publicly_accessible=false`)이고 data SG는 **`app` SG에서만 5432 inbound**를 받는다 → **app 서브넷 + `app` SG를 단 일회성 Fargate 태스크**로 접속한다. 컴퓨팅은 실행 후 종료돼 지속 인프라가 남지 않는다(제어면 기록은 일정 시간 남으며, 비용도 권한도 없다).

```bash
# ACCOUNT_ID·PROD_ADDR·PROD_USER·PROD_DB·PROD_SECRET·DIGEST 는 §2.5 ⑧에서 이미 고정됐다.
A_CODES=""   # ← §3.1 응답의 실제 sentinel-A code로 치환. 예: "'aaa111','bbb222'"(값 쪽에 작은따옴표)
A_EXPECTED_COUNT="${A_EXPECTED_COUNT:-}"   # ← 응답으로 확인해 원장에 고정한 code 개수(1~3)
A_EXPECTED_OK=0
case "$A_EXPECTED_COUNT" in 1|2|3) A_EXPECTED_OK=1 ;; esac

# A_CODES 형태 게이트 — B의 base62 게이트(§8.2)와 대칭이다. A는 **사람이 손으로 옮겨 적는** 값이라
# 값 쪽 따옴표를 빠뜨리기 쉬운데, `'a','b'` 형태가 아니면 SQL이 code를 **식별자**로 읽어
# `ON_ERROR_STOP=1`에서 죽는다. TD-A 시도는 2회뿐이고, 재진입에서 틀리면 **복원본이 과금 중인 상태로**
# TD-B 3회까지 태운다. 그래서 등록 전에 형태를 막는다(§12 진단표는 사후 설명일 뿐이다).
A_CODES_OK=0
case "$A_CODES" in
  ''|*[!0-9A-Za-z"'"',']*) ;;                      # 영숫자·작은따옴표·쉼표 외 문자가 있으면 거부
  *) printf %s "$A_CODES" \
       | grep -Eq "^'[0-9A-Za-z]+'(,'[0-9A-Za-z]+')*$" && A_CODES_OK=1 ;;
esac
[ "$A_CODES_OK" = 1 ] || echo "STOP: A_CODES 형태 오류 — \"'aaa111','bbb222'\" 처럼 값 쪽에 작은따옴표를 두고 **공백 없이 쉼표로만** 잇는다: '$A_CODES'"

TD_A_READY=0
TD_A_ARN=""

if [ "${DRILL_MUTATION_OK:-0}" != 1 ] || ! lock_live \
   || [ "$VALUES_OK" != 1 ] || [ "$TD_A" != "linkpulse-restore-drill-a" ] \
   || [ -z "$A_CODES" ] || [ "$A_CODES_OK" != 1 ] || [ "$A_EXPECTED_OK" != 1 ]; then
  echo "STOP: 변경 자격 없음(DRILL_MUTATION_OK=${DRILL_MUTATION_OK:-미설정} 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)), §2.5 ⑧ 값 미확정(VALUES_OK=$VALUES_OK), A_CODES 미치환/형태 오류(A_CODES_OK=$A_CODES_OK) 또는 A_EXPECTED_COUNT 범위 오류 — 아래 블록(td-a.json 생성·등록)을 실행하지 않는다"
else

cat > "$DRILL_DIR/td-a.json" <<JSON
{
  "family": "$TD_A",
  "requiresCompatibilities": ["FARGATE"],
  "networkMode": "awsvpc",
  "cpu": "256",
  "memory": "512",
  "executionRoleArn": "arn:aws:iam::${ACCOUNT_ID}:role/linkpulse-prod-ecs-execution",
  "runtimePlatform": { "operatingSystemFamily": "LINUX", "cpuArchitecture": "ARM64" },
  "containerDefinitions": [
    {
      "name": "psql",
      "image": "postgres:16@$DIGEST",
      "essential": true,
      "environment": [
        { "name": "PGHOST", "value": "$PROD_ADDR" },
        { "name": "PGPORT", "value": "5432" },
        { "name": "PGUSER", "value": "$PROD_USER" },
        { "name": "PGDATABASE", "value": "$PROD_DB" },
        { "name": "PGSSLMODE", "value": "require" },
        { "name": "PGCONNECT_TIMEOUT", "value": "15" }
      ],
      "secrets": [
        { "name": "PGPASSWORD", "valueFrom": "${PROD_SECRET}:password::" }
      ],
      "command": ["psql","-v","ON_ERROR_STOP=1","-X","-A","-F","|","-P","pager=off","-c",
        "SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"') AS t_baseline_db, count(*) AS row_count, coalesce(sum(clicks),0) AS clicks_sum, to_char(max(created_at) AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"') AS max_created_at, cardinality(ARRAY[$A_CODES]::text[]) AS sentinel_a_expected, count(*) FILTER (WHERE code IN ($A_CODES)) AS sentinel_a_hits FROM links;"],
      "logConfiguration": {
        "logDriver": "awslogs",
        "options": {
          "awslogs-group": "$LOG_GROUP",
          "awslogs-region": "$AWS_REGION",
          "awslogs-stream-prefix": "$TOKEN"
        }
      }
    }
  ]
}
JSON

# JSON 생성 중 다른 드릴이 같은 고정 family를 등록하는 레이스를 닫기 위해 호출 직전에 다시 확인한다.
TD_A_REGISTER_OK=0
TD_A_NOW=$(aws ecs list-task-definitions --family-prefix "$TD_A" --status ACTIVE \
  --query 'taskDefinitionArns' --output text 2>&1)
TD_A_NOW_RC=$?
TD_A_EXACT=""
if [ "$TD_A_NOW_RC" -eq 0 ]; then
  for arn in $TD_A_NOW; do
    [ "${arn%:*}" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task-definition/$TD_A" ] \
      && TD_A_EXACT="$TD_A_EXACT $arn"
  done
  [ -z "$TD_A_EXACT" ] && TD_A_REGISTER_OK=1
fi

DISPATCH_DIR="$DRILL_DIR/dispatch"
# `lock_live`를 여기서 다시 본다 — 블록 입구 게이트 이후 **`list-task-definitions` 재조회 한 번과 파일
# 쓰기**가 끼므로, 그 사이에 만료되면 새 락 소유자와 겹친 채 revision이 등록된다(§8.1 IAM과 같은 이유).
if [ "$TD_A_REGISTER_OK" = 1 ] && lock_live && mkdir -p "$DISPATCH_DIR/td-pending" \
     && : > "$DISPATCH_DIR/td-pending/$TD_A" && [ -f "$DISPATCH_DIR/td-pending/$TD_A" ]; then
  # **호출 전에 미대사 등록을 디스크에 남긴다**(§3.3 run-task와 같은 관용구). 응답을 잃으면 서비스에는
  # revision이 생겼는데 `TD_A_ARN`은 비어, 이후 TD 탐색의 "성공한 빈 목록"이 0건 완결로 닫힌다.
  # 파일명은 family — 등록은 family당 1회이고, 재실행해도 같은 파일이라 이중 계상되지 않는다.
  TD_A_OUT=$(aws ecs register-task-definition --cli-input-json "file://$DRILL_DIR/td-a.json" \
    --tags key=purpose,value=rds-restore-drill key=drill-token,value="$TOKEN" \
    --query 'taskDefinition.taskDefinitionArn' --output text 2>&1)
  TD_A_RC=$?
  echo "TD-A 등록 rc=$TD_A_RC $TD_A_OUT"
  if [ "$TD_A_RC" -eq 0 ] \
     && [ "${TD_A_OUT%:*}" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task-definition/$TD_A" ]; then
    TD_A_ARN="$TD_A_OUT"
    grep -Fxq -- "$TD_A_ARN" "$DISPATCH_DIR/td-arns" 2>/dev/null \
      || printf '%s\n' "$TD_A_ARN" >> "$DISPATCH_DIR/td-arns"
    if grep -Fxq -- "$TD_A_ARN" "$DISPATCH_DIR/td-arns" 2>/dev/null; then
      rm -f "$DISPATCH_DIR/td-pending/$TD_A"      # ARN 기록 확인 뒤에만 닫는다
      echo "TD_A_ARN=$TD_A_ARN"   # ← 즉시 원장에 기입(미대사 파일은 코드가 닫았다)
    else
      echo "STOP: revision ARN을 원장에 기록하지 못했다 — 미대사 파일을 남긴다"
    fi
  else
    echo "STOP: revision ARN을 대사하지 못했다 — **미대사 파일이 남는다**(정상). §10.3의 drill-token 태그"
    echo "      재탐색이 이번 토큰 revision을 정확히 1건 찾으면 그 파일을 자동으로 닫는다"
  fi
else
  # 이 `else`는 family 재확인·원장 생성 실패뿐 아니라 **등록 직전 `lock_live` 실패**로도 도달한다.
  echo "STOP: TD-A family가 absent로 재확인되지 않았거나(rc=$TD_A_NOW_RC exact=$TD_A_EXACT) 미대사 등록 원장을 만들지 못했거나 등록 직전 락이 만료됐다(락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)) — 등록하지 않는다"
fi

fi   # ← 위 VALUES_OK 가드 블록의 끝
```

**TD-A 생성과 readiness 판정은 분리한다.** 신규 등록 직후와 세션 재진입 모두 아래 **읽기 전용 reconciliation**을 실행한다. 원장의 정확한 `TD_A_ARN`이 비었거나, ARN·family·revision·상태·태그·핵심 구성이 하나라도 다르면 `TD_A_READY=0`이며 등록을 재시도하지 않는다.

```bash
TD_A_READY=0
TD_A_BASE=${TD_A_ARN%:*}
TD_A_REV=${TD_A_ARN##*:}
TD_A_ARN_FORMAT_OK=0
if [ "$TD_A_BASE" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task-definition/$TD_A" ]; then
  case "$TD_A_REV" in ""|*[!0-9]*) ;; *) TD_A_ARN_FORMAT_OK=1 ;; esac
fi

if [ "$TD_A_ARN_FORMAT_OK" = 1 ]; then
  TD_A_META=$(aws ecs describe-task-definition --task-definition "$TD_A_ARN" --include TAGS \
    --query '[taskDefinition.taskDefinitionArn,taskDefinition.status,taskDefinition.family,
      taskDefinition.revision,taskDefinition.executionRoleArn,taskDefinition.networkMode,
      taskDefinition.cpu,taskDefinition.memory,taskDefinition.runtimePlatform.cpuArchitecture,
      taskDefinition.containerDefinitions[0].image,
      taskDefinition.containerDefinitions[0].environment[?name==`PGHOST`].value|[0],
      taskDefinition.containerDefinitions[0].secrets[?name==`PGPASSWORD`].valueFrom|[0],
      taskDefinition.containerDefinitions[0].logConfiguration.options."awslogs-group",
      taskDefinition.containerDefinitions[0].logConfiguration.options."awslogs-stream-prefix",
      tags[?key==`purpose`].value|[0],tags[?key==`drill-token`].value|[0],
      length(taskDefinition.containerDefinitions),taskDefinition.taskRoleArn]' \
    --output text 2>&1)
  TD_A_META_RC=$?
  if [ "$TD_A_META_RC" -eq 0 ]; then
    TD_ARN_OBS=$(printf %s "$TD_A_META" | awk '{print $1}')
    TD_STATUS_OBS=$(printf %s "$TD_A_META" | awk '{print $2}')
    TD_FAMILY_OBS=$(printf %s "$TD_A_META" | awk '{print $3}')
    TD_REV_OBS=$(printf %s "$TD_A_META" | awk '{print $4}')
    TD_ROLE_OBS=$(printf %s "$TD_A_META" | awk '{print $5}')
    TD_NET_OBS=$(printf %s "$TD_A_META" | awk '{print $6}')
    TD_CPU_OBS=$(printf %s "$TD_A_META" | awk '{print $7}')
    TD_MEMORY_OBS=$(printf %s "$TD_A_META" | awk '{print $8}')
    TD_ARCH_OBS=$(printf %s "$TD_A_META" | awk '{print $9}')
    TD_IMAGE_OBS=$(printf %s "$TD_A_META" | awk '{print $10}')
    TD_HOST_OBS=$(printf %s "$TD_A_META" | awk '{print $11}')
    TD_SECRET_OBS=$(printf %s "$TD_A_META" | awk '{print $12}')
    TD_LOG_OBS=$(printf %s "$TD_A_META" | awk '{print $13}')
    TD_PREFIX_OBS=$(printf %s "$TD_A_META" | awk '{print $14}')
    TD_PURPOSE_OBS=$(printf %s "$TD_A_META" | awk '{print $15}')
    TD_TOKEN_OBS=$(printf %s "$TD_A_META" | awk '{print $16}')
    # 컨테이너 개수·task role 부재까지 본다 — TD-A에는 **운영 마스터 비밀번호**가 주입되므로,
    # 사이드카가 하나 더 붙거나 task role이 생기면 그 비밀번호에 닿는 경로가 늘어난다.
    TD_COUNT_OBS=$(printf %s "$TD_A_META" | awk '{print $17}')
    TD_TASKROLE_OBS=$(printf %s "$TD_A_META" | awk '{print $18}')
    if [ "$TD_COUNT_OBS" = 1 ] && [ "$TD_TASKROLE_OBS" = None ] \
       && [ "$TD_ARN_OBS" = "$TD_A_ARN" ] && [ "$TD_STATUS_OBS" = ACTIVE ] \
       && [ "$TD_FAMILY_OBS" = "$TD_A" ] && [ "$TD_REV_OBS" = "$TD_A_REV" ] \
       && [ "$TD_ROLE_OBS" = "arn:aws:iam::${ACCOUNT_ID}:role/linkpulse-prod-ecs-execution" ] \
       && [ "$TD_NET_OBS" = awsvpc ] && [ "$TD_CPU_OBS" = 256 ] && [ "$TD_MEMORY_OBS" = 512 ] \
       && [ "$TD_ARCH_OBS" = ARM64 ] && [ "$TD_IMAGE_OBS" = "postgres:16@$DIGEST" ] \
       && [ "$TD_HOST_OBS" = "$PROD_ADDR" ] && [ "$TD_SECRET_OBS" = "${PROD_SECRET}:password::" ] \
       && [ "$TD_LOG_OBS" = "$LOG_GROUP" ] && [ "$TD_PREFIX_OBS" = "$TOKEN" ] \
       && [ "$TD_PURPOSE_OBS" = rds-restore-drill ] && [ "$TD_TOKEN_OBS" = "$TOKEN" ]; then
      TD_A_READY=1
    fi
  fi
fi
echo "TD_A_READY=$TD_A_READY TD_A_ARN=$TD_A_ARN rc=${TD_A_META_RC:-미호출}"
```

**태스크 기동 전제(하나라도 빠지면 당일 기동 실패)**:

- **시크릿 JSON 키** — RDS 관리 시크릿 값은 JSON이고, ECS는 키를 생략하면 **SecretString 전체**를 환경변수에 넣는다. 반드시 **`:password::`** 접미를 쓴다(운영 `ecs.tf:55`와 같은 형태). `PGPASSWORD`면 psql이 자동으로 쓴다. 나머지 접속정보는 비민감이라 일반 env.
- **플랫폼 버전** — 시크릿의 JSON 키 주입은 Linux Fargate **1.4.0 이상** → `run-task --platform-version 1.4.0` 명시.
- **로그 그룹** — 실행 role의 `AmazonECSTaskExecutionRolePolicy`에는 **`logs:CreateLogGroup`이 없다** → `awslogs-create-group`을 쓰지 말고 **기존 `/ecs/linkpulse-prod-app`을 재사용**한다. 스트림 접두사는 드릴 토큰.
- **task role은 부여하지 않는다** — 컨테이너가 AWS API를 호출하지 않으므로 불필요(최소권한 경계).
- **네트워크** — app 서브넷 + `app` SG + `assignPublicIp=DISABLED`(NAT egress로 이미지 pull·Secrets/Logs 도달).

### 3.3 실행 (TD-A, **총 시도 상한 2회**)

**§3.1 sentinel-A와 같은 2단 구조를 쓴다** — `run-task`는 되돌릴 수 없는 호출이고, 응답을 잃으면 **운영 마스터 비밀번호를 주입받은 컨테이너**가 우리가 모르는 채로 뜬다. 그래서 **(1) 예약이 미대사 상태를 디스크에 먼저 남기고**, (2) 전송이 같은 셸의 일회성 허용값만 소비한다.

**(1) 예약** — `$DRILL_DIR/dispatch/task-pending/<client token>`을 만든다(§2.6).

```bash
# APP_SUBNETS·APP_SG 는 §2.5 ⑧에서 고정됐다(비어 있으면 거기서 멈췄어야 한다).
# **시도 번호는 §2.1이 들고 있다.** 재시도에서 같은 client token을 쓰면 멱등 TTL(24시간, §2.3 ⓖ) 때문에
# **새 태스크가 뜨지 않고 이전 결과가 돌아온다** — "재시도했는데 아무 일도 안 일어남"이 되고 원인이 겉으로
# 드러나지 않는다. 여기서 `=1`로 대입하면 **블록을 다시 붙여 넣을 때마다 조용히 1로 리셋**되므로,
# 기존 값을 보존하고 다음 논리 시도로 넘어갈 때 사람이 직접 올린다(TD-A 상한 2 = §2.6 원장의 토큰 개수).
# 신규 드릴은 §2.1이 1로 시작한다. 재진입 빈 값은 1로 보정하지 않고 아래에서 STOP한다.
TDA_TRY="${TDA_TRY:-}"
TD_A_RUN_OK=0
TD_A_RUN_BASE=${TD_A_ARN%:*}
TD_A_RUN_REV=${TD_A_ARN##*:}
if [ "$TD_A_RUN_BASE" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task-definition/linkpulse-restore-drill-a" ]; then
  case "$TD_A_RUN_REV" in ""|*[!0-9]*) ;; *) TD_A_RUN_OK=1 ;; esac
fi
case "$TDA_TRY" in
  "") TD_A_RUN_OK=0; echo "STOP: 원장에서 TD-A 시도 번호를 넣는다(응답 불명은 같은 번호, 확인된 실패 뒤에만 +1)" ;;
  1|2) ;;
  *) TD_A_RUN_OK=0; echo "STOP: TD-A 시도 상한 2회 초과(TDA_TRY=$TDA_TRY)" ;;
esac
TDA_SEND_ALLOWED=0
TDA_RUN_TOKEN="$TOKEN-tda-$TDA_TRY"   # §8.3과 이름을 나눈다 — 공유하면 §8.3 (1)이 이 값을 덮어
DISPATCH_DIR="$DRILL_DIR/dispatch"    #   §3.3 (2)가 **다른 토큰으로 전송**하고 남의 pending을 지운다
RESERVE_OK=0
if [ "${DRILL_MUTATION_OK:-0}" = 1 ] && [ "$VALUES_OK" = 1 ] && [ "$TD_A_READY" = 1 ] && [ "$TD_A_RUN_OK" = 1 ] \
   && lock_live \
   && [ "$CLUSTER" = "linkpulse-prod-cluster" ] && [ -n "$TOKEN" ] \
   && [ -n "${DRILL_DIR:-}" ] && mkdir -p "$DISPATCH_DIR/task-pending"; then
  # **미대사 시도를 AWS 호출 전에 디스크에 남긴다.** 셸 변수·사람이 세는 숫자로 두면 터미널이 죽는
  # 바로 그 순간에 사라지고, 같은 client token 재전송(= 원응답 회수, §2.6 ⓸)이 이중 계상된다.
  # **같은 토큰은 같은 파일명**이라 재전송해도 파일이 하나뿐이고, 대사에 성공하면 그 하나가 지워진다.
  # **쓰기 성공을 확인한 뒤에만 전송 자격을 연다** — 파일 원장이 단일 소스이므로, 기록에 실패한 채
  # 호출하면 응답을 잃었을 때 그 시도의 흔적이 어디에도 없다(= 이 원장을 만든 이유 그 자체).
  if : > "$DISPATCH_DIR/task-pending/$TDA_RUN_TOKEN" \
     && [ -f "$DISPATCH_DIR/task-pending/$TDA_RUN_TOKEN" ]; then
    RESERVE_OK=1
    TDA_SEND_ALLOWED=1
    echo "client token = $TDA_RUN_TOKEN"      # ← 원장의 시도별 토큰과 대조한다
    echo "미대사 예약: $(ls "$DISPATCH_DIR/task-pending" | tr '\n' ' ')"
  else
    echo "STOP: 미대사 원장 파일을 만들지 못했다($DISPATCH_DIR/task-pending/$TDA_RUN_TOKEN) — run-task를 호출하지 않는다"
  fi
else
  echo "STOP: DRILL_MUTATION_OK=${DRILL_MUTATION_OK:-미설정} VALUES_OK=${VALUES_OK:-미설정} TD_A_READY=$TD_A_READY TD_A_RUN_OK=$TD_A_RUN_OK CLUSTER='$CLUSTER' TOKEN='${TOKEN:-미설정}' DRILL_DIR='${DRILL_DIR:-미설정}' 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s) — run-task를 예약하지 않았다"
fi
```

**(2) 전송** — 같은 셸의 일회성 허용값만 소비한다. **태스크에 `drill-client-token` 태그를 붙여** 나중에 재탐색으로 회수했을 때 어느 시도가 만든 태스크인지 코드가 대사할 수 있게 한다(`Task` 응답에는 client token 필드가 없다 — 태그가 유일한 상관 키다. `ecs:TagResource`는 §2.5 권한 목록에 이미 있고, 태그 실패 시 ECS가 태스크 생성 자체를 롤백하므로 "태그 없는 드릴 태스크"가 생기지 않는다).

```bash
if [ "${DRILL_MUTATION_OK:-0}" = 1 ] && [ "${TDA_SEND_ALLOWED:-0}" = 1 ] && [ "${RESERVE_OK:-0}" = 1 ] \
   && lock_live \
   && [ -f "$DISPATCH_DIR/task-pending/$TDA_RUN_TOKEN" ]; then   # 예약 파일을 다시 확인한다
  TDA_SEND_ALLOWED=0
  RUN_OUT=$(aws ecs run-task --cluster "$CLUSTER" --launch-type FARGATE --platform-version 1.4.0 \
    --task-definition "$TD_A_ARN" \
    --started-by "$TOKEN" --client-token "$TDA_RUN_TOKEN" \
    --tags "key=drill-client-token,value=$TDA_RUN_TOKEN" "key=drill-token,value=$TOKEN" \
    --network-configuration "awsvpcConfiguration={subnets=[$APP_SUBNETS],securityGroups=[$APP_SG],assignPublicIp=DISABLED}" \
    --query '{arn:tasks[].taskArn,failures:failures}' --output json 2>&1)
  RUN_RC=$?
  printf '%s\n' "$RUN_OUT"
  # 정확한 taskArn 또는 확정적 failures[](`reason` 포함)를 얻었을 때만 그 시도를 닫는다.
  RUN_ARN=$(printf %s "$RUN_OUT" | grep -oE "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task/[^\"]+" | head -1)
  if [ "$RUN_RC" -eq 0 ] && { [ -n "$RUN_ARN" ] || printf %s "$RUN_OUT" | grep -q '"reason"'; }; then
    # **ARN 기록에 성공한 것을 확인한 뒤에만 pending을 지운다.** append가 실패했는데 지우면
    # 디스크에 남은 유일한 증거가 사라진다(codex-cli·codex-ide r21 실측 경로).
    ARNS_OK=1
    if [ -n "$RUN_ARN" ]; then
      ARNS_OK=0
      grep -Fxq -- "$RUN_ARN" "$DISPATCH_DIR/task-arns" 2>/dev/null \
        || printf '%s\n' "$RUN_ARN" >> "$DISPATCH_DIR/task-arns"
      grep -Fxq -- "$RUN_ARN" "$DISPATCH_DIR/task-arns" 2>/dev/null && ARNS_OK=1
    fi
    if [ "$ARNS_OK" = 1 ]; then
      rm -f "$DISPATCH_DIR/task-pending/$TDA_RUN_TOKEN"
      echo "대사 완료 arn='$RUN_ARN' 남은 미대사: $(ls "$DISPATCH_DIR/task-pending" | tr '\n' ' ')"
    else
      echo "STOP: taskArn을 원장에 기록하지 못했다($DISPATCH_DIR/task-arns) — **미대사 파일을 남긴다.**"
      echo "      arn='$RUN_ARN' 을 손으로 그 파일에 적고 §10.3 ⓒ로 닫는다"
    fi
  else
    echo "STOP: 응답을 대사하지 못했다(rc=$RUN_RC) — **미대사 파일이 남는다**(정상). 같은 client token으로"
    echo "      (1)(2)를 다시 실행하면 멱등 원응답을 회수해 그 파일이 닫힌다(§2.6 ⓸). 그래도 안 되면 §10.3 ⓒ의 회수 블록."
  fi
else
  # **"예약되지 않았다"만 찍으면 락 만료로 막힌 경우 거짓 진술이 된다** — 예약은 성공했고 pending 파일도
  # 그대로 있다. 그 안내대로 (1)을 다시 돌리면 (1)도 만료로 STOP하므로 결국 도달은 하지만, 그 사이
  # 디스패치되지 않은 pending이 남아 §10.3이 `TASK_DISCOVERY_STATE=unknown` → (d) FAIL로 닫힌다.
  # 게이트가 보는 네 조건을 그대로 열거해 락 갱신 → ⓪ 재실행 → 전송으로 곧장 복구할 수 있게 한다.
  echo "STOP: TD-A run-task를 전송하지 않았다 — mutation=${DRILL_MUTATION_OK:-미설정} send_allowed=${TDA_SEND_ALLOWED:-미설정} reserve_ok=${RESERVE_OK:-미설정} pending파일=$([ -f "$DISPATCH_DIR/task-pending/$TDA_RUN_TOKEN" ] && echo 있음 || echo 없음) 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)"
  echo "      예약(1)이 이미 성공했고 pending 파일이 있으면 (1)을 다시 돌리지 않는다 — 락을 갱신하고 §2.5 ⓪만 다시 실행한 뒤 이 블록을 재실행한다"
fi
# → taskArn·failures를 시도별로 원장에 기입(실패한 시도의 태스크도 정리 대상이다).
# 새 TDA_TASK_ARN을 채택할 때 같은 행의 TDA_RESULT_READ_STATE=not-read도 함께 적는다.
# → 미대사 상태·회수 ARN은 `$DRILL_DIR/dispatch/`가 들고 있으므로 손으로 옮겨 적지 않는다.
```

### 3.4 결과 판정 — **로그를 읽기 전에 exitCode부터 본다**

**§2.1 규약대로 플래그를 세우고, 플래그가 서지 않으면 `T_baseline`을 채택하지 않는다.** `T_baseline`은 §4.1·§4.2의 60초 마진과 `RESTORE_POINT_OK`를 통해 **(b) 단조성 논증 전체의 기준**이 되므로, 비영 종료나 빈 로그에서 값을 주우면 판정 근거가 오염된다.

**이 블록은 읽기 전용이라 태스크가 조회되는 재진입에서는 그대로 다시 돌린다.** 원장의 `TDA_TASK_ARN`·`TDA_RESULT_READ_STATE`·`A_EXPECTED_COUNT`와 같은 태스크 로그를 대조해 `BASELINE_RESULT_OK`를 **재도출**한다 — 그래서 플래그를 손으로 `1`로 적을 필요가 없다(§2.1 규약). 현재 taskArn의 상태가 `not-read`이고 baseline tuple 전 필드가 비어 있으면 재진입 세션이어도 그 순간이 정상적인 최초 판독이라 캡처 시각과 tuple을 한 번 기록한다. `read`이면 보존된 tuple 전체가 현재 로그와 정확히 같을 때만 성공 플래그를 다시 세우며, 부분 유실을 현재 시각이나 로그값으로 메우지 않는다. 복원 **전**에 태스크 조회 보관이 끝났다면 baseline을 다시 뜨거나(`TDA_TRY` 규율 준수) 중단한다. 이미 복원을 요청한 뒤라면 새 baseline은 기존 복구 지점보다 늦어질 수 있으므로 다시 뜨지 않고, §5 reconciliation → §4.3의 원장 tuple 경로로 간다.

```bash
BASELINE_RESULT_OK=0
TDA_TASK_ARN="${TDA_TASK_ARN:-}"   # ← §3.3 응답의 taskArn(원장 사후값). 재진입에서 원장 값을 덮지 않는다
TDA_RESULT_READ_STATE="${TDA_RESULT_READ_STATE:-unknown}"
# **관측값을 매 실행마다 비운다.** 실패 재실행에서 이전 실행의 값이 남아 출력되면
# "이번 실행이 얻은 값"과 구분되지 않는다(플래그는 0인데 값은 있는 상태 = 오독의 씨앗).
T_BASELINE_OBS=""
BASE_ROW_COUNT_OBS=""; BASE_CLICKS_SUM_OBS=""; BASE_MAX_CREATED_OBS=""
A_EXPECTED_OBS=""; A_HITS_OBS=""
BASE_LOG=""
if [ -z "$TDA_TASK_ARN" ]; then
  echo "STOP: TDA_TASK_ARN이 비었다 — §3.3 응답의 taskArn을 붙여넣는다"
elif [ "${TDA_TASK_ARN%%/*}" != "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task" ]; then
  echo "STOP: 이번 계정·리전의 task ARN이 아니다: $TDA_TASK_ARN"
elif ! aws ecs wait tasks-stopped --cluster "$CLUSTER" --tasks "$TDA_TASK_ARN"; then   # 상한 600초
  echo "STOP: tasks-stopped waiter 미통과(상한 초과·권한 오류) — 결과를 읽지 않는다"
else
  aws ecs describe-tasks --cluster "$CLUSTER" --tasks "$TDA_TASK_ARN" \
    --query 'tasks[0].{last:lastStatus,desired:desiredStatus,stopped:stoppedReason,
             exit:containers[0].exitCode,reason:containers[0].reason}' --output json
  # **exitCode를 셸 조건으로 본다.** 사람이 JSON을 눈으로 보는 것만으로는 게이트가 아니다.
  EXITC=$(aws ecs describe-tasks --cluster "$CLUSTER" --tasks "$TDA_TASK_ARN" \
          --query 'tasks[0].containers[0].exitCode' --output text 2>&1)
  EXITC_RC=$?
  TASK_ID=${TDA_TASK_ARN##*/}
  if [ "$EXITC_RC" -ne 0 ] || [ "$EXITC" != 0 ]; then
    # exitCode != 0 이면 로그가 비어 있을 수 있다 → §12 진단. "결과 없음"으로 오판하지 않는다.
    echo "STOP: exitCode=$EXITC (rc=$EXITC_RC) — T_baseline을 채택하지 않고 §12 진단으로 간다"
  else
    BASE_LOG=$(aws logs get-log-events --log-group-name "$LOG_GROUP" \
      --log-stream-name "$TOKEN/psql/$TASK_ID" --query 'events[].message' --output text 2>&1)
    BASE_LOG_RC=$?
    printf '%s\n' "$BASE_LOG"
    # 채택은 **파싱 성공까지** 확인한다. 로그 조회 실패·빈 로그를 "값 없음"으로 넘기지 않는다.
    # `--output text`는 메시지 목록을 **TAB으로 이어 한 줄**로 내므로 줄로 되돌린 뒤 파싱한다.
    # psql은 `-A -F'|'`이라 **헤더 1줄 + 값 1줄**(뒤에 `(1 row)` 꼬리)이다 → 헤더에서 컬럼 위치를 찾는다.
    BASE_ROWS=$(printf '%s' "$BASE_LOG" | tr '\t' '\n')
    T_BASELINE_OBS=$(printf '%s\n' "$BASE_ROWS" | awk -F'|' -v want=t_baseline_db '
      ci==0 { for (i=1; i<=NF; i++) if ($i==want) ci=i; next }
      ci>0 && NF>=ci { print $ci; exit }')
    BASE_ROW_COUNT_OBS=$(printf '%s\n' "$BASE_ROWS" | awk -F'|' -v want=row_count '
      ci==0 { for (i=1; i<=NF; i++) if ($i==want) ci=i; next }
      ci>0 && NF>=ci { print $ci; exit }')
    BASE_CLICKS_SUM_OBS=$(printf '%s\n' "$BASE_ROWS" | awk -F'|' -v want=clicks_sum '
      ci==0 { for (i=1; i<=NF; i++) if ($i==want) ci=i; next }
      ci>0 && NF>=ci { print $ci; exit }')
    BASE_MAX_CREATED_OBS=$(printf '%s\n' "$BASE_ROWS" | awk -F'|' -v want=max_created_at '
      ci==0 { for (i=1; i<=NF; i++) if ($i==want) ci=i; next }
      ci>0 && NF>=ci { print $ci; exit }')
    A_EXPECTED_OBS=$(printf '%s\n' "$BASE_ROWS" | awk -F'|' -v want=sentinel_a_expected '
      ci==0 { for (i=1; i<=NF; i++) if ($i==want) ci=i; next }
      ci>0 && NF>=ci { print $ci; exit }')
    A_HITS_OBS=$(printf '%s\n' "$BASE_ROWS" | awk -F'|' -v want=sentinel_a_hits '
      ci==0 { for (i=1; i<=NF; i++) if ($i==want) ci=i; next }
      ci>0 && NF>=ci { print $ci; exit }')
    # **형태까지 검증한다**(§4.1·§5와 같은 관용구). `t_baseline_db`는 SQL의 1번 컬럼이라 값 행이
    # 아직 플러시되지 않아 `(1 row)` 꼬리가 다음 줄로 걸리면 그것이 그대로 잡힐 수 있다.
    BASE_PARSE_OK=1
    if [ "$BASE_LOG_RC" -ne 0 ]; then
      BASE_PARSE_OK=0; echo "STOP: 로그 조회 실패 rc=$BASE_LOG_RC"
    fi
    printf %s "$T_BASELINE_OBS" \
      | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$' \
      || { BASE_PARSE_OK=0; echo "STOP: t_baseline_db가 RFC3339가 아니다: '$T_BASELINE_OBS'"; }
    printf %s "$BASE_MAX_CREATED_OBS" \
      | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$' \
      || { BASE_PARSE_OK=0; echo "STOP: max_created_at이 RFC3339가 아니다: '$BASE_MAX_CREATED_OBS'"; }
    case "$BASE_ROW_COUNT_OBS" in
      ""|*[!0-9]*) BASE_PARSE_OK=0; echo "STOP: baseline row_count가 비음수 정수가 아니다: '$BASE_ROW_COUNT_OBS'" ;;
    esac
    case "$BASE_CLICKS_SUM_OBS" in
      ""|*[!0-9]*) BASE_PARSE_OK=0; echo "STOP: baseline clicks_sum이 비음수 정수가 아니다: '$BASE_CLICKS_SUM_OBS'" ;;
    esac
    case "$A_EXPECTED_COUNT:$A_EXPECTED_OBS" in
      1:1|2:2|3:3) ;;
      *) BASE_PARSE_OK=0; echo "STOP: 원장/SQL sentinel-A 기대 개수가 일치하는 1~3이 아니다: ledger='$A_EXPECTED_COUNT' sql='$A_EXPECTED_OBS'" ;;
    esac
    case "$A_HITS_OBS" in
      ""|*[!0-9]*) BASE_PARSE_OK=0; echo "STOP: sentinel_a_hits가 정수가 아니다: '$A_HITS_OBS'" ;;
    esac
    if [ "$A_HITS_OBS" != "$A_EXPECTED_COUNT" ]; then
      BASE_PARSE_OK=0
      echo "STOP: sentinel-A 전건이 baseline에 존재하지 않는다(expected=$A_EXPECTED_COUNT hits=$A_HITS_OBS)"
    fi
    BASELINE_CAPTURE_OK=0
    TDA_FIRST_READ=0
    if [ "$BASE_PARSE_OK" = 1 ]; then
      case "$TDA_RESULT_READ_STATE" in
        not-read)
          if [ "$RESTORE_CALL_STATE" = not-started ] \
             && [ -z "$T_BASELINE" ] && [ -z "$BASELINE_ROW_COUNT" ] \
             && [ -z "$BASELINE_CLICKS_SUM" ] && [ -z "$BASELINE_MAX_CREATED_AT" ] \
             && [ -z "$BASELINE_A_HITS" ] && [ -z "$BASELINE_CAPTURED_AT" ] \
             && [ -z "$BASELINE_SOURCE_TASK_ARN" ]; then
            # 새 셸이어도 이 taskArn을 처음 읽는 순간이면 유효한 최초 캡처다.
            BASELINE_CAPTURED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
            if printf %s "$BASELINE_CAPTURED_AT" \
                 | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$'; then
              BASELINE_CAPTURE_OK=1
              TDA_FIRST_READ=1
            fi
          else
            echo "STOP: not-read인데 baseline tuple 일부가 있거나 복원 요청 뒤다 — 현재 값으로 덮어쓰지 않는다"
          fi
          ;;
        read)
          if printf %s "$BASELINE_CAPTURED_AT" \
               | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$' \
             && [ "$BASELINE_SOURCE_TASK_ARN" = "$TDA_TASK_ARN" ] \
             && [ "$T_BASELINE" = "$T_BASELINE_OBS" ] \
             && [ "$BASELINE_ROW_COUNT" = "$BASE_ROW_COUNT_OBS" ] \
             && [ "$BASELINE_CLICKS_SUM" = "$BASE_CLICKS_SUM_OBS" ] \
             && [ "$BASELINE_MAX_CREATED_AT" = "$BASE_MAX_CREATED_OBS" ] \
             && [ "$BASELINE_A_HITS" = "$A_HITS_OBS" ]; then
            BASELINE_CAPTURE_OK=1
          else
            echo "STOP: read 상태의 baseline tuple이 유실됐거나 현재 task 로그와 다르다 — 재생성하지 않는다"
          fi
          ;;
        *)
          echo "STOP: TDA_RESULT_READ_STATE가 unknown/미기입 — 최초 판독 여부를 추정하지 않는다"
          ;;
      esac
    fi
    [ "$BASELINE_CAPTURE_OK" = 1 ] || BASE_PARSE_OK=0
    if [ "$BASE_PARSE_OK" = 1 ]; then
      # **채택을 여기서 수행한다.** 산문으로 "1일 때만 채택하라"고 두면 게이트가 아니다 —
      # 실패 뒤에도 stale·수기 T_BASELINE으로 §4가 진행할 수 있었던 것이 그 구멍이었다.
      T_BASELINE="$T_BASELINE_OBS"
      BASELINE_ROW_COUNT="$BASE_ROW_COUNT_OBS"
      BASELINE_CLICKS_SUM="$BASE_CLICKS_SUM_OBS"
      BASELINE_MAX_CREATED_AT="$BASE_MAX_CREATED_OBS"
      BASELINE_A_HITS="$A_HITS_OBS"
      BASELINE_SOURCE_TASK_ARN="$TDA_TASK_ARN"
      if [ "$TDA_FIRST_READ" = 1 ]; then
        TDA_RESULT_READ_STATE=read
      fi
      BASELINE_RESULT_OK=1
    fi
  fi
fi
echo "BASELINE_RESULT_OK=$BASELINE_RESULT_OK T_BASELINE=$T_BASELINE row_count=$BASELINE_ROW_COUNT clicks_sum=$BASELINE_CLICKS_SUM"
echo "max_created_at=$BASELINE_MAX_CREATED_AT A_expected=$A_EXPECTED_COUNT A_hits=$BASELINE_A_HITS captured_at=$BASELINE_CAPTURED_AT source_task=$BASELINE_SOURCE_TASK_ARN tda_read=$TDA_RESULT_READ_STATE"
# ↑ 신규 복원 전 0이면 §4로 가지 않는다. §4.1·§4.2·sentinel-B 생성·§5 호출은 이 live 플래그를 요구한다.
```

**`BASELINE_RESULT_OK=1`이면 위 블록이 baseline tuple 전체를 직접 채택한다**(사람이 값별 성공 여부를 추정하지 않는다). `T_BASELINE`·`row_count`·`clicks_sum`·`max_created_at`·A expected/hits·`BASELINE_CAPTURED_AT`·source taskArn을 한 묶음으로 원장에 적는다. 복원 후 TD-A 조회 보관이 끝나도 §4.3이 이 tuple의 값·출처·시간 관계를 모두 확인해 증거를 재구성하며, 집계값 하나라도 없으면 (b)는 미완이다. `url`은 출력하지 않는다. 위 파싱은 §3.2 TD-A의 `psql -A -F'|'` 출력(헤더 1줄 + 값 1줄)을 전제하므로, **`command`의 출력 형식을 바꾸면 각 `awk`와 형태 검사도 함께 바꾼다.**

> 단일 SQL 한 방으로 뽑는 이유: 집계를 여러 `SELECT`로 나누면 그 사이의 쓰기 때문에 **정상 복원이 (b) FAIL로 뒤집히는 거짓 실패**가 가능하다. `clock_timestamp()`는 문 실행 중 시각이라 집계 스냅샷보다 **같거나 살짝 뒤**이며, 그 방향은 (i) 마진 판정을 더 엄격하게 만들 뿐이라 안전하다.

---

## 4. (c) 복원 지점 확보 + sentinel-B

### 4.1 `LatestRestorableTime` 폴링 (`T0` 전 · 상한 30분)

성공 조건은 **관측된 `LatestRestorableTime` > `T_baseline` + 60초**이고, 상한 30분은 루프가 직접 강제한다(설명이 아니라 코드로 — 무기한 대기 금지).

```bash
# T_BASELINE 은 **§3.4가 코드로 채택한다.** 여기서 손으로 넣지 않는다.
# **원장 값을 덮지 않는다**(§11 `SNAPSHOT_ID`와 같은 이유) — 재진입에서 §3.4를 다시 돌려 재도출한
# 값을 이 줄이 빈 문자열로 지우면, 뒤의 §4.2·§4.3·§5가 전부 빈 값 위에서 판정하게 된다.
T_BASELINE="${T_BASELINE:-}"
LRT_OK=0

# 이 폴링은 **신규 복원 요청 전용**이다. 형식만 맞는 stale·수기 T_BASELINE으로 시작하지 않도록
# 현재 세션의 §3.4 성공을 요구한다. 이미 복원을 요청한 재진입은 이 절을 다시 폴링하지 않고,
# §5 reconciliation으로 RESTORE_OK를 먼저 재도출한 뒤 §4.3의 읽기 전용 경계 블록으로 돌아온다.
if [ "${BASELINE_RESULT_OK:-0}" != 1 ]; then
  echo "STOP: BASELINE_RESULT_OK=${BASELINE_RESULT_OK:-미설정} — §3.4를 먼저 통과해야 한다"
  echo "      (복원 전이면 §3.4 재도출, 복원 후 재진입이면 §5 → §4.3 읽기 전용 경로)"
else
  # RFC3339 aware datetime으로 파싱한다. 형식·오프셋이 틀리면 빈 값이 되어 폴링을 막는다.
  BASELINE_EPOCH=$(python3 -c 'import datetime as d,re,sys
s=sys.argv[1]
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s)
x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
assert x.utcoffset() is not None
print(int(x.timestamp()))' "$T_BASELINE" 2>/dev/null)

  case "$BASELINE_EPOCH" in ""|*[!0-9]*)
    echo "STOP: T_BASELINE 치환/형식 오류 — 폴링을 시작하지 않는다(값: '$T_BASELINE')"
    ;;
  *)
    TARGET_EPOCH=$((BASELINE_EPOCH + 60))
    DEADLINE=$(( $(date -u +%s) + 1800 ))            # 30분 상한
    while :; do
      LRT=$(aws rds describe-db-instances --db-instance-identifier "$SRC_DB" \
            --query 'DBInstances[0].LatestRestorableTime' --output text)
      LRT_EPOCH=$(python3 -c 'import datetime as d,re,sys
s=sys.argv[1]
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s)
x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
assert x.utcoffset() is not None
print(int(x.timestamp()))' "$LRT" 2>/dev/null)
      echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) LatestRestorableTime=$LRT target_epoch>$TARGET_EPOCH"
      case "$LRT_EPOCH" in
        ""|*[!0-9]*) ;;
        *)
          if [ "$LRT_EPOCH" -gt "$TARGET_EPOCH" ]; then
            LRT_OK=1
            echo "OK: 복원 지점 확보 가능"
            break
          fi
          ;;
      esac
      if [ "$(date -u +%s)" -ge "$DEADLINE" ]; then echo "STOP: 30분 상한 초과"; break; fi
      sleep 60
    done
    ;;
  esac
fi
echo "LRT_OK=$LRT_OK"    # ← 1이 아니면 §4.2로 진행하지 않는다(중단 → §10 정리 게이트)
```

- **`LRT_OK=1`일 때만** §4.2로. **그 밖(상한 초과·형식 오류·관측 실패)** → `T_baseline`·최종 관측 `LatestRestorableTime`·경과 시간을 기록하고 **중단 → §10 정리 게이트**. 이 시점은 T0 전이라 잔존물이 sentinel-A뿐이다.
- 상한 초과는 단순 실패가 아니라 **복구 지점 지연의 하한을 알려주는 DR 발견**이다(로그 업로드 주기 5분 대비 이상 지연). 실패한 드릴에서도 ADR에 쓸 숫자가 나오도록 위 3개 값을 반드시 남긴다.

### 4.2 `restore point` 확정

**`T_baseline + 60초 ≤ restore point < 현재 LatestRestorableTime`** 을 만족하는 값(예: 관측값에서 수십 초 뺀 시각)으로 정한다.

- **하한 60초**는 DB 시계와 RDS 제어면 시계가 다르기 때문이다(교차 시계 (i)). 이 마진이 없으면 DB 시계가 앞서 있을 때 sentinel-A 누락·`count(*) < baseline`이 나와 **정상 복원을 (b) FAIL로 오판**한다.
- **상한은 관측값 그 자체가 아니다.** 제약이 _"Must be **before** the latest restorable time"_ 이라 폴링 직후 관측값을 그대로 넣으면 요청 시점에도 같은 값이어서 **경계에서 `InvalidParameterValue`로 튕길 수 있다** — 그것도 baseline을 이미 끝낸, 가장 되돌리기 번거로운 지점에서.

설명만 읽고 통과시키지 않는다. 아래 값은 빈 문자열에서 시작하며, 실제 `RESTORE_POINT`를 엄격한 RFC3339로 파싱해 하한·상한을 모두 확인한다.

```bash
# **원장 값을 덮지 않는다.** 재진입에서 이 줄이 빈 문자열로 초기화하면 §5 공통 reconciliation의
# source 대조(`RESTORE_REQUEST_SOURCE = RESTORE_POINT`)가 실패해 `SOURCE_LEDGER_OK=0`이 되고,
# **이미 과금 중인 PITR 복원본이 `RESTORE_OK`를 다시 얻지 못해** §6 이후로 진행할 수 없다(§11과 같은 실패).
RESTORE_POINT="${RESTORE_POINT:-}"   # ← 신규 실행일 때만 확정한 실제 복원 지점을 넣는다(따옴표 안에)
RESTORE_POINT_OK=0
RESTORE_POINT_EPOCH=$(python3 -c 'import datetime as d,re,sys
s=sys.argv[1]
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s)
x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
assert x.utcoffset() is not None
print(int(x.timestamp()))' "$RESTORE_POINT" 2>/dev/null)

case "$BASELINE_EPOCH:$RESTORE_POINT_EPOCH:$LRT_EPOCH" in
  ""|:*|*:|*::*|*[!0-9:]*)
    echo "STOP: baseline/restore point/LRT 중 하나가 RFC3339로 파싱되지 않았다"
    ;;
  *)
    if [ "${BASELINE_RESULT_OK:-0}" = 1 ] \
       && [ "$RESTORE_POINT_EPOCH" -ge $((BASELINE_EPOCH + 60)) ] \
       && [ "$RESTORE_POINT_EPOCH" -lt "$LRT_EPOCH" ]; then
      RESTORE_POINT_OK=1
    else
      echo "STOP: baseline 성공(BASELINE_RESULT_OK=${BASELINE_RESULT_OK:-미설정}) 또는"
      echo "      T_baseline+60초 ≤ restore point < LRT 경계를 만족하지 않는다"
    fi
    ;;
esac
echo "RESTORE_POINT_OK=$RESTORE_POINT_OK point=$RESTORE_POINT baseline=$T_BASELINE lrt=$LRT"

# 삭제할 수 없는 sentinel-B를 만들 수 있는 **일회성 쓰기 허용값**.
# PITR과 스냅샷이 각자 생산하고 §4.3 생성 블록만 소비한다. 읽기 전용 경계 플래그와 섞지 않는다.
SENTINEL_B_CREATE_ALLOWED=0
SENTINEL_B_CREATE_MODE=""
B_CREATE_SLOT_OK=0
case "$SENTINEL_B_CREATE_STATE:$SENTINEL_B_TRY" in
  not-used:1|retry-authorized:2) B_CREATE_SLOT_OK=1 ;;
esac
if [ "${BASELINE_RESULT_OK:-0}" = 1 ] && [ "$LRT_OK" = 1 ] \
   && [ "$RESTORE_POINT_OK" = 1 ] && [ "$RESTORE_CALL_STATE" = not-started ] \
   && [ "$B_CREATE_SLOT_OK" = 1 ]; then
  SENTINEL_B_CREATE_ALLOWED=1
  SENTINEL_B_CREATE_MODE=pitr
fi
echo "SENTINEL_B_CREATE_ALLOWED=$SENTINEL_B_CREATE_ALLOWED mode=$SENTINEL_B_CREATE_MODE state=$SENTINEL_B_CREATE_STATE try=$SENTINEL_B_TRY"
```

### 4.3 sentinel-B — 생성(1회성)과 경계 재도출(읽기 전용)을 **분리한다**

**생성 경로와 읽기 전용 판정을 반드시 나누고, 생성도 예약과 전송으로 한 번 더 나눈다.** §2.1은 플래그를 "원장에서 붙여 넣지 말고 읽기 전용 블록으로 재도출"하라고 규정하는데, 생성과 판정이 한 블록에 있으면 새 터미널에서 `B_MARGIN_OK`를 되살리려다 **`curl POST`가 다시 실행돼 삭제 API가 없는 영구 행이 하나 더 생긴다** — §2.2가 사전 승인한 상한(sentinel 총 5건 / B 2건)을 넘기는 경로다.

**(1) 생성 — 시도별 1회만.** 재진입에서는 실행하지 않는다. 첫 시도는 `not-used:1`, 두 번째는 아래 경계 블록이 **유효 응답의 마진 미달을 확인한 경우에만** `retry-authorized:2`로 연다. `dispatched`·`accepted`·`ambiguous`를 생산 블록 재실행으로 되돌리지 않는다.

먼저 **(1-a) 전송 예약**을 실행한다. 60초 대기 뒤 허용값을 소비하고 영속 상태를 `dispatched`로 바꾼다. 출력된 state/try/mode를 원장에 적은 다음에만 (1-b)를 실행한다. 예약 뒤 터미널이 끊기면 실제 전송 여부를 증명할 수 없으므로 그 시도는 보수적으로 소비되고, 새 터미널에서 `SENTINEL_B_SEND_ALLOWED=1`을 손으로 만들지 않는다.

```bash
SENTINEL_B_SEND_ALLOWED=0
B_CREATE_PREV_STATE="$SENTINEL_B_CREATE_STATE"
if [ "${SHELL_OK:-0}" = 1 ] && [ "${DRILL_MUTATION_OK:-0}" = 1 ] \
   && lock_live \
   && [ "${SENTINEL_B_CREATE_ALLOWED:-0}" = 1 ] \
   && { [ "$SENTINEL_B_CREATE_MODE" = pitr ] || [ "$SENTINEL_B_CREATE_MODE" = snapshot ]; } \
   && [ "$RESTORE_CALL_STATE" = not-started ]; then
  sleep 60      # 복구 지점 확정 후 60초 이상 지난 뒤
  SENTINEL_B_CREATE_ALLOWED=0
  SENTINEL_B_CREATE_STATE=dispatched
  SENTINEL_B_SEND_ALLOWED=1
  echo "SENTINEL_B_CREATE_STATE=$SENTINEL_B_CREATE_STATE try=$SENTINEL_B_TRY mode=$SENTINEL_B_CREATE_MODE allowed=$SENTINEL_B_CREATE_ALLOWED ← POST 전에 원장 기입"
else
  echo "STOP: sentinel-B 생성 안 함 — shell=${SHELL_OK:-미설정} mutation=${DRILL_MUTATION_OK:-미설정} allowed=${SENTINEL_B_CREATE_ALLOWED:-미설정} mode=${SENTINEL_B_CREATE_MODE:-미설정} restore_state=$RESTORE_CALL_STATE 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)"
fi
```

다음 **(1-b) 전송**은 같은 셸의 일회성 `SENTINEL_B_SEND_ALLOWED`만 소비한다. 플래그를 POST 전에 0으로 닫으므로 블록을 두 번 붙여 넣어도 두 번째 호출은 없다. HTTP `429`만 명시적 미생성으로 보고 같은 시도를 다시 예약할 수 있다. 그 밖의 응답 유실·timeout·파싱 실패는 `ambiguous`로 시도를 소비하며 재전송하지 않는다.

```bash
if [ "${SHELL_OK:-0}" = 1 ] && [ "${DRILL_MUTATION_OK:-0}" = 1 ] \
   && lock_live \
   && [ "${SENTINEL_B_SEND_ALLOWED:-0}" = 1 ] \
   && [ "$SENTINEL_B_CREATE_STATE" = dispatched ] \
   && [ "$RESTORE_CALL_STATE" = not-started ]; then
  SENTINEL_B_SEND_ALLOWED=0
  B_HTTP_OUT=$(curl -sS --max-time 10 -w '\n%{http_code}' \
    -X POST https://lpulse.live/api/links \
    -H 'content-type: application/json' -d '{"url":"https://example.com/"}' 2>&1)
  B_CURL_RC=$?
  B_HTTP_STATUS=$(printf '%s\n' "$B_HTTP_OUT" | tail -n 1)
  B_RESPONSE_BODY=$(printf '%s\n' "$B_HTTP_OUT" | sed '$d')
  printf '%s\n' "$B_RESPONSE_BODY"

  B_RESPONSE_FIELDS=""
  case "$B_HTTP_STATUS" in
    2[0-9][0-9])
      if [ "$B_CURL_RC" -eq 0 ]; then
        B_RESPONSE_FIELDS=$(python3 -c 'import json,re,sys
d=json.loads(sys.argv[1])
code=d.get("code","")
created=d.get("created_at","")
assert re.fullmatch(r"[0-9A-Za-z]+",code)   # 앱 알파벳과 동일(app/internal/links/code.go base62Alphabet). §8.2 게이트와 같은 집합.
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",created)
print(code)
print(created)' "$B_RESPONSE_BODY" 2>/dev/null)
      fi
      ;;
  esac
  B_CODE_OBS=$(printf '%s\n' "$B_RESPONSE_FIELDS" | sed -n '1p')
  B_CREATED_OBS=$(printf '%s\n' "$B_RESPONSE_FIELDS" | sed -n '2p')

  if [ -n "$B_CODE_OBS" ] && [ -n "$B_CREATED_OBS" ]; then
    B_CODE="$B_CODE_OBS"
    B_CREATED_AT="$B_CREATED_OBS"
    SENTINEL_B_CREATE_STATE=accepted
  elif [ "$B_CURL_RC" -eq 0 ] && [ "$B_HTTP_STATUS" = 429 ]; then
    SENTINEL_B_CREATE_STATE="$B_CREATE_PREV_STATE"
    echo "429: 행이 생성되지 않았음이 명시됐다 — 잠시 후 같은 시도를 생산 블록부터 다시 실행"
  else
    SENTINEL_B_CREATE_STATE=ambiguous
    echo "STOP: sentinel-B 응답 불명/파싱 실패 — 이 시도는 소비하며 재전송하지 않는다"
  fi
  echo "SENTINEL_B_CREATE_STATE=$SENTINEL_B_CREATE_STATE try=$SENTINEL_B_TRY code=$B_CODE created_at=$B_CREATED_AT ← 원장 기입"
else
  echo "STOP: sentinel-B POST 안 함 — mutation=${DRILL_MUTATION_OK:-미설정} send_allowed=${SENTINEL_B_SEND_ALLOWED:-미설정} state=$SENTINEL_B_CREATE_STATE restore_state=$RESTORE_CALL_STATE 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)"
fi
```

**(2) baseline 증거와 PITR 경계 재도출 — 읽기 전용.** 새 터미널·재진입에서는 **생성 블록을 건너뛰고 이 블록만** 실행한다(AWS·앱 호출 0회, 순수 계산). 복원 후 재진입이면 먼저 §5의 공통 reconciliation으로 `RESTORE_OK=1`을 재도출한 뒤 돌아온다. `BASELINE_RESULT_OK`는 영구 쓰기·최초 복원 호출 전의 live 플래그다. `BASELINE_LEDGER_OK`는 원장의 시각·집계·A expected/hits·source task와 최초 호출 전 봉인한 request tuple을 함께 검증하며, `BASELINE_EVIDENCE_OK`는 이미 만들어진 복원본의 데이터 판정에만 쓴다. `ambiguous + authorized` 재시도는 `BASELINE_EVIDENCE_OK`가 아니라 §5의 별도 retry 가드가 `BASELINE_LEDGER_OK`와 동일 source·경계를 함께 요구한다.

```bash
# 아래 두 값은 (1)에서 채택해 원장에 적어 둔 값을 붙여 넣는다. 여기서 새로 만들지 않는다.
# §11 `SNAPSHOT_ID`·`A_CREATED_MAX`와 같은 이유로 **원장 값을 덮지 않는다** — 재진입에서 빈 문자열로
# 지우면 아래 경계 판정이 STOP으로 떨어져, 정상적으로 만든 sentinel-B를 다시 만들고 싶어진다(상한 2건 소비).
B_CODE="${B_CODE:-}"
B_CREATED_AT="${B_CREATED_AT:-}"
T_BASELINE="${T_BASELINE:-}"
BASELINE_CAPTURED_AT="${BASELINE_CAPTURED_AT:-}"
RESTORE_POINT="${RESTORE_POINT:-}"

BASELINE_LEDGER_OK=0
BASELINE_EVIDENCE_OK=0
BASELINE_VALUE_EPOCH=$(python3 -c 'import datetime as d,re,sys
s=sys.argv[1]
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s)
x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
assert x.utcoffset() is not None
print(int(x.timestamp()))' "$T_BASELINE" 2>/dev/null)
BASELINE_CAPTURE_EPOCH=$(python3 -c 'import datetime as d,re,sys
s=sys.argv[1]
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z",s)
x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
print(int(x.timestamp()))' "$BASELINE_CAPTURED_AT" 2>/dev/null)
BASELINE_MAX_CREATED_EPOCH=$(python3 -c 'import datetime as d,re,sys
s=sys.argv[1]
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s)
x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
assert x.utcoffset() is not None
print(int(x.timestamp()))' "$BASELINE_MAX_CREATED_AT" 2>/dev/null)
RESTORE_T0_LEDGER_EPOCH=$(python3 -c 'import datetime as d,re,sys
s=sys.argv[1]
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s)
x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
assert x.utcoffset() is not None
print(int(x.timestamp()))' "$RESTORE_T0" 2>/dev/null)

BASELINE_TUPLE_SHAPE_OK=1
case "$BASELINE_ROW_COUNT" in ""|*[!0-9]*) BASELINE_TUPLE_SHAPE_OK=0 ;; esac
case "$BASELINE_CLICKS_SUM" in ""|*[!0-9]*) BASELINE_TUPLE_SHAPE_OK=0 ;; esac
case "$A_EXPECTED_COUNT:$BASELINE_A_HITS" in 1:1|2:2|3:3) ;; *) BASELINE_TUPLE_SHAPE_OK=0 ;; esac
case "$BASELINE_VALUE_EPOCH:$BASELINE_CAPTURE_EPOCH:$BASELINE_MAX_CREATED_EPOCH:$RESTORE_T0_LEDGER_EPOCH" in
  ""|:*|*:|*::*|*[!0-9:]*) BASELINE_TUPLE_SHAPE_OK=0 ;;
esac

# DB 시계 T_baseline과 셸 시계 captured_at의 비교에는 기존 교차 시계 흡수폭 60초를 적용한다.
# captured_at과 T0는 같은 셸 시계이므로 순서를 그대로 비교한다.
if [ "$BASELINE_TUPLE_SHAPE_OK" = 1 ] \
   && [ "$TDA_RESULT_READ_STATE" = read ] \
   && [ "$BASELINE_CAPTURE_EPOCH" -ge $((BASELINE_VALUE_EPOCH - 60)) ] \
   && [ "$BASELINE_CAPTURE_EPOCH" -le "$RESTORE_T0_LEDGER_EPOCH" ] \
   && [ -n "$BASELINE_SOURCE_TASK_ARN" ] \
   && [ "${BASELINE_SOURCE_TASK_ARN%%/*}" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task" ] \
   && [ "$BASELINE_SOURCE_TASK_ARN" = "$TDA_TASK_ARN" ] \
   && [ "$RESTORE_PRECHECK_STATE" = ready ] \
   && [ "$RESTORE_PRECHECK_BASELINE_TASK_ARN" = "$BASELINE_SOURCE_TASK_ARN" ] \
   && [ -n "$RESTORE_REQUEST_SOURCE" ] \
   && [ "$RESTORE_PRECHECK_KIND" = "$RESTORE_REQUEST_KIND" ] \
   && [ "$RESTORE_PRECHECK_SOURCE" = "$RESTORE_REQUEST_SOURCE" ] \
   && { [ "$RESTORE_CALL_STATE" = accepted ] || [ "$RESTORE_CALL_STATE" = ambiguous ]; }; then
  BASELINE_LEDGER_OK=1
fi

if [ "${BASELINE_RESULT_OK:-0}" = 1 ] && [ -n "$BASELINE_VALUE_EPOCH" ]; then
  BASELINE_EVIDENCE_OK=1
elif [ "${RESTORE_OK:-0}" = 1 ] && [ "$BASELINE_LEDGER_OK" = 1 ]; then
  # 이미 만들어진 복원본을 검증하는 원장 fallback이다. 최초 호출은 절대 열지 않는다.
  BASELINE_EVIDENCE_OK=1
fi

PITR_POINT_SOURCE_OK=0
if [ "$RESTORE_CALL_STATE" = not-started ] \
   && [ "${RESTORE_POINT_OK:-0}" = 1 ] && [ "${BASELINE_RESULT_OK:-0}" = 1 ]; then
  PITR_POINT_SOURCE_OK=1
elif [ "${RESTORE_OK:-0}" = 1 ] && [ "$RESTORE_REQUEST_KIND" = pitr ] \
     && [ -n "$RESTORE_REQUEST_SOURCE" ] \
     && [ "$RESTORE_REQUEST_SOURCE" = "$RESTORE_POINT" ]; then
  PITR_POINT_SOURCE_OK=1
elif [ "$RESTORE_CALL_STATE" = ambiguous ] && [ "${RESTORE_RETRY_ALLOWED:-0}" = 1 ] \
     && [ "$RESTORE_RETRY_STATE" = authorized ] && [ "$RESTORE_REQUEST_KIND" = pitr ] \
     && [ -n "$RESTORE_REQUEST_SOURCE" ] \
     && [ "$RESTORE_REQUEST_SOURCE" = "$RESTORE_POINT" ]; then
  PITR_POINT_SOURCE_OK=1
fi

B_MARGIN_OK=0
B_MARGIN_RELATION=unknown

# 복원 호출이 이미 나갔다면(재진입·재시도 경로) 원장의 B가 **최초 호출 직전에 봉인해 둔 그 B**여야 한다.
# 경계 계산은 하한(restore point + 60초)만 보므로, 이 대조가 없으면 T0 **뒤에** 만든 다른 B를
# 원장에 섞어 "복원본에 B가 없다 = 복구 지점 준수"를 거짓으로 세울 수 있다.
# 새 시계 오차 상수를 만들지 않으려고 **정확 일치**로만 판정한다(호출 전에는 봉인이 없으니 대조도 없다).
B_SEAL_OK=1
if [ "$RESTORE_CALL_STATE" != not-started ]; then
  if [ "$RESTORE_PRECHECK_STATE" = ready ] \
     && [ -n "$RESTORE_PRECHECK_B_CODE" ] && [ -n "$RESTORE_PRECHECK_B_CREATED_AT" ] \
     && [ "$RESTORE_PRECHECK_B_CODE" = "$B_CODE" ] \
     && [ "$RESTORE_PRECHECK_B_CREATED_AT" = "$B_CREATED_AT" ]; then
    B_SEAL_OK=1
  else
    B_SEAL_OK=0
    echo "STOP: 원장의 sentinel-B가 최초 호출 직전 봉인값과 다르다 — T0 이후 B로 경계를 세우지 않는다"
    echo "      봉인 code=$RESTORE_PRECHECK_B_CODE created=$RESTORE_PRECHECK_B_CREATED_AT / 현재 code=$B_CODE created=$B_CREATED_AT"
  fi
fi

RESTORE_POINT_EPOCH=$(python3 -c 'import datetime as d,re,sys
s=sys.argv[1]
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s)
x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
assert x.utcoffset() is not None
print(int(x.timestamp()))' "$RESTORE_POINT" 2>/dev/null)
B_CREATED_EPOCH=$(python3 -c 'import datetime as d,re,sys
s=sys.argv[1]
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s)
x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
assert x.utcoffset() is not None
print(int(x.timestamp()))' "$B_CREATED_AT" 2>/dev/null)
case "$RESTORE_POINT_EPOCH:$B_CREATED_EPOCH" in
  ""|:*|*:|*::*|*[!0-9:]*)
    echo "STOP: restore point 또는 sentinel-B created_at 형식 오류"
    ;;
  *)
    if [ "$PITR_POINT_SOURCE_OK" = 1 ] && [ -n "$B_CODE" ] \
       && [ "$B_SEAL_OK" = 1 ] \
       && [ "$SENTINEL_B_CREATE_STATE" = accepted ]; then
      if [ "$B_CREATED_EPOCH" -gt $((RESTORE_POINT_EPOCH + 60)) ]; then
        B_MARGIN_OK=1
        B_MARGIN_RELATION=sufficient
      else
        B_MARGIN_RELATION=insufficient
      fi
    else
      echo "STOP: 채택한 sentinel-B가 restore point+60초보다 뒤라는 증거가 없다(b_seal_ok=$B_SEAL_OK)"
    fi
    ;;
esac
if [ "$B_MARGIN_RELATION" = insufficient ] \
   && [ "$SENTINEL_B_CREATE_STATE" = accepted ] && [ "$SENTINEL_B_TRY" = 1 ] \
   && [ "$RESTORE_CALL_STATE" = not-started ]; then
  SENTINEL_B_CREATE_STATE=retry-authorized
  SENTINEL_B_TRY=2
  echo "sentinel-B 첫 유효 응답의 마진 미달 확인 → state=$SENTINEL_B_CREATE_STATE try=$SENTINEL_B_TRY ← 원장 기입"
fi
echo "B_MARGIN_OK=$B_MARGIN_OK relation=$B_MARGIN_RELATION code=$B_CODE created_at=$B_CREATED_AT state=$SENTINEL_B_CREATE_STATE try=$SENTINEL_B_TRY"

# 이후 §8은 복원 방식과 무관한 **공통 플래그**로 판단한다(PITR은 여기서, 스냅샷은 §11에서 세운다):
#  · SENTINEL_BOUNDARY_OK  = A/B sentinel이 복구 지점을 사이에 두고 브래킷됨이 검증됨
#  · BASELINE_COMPARABLE_OK = 복구 지점이 T_baseline보다 뒤라 `count(*) ≥ baseline` 단조성 논증이 성립함
# **두 모드의 A쪽 근거 강도는 다르다**: 스냅샷(§11)은 A_CREATED_MAX를 직접 대조하지만, PITR은
# B_MARGIN_OK와 아래 BASELINE_COMPARABLE_OK를 통해 **간접** 성립한다
# — A는 T_baseline 캡처 직전에 만들어졌으므로 restore point보다 앞선다는 것이 그 사슬이다(의도된 비대칭).
SENTINEL_BOUNDARY_OK="$B_MARGIN_OK"
BASELINE_COMPARABLE_OK=0
if [ "$PITR_POINT_SOURCE_OK" = 1 ] && [ "$BASELINE_EVIDENCE_OK" = 1 ] \
   && [ -n "$BASELINE_VALUE_EPOCH" ] \
   && [ "$RESTORE_POINT_EPOCH" -ge $((BASELINE_VALUE_EPOCH + 60)) ]; then
  BASELINE_COMPARABLE_OK=1
fi
echo "BASELINE_LEDGER_OK=$BASELINE_LEDGER_OK BASELINE_EVIDENCE_OK=$BASELINE_EVIDENCE_OK SENTINEL_BOUNDARY_OK=$SENTINEL_BOUNDARY_OK BASELINE_COMPARABLE_OK=$BASELINE_COMPARABLE_OK (mode=pitr)"
```

마진 없이 붙여 만들면 시계 오차·로그 재생 경계 해상도만으로 판정이 뒤집혀 **정상 복원을 "복원 지점 위반"으로 오판**한다.

---

## 5. (d) PITR 복원 — `T0` 기록 후 즉시 호출 💸⚠️

**신규 요청에서만 `T0`를 호출 직전에 한 번 기록한다.** 생성 호출과 읽기 전용 reconciliation을 분리하며, 대상이 이미 있거나 호출 상태가 `not-started`가 아니면 새 `T0`도 새 요청도 만들지 않는다.

```bash
RESTORE_OK=0

# stale TARGET_ABSENT_OK를 재사용하지 않는다. 이 호출 **직전** 0부터 다시 도출한다.
TARGET_ABSENT_OK=0
TARGET_STATE=unknown
TARGET_OUT=$(aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" \
  --query 'DBInstances[0].DBInstanceArn' --output text 2>&1)
TARGET_RC=$?
if [ "$TARGET_RC" -eq 0 ]; then
  TARGET_STATE=present
elif printf %s "$TARGET_OUT" | grep -q "DBInstanceNotFound"; then
  TARGET_STATE=absent
  TARGET_ABSENT_OK=1
fi
echo "호출 직전 TARGET_STATE=$TARGET_STATE TARGET_ABSENT_OK=$TARGET_ABSENT_OK rc=$TARGET_RC"

# 호출 자격은 둘뿐이다: **최초**(`not-started`) 또는 아래 reconciliation이 연 **1회 재시도 창**.
# 재시도 창에서도 상태는 `ambiguous` 그대로다 — 최초 요청이 뒤늦게 접수됐을 가능성을 지우지 않기 위함이다.
# **자격은 영속 상태(`authorized`)와 일회성 플래그(`RESTORE_RETRY_ALLOWED`)를 함께 요구한다.**
# 셸 플래그만 보면, 아래 aws 호출 중 Ctrl-C로 블록이 끊긴 뒤 같은 셸에서 다시 실행할 때
# `dispatched`인데도 플래그가 1로 남아 **같은 변경 API를 두 번 전송**한다.
RESTORE_CALL_ALLOWED=0
IS_RETRY=0                  # 이번 호출이 재시도인가 — 응답 분류에 쓰는 **호출 로컬** 값(원장 아님)
if [ "$RESTORE_CALL_STATE" = not-started ]; then
  RESTORE_CALL_ALLOWED=1
elif [ "$RESTORE_CALL_STATE" = ambiguous ] && [ "$RESTORE_RETRY_ALLOWED" = 1 ] \
     && [ "$RESTORE_RETRY_STATE" = authorized ] \
     && [ "$RESTORE_REQUEST_KIND" = pitr ] \
     && [ -n "$RESTORE_REQUEST_SOURCE" ] \
     && [ "$RESTORE_REQUEST_SOURCE" = "$RESTORE_POINT" ]; then
  RESTORE_CALL_ALLOWED=1
  IS_RETRY=1
fi

# 최초 호출은 현재 세션의 live 전제를, authorized 재시도는 최초 호출 전에 봉인한 원장 tuple을 쓴다.
# 원장 fallback이 `not-started` 최초 호출을 여는 일은 없다.
INITIAL_RESTORE_GUARD_OK=0
RETRY_RESTORE_GUARD_OK=0
if [ "$IS_RETRY" = 0 ] \
   && [ "${BASELINE_RESULT_OK:-0}" = 1 ] && [ "$LRT_OK" = 1 ] \
   && [ "$RESTORE_POINT_OK" = 1 ] && [ "$B_MARGIN_OK" = 1 ] \
   && [ "$SENTINEL_B_CREATE_STATE" = accepted ]; then
  INITIAL_RESTORE_GUARD_OK=1
elif [ "$IS_RETRY" = 1 ] \
     && [ "${BASELINE_LEDGER_OK:-0}" = 1 ] && [ "${PITR_POINT_SOURCE_OK:-0}" = 1 ] \
     && [ "$B_MARGIN_OK" = 1 ] && [ "$SENTINEL_B_CREATE_STATE" = accepted ] \
     && [ "$RESTORE_PRECHECK_STATE" = ready ] && [ "$RESTORE_PRECHECK_KIND" = pitr ] \
     && [ -n "$RESTORE_PRECHECK_SOURCE" ] \
     && [ "$RESTORE_PRECHECK_SOURCE" = "$RESTORE_REQUEST_SOURCE" ] \
     && [ "$RESTORE_PRECHECK_BASELINE_TASK_ARN" = "$BASELINE_SOURCE_TASK_ARN" ]; then
  RETRY_RESTORE_GUARD_OK=1
fi

# 안전 가드: 인자가 하나라도 비거나 대상 부재·모드별 전제가 확정되지 않으면 복원을 호출하지 않는다.
if [ "$RESTORE_CALL_ALLOWED" = 1 ] \
   && { [ "$INITIAL_RESTORE_GUARD_OK" = 1 ] || [ "$RETRY_RESTORE_GUARD_OK" = 1 ]; } \
   && [ "${DRILL_MUTATION_OK:-0}" = 1 ] \
   && lock_live \
   && [ "$VALUES_OK" = 1 ] && [ "$TARGET_ABSENT_OK" = 1 ] \
   && [ -n "$RESTORE_POINT" ] && [ -n "$SUBNET_GROUP" ] \
   && [ -n "$DATA_SG" ] && [ -n "$PROD_PG" ] \
   && [ "$DRILL_DB" != "$SRC_DB" ] && [ "$DRILL_DB" = "linkpulse-restore-drill" ]; then

  if [ "$IS_RETRY" = 1 ]; then
    # **재시도는 최초 T0·source·상태를 덮지 않는다.** 최초 요청의 미접수를 증명할 수 없으므로
    # 측정용 T0는 최초 값을 유지한다(보수적). 재시도 시각은 원장에 병기만 한다.
    # **재시도 소진은 창 개방이 아니라 여기서 기록한다.** reconciliation에서 소진하면, 창만 열고
    # 재호출 전에 세션이 끊겼을 때 실제로는 한 번도 보내지 않았는데 자격이 영구히 사라진다.
    # 반대로 이 줄 **뒤에** 끊기면 소진으로 남긴다 — 전송 여부를 증명할 수 없기 때문이다(보수적).
    # **영속 상태와 일회성 플래그를 여기서 함께 닫는다.** 둘이 갈라지면 호출 중 인터럽트된 셸에
    # `dispatched + allowed=1`이 남아 같은 API를 두 번 보낼 수 있다(이 블록은 붙여넣기로 재실행된다).
    RESTORE_RETRY_STATE=dispatched
    RESTORE_RETRY_ALLOWED=0
    echo "RESTORE_RETRY_STATE=$RESTORE_RETRY_STATE (allowed=$RESTORE_RETRY_ALLOWED)   ← **호출 전에** 원장에 적는다"
    echo "재시도 호출(1회): 최초 T0=$RESTORE_T0 유지 / 재시도 시각=$(date -u +%Y-%m-%dT%H:%M:%SZ) ← 원장 병기"
  else
    RESTORE_PRECHECK_STATE=ready
    RESTORE_PRECHECK_KIND=pitr
    RESTORE_PRECHECK_SOURCE="$RESTORE_POINT"
    RESTORE_PRECHECK_BASELINE_TASK_ARN="$BASELINE_SOURCE_TASK_ARN"
    RESTORE_PRECHECK_B_CODE="$B_CODE"
    RESTORE_PRECHECK_B_CREATED_AT="$B_CREATED_AT"
    echo "RESTORE_PRECHECK_STATE=$RESTORE_PRECHECK_STATE kind=$RESTORE_PRECHECK_KIND source=$RESTORE_PRECHECK_SOURCE baseline_task=$RESTORE_PRECHECK_BASELINE_TASK_ARN b_code=$RESTORE_PRECHECK_B_CODE b_created=$RESTORE_PRECHECK_B_CREATED_AT ← 최초 호출 전에 원장 기입"
    RESTORE_T0=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    RESTORE_REQUEST_KIND=pitr
    RESTORE_REQUEST_SOURCE="$RESTORE_POINT"
    RESTORE_EXPECTED_ARN=""
    RESTORE_INSTANCE_CREATED=""
    RESTORE_CALL_STATE=ambiguous
    # 아래 한 줄을 **요청 전에 원장에 기입**한다. 터미널 종료 뒤에도 최초 T0/source를 보존한다.
    echo "RESTORE_CALL_STATE=$RESTORE_CALL_STATE RESTORE_T0=$RESTORE_T0 kind=$RESTORE_REQUEST_KIND source=$RESTORE_REQUEST_SOURCE"
  fi

  RESTORE_OUT=$(aws rds restore-db-instance-to-point-in-time \
    --source-db-instance-identifier "$SRC_DB" \
    --target-db-instance-identifier "$DRILL_DB" \
    --restore-time "$RESTORE_POINT" \
    --db-subnet-group-name "$SUBNET_GROUP" \
    --vpc-security-group-ids "$DATA_SG" \
    --db-parameter-group-name "$PROD_PG" \
    --no-publicly-accessible --no-multi-az --no-deletion-protection \
    --backup-retention-period 0 \
    --tags Key=purpose,Value=rds-restore-drill Key=drill-token,Value="$TOKEN" \
    --query 'DBInstance.[DBInstanceIdentifier,DBInstanceArn,DBInstanceStatus,InstanceCreateTime]' \
    --output text 2>&1)
  RESTORE_RC=$?
  echo "복원 호출 rc=$RESTORE_RC $RESTORE_OUT"
  if [ "$RESTORE_RC" -eq 0 ]; then
    RESPONSE_ID=$(printf %s "$RESTORE_OUT" | awk '{print $1}')
    RESPONSE_ARN=$(printf %s "$RESTORE_OUT" | awk '{print $2}')
    RESPONSE_CREATED=$(printf %s "$RESTORE_OUT" | awk '{print $4}')
    if [ "$RESPONSE_ID" = "$DRILL_DB" ] && [ -n "$RESPONSE_ARN" ] \
       && [ -n "$RESPONSE_CREATED" ] && [ "$RESPONSE_CREATED" != "None" ]; then
      RESTORE_CALL_STATE=accepted
      RESTORE_EXPECTED_ARN="$RESPONSE_ARN"
      RESTORE_INSTANCE_CREATED="$RESPONSE_CREATED"
    else
      RESTORE_CALL_STATE=ambiguous
    fi
  elif printf %s "$RESTORE_OUT" | grep -Eqi \
    "RequestTimeout|Read timeout|Connect timeout|Connection reset|Could not connect to the endpoint|EndpointConnectionError|TLS handshake timeout"; then
    RESTORE_CALL_STATE=ambiguous
  elif printf %s "$RESTORE_OUT" | grep -Eq \
    "DBInstanceAlreadyExists|InvalidParameterValue|InvalidParameterCombination|InvalidDBInstanceState|DBInstanceNotFound|DBSubnetGroupNotFound|DBParameterGroupNotFound|InstanceQuotaExceeded|KMSKeyNotAccessible|AccessDenied|UnauthorizedOperation|OptInRequired|ValidationError"; then
    # **화이트리스트**: AWS가 의미를 명시해 거부한 것만 rejected 다.
    RESTORE_CALL_STATE=rejected
  else
    # 알 수 없는 오류(미상 5xx·처음 보는 문자열)는 **거부의 증거가 아니다** → 보수적으로 ambiguous.
    # 제어면 결과를 거부로 단정하지 않고, 공통 reconciliation이 이번 토큰의 후보를 확인하게 한다.
    RESTORE_CALL_STATE=ambiguous
    echo "주의: 분류되지 않은 오류 → ambiguous 로 보수 처리(제어면 결과 재확인)"
  fi

  # **재시도 호출은 최초 ambiguous 계보를 덮지 않는다.** 응답만 유실된 최초 요청이 뒤늦게 접수되면
  # 재호출은 DBInstanceAlreadyExists 를 받는데, 그것은 "우리 첫 요청이 실제로 만들었다"는 **증거**다.
  # 재시도에서는 accepted 로의 상향만 허용하고, 그 외는 최초 요청의 제어면 결과가 여전히 불명확하므로
  # ambiguous 를 유지한 뒤 공통 reconciliation에서 태그·ARN·생성 시각을 확인한다.
  # 판정에는 **호출 로컬** `IS_RETRY`를 쓴다 — `RESTORE_RETRY_ALLOWED`는 호출 전에 이미 닫혔다.
  if [ "$IS_RETRY" = 1 ] && [ "$RESTORE_CALL_STATE" != accepted ]; then
    echo "재시도 결과 '$RESTORE_CALL_STATE' → 최초 ambiguous 계보 보존을 위해 ambiguous 로 되돌린다"
    RESTORE_CALL_STATE=ambiguous
  fi
  echo "RESTORE_CALL_STATE=$RESTORE_CALL_STATE RESTORE_T0=$RESTORE_T0 source=$RESTORE_REQUEST_SOURCE expected_arn=$RESTORE_EXPECTED_ARN created=$RESTORE_INSTANCE_CREATED"
else
  # **막은 값을 전부 찍는다**(§8.2와 같은 규율) — 위 `if`가 보는 조건은 12개인데 넷만 찍으면
  # 찍힌 값이 전부 정상인 채로 막히는 일이 생긴다. 특히 `DRILL_MUTATION_OK`의 여섯 구성요소 중
  # 락 TTL·AWS 주체·원장 소유권 셋은 **긴 대기나 터미널 교체만으로 조용히 0이 된다**(§2.4).
  echo "STOP: 복원 호출 안 함 — state=$RESTORE_CALL_STATE target=$TARGET_STATE initial_guard=$INITIAL_RESTORE_GUARD_OK retry_guard=$RETRY_RESTORE_GUARD_OK"
  echo "      자격: DRILL_MUTATION_OK=${DRILL_MUTATION_OK:-미설정}(락=${EXCLUSIVE_DRILL_LOCK_OK:-미설정} 도구=${TOOLS_OK:-미설정} 주체=${SESSION_AWS_OK:-미설정} 원장=${MUTATION_CONTEXT_OK:-미설정}) 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)"
  echo "      값: VALUES_OK=${VALUES_OK:-미설정} TARGET_ABSENT_OK=${TARGET_ABSENT_OK:-미설정} RESTORE_POINT='${RESTORE_POINT:-미설정}' SUBNET_GROUP='${SUBNET_GROUP:-미설정}' DATA_SG='${DATA_SG:-미설정}' PROD_PG='${PROD_PG:-미설정}' DRILL_DB='${DRILL_DB:-미설정}'"
fi
```

신규 호출 직후 또는 세션 재진입에서 아래 블록만 실행한다. **이 블록 자체는 생성 명령을 재전송하지 않는다.** `accepted`는 응답의 ARN·생성 시각까지 정확히 일치해야 하고, transport 또는 미분류 응답으로 결과가 불명인 `ambiguous`만 원래 `T0` 이후 생성된 이번 토큰의 인스턴스로 회수할 수 있다. 최초 호출의 명시적 `rejected`는 승인하지 않으며, 재시도의 `DBInstanceAlreadyExists`는 최초 `ambiguous` 계보를 유지한 채 태그로 대사한다.

```bash
RESTORE_OK=0
RESTORE_LEDGER_OK=0
CANDIDATE_STATE=unknown     # unknown | absent | present
CANDIDATE_NOT_FOUND_COUNT=0
# 교차 시계 (ii) 흡수폭 — §2.5 ⑥에서 **실제로 측정한** 셸↔AWS 오차의 절대값에 여유를 더해 넣는다.
# 측정값이 없으면 300초를 쓴다. **0으로 두지 않는다**: 이 비교만 마진이 없으면 셸이 AWS보다 몇 초
# 앞선 것만으로 정상 생성된 **유료** 복원본이 PROVENANCE_OK=0 → 검증도 못 해 보고 §10으로 폐기된다.
# 값은 §2.5 ⑥에서 **실측해 원장에 적은** CLOCK_SKEW_SECONDS(셸 − AWS, 부호 포함 초)에서 도출한다.
# 측정값이 없으면 300초. 어떤 경우에도 300초 미만으로 내려가지 않는다.
CLOCK_SKEW_SECONDS="${CLOCK_SKEW_SECONDS:-}"
if printf %s "$CLOCK_SKEW_SECONDS" | grep -Eq '^-?[0-9]+$'; then
  PROVENANCE_SKEW_ALLOWANCE=$(( ${CLOCK_SKEW_SECONDS#-} + 60 ))
  [ "$PROVENANCE_SKEW_ALLOWANCE" -lt 300 ] && PROVENANCE_SKEW_ALLOWANCE=300
else
  PROVENANCE_SKEW_ALLOWANCE=300
fi
echo "PROVENANCE_SKEW_ALLOWANCE=${PROVENANCE_SKEW_ALLOWANCE}초 (실측 오차=${CLOCK_SKEW_SECONDS:-미측정})"
RESTORE_T0_EPOCH=$(python3 -c 'import datetime as d,re,sys
s=sys.argv[1]
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s)
x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
assert x.utcoffset() is not None
print(int(x.timestamp()))' "$RESTORE_T0" 2>/dev/null)

case "$RESTORE_CALL_STATE" in
  accepted|ambiguous)
    SOURCE_LEDGER_OK=0
    case "$RESTORE_REQUEST_KIND" in
      pitr)
        [ -n "$RESTORE_REQUEST_SOURCE" ] \
          && [ "$RESTORE_REQUEST_SOURCE" = "$RESTORE_POINT" ] && SOURCE_LEDGER_OK=1
        ;;
      snapshot)
        [ -n "$RESTORE_REQUEST_SOURCE" ] \
          && [ "$RESTORE_REQUEST_SOURCE" = "$SNAPSHOT_ID" ] && SOURCE_LEDGER_OK=1
        ;;
    esac
    if [ "$SOURCE_LEDGER_OK" = 1 ]; then
      case "$RESTORE_T0_EPOCH" in ""|*[!0-9]*) ;; *) RESTORE_LEDGER_OK=1 ;; esac
    fi
    ;;
esac

if [ "$RESTORE_LEDGER_OK" = 1 ]; then
  # **부재는 1회 조회로 확정하지 않는다(총 3회).** 응답을 잃은 시점에 서버가 아직 요청을 처리 중일 수 있고,
  # 그때의 DBInstanceNotFound는 "그 조회 시점에 아직 안 보였다"만 말한다. 이 문서가 ECS MISSING·태그 조회·
  # 시크릿에 이미 적용한 재조회 규율을 **가장 비싼 판정에도** 동일하게 적용한다.
  for CAND_TRY in 1 2 3; do
    CANDIDATE=$(aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" \
      --query 'DBInstances[0].[DBInstanceIdentifier,DBInstanceArn,InstanceCreateTime]' --output text 2>&1)
    CANDIDATE_RC=$?
    if [ "$CANDIDATE_RC" -eq 0 ]; then
      CANDIDATE_STATE=present
      break
    elif printf %s "$CANDIDATE" | grep -q "DBInstanceNotFound"; then
      CANDIDATE_NOT_FOUND_COUNT=$((CANDIDATE_NOT_FOUND_COUNT + 1))
      echo "복원본 부재 관측 $CAND_TRY/3 (전파 지연일 수 있다)"
    else
      CANDIDATE_STATE=unknown         # 조회 실패는 부재가 아니다
      echo "복원본 조회 실패 $CAND_TRY/3 rc=$CANDIDATE_RC $CANDIDATE"
    fi
    if [ "$CAND_TRY" -lt 3 ]; then sleep 30; fi
  done
  if [ "$CANDIDATE_STATE" != present ]; then
    if [ "$CANDIDATE_NOT_FOUND_COUNT" -eq 3 ]; then
      CANDIDATE_STATE=absent
    else
      CANDIDATE_STATE=unknown
    fi
  fi

  if [ "$CANDIDATE_STATE" = present ]; then
    CANDIDATE_ID=$(printf %s "$CANDIDATE" | awk '{print $1}')
    CANDIDATE_ARN=$(printf %s "$CANDIDATE" | awk '{print $2}')
    CANDIDATE_CREATED=$(printf %s "$CANDIDATE" | awk '{print $3}')
    # 태그 **조회 실패를 태그 불일치로 읽지 않는다**(§10.4와 같은 원칙) → unknown으로 두고 30초×3 재조회.
    TAG_STATE=unknown
    PURPOSE_TAG=""
    TOKEN_TAG=""
    for TAG_TRY in 1 2 3; do
      TAGS_OUT=$(aws rds list-tags-for-resource --resource-name "$CANDIDATE_ARN" \
        --query 'TagList[?Key==`purpose`||Key==`drill-token`].[Key,Value]' --output text 2>&1)
      TAGS_RC=$?
      if [ "$TAGS_RC" -eq 0 ]; then
        PURPOSE_TAG=$(printf %s "$TAGS_OUT" | awk '$1=="purpose"{print $2}')
        TOKEN_TAG=$(printf %s "$TAGS_OUT" | awk '$1=="drill-token"{print $2}')
        TAG_STATE=ok
        break
      fi
      echo "태그 조회 실패($TAG_TRY/3) rc=$TAGS_RC $TAGS_OUT"
      if [ "$TAG_TRY" -lt 3 ]; then sleep 30; fi
    done
    CANDIDATE_CREATED_EPOCH=$(python3 -c 'import datetime as d,re,sys
s=sys.argv[1]
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s)
x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
assert x.utcoffset() is not None
print(int(x.timestamp()))' "$CANDIDATE_CREATED" 2>/dev/null)

    PROVENANCE_OK=0
    case "$CANDIDATE_CREATED_EPOCH" in
      ""|*[!0-9]*) ;;
      *)
        if [ "$TAG_STATE" = ok ] && [ "$CANDIDATE_ID" = "$DRILL_DB" ] \
           && [ "$PURPOSE_TAG" = rds-restore-drill ] && [ -n "$TOKEN" ] && [ "$TOKEN_TAG" = "$TOKEN" ]; then
          if [ "$RESTORE_CALL_STATE" = accepted ] \
             && [ "$CANDIDATE_ARN" = "$RESTORE_EXPECTED_ARN" ] \
             && [ "$CANDIDATE_CREATED" = "$RESTORE_INSTANCE_CREATED" ]; then
            # accepted는 **응답의 exact ARN + InstanceCreateTime**이 그대로 일치한다 = 제어면 단독 증거.
            # 여기에 셸 시계 순서까지 요구하면 오차 몇 초로 정상 복원본을 버리므로 넣지 않는다(시각은 아래 증거로만).
            PROVENANCE_OK=1
          elif [ "$RESTORE_CALL_STATE" = ambiguous ] \
               && [ "$CANDIDATE_CREATED_EPOCH" -ge $((RESTORE_T0_EPOCH - PROVENANCE_SKEW_ALLOWANCE)) ] \
               && { [ -z "$RESTORE_EXPECTED_ARN" ] || [ "$CANDIDATE_ARN" = "$RESTORE_EXPECTED_ARN" ]; } \
               && { [ -z "$RESTORE_INSTANCE_CREATED" ] || [ "$CANDIDATE_CREATED" = "$RESTORE_INSTANCE_CREATED" ]; }; then
            # ambiguous는 응답이 없어 제어면 증거가 태그뿐이다 → 시각 비교를 **흡수폭과 함께** 보강으로 쓴다.
            PROVENANCE_OK=1
            RESTORE_EXPECTED_ARN="$CANDIDATE_ARN"
            RESTORE_INSTANCE_CREATED="$CANDIDATE_CREATED"
            echo "원장 갱신: expected_arn=$RESTORE_EXPECTED_ARN created=$RESTORE_INSTANCE_CREATED"
          fi
        fi
        echo "교차 시계 (ii) 증거: InstanceCreateTime=$CANDIDATE_CREATED(제어면) vs T0=$RESTORE_T0(셸), 흡수폭 ${PROVENANCE_SKEW_ALLOWANCE}초, tag_state=$TAG_STATE"
        ;;
    esac
    [ "$PROVENANCE_OK" = 1 ] && RESTORE_OK=1
  fi
fi

# `ambiguous` ∧ **3회 연속 부재**면 재호출 창을 1회 연다.
# **상태를 되돌리지 않는다** — `not-started`로 바꾸면 최초 요청의 계보가 사라지고, 그 요청이 뒤늦게 접수돼
# 인스턴스를 만들었을 때 어느 T0·source에 대한 결과인지 판정할 수 없고 측정값도 오염된다.
# 열리는 것은 호출 자격(`RESTORE_RETRY_ALLOWED`)뿐이고, `RESTORE_T0`·source·`ambiguous`는 그대로 보존된다.
# **여기서 재시도를 소진하지 않는다.** 자격만 `authorized`로 영속화하고, 소진(`dispatched`)은 호출부가
# 재호출 직전에 적는다 — 창 개방과 재호출 사이에서 세션이 끊겨도 재진입에서 같은 창을 다시 열 수 있다.
# 일회성 플래그는 **여기서 먼저 0으로 닫는다.** 호출 중 인터럽트로 같은 셸에 stale 1이 남아 있어도
# 이 읽기 전용 블록을 지나면 사라지고, 자격은 아래 조건(= 영속 상태)만이 다시 준다.
RESTORE_RETRY_ALLOWED=0
if [ "$RESTORE_OK" = 0 ] && [ "$RESTORE_CALL_STATE" = ambiguous ] \
   && [ "$CANDIDATE_STATE" = absent ] && [ "$CANDIDATE_NOT_FOUND_COUNT" -eq 3 ] \
   && { [ "$RESTORE_RETRY_STATE" = not-used ] || [ "$RESTORE_RETRY_STATE" = authorized ]; }; then
  case "$RESTORE_REQUEST_KIND" in
    pitr|snapshot)
      RESTORE_RETRY_STATE=authorized
      RESTORE_RETRY_ALLOWED=1
      echo "재시도 창 개방(1회): ambiguous + 3회 연속 DBInstanceNotFound → RESTORE_RETRY_STATE=authorized ← 원장 기입"
      echo "  → RESTORE_CALL_STATE=ambiguous **유지**(요청 계보 보존), RESTORE_T0=$RESTORE_T0 유지"
      echo "  → PITR은 §4.3 (2) 원장 tuple·경계 재도출 후 §5, 스냅샷은 §11 2)·2-b) 후 3)으로 간다(mode=$RESTORE_REQUEST_KIND)"
      echo "     재호출이 AlreadyExists를 받으면 그것은 '최초 요청이 실제로 접수됐다'는 증거이므로"
      echo "     rejected 로 적지 않고 ambiguous 를 유지한 뒤, 이 블록으로 다시 와서 태그·ARN·생성 시각으로 결과를 확정한다"
      ;;
    *) RESTORE_RETRY_ALLOWED=0; echo "STOP: 알 수 없는 복원 mode — 재시도 창을 닫고 §10으로 간다" ;;
  esac
fi
echo "RESTORE_OK=$RESTORE_OK call_state=$RESTORE_CALL_STATE t0=$RESTORE_T0 source=$RESTORE_REQUEST_SOURCE retry_state=$RESTORE_RETRY_STATE cand=$CANDIDATE_STATE"
```

- `RESTORE_OK=1`이 아니면 §6 이후로 진행하지 않고 §10 정리 게이트로 간다. **예외는 위 블록이 `RESTORE_RETRY_ALLOWED=1`로 재시도 창을 연 경우 하나뿐**(전송 불명 + 세 조회가 모두 명시적 부재) — PITR은 §4.3 (2)에서 원장 baseline tuple·동일 source·B 경계를 다시 검증한 뒤 §5 첫 블록으로, 스냅샷은 §11의 읽기 전용 2)·2-b)를 거쳐 3) 호출 블록으로 돌아가 **같은 API를 1회만** 재호출한다.
- **재시도는 상태를 되돌리지 않는다.** `RESTORE_CALL_STATE`는 `ambiguous`로, `RESTORE_T0`·source는 최초 값으로 남는다. 최초 요청이 접수되지 않았다는 것을 **증명할 수 없기 때문**이다 — 되돌리면 그 요청이 뒤늦게 만든 인스턴스를 어느 요청의 결과로 대사해야 하는지와 최초 측정 시점을 잃는다.
- **재시도 자격은 영속 상태와 일회성 플래그를 함께 요구한다.** 호출부의 자격식은 `RESTORE_RETRY_STATE = authorized` ∧ `RESTORE_RETRY_ALLOWED = 1`이고, 재호출 직전에 **둘을 함께**(`dispatched` + `0`) 닫는다. 하나만 보면 aws 호출 중 `Ctrl-C`로 블록이 끊긴 뒤 같은 셸에 남은 값으로 **같은 변경 API가 두 번 전송**된다 — 이 문서의 블록은 붙여넣기로 재실행되는 것이 정상 경로다. 응답 분류에 필요한 "이번 호출이 재시도였나"는 원장이 아닌 호출 로컬 `IS_RETRY`가 들고 있다.
- **재시도 자격과 소진은 다른 사건이다.** reconciliation은 `RESTORE_RETRY_STATE=authorized`까지만 적고, `dispatched`는 호출부가 **재호출 직전**에 적는다. 이렇게 나누지 않으면 창을 연 직후 터미널이 끊겼을 때 **재호출을 한 번도 하지 않았는데 자격만 소진**돼, 뒤늦게 나타났을 수도 있는 복원본의 검증을 포기하고 재드릴(= 삭제 불가능한 sentinel 행을 다시 소비)해야 한다. `authorized`로 재진입하면 같은 창을 다시 열고, `dispatched`는 다시 열지 않는다.
- **재시도 응답의 `DBInstanceAlreadyExists`는 실패가 아니라 증거다** — "최초 요청이 실제로 접수됐다"는 뜻이므로 `rejected`로 적지 않는다. 분류되지 않은 오류도 같은 이유로 `ambiguous`로 보수 처리한다(`rejected`는 AWS가 의미를 명시한 화이트리스트 응답만).
- **교차 시계 (ii)는 판정 근거가 아니라 증거다.** `accepted`의 소유권은 응답의 exact ARN·`InstanceCreateTime`(제어면 단독)으로 성립하고, `ambiguous`에서만 흡수폭(`PROVENANCE_SKEW_ALLOWANCE`)과 함께 보강으로 쓴다. 태그 조회 실패는 불일치가 아니라 `TAG_STATE=unknown`이며 30초×3 재조회 후에도 실패면 승인하지 않는다.
- 최초 요청 직후 `LatestRestorableTime`을 한 번 더 읽어 원장에 “`T0` 근처 관측값”으로 기록한다. 이 조회는 복원 provenance를 바꾸지 않는다.
- **`--manage-master-user-password`는 넣지 않는다** — 복원 계열 명령의 이 옵션은 _"Applies to RDS for Oracle only"_ 이고 대상은 PostgreSQL 16이다. 자격증명은 §7에서 `modify-db-instance`로 전환한다.
- **서브넷 그룹·SG·파라미터그룹을 반드시 명시하는 이유**: 복원본은 _"created with most of the original configuration, but … with the **default security group, the default subnet group, and the default DB parameter group**"_ 이다(로컬 help). 지정하지 않으면 **운영 커스텀 그룹이 아닌 엔진 기본 그룹**으로 뜨고 네트워크 경계도 달라진다. — **TLS가 뚫린다는 뜻은 아니다**: `rds.force_ssl`의 기본값은 [RDS for PostgreSQL 15 이상에서 `1`(on)](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/PostgreSQL.Concepts.General.SSL.html)이고 대상은 16이라 기본 그룹에서도 비TLS는 거부된다. 명시하는 이유는 **구성 동등성 자체**(운영과 같은 그룹·같은 파라미터 집합)와 앞으로 추가될 커스텀 파라미터의 보존이다.
- 응답의 **`InstanceCreateTime`(제어면 시계)** 과 위 `LatestRestorableTime`을 원장에 기입한다(교차 시계 (ii)).

---

## 6. (e) available 대기 + 구성 게이트 → `T_available`

**① available 대기 — waiter가 성공했을 때만 시각을 찍는다.** waiter는 1800초를 넘기면 **exit 255**로 끝나는데, 종료 코드를 보지 않고 다음 줄에서 `date`를 찍으면 **아직 `creating`인 인스턴스에 "available 통과" 시각이 박힌다**(§2.1 규약).

```bash
AVAILABLE_OK=0
if [ "$RESTORE_OK" != 1 ]; then
  echo "STOP: RESTORE_OK=$RESTORE_OK — 이번 호출의 복원본이라는 증거가 없어 waiter를 실행하지 않는다"
else
  if aws rds wait db-instance-available --db-instance-identifier "$DRILL_DB"; then   # 30초×60=1800초
    AVAILABLE_OK=1
    date -u +%Y-%m-%dT%H:%M:%SZ                                        # ← T_available (원장 기입)
  else
    echo "STOP: available waiter 미통과(1800초 초과 또는 조회 실패) — T_available을 찍지 않는다"
    aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" \
      --query 'DBInstances[0].DBInstanceStatus' --output text 2>&1 | tail -1   # → 재호출/중단 판단(§2.4)
  fi
fi
```

**② 구성 조회 — 사람이 읽는 전체 값.** 아래 ③은 노출·경로뿐 아니라 엔진·버전·클래스도 §2.5에서 고정한 원본 값과 기계적으로 대조한다.

```bash
if [ "$AVAILABLE_OK" = 1 ]; then
aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" --query 'DBInstances[0].{
  status:DBInstanceStatus, pub:PubliclyAccessible, enc:StorageEncrypted,
  engine:Engine, ev:EngineVersion, class:DBInstanceClass,
  subnetGroup:DBSubnetGroup.DBSubnetGroupName, sgs:VpcSecurityGroups[].[VpcSecurityGroupId,Status],
  pg:DBParameterGroups, delProt:DeletionProtection, addr:Endpoint.Address,
  arn:DBInstanceArn}' --output json
fi
```

**두 부류를 분리한다.**

- **중단(abort) — 접속 전 즉시 중단, §10 정리 게이트로**: `PubliclyAccessible=true` · 기대와 다른 SG/서브넷 그룹 · `StorageEncrypted=false` · 엔진/버전/클래스 불일치 · `DeletionProtection=true`. 노출·경로 문제라 검증을 진행할 이유가 없다.
  - abort는 T0 이후라 **sentinel-A·B가 이미 존재한다** → 중단 기록에도 **sentinel code·`created_at`·`restore point`·확보한 시각들**을 남겨 실패한 드릴의 증거를 완결시킨다.
- **파라미터그룹 축 — 어느 쪽이든 중단하지 않는다**(판정은 전부 ④가 한다):
  - **교정(remediate)**: `DBParameterGroups[].ParameterApplyStatus`가 `in-sync`가 아님. 이 레포의 `rds.force_ssl`은 `apply_method="pending-reboot"`로 명시돼 있어 복원본에서도 `pending-reboot`로 보고될 수 있다. 정답은 드릴 폐기가 아니라 **재부팅 후 재확인**이다.
  - **기록만((a) FAIL 확정)**: 붙은 그룹 이름이 운영 `$PROD_PG`가 아님(= `--db-parameter-group-name` 누락·오타). 재부팅으로 고쳐지지 않으므로 교정하지 않지만, 인스턴스는 여전히 비공개·암호화·기대 SG/서브넷 그룹이라 **검증은 계속한다.**

**③ abort 조건과 대상 확인을 셸 조건으로 건다.** 눈으로 보고 다음 블록을 붙여 넣는 방식은 가드가 아니다 — 노출된 인스턴스에 reboot·자격증명 전환이 그대로 걸린다.

```bash
SAFE_OK=0        # 노출·경로 문제 없음(= abort 아님)
SAFE_STATE=unknown  # ok | fail(=구성 불일치 → (a) FAIL) | unknown(=조회 실패 → 판정 미완)
TARGET_OK=0      # 상태를 바꿔도 되는 대상임(§6 reboot·§7 modify의 전제)
if [ "$AVAILABLE_OK" = 1 ]; then
  # **조회 실패를 구성 불일치로 읽지 않는다**(§6 ④ CONFIG_STATE와 같은 규율). 총 3회 재조회하고,
  # 끝까지 실패하면 SAFE_STATE=unknown = 판정 미완이다 — (a) FAIL로 기록하면 안 된다.
  CONF_READ_STATE=unknown   # **조회가 성공했나**(구성이 맞나를 뜻하는 §6 ④ CONFIG_STATE와 다른 축이다)
  for CONF_TRY in 1 2 3; do
    CONF=$(aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" --query \
      'DBInstances[0].[PubliclyAccessible,StorageEncrypted,DeletionProtection,DBSubnetGroup.DBSubnetGroupName,Engine,EngineVersion,DBInstanceClass,DBParameterGroups[0].DBParameterGroupName]' \
      --output text 2>&1)
    CONF_RC=$?
    if [ "$CONF_RC" -eq 0 ]; then CONF_READ_STATE=ok; break; fi
    echo "구성 조회 실패($CONF_TRY/3) rc=$CONF_RC $CONF"
    if [ "$CONF_TRY" -lt 3 ]; then sleep 30; fi
  done
  C_PUB=$(printf %s "$CONF"  | awk '{print $1}')      # 기대 False
  C_ENC=$(printf %s "$CONF"  | awk '{print $2}')      # 기대 True
  C_PROT=$(printf %s "$CONF" | awk '{print $3}')      # 기대 False
  C_SUBG=$(printf %s "$CONF" | awk '{print $4}')      # 기대 $SUBNET_GROUP
  C_ENGINE=$(printf %s "$CONF" | awk '{print $5}')
  C_ENGINE_VERSION=$(printf %s "$CONF" | awk '{print $6}')
  C_CLASS=$(printf %s "$CONF" | awk '{print $7}')
  # 파라미터그룹 이름은 **여기서 읽기만** 하고 판정은 ④가 한다. 이름 불일치는 노출·경로 문제가 아니라
  # **파라미터그룹 축**이라, abort(= 접속 없이 드릴 폐기)가 아니라 `CONFIG_STATE=fail`(= (a) FAIL 확정,
  # 검증은 계속)로 다뤄야 형제 조건(reboot 후에도 `in-sync` 아님)과 강도가 맞는다.
  C_PG=$(printf %s "$CONF" | awk '{print $8}')      # ④가 $PROD_PG와 대조한다
  # SG는 목록 전체를 비교한다 — 기대 SG 외에 하나라도 더 붙어 있으면 경로가 달라진 것이다.
  # 필터는 §2.5 ⑧ 원본 조회와 **같은 `Status==active`** 다. 복원 직후 SG가 `adding` 등으로 보고되면
  # 목록이 비어 정상 복원이 abort 조건에 걸리므로, 사람이 읽는 두 조회(§2.5 ①·§6 ②)에 `Status`를
  # 함께 남겨 이 가정을 드릴 당일 증거로 확인한다(waiter 통과 뒤라 실현 가능성은 낮다).
  SGS_STATE=unknown
  for SGS_TRY in 1 2 3; do
    C_SGS=$(aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" \
            --query 'DBInstances[0].VpcSecurityGroups[?Status==`active`].VpcSecurityGroupId' --output text 2>&1)
    C_SGS_RC=$?
    if [ "$C_SGS_RC" -eq 0 ]; then SGS_STATE=ok; break; fi
    echo "SG 목록 조회 실패($SGS_TRY/3) rc=$C_SGS_RC $C_SGS"
    if [ "$SGS_TRY" -lt 3 ]; then sleep 30; fi
  done
  echo "관측: pub=$C_PUB enc=$C_ENC delProt=$C_PROT subnetGroup=$C_SUBG sg=$C_SGS engine=$C_ENGINE version=$C_ENGINE_VERSION class=$C_CLASS pg=$C_PG"
  echo "기대: pub=False enc=True delProt=False subnetGroup=$SUBNET_GROUP sg=$DATA_SG engine=$EXPECTED_ENGINE version=$EXPECTED_ENGINE_VERSION class=$EXPECTED_CLASS pg=$PROD_PG"
  # **기대값이 비었으면 그것도 판정 미완이다**(조회 실패와 같은 축). 아래 다섯 값은 §2.5 ⑧이 채우는데
  # 이 블록은 `VALUES_OK`를 요구하지 않으므로, §2.5 ⑧을 건너뛴 재진입 세션에서 비어 있을 수 있다.
  # 그때 비교만 하면 실재하는 관측값과 달라 `else`로 떨어져 **원장 붙여넣기 누락이 확정 (a) FAIL + abort**가
  # 된다 — 원인은 구성이 아닌데 메시지는 노출·경로를 지목하고, 정상 복원본이 검증 없이 폐기된다.
  if [ "$CONF_READ_STATE" != ok ] || [ "$SGS_STATE" != ok ] \
     || [ -z "$SUBNET_GROUP" ] || [ -z "$DATA_SG" ] || [ -z "$EXPECTED_ENGINE" ] \
     || [ -z "$EXPECTED_ENGINE_VERSION" ] || [ -z "$EXPECTED_CLASS" ]; then
    SAFE_STATE=unknown
    echo "STOP: 구성 조회 3회 실패 또는 **기대값 미확보** = **판정 미완**(구성 불일치가 아니다) — (a)를 FAIL로 적지 않고"
    echo "      기대값이 비었으면 §2.5 ⑧을 다시 돌려 원장 값을 되살린다. 원인 해소 후 이 블록부터 재실행한다"
    echo "      접속·상태 변경은 하지 않는다(어느 값이 비었는지는 바로 위 '기대:' 줄에서 확인한다)"
  elif [ "$C_PUB" = "False" ] && [ "$C_ENC" = "True" ] && [ "$C_PROT" = "False" ] \
     && [ "$C_SUBG" = "$SUBNET_GROUP" ] && [ "$C_SGS" = "$DATA_SG" ] \
     && [ "$C_ENGINE" = "$EXPECTED_ENGINE" ] && [ "$C_ENGINE_VERSION" = "$EXPECTED_ENGINE_VERSION" ] \
     && [ "$C_CLASS" = "$EXPECTED_CLASS" ]; then
    SAFE_OK=1
    SAFE_STATE=ok
  else
    SAFE_STATE=fail
    echo "STOP: abort 조건(노출·경로·엔진 구성 불일치) — 접속하지 않고 §10 정리 게이트로 간다"
  fi

  # 대상 확인 — "운영이 아님"만으로는 부족하다. 변수가 재진입·수기 치환으로 오염되면
  # 스테이징 등 **다른** 인스턴스를 맞출 수 있으므로, §10.4 삭제 게이트와 **같은 축**
  # (고정 드릴 식별자 + purpose·drill-token 태그 **값**)을 상태 변경 명령의 전제로 쓴다.
  # 태그는 **한 번의 호출로 두 키를 함께** 가져온다(§5·§10.4와 같은 형태). 호출을 둘로 나누면
  # 실패 지점이 둘로 늘고 rc 대조도 두 벌이 된다. 조회 실패는 불일치가 아니라 unknown → 30초×3 재조회.
  if [ "$DRILL_DB" != "$SRC_DB" ] && [ "$DRILL_DB" = "linkpulse-restore-drill" ]; then
    TARGET_TAG_STATE=unknown; TAGV=""; TAG_TOKEN=""
    DRILL_ARN=$(aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" \
                --query 'DBInstances[0].DBInstanceArn' --output text 2>&1)
    DRILL_ARN_RC=$?
    if [ "$DRILL_ARN_RC" -ne 0 ]; then
      echo "STOP: 드릴 DB ARN 조회 실패 = 상태 불명 → TARGET_OK=0: $DRILL_ARN"
    else
      for T_TRY in 1 2 3; do
        T_TAGS=$(aws rds list-tags-for-resource --resource-name "$DRILL_ARN" \
          --query 'TagList[?Key==`purpose`||Key==`drill-token`].[Key,Value]' --output text 2>&1)
        T_TAGS_RC=$?
        if [ "$T_TAGS_RC" -eq 0 ]; then
          TAGV=$(printf %s "$T_TAGS" | awk '$1=="purpose"{print $2}')
          TAG_TOKEN=$(printf %s "$T_TAGS" | awk '$1=="drill-token"{print $2}')
          TARGET_TAG_STATE=ok
          break
        fi
        echo "대상 태그 조회 실패($T_TRY/3) rc=$T_TAGS_RC $T_TAGS"
        if [ "$T_TRY" -lt 3 ]; then sleep 30; fi
      done
      # 여기서 TARGET_OK=0이 되면 방향은 안전하지만 결과는 **과금 중인 정상 복원본을 폐기**하는 것이다
      # (§7 modify의 전제이므로). 그래서 한 번의 API 흔들림으로 0이 되지 않게 위에서 3회 재조회한다.
      # `[ -n "$TOKEN" ]`을 토큰 비교 **앞**에 둔다(§2.7 ④) — 재진입 템플릿은 `TOKEN=""`로 시작하므로,
      # 이것이 없으면 `drill-token` 태그가 없는 리소스에서 `"" = ""`로 소유권이 공허하게 성립한다.
      if [ "$TARGET_TAG_STATE" = ok ] && [ "$TAGV" = "rds-restore-drill" ] \
         && [ -n "$TOKEN" ] && [ "$TAG_TOKEN" = "$TOKEN" ]; then TARGET_OK=1; fi
    fi
  fi
fi
echo "SAFE_OK=$SAFE_OK SAFE_STATE=$SAFE_STATE TARGET_OK=$TARGET_OK purpose=$TAGV token=$TAG_TOKEN tag_state=${TARGET_TAG_STATE:-미실행}"
```

**④ 교정 — `pending-reboot`일 때만 재부팅한다.** 상태와 무관하게 항상 재부팅하면 불필요한 시간이 `T_config_ok − T_available` 구간에 섞여 **DB 복원 검증 병목 측정이 왜곡**된다. **④가 파라미터그룹 축을 통째로 판정한다** — ③이 읽어 온 `C_PG`의 **이름 대조**와 `ParameterApplyStatus`의 **적용 완료**를 둘 다 본다(`in-sync`는 붙어 있는 그룹이 무엇이든 적용만 끝나면 참이라 기본 그룹도 통과하므로, 이름 대조 없이는 `--db-parameter-group-name` 누락을 잡지 못한다). **어느 쪽이 어긋나도 abort하지 않는다** — 파라미터그룹 문제는 노출·경로 문제가 아니므로 `CONFIG_STATE=fail`(= (a) FAIL 확정)로 적고 **검증은 계속한다.**

```bash
CONFIG_STATE=unknown     # ok(=in-sync 확인) | fail(=(a) FAIL 확정) | unknown(=판정 미완)
PG_STATUS=$(aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" \
            --query 'DBInstances[0].DBParameterGroups[0].ParameterApplyStatus' --output text 2>&1)
PG_RC=$?
echo "ParameterApplyStatus=$PG_STATUS (rc=$PG_RC)"

if [ "$SAFE_OK" != 1 ]; then
  # SAFE_STATE로 두 경우를 구분해 적는다: fail = (a) FAIL 확정 / unknown = 판정 미완(조회 실패)
  echo "STOP: safe_state=$SAFE_STATE — 구성 판정도 reboot도 하지 않는다(unknown이면 (a)를 FAIL로 적지 않는다)"
elif [ -z "$PROD_PG" ]; then
  # 기대값이 없는 것은 "구성이 틀렸다"가 아니라 "판정할 재료가 없다"이다 — §6 ③이 다섯 기대값에 쓰는 것과
  # **같은 축**이고, 파라미터그룹 축만 여기서 판정하므로 `PROD_PG`의 몫이 이 분기다. 도달하는 경우는
  # **다섯은 채웠고 `PROD_PG`만 빈** 상태다(전부 비었으면 ③이 먼저 `SAFE_STATE=unknown`으로 닫는다).
  echo "STOP: 기대 파라미터그룹 이름을 얻지 못했다 = 판정 미완 — §2.5 ⑧을 다시 돌려 PROD_PG를 되살린 뒤 §6 ③부터 재실행한다"
elif [ "$C_PG" != "$PROD_PG" ]; then
  # **이름 대조를 `PG_RC` 앞에 둔다**: 이름은 ③의 `CONF`에서 온 값이라 이 절의 조회 성패와 무관하고,
  # §1의 합성 규칙이 **확인된 FAIL > 미완**이다 — 적용 상태를 못 읽었다고 확정된 불일치를 미완으로 덮지 않는다.
  # 이름이 다르면 reboot으로 고쳐지지 않으므로 교정 분기로 가지 않는다. 그러나 복원본은 여전히
  # 비공개·암호화·기대 SG/서브넷 그룹이라 **접속을 막을 안전상의 이유가 없다** — (a)만 FAIL로 확정하고
  # (b)·(c) 재료는 살린다(§1의 "(a) FAIL / (b) PASS" 부분 성공 기록이 이 경우다).
  CONFIG_STATE=fail
  echo "파라미터그룹 이름 불일치(관측=$C_PG 기대=$PROD_PG) → (a) FAIL 확정. **검증은 계속한다**"
elif [ "$PG_RC" -ne 0 ]; then
  echo "STOP: 파라미터그룹 상태 조회 실패 = 판정 미완(조회 실패를 판정으로 읽지 않는다)"
elif [ "$PG_STATUS" = "in-sync" ]; then
  CONFIG_STATE=ok; echo "in-sync — reboot 불필요"
elif [ "$PG_STATUS" = "pending-reboot" ] && [ "$TARGET_OK" = 1 ] \
     && { [ "${DRILL_MUTATION_OK:-0}" != 1 ] || ! lock_live; }; then
  # 재진입 세션은 읽기 전용 재도출만으로 TARGET_OK·SAFE_OK가 되살아나므로, **셸만 보면 락 없이 reboot이 열린다.**
  # §2.5 ⓪의 변경 자격을 직접 요구한다(SHELL_OK는 그 자격의 구성요소다). 만료 재확인도 여기서 함께 본다 —
  # 이 절은 §4.1 폴링(최대 30분) 뒤라 자격 발급 시점과 호출 시점의 간격이 특히 크다.
  echo "STOP: DRILL_MUTATION_OK=${DRILL_MUTATION_OK:-미설정}(shell=${SHELL_OK:-미설정} 락=${EXCLUSIVE_DRILL_LOCK_OK:-미설정} 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)) — 이 세션은 변경 자격이 없거나 락이 만료됐다. reboot을 호출하지 않는다(§2.1 ⓪·§2.5 ⓪부터 다시)"
elif [ "$PG_STATUS" = "pending-reboot" ] && [ "$TARGET_OK" = 1 ]; then
  REBOOT_OK=0
  if aws rds reboot-db-instance --db-instance-identifier "$DRILL_DB"; then   # ⚠️ TARGET_OK=1 ∧ DRILL_MUTATION_OK=1 에서만
    REBOOT_OK=1
  else
    echo "STOP: reboot 요청 실패 = 판정 미완"
  fi
  if [ "$REBOOT_OK" != 1 ]; then
    echo "STOP: reboot 요청 미확정이라 available waiter를 실행하지 않는다"
  elif aws rds wait db-instance-available --db-instance-identifier "$DRILL_DB"; then
    PG_STATUS=$(aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" \
                --query 'DBInstances[0].DBParameterGroups[0].ParameterApplyStatus' --output text 2>&1)
    PG_RC=$?
    echo "reboot 후 ParameterApplyStatus=$PG_STATUS (rc=$PG_RC)"
    if [ "$PG_RC" -ne 0 ]; then echo "STOP: 재확인 조회 실패 = 판정 미완"
    elif [ "$PG_STATUS" = "in-sync" ]; then CONFIG_STATE=ok
    else CONFIG_STATE=fail; echo "reboot 후에도 in-sync 아님 → (a) FAIL 확정(검증은 계속한다)"; fi
  else
    echo "STOP: reboot 후 available waiter 미통과 = 판정 미완"
  fi
elif [ "$PG_STATUS" = "pending-reboot" ]; then
  echo "STOP: pending-reboot 인데 TARGET_OK=0(식별자/태그 확인 실패) — reboot을 호출하지 않았다"
else
  CONFIG_STATE=fail; echo "예상 밖 ParameterApplyStatus='$PG_STATUS' → (a) FAIL 확정"
fi
echo "CONFIG_STATE=$CONFIG_STATE"
```

**⑤ `T_config_ok`는 구성 판정이 끝났을 때만 찍는다** — `ok`(in-sync 확인) 또는 `fail`((a) FAIL 확정)이다. reboot이 필요 없었다면 `T_available` 직후 값이 되고, 필요했다면 재부팅·재대기가 끝난 시각이 된다. **`unknown`(판정 미완)에서 찍으면 끝나지 않은 구성 검증 구간이 완료된 것처럼 기록된다.**

```bash
if [ "$CONFIG_STATE" = ok ] || [ "$CONFIG_STATE" = fail ]; then
  date -u +%Y-%m-%dT%H:%M:%SZ                                      # ← T_config_ok (원장 기입)
else
  echo "STOP: 구성 판정 미완 — T_config_ok를 찍지 않는다. 중단 → §10 정리 게이트"
fi
```

`CONFIG_STATE=fail`은 파라미터그룹 축의 확정 FAIL이고 원인은 셋이다 — **① 붙은 그룹 이름이 `$PROD_PG`가 아님 ② 재부팅 후에도 `in-sync`가 아님 ③ 예상 밖 `ParameterApplyStatus`**. 어느 쪽이든 **검증은 계속하되(다른 복원 증거는 여전히 가치 있다) 성공 판정의 (a)는 FAIL로 확정**하고 한계를 기록한다. 계속 진행 ≠ PASS. reboot 소요는 **DB 복원 검증 소요시간**에 포함하고 `T_config_ok`로 별도 구간을 남긴다.

---

## 7. (f) 자격증명 전환 → `T_creds_ready`

복원본은 관리 시크릿 없이 뜬다(§5). **드릴 식별자에만** 관리 비밀번호를 켜고, **ARN은 손으로 적지 말고 반드시 드릴 인스턴스 조회로 도출**한다 — 운영·드릴 시크릿 이름이 `rds!db-<resource-id>` 형태라 육안 구분이 거의 안 된다.

준비 완료 조건은 **`SecretStatus == active` ∧ `DBInstanceStatus == available`(modifying 복귀)** 둘 **다**이고, `T_creds_ready`는 그 조건이 참이 된 뒤에 찍는다 — 한 번만 조회하고 시각을 찍으면 `creating`/`modifying` 상태를 준비 완료로 오판해 **TD-B 기동 실패 + 잘못된 검증 소요시간**이 된다.

```bash
CREDS_OK=0
# ⚠️ §6 ③이 세운 두 플래그가 전제다: TARGET_OK(고정 드릴 식별자 + purpose·이번 drill-token 태그 = 우리가 만든 그 인스턴스)
#    ∧ SAFE_OK(abort 아님). "운영이 아님"만 확인하면 오염된 변수가 가리키는 **다른** RDS 인스턴스의
#    마스터 자격증명을 갈아치울 수 있다 — modify 는 되돌리기 어려운 상태 변경이다.
#    터미널을 새로 열어 플래그가 비었으면 **§6 ③ 블록을 다시 실행해 세운다.** 원장 값을 붙여 넣거나
#    TARGET_OK=1 을 손으로 치지 않는다 — 그 순간 이 가드는 확인이 아니라 선언이 된다.
if [ "${DRILL_MUTATION_OK:-0}" != 1 ] || ! lock_live; then
  echo "STOP: DRILL_MUTATION_OK=${DRILL_MUTATION_OK:-미설정}(shell=${SHELL_OK:-미설정} 락=${EXCLUSIVE_DRILL_LOCK_OK:-미설정} 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)) — 이 세션은 변경 자격이 없거나 락이 만료됐다. 자격증명 전환을 호출하지 않는다(§2.1 ⓪·§2.5 ⓪부터 다시)"
elif [ "$CONFIG_STATE" != ok ] && [ "$CONFIG_STATE" != fail ]; then
  echo "STOP: CONFIG_STATE=$CONFIG_STATE — 구성 판정 미완이라 자격증명 변경을 호출하지 않는다"
elif [ "$RESTORE_OK" = 1 ] && [ "$TARGET_OK" = 1 ] && [ "$SAFE_OK" = 1 ] \
     && [ "$SECRET_LIFECYCLE" = not-created ]; then
  # 요청 전 원장에 먼저 기록한다. 응답 유실·세션 종료여도 "생성 요청 가능성 있음"을 잃지 않는다.
  SECRET_LIFECYCLE=create-requested
  echo "SECRET_LIFECYCLE=$SECRET_LIFECYCLE"
  MODIFY_OUT=$(aws rds modify-db-instance --db-instance-identifier "$DRILL_DB" \
    --manage-master-user-password --apply-immediately \
    --query 'DBInstance.DBInstanceArn' --output text 2>&1)
  MODIFY_RC=$?
  if [ "$MODIFY_RC" -eq 0 ]; then
    echo "자격증명 전환 요청 접수: $MODIFY_OUT"
  elif printf %s "$MODIFY_OUT" | grep -Eqi \
    "RequestTimeout|Read timeout|Connect timeout|Connection reset|Could not connect to the endpoint|EndpointConnectionError|TLS handshake timeout"; then
    echo "STOP: 전송 불명(transport) — 재전송하지 않고 아래 읽기 전용 reconciliation으로 확인"
  else
    # 복원 호출과 같은 규율이다: AWS가 **응답한** 거부(AccessDenied·InvalidDBInstanceState 등)는 생성 증거가 아니다.
    # 이때 create-requested로 두면 시크릿이 없는데 ARN도 없어 §10.8이 영구히 unknown → (d)를 **거짓 FAIL**로 만든다.
    SECRET_LIFECYCLE=not-created
    echo "STOP: 자격증명 전환 확정 거부 rc=$MODIFY_RC $MODIFY_OUT"
    echo "      → 시크릿 미생성이 확정이므로 SECRET_LIFECYCLE=not-created로 수렴(원장 기입). 드릴은 §10으로 간다"
  fi
else
  echo "자격증명 생성 호출 안 함: RESTORE_OK=$RESTORE_OK TARGET_OK=$TARGET_OK SAFE_OK=$SAFE_OK lifecycle=$SECRET_LIFECYCLE"
fi

# 신규 요청 직후와 재진입 모두 이 읽기 전용 reconciliation을 실행한다.
# ⚠️ 시크릿 ARN 채택은 **이번 복원본의 소유권** 위에서만 한다. 고정 식별자가 다른 실행의 드릴 DB에
#    점유된 재진입에서 이 게이트가 없으면, 그 DB의 마스터 시크릿을 DRILL_SECRET으로 잡아
#    §8.1이 임시 role에 GetSecretValue 권한을 주고 §10.8이 삭제까지 시도한다.
SECRET_RECON_OK=0
if [ "$RESTORE_OK" = 1 ] && [ "$TARGET_OK" = 1 ] && [ -n "$RESTORE_EXPECTED_ARN" ]; then
  SECRET_RECON_OK=1
else
  echo "STOP: 소유권 미확립(RESTORE_OK=$RESTORE_OK TARGET_OK=$TARGET_OK expected_arn=$RESTORE_EXPECTED_ARN) — 시크릿 ARN을 채택하지 않는다"
fi

case "$SECRET_LIFECYCLE" in
  create-requested|arn-known)
    if [ "$SECRET_RECON_OK" = 1 ]; then
    DEADLINE=$(( $(date -u +%s) + 900 ))                       # 15분 상한
    while :; do
      CREDS_OBS=$(aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" \
        --query 'DBInstances[0].[DBInstanceStatus,MasterUserSecret.SecretStatus,MasterUserSecret.SecretArn,DBInstanceArn]' \
        --output text 2>&1)
      CREDS_OBS_RC=$?
      ST=$(printf %s "$CREDS_OBS" | awk '{print $1}')
      SS=$(printf %s "$CREDS_OBS" | awk '{print $2}')
      SECRET_ARN_OBS=$(printf %s "$CREDS_OBS" | awk '{print $3}')
      DB_ARN_OBS=$(printf %s "$CREDS_OBS" | awk '{print $4}')
      echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) status=$ST secret=$SS arn=$SECRET_ARN_OBS db=$DB_ARN_OBS rc=$CREDS_OBS_RC"
      if [ "$CREDS_OBS_RC" -eq 0 ] && [ "$DB_ARN_OBS" = "$RESTORE_EXPECTED_ARN" ] \
         && [ "$ST" = available ] && [ "$SS" = active ] \
         && [ -n "$SECRET_ARN_OBS" ] && [ "$SECRET_ARN_OBS" != None ] \
         && [ "$SECRET_ARN_OBS" != "$PROD_SECRET" ]; then
        DRILL_SECRET="$SECRET_ARN_OBS"
        # (DB ARN, secret ARN) 튜플을 원장에 남긴다 — DB가 지워진 뒤 §10.8은 이 튜플로만 소유권을 말할 수 있다.
        SECRET_OWNER_DB_ARN="$DB_ARN_OBS"
        SECRET_PROVENANCE_OK=1
        SECRET_LIFECYCLE=arn-known
        CREDS_OK=1
        break
      fi
      if [ "$(date -u +%s)" -ge "$DEADLINE" ]; then echo "STOP: 15분 상한 초과"; break; fi
      sleep 30
    done
    fi
    ;;
  *) echo "자격증명 reconciliation 생략: lifecycle=$SECRET_LIFECYCLE" ;;
esac

if [ "$CREDS_OK" = 1 ]; then
  date -u +%Y-%m-%dT%H:%M:%SZ                                # ← T_creds_ready (원장 기입)
  echo "SECRET_LIFECYCLE=$SECRET_LIFECYCLE DRILL_SECRET=$DRILL_SECRET"
  echo "SECRET_PROVENANCE_OK=$SECRET_PROVENANCE_OK SECRET_OWNER_DB_ARN=$SECRET_OWNER_DB_ARN  # ← 둘 다 원장에 기입"
else
  echo "STOP: 자격증명 미준비 — T_creds_ready를 찍지 않는다. 중단 → §10 정리 게이트"
fi
```

`SecretStatus`는 `creating` → **`active`** 로 간다(`rotating`·`impaired`도 존재). 이 전환 시간은 **DB 복원 검증 소요시간**에 포함하며 서비스 RTO로 해석하지 않는다.

> **불채택 대안**: 복원본은 원본의 마스터 자격증명을 승계하므로 운영 시크릿 값으로 접속할 수도 있다. 운영 자격증명을 드릴 경로에 끌어들이고 운영 시크릿 로테이션과 얽히므로 **쓰지 않는다.**

---

## 8. (g) 검증 접속·쿼리 → `T_verified`

### 8.1 임시 IAM role

기존 실행 role의 Secrets 정책은 **운영 시크릿 ARN 하나로 한정**돼 있어 드릴 시크릿을 읽지 못한다. 운영 role에 권한을 붙였다 떼는 것보다 **드릴 전용 role을 만들고 정리 게이트에서 지우는** 편이 최소권한·원복 모두 깔끔하다.

**이 가드는 이 런북에서 가장 중요한 하나다.** `DRILL_SECRET`이 어떤 이유로든 **운영 ARN과 같아지면**(§7을 상한 초과로 빠져나온 뒤 값이 섞이거나, 재진입 세션에서 잘못 붙여 넣는 경우) 드릴 role에 **운영 마스터 비밀번호 읽기 권한**이 붙고, 그 role은 TD-B의 execution role이라 곧바로 운영 비밀번호가 검증 컨테이너로 주입된다 — 임시 role을 따로 만든 이유 자체가 무너진다. 그래서 **조건이 참일 때만** 정책 파일 생성부터 role 생성까지 전부 실행한다.

```bash
# **IAM 문서·목록은 골라 보지 않고 통째로 대조한다.** 선택 투영(`Statement[0].Principal.Service`,
# `AttachedPolicies[?PolicyArn==...]`)은 **고르지 않은 것을 못 본다** — 같은 Statement에 `Principal.AWS`를
# 얹거나 `AdministratorAccess`를 덧붙여도 투영값은 기대와 똑같이 나온다(r31 codex-cli·codex-ide 실측,
# 병합 중 재현: `ROLE_RESUME_OK=1`). 이 드릴의 role은 `trust.json`·`drill-secret.json`대로 **정확히 하나의
# 형태로만** 만들어지므로 **허용 목록 정확 일치**가 옳은 기준이다. 불일치는 **변경하지 않고** §10으로 보낸다.
# ⚠️ IAM이 저장 문서를 그대로 돌려주는지는 §2.3 ⓜ 미확인 — 정규화 차이로 불일치가 나면 fail-closed다
#    (변경하지 않고 멈춘다). 드릴 당일 첫 `get-role` 응답을 증거로 남겨 이 항목을 닫는다.
json_eq() {  # json_eq <기대 JSON 문자열> ; stdin = 관측 JSON → 완전히 같으면 0
  python3 -c 'import json,sys
try:
    want = json.loads(sys.argv[1]); got = json.load(sys.stdin)
except Exception:
    sys.exit(1)
sys.exit(0 if want == got else 1)' "$1"
}
TRUST_WANT='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ecs-tasks.amazonaws.com"},"Action":"sts:AssumeRole"}]}'
ATTACHED_WANT='["arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"]'
INLINE_NAMES_WANT='["read-drill-db-secret"]'
# 인라인 문서는 `DRILL_SECRET`이 들어가므로 여기서 조립한다(파이썬 json.dumps로 이스케이프까지 맡긴다).
INLINE_WANT=$(python3 -c 'import json,sys
print(json.dumps({"Version":"2012-10-17","Statement":[{"Sid":"ReadDrillDBPassword","Effect":"Allow",
  "Action":"secretsmanager:GetSecretValue","Resource":sys.argv[1]}]}))' "$DRILL_SECRET")

ROLE_READY=0
ROLE_CREATE_STATE=unknown
ROLE_RESUME_OK=0   # present인데 **이번 드릴이 만든 것**이면 정책 연결부터 이어 간다(아래 근거)
# 태그까지 함께 읽는다 — `present`를 무조건 "남의 것"으로 보면 **이 드릴이 방금 만든 부분 생성 role**을
# 완결할 길이 없다. r27이 넣은 IAM 3호출 사이 `lock_live` 재확인이 create와 attach 사이에서 막으면
# role만 있고 정책이 없는 상태가 남는데, 다음 세션은 `get-role` 성공 → `present` → 진입 조건
# `absent`에 걸려 **영원히 못 들어간다**. 그러면 `ROLE_READY=0`이 고정돼 §8.2·§8.3이 닫히고,
# **유료 복원본을 띄운 채 검증을 포기**하게 된다(r29 claude-ide 실측). 소유권이 이번 토큰으로
# 확인되면 재개는 롤백이 아니라 정상 변경이므로 막을 이유가 없다.
# **trust policy까지 함께 읽는다.** 태그 3요소는 §2.7 `OWNED`(= 삭제해도 되는가)의 기준이지, **새 권한을
# 부여해도 되는가**의 기준이 아니다 — 둘은 같지 않다(r30 codex-cli). 같은 태그를 달았지만 trust가
# `ec2.amazonaws.com`으로 바뀐 role에 드릴 DB 시크릿 읽기 정책을 붙이면, **EC2가 assume해 비밀번호를 읽는
# 권한 부여 창**이 열린다. readiness가 나중에 거부해도 그때는 이미 붙은 뒤다. 그래서 정책 단계에 들어가기
# 전에 **trust 문서 전체**가 `TRUST_WANT`와 정확히 같을 것을 요구한다 — 선택한 필드(tuple)만 맞춰 보면
# 같은 Statement에 얹은 `Principal.AWS`나 추가 Statement를 투영이 못 본다(r31). 불일치는 **변경하지 않고**
# §10 정리로 보낸다.
ROLE_CHECK=$(aws iam get-role --role-name "$DRILL_ROLE" \
  --query 'Role.[Arn,Tags[?Key==`purpose`].Value|[0],Tags[?Key==`drill-token`].Value|[0]]' --output text 2>&1)
ROLE_CHECK_RC=$?
if [ "$ROLE_CHECK_RC" -eq 0 ]; then
  ROLE_CREATE_STATE=present
  R_ARN_OBS=$(printf %s "$ROLE_CHECK" | awk 'NR==1{print $1}')
  R_PURPOSE_OBS=$(printf %s "$ROLE_CHECK" | awk 'NR==1{print $2}')
  R_TOKEN_OBS=$(printf %s "$ROLE_CHECK" | awk 'NR==1{print $3}')
  # trust는 **문서 전체**를 따로 받아 정확 비교한다(위 `json_eq` 근거).
  R_TRUST_DOC=$(aws iam get-role --role-name "$DRILL_ROLE" \
    --query 'Role.AssumeRolePolicyDocument' --output json 2>&1); R_TRUST_RC=$?
  R_TRUST_MATCH=0
  [ "$R_TRUST_RC" -eq 0 ] && printf %s "$R_TRUST_DOC" | json_eq "$TRUST_WANT" && R_TRUST_MATCH=1
  # 소유(태그 3요소) **∧** 구성(trust 문서 전체 일치)이 맞아야 재개한다.
  if [ "$R_TRUST_MATCH" = 1 ] \
     && [ -n "${ACCOUNT_ID:-}" ] && [ "$R_ARN_OBS" = "arn:aws:iam::${ACCOUNT_ID}:role/$DRILL_ROLE" ] \
     && [ "$R_PURPOSE_OBS" = "rds-restore-drill" ] && [ -n "$TOKEN" ] && [ "$R_TOKEN_OBS" = "$TOKEN" ]; then
    ROLE_RESUME_OK=1
  fi
elif printf %s "$ROLE_CHECK" | grep -q "NoSuchEntity"; then
  ROLE_CREATE_STATE=absent
fi
echo "ROLE_CREATE_STATE=$ROLE_CREATE_STATE ROLE_RESUME_OK=$ROLE_RESUME_OK rc=$ROLE_CHECK_RC $ROLE_CHECK"
[ "$ROLE_CREATE_STATE" = present ] && [ "$ROLE_RESUME_OK" != 1 ] \
  && echo "      role이 있지만 이번 드릴 소유가 아니거나 trust가 기대와 다르다 — **아무것도 바꾸지 않고** §10 정리로 보낸다"

# `DRILL_MUTATION_OK`(= 셸·레포·단일 변경 작업 락·로컬 도구)을 맨 앞에 둔다 — 아래 3개 IAM 변경
# (create-role·attach·put)이 전부 이 블록 안에 있으므로 여기 하나로 세 호출이 함께 닫힌다.
# 셸만 보면 재진입에서 `CREDS_OK`가 읽기 전용으로 되살아나 **락 없이 role이 만들어진다.**
if [ "${DRILL_MUTATION_OK:-0}" = 1 ] \
   && lock_live \
   && [ "$CREDS_OK" = 1 ] && [ -n "$DRILL_SECRET" ] && [ "$DRILL_SECRET" != "None" ] \
   && [ -n "$PROD_SECRET" ] && [ "$DRILL_SECRET" != "$PROD_SECRET" ] \
   && [ "$DRILL_ROLE" = "linkpulse-restore-drill-exec" ] \
   && { [ "$ROLE_CREATE_STATE" = absent ] || [ "$ROLE_RESUME_OK" = 1 ]; }; then

  cat > "$DRILL_DIR/trust.json" <<'JSON'
{ "Version": "2012-10-17", "Statement": [
  { "Effect": "Allow", "Principal": { "Service": "ecs-tasks.amazonaws.com" },
    "Action": "sts:AssumeRole" } ] }
JSON

  cat > "$DRILL_DIR/drill-secret.json" <<JSON
{ "Version": "2012-10-17", "Statement": [
  { "Sid": "ReadDrillDBPassword", "Effect": "Allow",
    "Action": "secretsmanager:GetSecretValue", "Resource": "$DRILL_SECRET" } ] }
JSON

  # 정책 파일 검증 — **세 가지를 각각 성공 조건으로** 확인한 뒤에만 IAM 변경 3개를 실행한다.
  # `grep … && STOP || { create-role … }` 형태를 쓰지 않는 이유: grep은 **불일치(1)와 오류(2)를
  # 구분**하는데(파일 없음·읽기 실패 등이 2), 그 형태는 둘을 같은 `||` 분기로 합쳐
  # **"운영 ARN 없음"으로 오독하고 role을 만든다.** -F 로 정규식 해석도 끈다.
  grep -qF "\"Resource\": \"$DRILL_SECRET\"" "$DRILL_DIR/drill-secret.json"; DRILL_RC=$?   # 0 이어야 한다
  grep -qF "$PROD_SECRET" "$DRILL_DIR/drill-secret.json"; PROD_RC=$?                       # 1 이어야 한다
  echo "정책 파일 검사: drill_rc=$DRILL_RC(기대 0) prod_rc=$PROD_RC(기대 1 — 0=운영 ARN 발견, 2=grep 오류)"

  # **IAM 변경 3개는 각자 호출 직전에 `lock_live`를 다시 본다.** 블록 입구에서 한 번만 보면, 그 사이에
  # `create-role` → `get-role` 최대 3회 → `sleep 2` 최대 2회가 끼므로 **입구 통과 후 만료된 상태로**
  # attach·put이 실행될 수 있다(= 새 락 소유자와 겹친다). 중간에 만료되면 **롤백하지 않는다** —
  # 되돌리는 것도 변경이므로 락 없이 하지 않고, 폐기가 필요하면 §10이 자격 없이 지운다.
  # 대신 **락을 갱신한 뒤 남은 호출을 이어 가는 재개 경로**를 연다(위 `ROLE_RESUME_OK`).
  if [ -s "$DRILL_DIR/trust.json" ] && [ -s "$DRILL_DIR/drill-secret.json" ] \
     && [ "$DRILL_RC" -eq 0 ] && [ "$PROD_RC" -eq 1 ] && lock_live; then
    EXPECTED_ROLE_ARN="arn:aws:iam::${ACCOUNT_ID}:role/$DRILL_ROLE"
    if [ "$ROLE_CREATE_STATE" = absent ]; then
      CREATED_ROLE_ARN=$(aws iam create-role --role-name "$DRILL_ROLE" \
        --assume-role-policy-document "file://$DRILL_DIR/trust.json" \
        --tags Key=purpose,Value=rds-restore-drill Key=drill-token,Value="$TOKEN" \
        --query 'Role.Arn' --output text 2>&1)
      CREATE_ROLE_RC=$?
    else
      # 재개 — role은 이미 있고 **이번 토큰 소유로 확인됐다**(`ROLE_RESUME_OK=1`이 아니면 여기 못 온다).
      CREATED_ROLE_ARN="$EXPECTED_ROLE_ARN"; CREATE_ROLE_RC=0
      echo "재개: role이 이미 존재하고 이번 드릴 소유다 — create-role을 건너뛰고 정책부터 이어 간다"
    fi

    CREATED_ROLE_OK=0
    if [ "$CREATE_ROLE_RC" -eq 0 ] && [ "$CREATED_ROLE_ARN" = "$EXPECTED_ROLE_ARN" ]; then
      for attempt in 1 2 3; do
        CREATED_CHECK=$(aws iam get-role --role-name "$DRILL_ROLE" \
          --query 'Role.[Arn,Tags[?Key==`purpose`].Value|[0],Tags[?Key==`drill-token`].Value|[0]]' \
          --output text 2>&1)
        CREATED_CHECK_RC=$?
        C_ROLE_ARN=$(printf %s "$CREATED_CHECK" | awk '{print $1}')
        C_ROLE_PURPOSE=$(printf %s "$CREATED_CHECK" | awk '{print $2}')
        C_ROLE_TOKEN=$(printf %s "$CREATED_CHECK" | awk '{print $3}')
        if [ "$CREATED_CHECK_RC" -eq 0 ] && [ "$C_ROLE_ARN" = "$EXPECTED_ROLE_ARN" ] \
           && [ "$C_ROLE_PURPOSE" = "rds-restore-drill" ] && [ "$C_ROLE_TOKEN" = "$TOKEN" ]; then
          CREATED_ROLE_OK=1
          break
        fi
        [ "$attempt" -lt 3 ] && sleep 2
      done
    fi

    # **재개 경로에서는 이미 붙어 있는 것을 다시 붙이지 않는다.** `AttachRolePolicy`의 재연결 동작은
    # 1차 출처로 확인하지 않았으므로(§2.3 ⓛ) **멱등성을 가정하지 않는다** — 대신 현재 상태를 조회해
    # 없는 것만 호출한다. 조회는 읽기 전용이라 이 확인 자체가 새 위험을 만들지 않는다.
    #
    # **조회 실패를 "붙어 있지 않음"으로 읽지 않는다**(r30 codex-ide·claude-ide). 실패해도 정책은 이미
    # 붙어 있을 수 있으므로, 그 상태에서 호출하면 그것이 바로 위에서 금지한 "멱등성 가정"이다. 이 문서의
    # 다른 조회 9곳과 같은 관용구를 쓴다 — **30초 간격 총 3회, 그래도 실패하면 `unknown`**. `unknown`이면
    # 두 IAM 변경을 **하나도** 실행하지 않는다(락을 갱신해 다시 오는 것이 이 절의 재개 경로다).
    NEED_ATTACH=1; NEED_PUT=1
    if [ "$ROLE_CREATE_STATE" = present ]; then
      NEED_ATTACH=unknown; NEED_PUT=unknown
      # 관리형 정책은 **연결 목록 전체**를 기대 하나와 대조한다 — 기대 ARN만 필터하면
      # `AdministratorAccess`가 함께 붙어 있어도 투영값은 같아 최소권한이 조용히 깨진다(r31 codex-cli).
      for A_TRY in 1 2 3; do
        A_NOW=$(aws iam list-attached-role-policies --role-name "$DRILL_ROLE" \
          --query 'AttachedPolicies[].PolicyArn' --output json 2>&1); A_NOW_RC=$?
        if [ "$A_NOW_RC" -eq 0 ]; then
          if printf %s "$A_NOW" | json_eq "$ATTACHED_WANT"; then NEED_ATTACH=0
          elif printf %s "$A_NOW" | json_eq '[]'; then NEED_ATTACH=1
          else
            NEED_ATTACH=unknown
            echo "STOP: 연결된 관리형 정책이 기대 목록과 다르다(예상 밖 정책이 붙어 있다) — 변경하지 않는다: $A_NOW"
          fi
          break
        fi
        echo "관리형 정책 조회 실패($A_TRY/3): $A_NOW"
        [ "$A_TRY" -lt 3 ] && sleep 30
      done
      # 인라인 정책도 **이름 목록 전체 + 문서 전체**를 대조한다. 기대 Sid만 필터하면 다른 Sid로 얹은
      # 운영 시크릿 Allow나 드릴 시크릿 Deny를 못 본다(r31 codex-cli). 문서가 다르면 `put`으로 교정하지만,
      # **이름 목록이 다르면**(예상 밖 인라인 정책 존재) 교정 대상이 아니므로 변경하지 않고 멈춘다.
      for P_TRY in 1 2 3; do
        P_NAMES=$(aws iam list-role-policies --role-name "$DRILL_ROLE" \
          --query 'PolicyNames' --output json 2>&1); P_NAMES_RC=$?
        [ "$P_NAMES_RC" -ne 0 ] && { echo "인라인 이름 조회 실패($P_TRY/3): $P_NAMES"; [ "$P_TRY" -lt 3 ] && sleep 30; continue; }
        if printf %s "$P_NAMES" | json_eq '[]'; then NEED_PUT=1; break; fi
        if ! printf %s "$P_NAMES" | json_eq "$INLINE_NAMES_WANT"; then
          NEED_PUT=unknown
          echo "STOP: 인라인 정책 이름 목록이 기대와 다르다 — 변경하지 않는다: $P_NAMES"; break
        fi
        P_DOC=$(aws iam get-role-policy --role-name "$DRILL_ROLE" --policy-name read-drill-db-secret \
          --query 'PolicyDocument' --output json 2>&1); P_DOC_RC=$?
        if [ "$P_DOC_RC" -eq 0 ]; then
          if printf %s "$P_DOC" | json_eq "$INLINE_WANT"; then NEED_PUT=0; else NEED_PUT=1; fi
          break
        fi
        echo "인라인 문서 조회 실패($P_TRY/3): $P_DOC"
        [ "$P_TRY" -lt 3 ] && sleep 30
      done
      echo "재개 판정: NEED_ATTACH=$NEED_ATTACH NEED_PUT=$NEED_PUT"
      # **막기만 하고 이어 갈 길을 안 주면 그것이 또 데드엔드다**(r29 claude-ide #2와 같은 유형 — 이
      # `unknown` 분기를 만들면서 같은 실수를 반복했다). 탈출구를 셋 다 적는다.
      if [ "$NEED_ATTACH" = unknown ] || [ "$NEED_PUT" = unknown ]; then
        echo "STOP: 현재 정책 상태를 확인하지 못했다 — IAM 변경을 하나도 실행하지 않는다"
        echo "      원인은 둘 중 하나이고 **바로 위 줄이 어느 쪽인지 지목한다**:"
        echo "      (가) 예상 밖 구성(기대와 다른 관리형/인라인 목록) → 교정 대상이 아니다. ③으로 간다"
        echo "      (나) 3회 재조회 실패(스로틀링·네트워크·권한) → ①②를 본다"
        echo "      ① 일시적 오류(스로틀링·네트워크)면 락을 확인하고 **이 블록을 다시 실행**한다 — 재개는 멱등이다"
        echo "      ② AccessDenied가 반복되면 §2.5 ⑤ 권한표의 iam:ListAttachedRolePolicies·iam:ListRolePolicies·iam:GetRolePolicy를 확인한다"
        echo "      ③ 그래도 확인되지 않으면 **§10 정리 게이트로 진입한다**(복원본을 폐기하고 드릴을 (b)·(c) 재료 없이 종료)."
        echo "         정리는 이 자격도 락도 요구하지 않으므로 언제든 열려 있다(§10.2)"
      fi
    fi

    # `unknown`이면 아래 두 변경을 통째로 건너뛴다 — 확인되지 않은 상태에서는 호출하지 않는다.
    # (한 줄로 두는 이유: 줄 끝 `\ `로 이어 쓰면 회귀 검사의 "게이트 양성 12건" 패턴에 섞인다. 이 자리는
    #  게이트가 아니라 **호출 직전 인접 재확인**이라 개수 의미가 달라진다.)
    if [ "$CREATED_ROLE_OK" = 1 ] && lock_live && [ "$NEED_ATTACH" != unknown ] && [ "$NEED_PUT" != unknown ]; then
      if [ "$NEED_ATTACH" = 0 ] || aws iam attach-role-policy --role-name "$DRILL_ROLE" \
        --policy-arn arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy; then
        if [ "$NEED_PUT" = 0 ] || { lock_live && aws iam put-role-policy --role-name "$DRILL_ROLE" \
          --policy-name read-drill-db-secret --policy-document "file://$DRILL_DIR/drill-secret.json"; }; then
          echo "role 정책 구성 완료 — readiness는 아래 읽기 전용 reconciliation에서 판정"
        else
          echo "STOP: 인라인 정책 생성 실패 또는 그 직전 락 만료(만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)) — 부분 생성된 role은 §10에서 정리한다"
        fi
      else
        echo "STOP: 관리형 정책 연결 실패 — 인라인 정책은 만들지 않고 §10에서 정리한다"
      fi
    else
      echo "STOP: create-role 실패, 반환 ARN·purpose·drill-token 확인 실패, 또는 그 직후 락 만료(rc=$CREATE_ROLE_RC arn=$CREATED_ROLE_ARN 만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)) — 정책을 연결하지 않았다"
    fi
  else
    # 이 `else`는 정책 파일 검증 실패뿐 아니라 **그 직후 `lock_live` 실패**로도 도달한다 — 둘을 구분해 적는다.
    echo "STOP: 정책 파일 생성/검증 실패 또는 create-role 직전 락 만료(drill_rc=$DRILL_RC prod_rc=$PROD_RC 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)) — role을 만들지 않았다(운영 시크릿 권한이 붙을 수 있는 경로)"
  fi
else
  # `락=${EXCLUSIVE_DRILL_LOCK_OK}`는 ⓪ 실행 시점의 값이라 **만료 뒤에도 1**이다 — 그것만 찍으면
  # "자격은 정상"이라 단언하면서 막는 셈이 된다. 만료 판정을 같은 줄에 넣어 원인을 지목한다.
  echo "STOP: 변경 자격 없음(DRILL_MUTATION_OK=${DRILL_MUTATION_OK:-미설정}, shell=${SHELL_OK:-미설정} 락=${EXCLUSIVE_DRILL_LOCK_OK:-미설정} 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s))·드릴 시크릿 ARN 미확정·운영 ARN과 동일·role 부재 미확정 중 하나 — role을 만들지 않았다"
fi
echo "role 생성 단계 종료 — 현재 ROLE_READY=$ROLE_READY"
```

신규 생성 직후와 세션 재진입 모두 **§8.1의 두 블록을 순서대로 실행해** readiness를 다시 도출한다 — 아래 블록은 앞 블록이 정의하는 `json_eq`와 `*_WANT` 넷을 쓰므로 **단독으로 붙여 넣으면 안 된다**(그래서 첫 줄에 전제 검사를 둔다). role이 `present`라는 사실만으로는 부족하며, **고정 ARN + 이번 태그 + trust policy + 관리형 정책 + 인라인 정책의 정확한 드릴 시크릿 ARN**이 모두 일치해야 한다.

```bash
# readiness도 **선택 투영이 아니라 전체 대조**다(§8.1 생성 블록의 `json_eq` 근거와 같다).
# 기대한 것의 **존재**만 확인하면 `AmazonECSTaskExecutionRolePolicy + AdministratorAccess`나
# 기대 Statement + 운영 시크릿 Allow가 붙은 role도 ready가 된다 — 전자는 최소권한을 깨고 후자는
# 비밀 노출을 숨긴다(r31 codex-cli). trust·관리형 목록·인라인 이름·인라인 문서를 **네 개 다 전체 비교**한다.
ROLE_READY=0
# **관측 플래그는 매 실행 비운다**(§3.4의 규율). `if` 안에서만 0으로 두면, 같은 셸에서 두 번째 실행의
# `get-role`이 실패했을 때 1차 실행의 1이 그대로 남아 `role_rc=255 trust=1 attached=1`처럼
# **"이번 실행이 얻은 값"과 구분되지 않는 출력**이 된다(판정은 안전하지만 읽는 사람이 오독한다).
TRUST_OK=0; ATTACHED_OK=0; INLINE_NAMES_OK=0; INLINE_OK=0
# **전제: 이 블록은 §8.1 첫 블록 뒤에만 성립한다.** `json_eq`·`*_WANT` 넷이 없으면 네 비교가 전부
# 실패해 `ROLE_READY=0`이 된다 — 방향은 fail-closed지만 출력은 "role은 멀쩡히 조회됐는데 네 문서가
# 전부 기대와 다르다"로 읽혀 **정상 role을 의심하고 §10 폐기를 검토**하게 만든다. 막는 것과 원인을
# 지목하는 것은 다르므로(r30 `unknown` STOP과 같은 규율) 먼저 전제를 확인하고 원인을 지목한다.
READY_PRECOND_OK=0
if command -v json_eq >/dev/null 2>&1 && [ -n "${TRUST_WANT:-}" ] && [ -n "${ATTACHED_WANT:-}" ] \
   && [ -n "${INLINE_NAMES_WANT:-}" ] && [ -n "${INLINE_WANT:-}" ]; then
  READY_PRECOND_OK=1
else
  echo "STOP: 비교 헬퍼 json_eq 또는 기대값 *_WANT 4개(TRUST/ATTACHED/INLINE_NAMES/INLINE)가 이 셸에 없다"
  echo "      → **IAM 상태 문제가 아니다.** §8.1 **첫 블록부터** 순서대로 실행한 뒤 여기로 돌아온다(재개는 멱등)"
fi
EXPECTED_ROLE_ARN="arn:aws:iam::${ACCOUNT_ID}:role/$DRILL_ROLE"
ROLE_META=""; ROLE_META_RC=-1        # -1 = 미호출. **수치 sentinel**을 쓴다 — 아래 `-eq` 비교에 문자열이
                                     # 들어가면 status 2가 되어 `lock_live`가 함수로 피했던 함정이 되살아난다
if [ "$READY_PRECOND_OK" = 1 ]; then
  ROLE_META=$(aws iam get-role --role-name "$DRILL_ROLE" \
    --query 'Role.[Arn,Tags[?Key==`purpose`].Value|[0],Tags[?Key==`drill-token`].Value|[0]]' --output text 2>&1)
  ROLE_META_RC=$?
fi
if [ "$READY_PRECOND_OK" = 1 ] && [ "$ROLE_META_RC" -eq 0 ]; then
  ROLE_ARN_OBS=$(printf %s "$ROLE_META" | awk 'NR==1{print $1}')
  ROLE_PURPOSE_OBS=$(printf %s "$ROLE_META" | awk 'NR==1{print $2}')
  ROLE_TOKEN_OBS=$(printf %s "$ROLE_META" | awk 'NR==1{print $3}')
  TRUST_DOC=$(aws iam get-role --role-name "$DRILL_ROLE" \
    --query 'Role.AssumeRolePolicyDocument' --output json 2>&1); TRUST_RC=$?
  ATTACHED_ALL=$(aws iam list-attached-role-policies --role-name "$DRILL_ROLE" \
    --query 'AttachedPolicies[].PolicyArn' --output json 2>&1); ATTACHED_RC=$?
  INLINE_NAMES=$(aws iam list-role-policies --role-name "$DRILL_ROLE" \
    --query 'PolicyNames' --output json 2>&1); INLINE_NAMES_RC=$?
  INLINE_DOC=$(aws iam get-role-policy --role-name "$DRILL_ROLE" \
    --policy-name read-drill-db-secret --query 'PolicyDocument' --output json 2>&1); INLINE_RC=$?
  [ "$TRUST_RC" -eq 0 ]        && printf %s "$TRUST_DOC"    | json_eq "$TRUST_WANT"        && TRUST_OK=1
  [ "$ATTACHED_RC" -eq 0 ]     && printf %s "$ATTACHED_ALL" | json_eq "$ATTACHED_WANT"     && ATTACHED_OK=1
  [ "$INLINE_NAMES_RC" -eq 0 ] && printf %s "$INLINE_NAMES" | json_eq "$INLINE_NAMES_WANT" && INLINE_NAMES_OK=1
  [ "$INLINE_RC" -eq 0 ]       && printf %s "$INLINE_DOC"   | json_eq "$INLINE_WANT"       && INLINE_OK=1
  if [ "$ROLE_ARN_OBS" = "$EXPECTED_ROLE_ARN" ] \
     && [ "$ROLE_PURPOSE_OBS" = rds-restore-drill ] && [ -n "$TOKEN" ] && [ "$ROLE_TOKEN_OBS" = "$TOKEN" ] \
     && [ "$TRUST_OK" = 1 ] && [ "$ATTACHED_OK" = 1 ] \
     && [ "$INLINE_NAMES_OK" = 1 ] && [ "$INLINE_OK" = 1 ] \
     && [ -n "$DRILL_SECRET" ] && [ "$DRILL_SECRET" != "$PROD_SECRET" ]; then
    ROLE_READY=1
  fi
fi
echo "ROLE_READY=$ROLE_READY precond=$READY_PRECOND_OK role_rc=$ROLE_META_RC trust=$TRUST_OK attached=$ATTACHED_OK inline_names=$INLINE_NAMES_OK inline_doc=$INLINE_OK"
```

> RDS 관리 시크릿은 AWS 관리형 KMS 키(`aws/secretsmanager`)라 **별도 kms 정책이 필요 없다**(운영 `iam.tf`의 기존 판단과 동일). **task role은 부여하지 않는다.**

### 8.2 임시 TD-B 등록

TD-A와 **네 가지만 다르다**: family(`$TD_B`), `executionRoleArn`(임시 role), `PGHOST`(**드릴 인스턴스** 엔드포인트), `PGPASSWORD`의 `valueFrom`(**드릴 시크릿 ARN**`:password::`). 이미지 digest·로그그룹·스트림 접두사는 그대로 쓴다.

```bash
TD_B_READY=0
TD_B_ARN=""
DRILL_ADDR=$(aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" \
  --query 'DBInstances[0].Endpoint.Address' --output text)
# A_CODES와 B_CODE는 §3.2·§4.3에서 검증한 실제 값을 그대로 쓴다.
# 단 둘의 인용 형태가 다르다: A_CODES는 사람이 값 쪽에 따옴표를 넣어 적은 목록("'a','b'")이고,
# B_CODE는 §4.3 API 응답에서 자동 채택된 **따옴표 없는 raw base62**다. 원장·증거값은 raw로 두어야
# 하므로(§2.1 tuple) SQL 주입 위치에는 여기서 만든 리터럴 B_CODE_SQL만 쓴다.
# `case`로 검사한다 — `grep -Eq '^…$'`는 **줄 단위**라 `abc123\nnot-base62` 같은 다중행 값도
# 첫 줄만 맞으면 통과한다(재진입 원장을 손으로 붙여 넣는 경로에는 API 파서의 보장이 없다).
B_CODE_SQL=""
case "$B_CODE" in
  ''|*[!0-9A-Za-z]*)
    echo "STOP: B_CODE가 base62(영숫자) 한 줄이 아니다 — SQL 리터럴을 만들지 않는다: '$B_CODE'" ;;
  *) B_CODE_SQL="'$B_CODE'" ;;
esac

# A_CODES도 §3.2와 같은 형태 게이트를 다시 세운다 — 재진입에서는 §3.2 블록을 거치지 않고
# 원장에서 붙여 넣으므로, 플래그를 물려받지 않고 이 자리에서 재도출한다.
A_CODES_OK=0
case "$A_CODES" in
  ''|*[!0-9A-Za-z"'"',']*) ;;
  *) printf %s "$A_CODES" \
       | grep -Eq "^'[0-9A-Za-z]+'(,'[0-9A-Za-z]+')*$" && A_CODES_OK=1 ;;
esac
[ "$A_CODES_OK" = 1 ] || echo "STOP: A_CODES 형태 오류 — 값 쪽 작은따옴표 누락·쉼표 뒤 공백 의심(\"'aaa111','bbb222'\"): '$A_CODES'"

# **이 절은 `VALUES_OK`(§2.5 ⑧ 계보)를 요구하지 않는다.** 재진입은 ⑧을 거치지 않는 것이 정상 경로인데
# (§2.5 머리말), §8.1 IAM과 §8.3 run-task는 이미 `DRILL_MUTATION_OK`만 본다. 그 사이의 §8.2만 계보를
# 요구하면 **과금 중인 복원본을 둔 재진입이 §8.1에서 role을 만든 뒤 여기서 막힌다** — 검증을 완주하지
# 못한 채 드릴 DB 요금만 계속 나간다. 대신 §8.2가 td-b.json에 **실제로 넣는 값**을 여기서 개별 검증한다.
TD_B_ARGS_OK=1
case "$ACCOUNT_ID" in ''|*[!0-9]*) TD_B_ARGS_OK=0; echo "STOP: ACCOUNT_ID가 비었거나 숫자가 아니다: '$ACCOUNT_ID'" ;; esac
# digest는 **비어 있지 않다**만으로 부족하다 — 플레이스홀더도 형식만 맞으면 TD 등록은 성공하고
# 태스크 기동 때 CannotPullContainerError로 늦게 터진다(§2.5 ⑧과 같은 이유).
case "$DIGEST" in
  sha256:0000000000000000000000000000000000000000000000000000000000000000)
    TD_B_ARGS_OK=0; echo "STOP: DIGEST가 플레이스홀더 그대로다 — §2.5 ⑦의 실측값으로 치환" ;;
  sha256:*)
    D_HEX=${DIGEST#sha256:}
    case "$D_HEX" in
      *[!0-9a-f]*) TD_B_ARGS_OK=0; echo "STOP: DIGEST에 16진수가 아닌 문자가 있다: '$DIGEST'" ;;
      *) [ "${#D_HEX}" -eq 64 ] || { TD_B_ARGS_OK=0; echo "STOP: DIGEST hex 길이가 64가 아니다(${#D_HEX}): '$DIGEST'"; } ;;
    esac ;;
  *) TD_B_ARGS_OK=0; echo "STOP: DIGEST가 sha256:<64 hex> 형식이 아니다: '$DIGEST'" ;;
esac
# `PROD_SECRET`이 이 목록에 있는 이유는 td-b.json에 들어가서가 아니라 **들어가면 안 되는 것을 막는
# 기준값**이기 때문이다. 비어 있으면 아래 `! grep -qF "$PROD_SECRET"`가 **빈 패턴 = 전건 일치**가 되어
# (`grep -qF "" <비지 않은 파일>` rc=0) 정상 파일에서도 거짓이 되고, 운영자는 `DRILL_SECRET`을 의심하라는
# **틀린 STOP**을 받는다(r26 claude-ide 실측). §2.5 ⑧의 `VALUES_OK` 루프가 검사하던 값이라 여기로 옮긴다.
for v in TOKEN PROD_USER PROD_DB LOG_GROUP DRILL_ROLE PROD_SECRET AWS_REGION; do
  eval "val=\$$v"
  if [ -z "$val" ] || [ "$val" = "None" ]; then TD_B_ARGS_OK=0; echo "STOP: $v 가 비었다 — td-b.json 생성·검증에 그대로 쓰인다"; fi
done   # ← break 하지 않고 끝까지 돌려 빈 값을 한 번에 전부 드러낸다(§2.5 ⑧과 같은 관용구)

if [ "${DRILL_MUTATION_OK:-0}" != 1 ] || ! lock_live \
   || [ "$TD_B_ARGS_OK" != 1 ] || [ "$RESTORE_OK" != 1 ] || [ "$TARGET_OK" != 1 ] \
   || [ "$CREDS_OK" != 1 ] || [ "$ROLE_READY" != 1 ] \
   || [ "$SENTINEL_BOUNDARY_OK" != 1 ] \
   || [ "$TD_B" != "linkpulse-restore-drill-b" ] \
   || [ -z "$DRILL_ADDR" ] || [ "$DRILL_ADDR" = "None" ] || [ -z "$A_CODES" ] || [ -z "$B_CODE" ] \
   || [ "$A_CODES_OK" != 1 ] || [ -z "$B_CODE_SQL" ] \
   || [ -z "$DRILL_SECRET" ] || [ "$DRILL_SECRET" = "$PROD_SECRET" ]; then
  # STOP 메시지는 **실제로 막은 값을 전부** 출력한다 — 통과한 조건만 찍으면 원인을 못 찾는다.
  echo "STOP: TD-B 등록 전제 미충족 — DRILL_MUTATION_OK=${DRILL_MUTATION_OK:-미설정} TD_B_ARGS_OK=$TD_B_ARGS_OK RESTORE_OK=$RESTORE_OK TARGET_OK=$TARGET_OK CREDS_OK=$CREDS_OK ROLE_READY=$ROLE_READY SENTINEL_BOUNDARY_OK=$SENTINEL_BOUNDARY_OK A_CODES_OK=$A_CODES_OK DRILL_ADDR='${DRILL_ADDR:-미설정}' B_CODE_SQL='${B_CODE_SQL:-미설정}' DRILL_SECRET='${DRILL_SECRET:-미설정}' 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)"
  echo "      아래 블록(td-b.json 생성·등록)을 실행하지 않는다. 재진입이면 §2.5 ⓪ → §6·§7의 읽기 전용 reconciliation → §8.1 순서로 되살린다"
else

cat > "$DRILL_DIR/td-b.json" <<JSON
{
  "family": "$TD_B",
  "requiresCompatibilities": ["FARGATE"],
  "networkMode": "awsvpc",
  "cpu": "256",
  "memory": "512",
  "executionRoleArn": "arn:aws:iam::${ACCOUNT_ID}:role/$DRILL_ROLE",
  "runtimePlatform": { "operatingSystemFamily": "LINUX", "cpuArchitecture": "ARM64" },
  "containerDefinitions": [
    {
      "name": "psql",
      "image": "postgres:16@$DIGEST",
      "essential": true,
      "environment": [
        { "name": "PGHOST", "value": "$DRILL_ADDR" },
        { "name": "PGPORT", "value": "5432" },
        { "name": "PGUSER", "value": "$PROD_USER" },
        { "name": "PGDATABASE", "value": "$PROD_DB" },
        { "name": "PGSSLMODE", "value": "require" },
        { "name": "PGCONNECT_TIMEOUT", "value": "15" }
      ],
      "secrets": [
        { "name": "PGPASSWORD", "valueFrom": "${DRILL_SECRET}:password::" }
      ],
      "command": ["psql","-v","ON_ERROR_STOP=1","-X","-A","-F","|","-P","pager=off",
        "-c","SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"') AS t_verify_db, count(*) AS row_count, coalesce(sum(clicks),0) AS clicks_sum, to_char(max(created_at) AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"') AS max_created_at, count(*) FILTER (WHERE code IN ($A_CODES)) AS sentinel_a_hits, count(*) FILTER (WHERE code IN ($B_CODE_SQL)) AS sentinel_b_hits FROM links;",
        "-c","SELECT name, setting, source FROM pg_settings WHERE name = 'rds.force_ssl';"],
      "logConfiguration": {
        "logDriver": "awslogs",
        "options": {
          "awslogs-group": "$LOG_GROUP",
          "awslogs-region": "$AWS_REGION",
          "awslogs-stream-prefix": "$TOKEN"
        }
      }
    }
  ]
}
JSON

# 치환 확인: 드릴 ARN이 들어갔고 운영 ARN은 없어야 한다(`:password::`만 세는 grep은 이 구멍을 못 잡는다).
# **`[ -n "$PROD_SECRET" ]`를 앞에 둬 방어를 이중화한다** — 이 줄은 운영 시크릿이 드릴 TD로 새는 것을 막는
# 마지막 검사라 빈 값 의미론(`grep -qF ""` = 전건 일치)에 기대면 안 된다. 위 `TD_B_ARGS_OK`가 이미
# 빈 값을 막지만, 이 검사만 따로 복사해 쓰는 경우에도 성립해야 한다.
if grep -qF "${DRILL_SECRET}:password::" "$DRILL_DIR/td-b.json" \
   && [ -n "$PROD_SECRET" ] && ! grep -qF "$PROD_SECRET" "$DRILL_DIR/td-b.json"; then
  # 호출 직전 family를 다시 확인해 프리플라이트 이후의 동시 등록 레이스를 닫는다.
  TD_B_REGISTER_OK=0
  TD_B_NOW=$(aws ecs list-task-definitions --family-prefix "$TD_B" --status ACTIVE \
    --query 'taskDefinitionArns' --output text 2>&1)
  TD_B_NOW_RC=$?
  TD_B_EXACT=""
  if [ "$TD_B_NOW_RC" -eq 0 ]; then
    for arn in $TD_B_NOW; do
      [ "${arn%:*}" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task-definition/$TD_B" ] \
        && TD_B_EXACT="$TD_B_EXACT $arn"
    done
    [ -z "$TD_B_EXACT" ] && TD_B_REGISTER_OK=1
  fi
  DISPATCH_DIR="$DRILL_DIR/dispatch"
  # §3.2와 같은 이유로 여기서 `lock_live`를 다시 본다(입구 게이트 이후 family 재조회가 끼었다).
  if [ "$TD_B_REGISTER_OK" = 1 ] && lock_live && mkdir -p "$DISPATCH_DIR/td-pending" \
       && : > "$DISPATCH_DIR/td-pending/$TD_B" && [ -f "$DISPATCH_DIR/td-pending/$TD_B" ]; then
    # §3.2와 같은 규율 — **호출 전에** 미대사 등록을 디스크에 남겨야 응답 유실 창에서 상태가 남는다.
    TD_B_OUT=$(aws ecs register-task-definition --cli-input-json "file://$DRILL_DIR/td-b.json" \
      --tags key=purpose,value=rds-restore-drill key=drill-token,value="$TOKEN" \
      --query 'taskDefinition.taskDefinitionArn' --output text 2>&1)
    TD_B_RC=$?
    echo "TD-B 등록 rc=$TD_B_RC $TD_B_OUT"
    if [ "$TD_B_RC" -eq 0 ] \
       && [ "${TD_B_OUT%:*}" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task-definition/$TD_B" ]; then
      TD_B_ARN="$TD_B_OUT"
      grep -Fxq -- "$TD_B_ARN" "$DISPATCH_DIR/td-arns" 2>/dev/null \
        || printf '%s\n' "$TD_B_ARN" >> "$DISPATCH_DIR/td-arns"
      if grep -Fxq -- "$TD_B_ARN" "$DISPATCH_DIR/td-arns" 2>/dev/null; then
        rm -f "$DISPATCH_DIR/td-pending/$TD_B"      # ARN 기록 확인 뒤에만 닫는다
        echo "TD_B_ARN=$TD_B_ARN"   # ← 즉시 원장에 기입(미대사 파일은 코드가 닫았다)
      else
        echo "STOP: revision ARN을 원장에 기록하지 못했다 — 미대사 파일을 남긴다"
      fi
    else
      echo "STOP: revision ARN을 대사하지 못했다 — **미대사 파일이 남는다**(정상). §10.3의 태그 재탐색이"
      echo "      이번 토큰 revision을 정확히 1건 찾으면 그 파일을 자동으로 닫는다"
    fi
  else
    # §3.2와 같은 이유 — 등록 직전 `lock_live` 실패도 이 분기로 온다.
    echo "STOP: TD-B family가 absent로 재확인되지 않았거나(rc=$TD_B_NOW_RC exact=$TD_B_EXACT) 미대사 등록 원장을 만들지 못했거나 등록 직전 락이 만료됐다(락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)) — 등록하지 않는다"
  fi
else
  echo "STOP: td-b.json의 시크릿 참조가 기대와 다르다 — 등록하지 않았다"
fi

fi   # ← 위 인자 가드 블록의 끝
echo "TD-B 생성 단계 종료 — 현재 TD_B_READY=$TD_B_READY TD_B_ARN=$TD_B_ARN"
```

신규 등록 직후와 세션 재진입 모두 아래 읽기 전용 reconciliation을 실행한다. exact revision ARN과 이번 태그뿐 아니라 드릴 endpoint·role·시크릿·이미지·로그 구성이 모두 일치해야 한다.

```bash
TD_B_READY=0
TD_B_BASE=${TD_B_ARN%:*}
TD_B_REV=${TD_B_ARN##*:}
TD_B_ARN_FORMAT_OK=0
if [ "$TD_B_BASE" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task-definition/$TD_B" ]; then
  case "$TD_B_REV" in ""|*[!0-9]*) ;; *) TD_B_ARN_FORMAT_OK=1 ;; esac
fi

if [ "$TD_B_ARN_FORMAT_OK" = 1 ]; then
  TD_B_META=$(aws ecs describe-task-definition --task-definition "$TD_B_ARN" --include TAGS \
    --query '[taskDefinition.taskDefinitionArn,taskDefinition.status,taskDefinition.family,
      taskDefinition.revision,taskDefinition.executionRoleArn,taskDefinition.networkMode,
      taskDefinition.cpu,taskDefinition.memory,taskDefinition.runtimePlatform.cpuArchitecture,
      taskDefinition.containerDefinitions[0].image,
      taskDefinition.containerDefinitions[0].environment[?name==`PGHOST`].value|[0],
      taskDefinition.containerDefinitions[0].secrets[?name==`PGPASSWORD`].valueFrom|[0],
      taskDefinition.containerDefinitions[0].logConfiguration.options."awslogs-group",
      taskDefinition.containerDefinitions[0].logConfiguration.options."awslogs-stream-prefix",
      tags[?key==`purpose`].value|[0],tags[?key==`drill-token`].value|[0],
      length(taskDefinition.containerDefinitions),taskDefinition.taskRoleArn]' \
    --output text 2>&1)
  TD_B_META_RC=$?
  if [ "$TD_B_META_RC" -eq 0 ]; then
    TD_ARN_OBS=$(printf %s "$TD_B_META" | awk '{print $1}')
    TD_STATUS_OBS=$(printf %s "$TD_B_META" | awk '{print $2}')
    TD_FAMILY_OBS=$(printf %s "$TD_B_META" | awk '{print $3}')
    TD_REV_OBS=$(printf %s "$TD_B_META" | awk '{print $4}')
    TD_ROLE_OBS=$(printf %s "$TD_B_META" | awk '{print $5}')
    TD_NET_OBS=$(printf %s "$TD_B_META" | awk '{print $6}')
    TD_CPU_OBS=$(printf %s "$TD_B_META" | awk '{print $7}')
    TD_MEMORY_OBS=$(printf %s "$TD_B_META" | awk '{print $8}')
    TD_ARCH_OBS=$(printf %s "$TD_B_META" | awk '{print $9}')
    TD_IMAGE_OBS=$(printf %s "$TD_B_META" | awk '{print $10}')
    TD_HOST_OBS=$(printf %s "$TD_B_META" | awk '{print $11}')
    TD_SECRET_OBS=$(printf %s "$TD_B_META" | awk '{print $12}')
    TD_LOG_OBS=$(printf %s "$TD_B_META" | awk '{print $13}')
    TD_PREFIX_OBS=$(printf %s "$TD_B_META" | awk '{print $14}')
    TD_PURPOSE_OBS=$(printf %s "$TD_B_META" | awk '{print $15}')
    TD_TOKEN_OBS=$(printf %s "$TD_B_META" | awk '{print $16}')
    TD_COUNT_OBS=$(printf %s "$TD_B_META" | awk '{print $17}')
    TD_TASKROLE_OBS=$(printf %s "$TD_B_META" | awk '{print $18}')
    if [ "$TD_COUNT_OBS" = 1 ] && [ "$TD_TASKROLE_OBS" = None ] \
       && [ "$TD_ARN_OBS" = "$TD_B_ARN" ] && [ "$TD_STATUS_OBS" = ACTIVE ] \
       && [ "$TD_FAMILY_OBS" = "$TD_B" ] && [ "$TD_REV_OBS" = "$TD_B_REV" ] \
       && [ "$TD_ROLE_OBS" = "arn:aws:iam::${ACCOUNT_ID}:role/$DRILL_ROLE" ] \
       && [ "$TD_NET_OBS" = awsvpc ] && [ "$TD_CPU_OBS" = 256 ] && [ "$TD_MEMORY_OBS" = 512 ] \
       && [ "$TD_ARCH_OBS" = ARM64 ] && [ "$TD_IMAGE_OBS" = "postgres:16@$DIGEST" ] \
       && [ "$TD_HOST_OBS" = "$DRILL_ADDR" ] && [ "$TD_SECRET_OBS" = "${DRILL_SECRET}:password::" ] \
       && [ "$TD_LOG_OBS" = "$LOG_GROUP" ] && [ "$TD_PREFIX_OBS" = "$TOKEN" ] \
       && [ "$TD_PURPOSE_OBS" = rds-restore-drill ] && [ "$TD_TOKEN_OBS" = "$TOKEN" ]; then
      TD_B_READY=1
    fi
  fi
fi
echo "TD_B_READY=$TD_B_READY TD_B_ARN=$TD_B_ARN rc=${TD_B_META_RC:-미호출}"
```

- **sentinel 플레이스홀더는 §3.2와 같은 규칙**이다 — 따옴표는 **값 쪽**에 둔다(`A_CODES="'a','b'"` → SQL은 `IN ($A_CODES)`). 템플릿 쪽에 따옴표를 두면 이중 인용으로 `ON_ERROR_STOP=1`에서 죽고, 하필 `T_verified` 직전이라 3회 시도 상한을 갉아먹는다.
  - **A와 B는 인용 주체가 다르다.** `A_CODES`는 사람이 §3.2에서 값 쪽 따옴표까지 포함해 적지만, `B_CODE`는 §4.3이 API 응답에서 **raw base62**로 자동 채택한다(원장·마진 비교가 raw 값을 쓰므로 여기에 따옴표를 섞으면 안 된다). 그래서 위 블록이 base62 검증 후 `B_CODE_SQL="'$B_CODE'"`를 따로 만들고, SQL에는 `IN ($B_CODE_SQL)`만 넣는다. `IN ($B_CODE)`로 되돌리면 PostgreSQL이 code를 **식별자**로 읽어 `column ... does not exist`로 죽는다(전부 숫자인 code는 `text = integer` 오류).
- `SHOW rds.force_ssl`을 쓰지 않는 이유: GUC가 등록돼 있지 않으면 **에러로 SQL 배치를 중단**시켜 다른 검증 결과까지 잃는다. `pg_settings`는 미등록이면 0행이라 첫 결과셋을 보존한다. 이 0행은 (a)의 재료 미획득일 뿐, §8.3이 이미 얻은 (b) 데이터 결과와 (c) 최초 결과 확보 시각을 무효화하지 않는다.
- 쿼리만 급히 고쳐야 하면 `run-task --overrides`로 command만 덮어써도 된다(비민감 값). **단 TD를 다시 등록하면 revision이 늘어나므로 원장 TD 표에 새 줄을 추가**한다(정리 대상이 revision 단위이기 때문).

### 8.3 실행 (TD-B, **총 시도 상한 3회** = 최초 1 + 재시도 2)

**§3.3과 같은 2단 구조다** — (1) 예약이 미대사 상태를 디스크에 남기고, (2) 전송이 일회성 허용값을 소비한다.

**(1) 예약**

```bash
# APP_SUBNETS·APP_SG는 §2.5 ⑧에서 고정된 값이다(셸 세션이 바뀌었으면 그 블록을 다시 실행).
# **IAM 전파 지연 재시도는 실제로 밟히는 경로다**(§12 진단표) → 시도 번호를 §2.1이 들고 있고
# 여기서는 보존만 한다(`=1` 대입은 블록 재실행 때 조용히 리셋되므로 쓰지 않는다).
# 신규 드릴은 §2.1이 1로 시작한다. 재진입 빈 값은 1로 보정하지 않고 아래에서 STOP한다.
TDB_TRY="${TDB_TRY:-}"
TD_B_RUN_OK=0
TD_B_RUN_BASE=${TD_B_ARN%:*}
TD_B_RUN_REV=${TD_B_ARN##*:}
if [ "$TD_B_RUN_BASE" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task-definition/linkpulse-restore-drill-b" ]; then
  case "$TD_B_RUN_REV" in ""|*[!0-9]*) ;; *) TD_B_RUN_OK=1 ;; esac
fi
case "$TDB_TRY" in
  "") TD_B_RUN_OK=0; echo "STOP: 원장에서 TD-B 시도 번호를 넣는다(응답 불명은 같은 번호, 확인된 실패 뒤에만 +1)" ;;
  1|2|3) ;;
  *) TD_B_RUN_OK=0; echo "STOP: TD-B 시도 상한 3회 초과(TDB_TRY=$TDB_TRY)" ;;
esac
TDB_SEND_ALLOWED=0
TDB_RUN_TOKEN="$TOKEN-tdb-$TDB_TRY"   # §3.3과 이름을 나눈다(공유하면 서로의 pending을 지운다)
DISPATCH_DIR="$DRILL_DIR/dispatch"
RESERVE_OK=0
if [ "${SHELL_OK:-0}" = 1 ] && [ "${DRILL_MUTATION_OK:-0}" = 1 ] \
   && lock_live \
   && [ "$TD_B_READY" = 1 ] && [ "$TD_B_RUN_OK" = 1 ] && [ "$ROLE_READY" = 1 ] \
   && [ "$SENTINEL_BOUNDARY_OK" = 1 ] \
   && [ "$CLUSTER" = "linkpulse-prod-cluster" ] && [ -n "$TOKEN" ] \
   && [ -n "${DRILL_DIR:-}" ] && mkdir -p "$DISPATCH_DIR/task-pending"; then
  # §3.3과 같은 규율 — 미대사 시도를 **AWS 호출 전에 디스크에** 남기고, **쓰기 성공을 확인한 뒤에만**
  # 전송 자격을 연다. 같은 토큰은 같은 파일명이라 재전송(= 원응답 회수)이 이중 계상되지 않는다.
  if : > "$DISPATCH_DIR/task-pending/$TDB_RUN_TOKEN" \
     && [ -f "$DISPATCH_DIR/task-pending/$TDB_RUN_TOKEN" ]; then
    RESERVE_OK=1
    TDB_SEND_ALLOWED=1
    echo "client token = $TDB_RUN_TOKEN"      # ← 원장의 시도별 토큰과 대조한다
    echo "미대사 예약: $(ls "$DISPATCH_DIR/task-pending" | tr '\n' ' ')"
  else
    echo "STOP: 미대사 원장 파일을 만들지 못했다($DISPATCH_DIR/task-pending/$TDB_RUN_TOKEN) — run-task를 호출하지 않는다"
  fi
else
  echo "STOP: SHELL_OK=${SHELL_OK:-미설정} DRILL_MUTATION_OK=${DRILL_MUTATION_OK:-미설정} TD_B_READY=$TD_B_READY TD_B_RUN_OK=$TD_B_RUN_OK ROLE_READY=$ROLE_READY SENTINEL_BOUNDARY_OK=${SENTINEL_BOUNDARY_OK:-미설정} CLUSTER='$CLUSTER' TOKEN='${TOKEN:-미설정}' DRILL_DIR='${DRILL_DIR:-미설정}' 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s) — run-task를 예약하지 않았다"
fi
```

**(2) 전송** — 같은 셸의 일회성 허용값만 소비한다. §3.3과 같이 `drill-client-token` 태그를 붙인다.

```bash
if [ "${DRILL_MUTATION_OK:-0}" = 1 ] && [ "${TDB_SEND_ALLOWED:-0}" = 1 ] && [ "${RESERVE_OK:-0}" = 1 ] \
   && lock_live \
   && [ -f "$DISPATCH_DIR/task-pending/$TDB_RUN_TOKEN" ]; then
  TDB_SEND_ALLOWED=0
  RUN_OUT=$(aws ecs run-task --cluster "$CLUSTER" --launch-type FARGATE --platform-version 1.4.0 \
    --task-definition "$TD_B_ARN" \
    --started-by "$TOKEN" --client-token "$TDB_RUN_TOKEN" \
    --tags "key=drill-client-token,value=$TDB_RUN_TOKEN" "key=drill-token,value=$TOKEN" \
    --network-configuration "awsvpcConfiguration={subnets=[$APP_SUBNETS],securityGroups=[$APP_SG],assignPublicIp=DISABLED}" \
    --query '{arn:tasks[].taskArn,failures:failures}' --output json 2>&1)
  RUN_RC=$?
  printf '%s\n' "$RUN_OUT"
  RUN_ARN=$(printf %s "$RUN_OUT" | grep -oE "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task/[^\"]+" | head -1)
  if [ "$RUN_RC" -eq 0 ] && { [ -n "$RUN_ARN" ] || printf %s "$RUN_OUT" | grep -q '"reason"'; }; then
    ARNS_OK=1
    if [ -n "$RUN_ARN" ]; then
      ARNS_OK=0
      grep -Fxq -- "$RUN_ARN" "$DISPATCH_DIR/task-arns" 2>/dev/null \
        || printf '%s\n' "$RUN_ARN" >> "$DISPATCH_DIR/task-arns"
      grep -Fxq -- "$RUN_ARN" "$DISPATCH_DIR/task-arns" 2>/dev/null && ARNS_OK=1
    fi
    if [ "$ARNS_OK" = 1 ]; then
      rm -f "$DISPATCH_DIR/task-pending/$TDB_RUN_TOKEN"
      echo "대사 완료 arn='$RUN_ARN' 남은 미대사: $(ls "$DISPATCH_DIR/task-pending" | tr '\n' ' ')"
    else
      echo "STOP: taskArn을 원장에 기록하지 못했다 — **미대사 파일을 남긴다.** arn='$RUN_ARN'"
    fi
  else
    echo "STOP: 응답을 대사하지 못했다(rc=$RUN_RC) — **미대사 파일이 남는다**(정상). 같은 client token으로"
    echo "      (1)(2)를 다시 실행하면 멱등 원응답을 회수해 그 파일이 닫힌다(§2.6 ⓸). 그래도 안 되면 §10.3 ⓒ의 회수 블록."
  fi
else
  # §3.3 (2)와 같은 이유 — 락 만료로 막힌 경우 "예약되지 않았다"는 거짓이다(예약은 성공해 있다).
  echo "STOP: TD-B run-task를 전송하지 않았다 — mutation=${DRILL_MUTATION_OK:-미설정} send_allowed=${TDB_SEND_ALLOWED:-미설정} reserve_ok=${RESERVE_OK:-미설정} pending파일=$([ -f "$DISPATCH_DIR/task-pending/$TDB_RUN_TOKEN" ] && echo 있음 || echo 없음) 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)"
  echo "      예약(1)이 이미 성공했고 pending 파일이 있으면 (1)을 다시 돌리지 않는다 — 락을 갱신하고 §2.5 ⓪만 다시 실행한 뒤 이 블록을 재실행한다"
fi
```

방금 만든 role은 **IAM 전파 지연**으로 첫 시도가 "unable to assume role" 계열로 실패할 수 있다. **확인된 실패 뒤 새 논리 시도**라면 30초 간격으로 `TDB_TRY`를 2·3으로 올려 다음 토큰을 쓰고, **응답 유실·timeout처럼 결과가 불명**이면 번호를 올리지 않고 같은 토큰으로 재전송해 원응답을 회수한다. 각 시도의 `taskArn`·`failures[]`를 원장에 남긴다(**실패한 시도도 태스크를 만들 수 있어 정리 대상**이다). 새 `TDB_TASK_ARN`을 채택할 때 그 **같은 원장 행**의 `TDB_RESULT_READ_STATE=not-read`를 함께 적고, 기존 ARN의 `read` 상태를 새 ARN에 복사하지 않는다.

**§3.4와 같은 데이터 게이트를 쓰되 축을 섞지 않는다.** `VERIFY_RESULT_OK`는 (b)·(c)의 첫 결과셋 다섯 값(`t_verify_db`·`row_count`·`clicks_sum`·sentinel-A/B hits)만 뜻하고, (a)의 `rds.force_ssl` 획득 여부는 `FORCE_SSL_OBTAINED`가 따로 뜻한다. 한쪽 재료가 없다고 다른 축에서 이미 얻은 증거까지 버리지 않는다.

`T_verified`는 첫 유효 데이터 결과를 읽은 **셸 시각을 한 번만** 원장에 적는다. `SESSION_MODE`가 아니라 현재 taskArn에 묶인 `TDB_RESULT_READ_STATE=not-read|read|unknown`이 최초 판독 여부를 가른다. 따라서 §8.3 전에 세션이 끊겼어도 새 터미널에서 `not-read`인 결과를 처음 읽으면 유효한 최초 시각을 기록한다. `read`인데 원장 시각이 유실됐거나 상태가 `unknown`이면 현재 시각으로 새로 찍지 않는다.

**"현재 시도를 읽었는가"와 "드릴 최초 유효 시각이 이미 있는가"는 별개다.** `T_verified`/source/taskArn tuple은 **드릴 전체에서 한 번만** 만들어지고, `TDB_RESULT_READ_STATE`는 taskArn마다 따로 움직인다. 그래서 첫 시도에서 시각을 확보한 뒤 (a) 재료 보강 등으로 새 TD-B 시도를 채택하면, 새 행은 규칙대로 `not-read`로 시작하되 그 결과를 읽을 때 **기존 tuple을 보존한 채** 현재 taskArn만 `read`로 올린다(`T_VERIFIED_OK=1` 유지). 최초 측정값은 후속 시도로 바뀌지 않으며, `T_VERIFIED_TASK_ARN`은 그 시각을 실제로 만든 시도를 계속 가리킨다. tuple이 **부분적으로만** 남아 있으면(시각 형식 오류·source 미허용·taskArn 공백) 보존이 아니라 STOP이다. `t_verify_db`는 그때의 별도 DB 시계 진단값일 뿐, 셸 시계 헤드라인 측정값을 대신하지 않는다.

```bash
VERIFY_RESULT_OK=0
FORCE_SSL_OBTAINED=0
T_VERIFIED_OK=0
TDB_TASK_ARN="${TDB_TASK_ARN:-}"   # ← §8.3 응답의 taskArn(원장 사후값, 시도마다 갱신)
TDB_RESULT_READ_STATE="${TDB_RESULT_READ_STATE:-unknown}"
T_VERIFIED="${T_VERIFIED:-}"       # ← 최초 유효 결과를 읽은 셸 시각. 재진입에서는 원장값
T_VERIFIED_SOURCE="${T_VERIFIED_SOURCE:-}"
T_VERIFIED_TASK_ARN="${T_VERIFIED_TASK_ARN:-}"
T_VERIFIED_TOKEN="${T_VERIFIED_TOKEN:-}"   # ← 이 시각을 만든 드릴의 TOKEN. 다른 드릴 원장 조각을 막는 계보 키

# 보존된 최초 tuple이 **이번 드릴 것**인지 판정한다(형식만 맞는 남의 원장 조각을 배제).
#  · TOKEN 일치      = 다른 드릴/다른 날짜의 tuple 배제
#  · ARN 계정·리전   = §8.3 waiter와 같은 기준(위 elif)을 최초 tuple에도 적용
#  · T0 ≤ T_verified = 둘 다 이 드릴의 같은 셸 시계(`date -u`)라 교차 시계 보정이 필요 없다
T_VERIFIED_LINEAGE_OK=0
if [ -n "$T_VERIFIED" ] && [ -n "$T_VERIFIED_TASK_ARN" ] \
   && [ -n "$T_VERIFIED_TOKEN" ] && [ "$T_VERIFIED_TOKEN" = "$TOKEN" ] \
   && [ "${T_VERIFIED_TASK_ARN%%/*}" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task" ] \
   && [ -n "$RESTORE_T0" ] \
   && [ "$(printf '%s\n%s\n' "$RESTORE_T0" "$T_VERIFIED" | sort | sed -n 1p)" = "$RESTORE_T0" ]; then
  T_VERIFIED_LINEAGE_OK=1
fi
# §3.4와 같은 이유로 관측값을 매 실행마다 비운다(실패 재실행에서 이전 값이 남지 않게).
T_VERIFY_DB=""; VERIFY_ROW_COUNT=""; VERIFY_CLICKS_SUM=""
A_HITS_V=""; B_HITS_V=""; FORCE_SSL_OBS=""
if [ -z "$TDB_TASK_ARN" ]; then
  echo "STOP: TDB_TASK_ARN이 비었다 — §8.3 응답의 taskArn을 붙여넣는다"
elif [ "${TDB_TASK_ARN%%/*}" != "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task" ]; then
  echo "STOP: 이번 계정·리전의 task ARN이 아니다: $TDB_TASK_ARN"
elif ! aws ecs wait tasks-stopped --cluster "$CLUSTER" --tasks "$TDB_TASK_ARN"; then
  echo "STOP: tasks-stopped waiter 미통과 — 결과를 읽지 않고 T_verified도 찍지 않는다"
else
  aws ecs describe-tasks --cluster "$CLUSTER" --tasks "$TDB_TASK_ARN" \
    --query 'tasks[0].{last:lastStatus,stopped:stoppedReason,
             exit:containers[0].exitCode,reason:containers[0].reason}' --output json
  EXITC_B=$(aws ecs describe-tasks --cluster "$CLUSTER" --tasks "$TDB_TASK_ARN" \
            --query 'tasks[0].containers[0].exitCode' --output text 2>&1)
  EXITC_B_RC=$?
  TASK_ID_B=${TDB_TASK_ARN##*/}
  if [ "$EXITC_B_RC" -ne 0 ] || [ "$EXITC_B" != 0 ]; then
    echo "STOP: exitCode=$EXITC_B (rc=$EXITC_B_RC) — §12 진단으로 간다. 다음 시도는 TDB_TRY를 올린다"
  else
    VERIFY_LOG=$(aws logs get-log-events --log-group-name "$LOG_GROUP" \
      --log-stream-name "$TOKEN/psql/$TASK_ID_B" --query 'events[].message' --output text 2>&1)
    VERIFY_LOG_RC=$?
    printf '%s\n' "$VERIFY_LOG"
    VERIFY_ROWS=$(printf '%s' "$VERIFY_LOG" | tr '\t' '\n')
    T_VERIFY_DB=$(printf '%s\n' "$VERIFY_ROWS" | awk -F'|' -v want=t_verify_db '
      ci==0 { for (i=1; i<=NF; i++) if ($i==want) ci=i; next }
      ci>0 && NF>=ci { print $ci; exit }')
    VERIFY_ROW_COUNT=$(printf '%s\n' "$VERIFY_ROWS" | awk -F'|' -v want=row_count '
      ci==0 { for (i=1; i<=NF; i++) if ($i==want) ci=i; next }
      ci>0 && NF>=ci { print $ci; exit }')
    VERIFY_CLICKS_SUM=$(printf '%s\n' "$VERIFY_ROWS" | awk -F'|' -v want=clicks_sum '
      ci==0 { for (i=1; i<=NF; i++) if ($i==want) ci=i; next }
      ci>0 && NF>=ci { print $ci; exit }')
    A_HITS_V=$(printf '%s\n' "$VERIFY_ROWS" | awk -F'|' -v want=sentinel_a_hits '
      ci==0 { for (i=1; i<=NF; i++) if ($i==want) ci=i; next }
      ci>0 && NF>=ci { print $ci; exit }')
    B_HITS_V=$(printf '%s\n' "$VERIFY_ROWS" | awk -F'|' -v want=sentinel_b_hits '
      ci==0 { for (i=1; i<=NF; i++) if ($i==want) ci=i; next }
      ci>0 && NF>=ci { print $ci; exit }')
    # 두 번째 결과셋(`name|setting|source`)의 `setting` 열 = rds.force_ssl 실효값.
    # 0행이면 값 행이 없어 자연히 빈 값이 되고, 아래 게이트가 그것을 STOP으로 잡는다.
    FORCE_SSL_OBS=$(printf '%s\n' "$VERIFY_ROWS" | awk -F'|' -v want=setting '
      ci==0 { for (i=1; i<=NF; i++) if ($i==want) ci=i; next }
      ci>0 && NF>=ci { print $ci; exit }')
    # **형태까지 검증한다**(§3.4와 같은 관용구). 어느 값이 없었는지 개별로 적어야 §12 진단이 짧아진다.
    VERIFY_PARSE_OK=1
    if [ "$VERIFY_LOG_RC" -ne 0 ]; then
      VERIFY_PARSE_OK=0; echo "STOP: 로그 조회 실패 rc=$VERIFY_LOG_RC"
    fi
    printf %s "$T_VERIFY_DB" \
      | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$' \
      || { VERIFY_PARSE_OK=0; echo "STOP: t_verify_db가 RFC3339가 아니다: '$T_VERIFY_DB'"; }
    case "$VERIFY_ROW_COUNT" in
      ""|*[!0-9]*) VERIFY_PARSE_OK=0; echo "STOP: 복원본 row_count가 비음수 정수가 아니다: '$VERIFY_ROW_COUNT'" ;;
    esac
    case "$VERIFY_CLICKS_SUM" in
      ""|*[!0-9]*) VERIFY_PARSE_OK=0; echo "STOP: 복원본 clicks_sum이 비음수 정수가 아니다: '$VERIFY_CLICKS_SUM'" ;;
    esac
    case "$A_HITS_V" in
      ""|*[!0-9]*) VERIFY_PARSE_OK=0; echo "STOP: sentinel_a_hits가 정수가 아니다: '$A_HITS_V'" ;;
    esac
    case "$B_HITS_V" in
      ""|*[!0-9]*) VERIFY_PARSE_OK=0; echo "STOP: sentinel_b_hits가 정수가 아니다: '$B_HITS_V'" ;;
    esac
    if [ "$VERIFY_LOG_RC" -eq 0 ] && [ -n "$FORCE_SSL_OBS" ]; then
      FORCE_SSL_OBTAINED=1
    else
      echo "STOP: rds.force_ssl 실효값을 얻지 못했다(pg_settings 0행 = 파라미터 미등록 가능)"
      echo "      → (a)만 **미완**으로 남긴다. 데이터 결과와 측정 시각은 독립 보존한다"
    fi
    if [ "$VERIFY_PARSE_OK" = 1 ]; then
      VERIFY_RESULT_OK=1
      case "$TDB_RESULT_READ_STATE" in
        not-read)
          if [ -z "$T_VERIFIED" ] && [ -z "$T_VERIFIED_SOURCE" ] \
             && [ -z "$T_VERIFIED_TASK_ARN" ] && [ -z "$T_VERIFIED_TOKEN" ]; then
            T_VERIFIED=$(date -u +%Y-%m-%dT%H:%M:%SZ)
            if [ "$SESSION_MODE" = reentry ]; then
              T_VERIFIED_SOURCE=shell-first-read-reentry
            else
              T_VERIFIED_SOURCE=shell-first-read
            fi
            T_VERIFIED_TASK_ARN="$TDB_TASK_ARN"
            T_VERIFIED_TOKEN="$TOKEN"
            TDB_RESULT_READ_STATE=read
            T_VERIFIED_OK=1
            echo "TDB_RESULT_READ_STATE=$TDB_RESULT_READ_STATE T_verified=$T_VERIFIED source=$T_VERIFIED_SOURCE task=$T_VERIFIED_TASK_ARN token=$T_VERIFIED_TOKEN ← 한 tuple로 원장에 즉시 기입"
          elif [ "$T_VERIFIED_LINEAGE_OK" = 1 ] \
               && printf %s "$T_VERIFIED" \
                 | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$' \
               && [ "$T_VERIFIED_TASK_ARN" != "$TDB_TASK_ARN" ] \
               && { [ "$T_VERIFIED_SOURCE" = shell-first-read ] \
                    || [ "$T_VERIFIED_SOURCE" = shell-first-read-reentry ]; }; then
            # 후속 TD-B 시도(예: force_ssl 재료 보강)의 최초 판독이다. 드릴 전체 최초 유효 시각은
            # 이전 시도에서 이미 확보됐으므로 **tuple은 그대로 두고** 현재 taskArn의 판독 상태만 올린다.
            # 최초 측정값은 새 시도로 바뀌지 않는다(§8.3 주 참고).
            TDB_RESULT_READ_STATE=read
            T_VERIFIED_OK=1
            echo "TDB_RESULT_READ_STATE=$TDB_RESULT_READ_STATE (현재 시도 판독) — 드릴 최초 T_verified=$T_VERIFIED source=$T_VERIFIED_SOURCE task=$T_VERIFIED_TASK_ARN 는 보존"
          else
            echo "STOP: not-read인데 기존 tuple을 이번 드릴 것으로 확인할 수 없다 — 덮어쓰지 않는다"
            echo "      lineage_ok=$T_VERIFIED_LINEAGE_OK (TOKEN 일치·계정/리전 task ARN·T0 ≤ T_verified) task=$T_VERIFIED_TASK_ARN token=$T_VERIFIED_TOKEN"
            echo "      부분 tuple이거나 그 taskArn이 현재 시도와 같아도 여기로 온다"
          fi
          ;;
        read)
          if [ "$T_VERIFIED_LINEAGE_OK" = 1 ] \
             && printf %s "$T_VERIFIED" \
               | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$'; then
            case "$T_VERIFIED_SOURCE" in
              shell-first-read|shell-first-read-reentry) T_VERIFIED_OK=1 ;;
              *) echo "STOP: T_verified source가 허용값이 아니다: '$T_VERIFIED_SOURCE'" ;;
            esac
            if [ "$T_VERIFIED_OK" = 1 ] && [ "$T_VERIFIED_TASK_ARN" != "$TDB_TASK_ARN" ]; then
              echo "참고: 드릴 최초 유효 시각은 이전 시도($T_VERIFIED_TASK_ARN)에서 확보됐다 — 현재 시도는 시각을 다시 찍지 않는다"
              echo "      → 원장에서 **이 taskArn 행의 TDB_RESULT_READ_STATE가 not-read로 적혀 있는지** 확인하고, 아니면 지금 고친다"
            fi
          else
            echo "STOP: read 상태인데 최초 tuple이 유실·형식 오류이거나 이번 드릴 계보가 아니다 — 현재 시각으로 덮지 않는다"
            echo "      lineage_ok=$T_VERIFIED_LINEAGE_OK token=$T_VERIFIED_TOKEN (현재 TOKEN=$TOKEN) task=$T_VERIFIED_TASK_ARN t0=$RESTORE_T0"
            echo "      t_verify_db=$T_VERIFY_DB 는 별도 DB 시계 증거로만 기록하고 (c)는 미완으로 남긴다"
          fi
          ;;
        *)
          echo "STOP: TDB_RESULT_READ_STATE가 unknown/미기입 — 최초 판독 여부를 추정하지 않는다"
          ;;
      esac
    else
      echo "      → 데이터 결과가 유효하지 않아 T_verified를 찍지 않는다. (b)·(c)는 미검증으로 남긴다"
    fi
  fi
fi
echo "VERIFY_RESULT_OK=$VERIFY_RESULT_OK FORCE_SSL_OBTAINED=$FORCE_SSL_OBTAINED T_VERIFIED_OK=$T_VERIFIED_OK"
echo "t_verify_db=$T_VERIFY_DB row_count=$VERIFY_ROW_COUNT clicks_sum=$VERIFY_CLICKS_SUM a_hits=$A_HITS_V b_hits=$B_HITS_V force_ssl=$FORCE_SSL_OBS"
echo "TDB_RESULT_READ_STATE=$TDB_RESULT_READ_STATE T_verified=$T_VERIFIED source=$T_VERIFIED_SOURCE task=$T_VERIFIED_TASK_ARN token=$T_VERIFIED_TOKEN lineage_ok=$T_VERIFIED_LINEAGE_OK"
# ↑ force_ssl 값이 `on`(또는 `1`)인지는 §9 (a)에서 사람이 판정한다. 여기서는 **얻었는지**만 본다.
```

**폴백(검증 접속이 끝내 안 될 때)**: ① **VPC 지원 CloudShell 환경**(app 서브넷·`app` SG)에서 psql 직접 — 태스크 정의·임시 role이 불필요하고 자격증명이 AWS 제어면 객체에 남지 않는다(**1순위 폴백**). ② 최소한 **RDS 레벨 복원 실증**(available·`LatestRestorableTime`·`describe` 일치)만이라도 기록하고 (b)는 미검증으로 남긴다. Bastion·SSM 포워딩은 드릴용으로 과해서 쓰지 않는다.

---

## 9. (h) 판정·측정 — 축을 섞지 않는다

| 축 | 판정 재료 |
| -- | --------- |
| **(a) 구성 동등성** | **`SAFE_STATE=ok`**(§6 abort 항목 전부 일치) ∧ **`CONFIG_STATE=ok`**(**붙은 그룹 이름 = `$PROD_PG`** ∧ 그 적용이 `in-sync` — 둘 다 ④가 본다) ∧ **`FORCE_SSL_OBTAINED=1`**이고 `FORCE_SSL_OBS`가 `on`(또는 `1`). 확인된 불일치·`off`는 FAIL이고, FAIL 재료 없이 `unknown`·미획득만 있으면 **판정 미완**이다 |
| **(b) 데이터 복원** | **`VERIFY_RESULT_OK=1`**(§8.3 첫 결과셋의 다섯 값: 시각·복원본 `row_count`·`clicks_sum`·A/B hits를 실제로 얻었다) ∧ **`A_HITS_V = A_EXPECTED_COUNT`** ∧ **`B_HITS_V = 0`** ∧ (`BASELINE_COMPARABLE_OK=1`일 때만) **`VERIFY_ROW_COUNT ≥ BASELINE_ROW_COUNT` ∧ `VERIFY_CLICKS_SUM ≥ BASELINE_CLICKS_SUM`**. PITR은 §4.3이 완전한 baseline tuple과 시간 경계를 확인해 비교 가능성을 세운다. sentinel-B 존재나 집계 미달처럼 확인된 FAIL 재료는 다른 재료 미획득보다 우선한다 |
| **(c) 측정** | **`VERIFY_RESULT_OK=1` ∧ `T_VERIFIED_OK=1`**인 최초 셸 `T_verified`로 DB 복원 검증 소요시간(`T_verified − T0`) + **중간 구간 4개**를 계산하고, **복구 지점 지연 = `T0 − restore point`**, 참고값 `T0 − LatestRestorableTime(T0)`, 제어면 단독값(`InstanceCreateTime` 기반)을 기록한다. **서비스 RTO 아님.** 데이터 결과가 있어도 최초 셸 시각 원장이 유실되면 (c)는 미완이며, 현재 시각·`t_verify_db`로 대체하지 않는다 |
| **(d) 정리** | §10.9 잔존 목록 2분류의 **(A) = 0** |

- **교차 시계 오차를 어느 숫자에 붙여 읽는지 미리 정한다**(§1.1 (ii)). **`T_verified − T0`는 셸 시계 단독**이라 오차가 상쇄되므로 보정하지 않는다. 반면 **`T0 − restore point`**·**`T0 − LatestRestorableTime`** 은 셸 시계와 제어면 시계를 섞으므로, ADR에는 값과 함께 **`±CLOCK_SKEW_SECONDS`**(§2.5 ⑥ 실측)를 병기한다. 제어면 단독값(`InstanceCreateTime` 기반)은 보정 불필요하다 — 이 구분을 적어 두지 않으면 step 5에서 같은 숫자를 두 번 다르게 읽는다.
- **sentinel-B가 존재하면** 복원 지점이 의도와 다르다는 신호 → **(b) FAIL**. 단 **B의 `created_at` − `restore point` 값을 함께 남겨** "복원 지점 미준수"인지 "마진 부족"인지 증거로 구분한다(ADR에 남을 결론이라 근거 없이 단정하지 않는다).
- 축별·전체 상태는 **확인된 FAIL > 미완 > PASS** 순으로 합성한다. 넷 전부 PASS일 때만 "드릴 성공"이고, FAIL이 하나라도 있으면 다른 미완이 있어도 전체 FAIL, FAIL 없이 미완만 있으면 판정 유보다. 둘 다 승인된 plan의 성공 조건은 충족하지 못하므로 축별 상태와 함께 **부분 성공/실패 또는 미완**으로 기록한다.

---

## 10. (i) 정리 게이트 — 멱등·재진입 가능

**중단·상한 초과·운영 알람 발생도 이 게이트로 들어오는 정상 경로다.** 드릴은 baseline·복원·자격증명·검증 어디서든 멈출 수 있고, 그때는 리소스가 **일부만** 존재하거나 태스크가 `RUNNING`이다.

### 10.1 공통 형태

**`존재 조회 → 있을 때만 조치 → waiter → 결과 확인`.** **NotFound·이미 종결 상태는 성공(멱등)으로 취급**한다. 어느 항목이 실패해도 **기록만 하고 다음 항목을 계속**하며(한 항목의 오류로 나머지를 포기하지 않는다), **마지막에 잔존 목록으로 (d)를 판정**한다. 절차 전체를 **몇 번을 다시 돌려도 같은 결과**여야 한다.

### 10.2 순서 — 과금을 먼저 끊고, 대기는 뒤로 모은다

⓪ 재탐색 대사 → ① 드릴 DB **삭제 호출만**(비동기, 여기서 기다리지 않는다) → ② 태스크 분류·정지 → ③ TD `deregister` → `delete` → ④ 임시 role → ⑤ **`wait db-instance-deleted` 통과 확인(= 과금 종료 판정)** → ⑥ 드릴 시크릿 → ⑦ 운영 무변경 확인 → ⑧ 잔존 목록 2분류 출력.

### 10.3 ⓪ 재탐색 대사 — 원장만 읽지 않는다

원장이 비어도(응답 유실·터미널 종료·기록 실패) 고정 식별자로 되찾는다. **정리 대상 = 원장 ∪ 재탐색 결과**이며, 원장에 없는데 잡힌 것도 정리하고 **"원장 누락"과 발견 시각을 증거에 남긴다**(절차 결함을 사후에 고치기 위함).

```bash
# ⓪-0 셸 확인 — **정리 세션은 §2.1을 거치지 않고 §0 요약에서 바로 여기로 들어온다**(새 터미널 = 기본 zsh).
#      아래 재탐색은 `for a in $ARNS`·`$REVS_UNION` 처럼 스칼라 단어 분할에 의존하므로, zsh에서는 여러 revision이
#      한 덩어리로 묶여 소유권 대조가 어긋난다. 그래서 §10.4~§10.8의 **모든 변경 게이트가 이 플래그를 소비한다**
#      (§10 밖에서도 §3.1·§4.3·§6·§7·§8.1·§8.3이 각자 도출한 같은 플래그를 직접 본다 — 머리말 ⓐ/ⓑ/ⓒ 분류).
SHELL_OK=0
if [ -n "${BASH_VERSION:-}" ]; then
  SHELL_OK=1
  echo "SHELL_OK=1 bash $BASH_VERSION"
else
  echo "STOP: Bash가 아니다(zsh 등) — 'bash' 를 실행해 새 셸로 들어간 뒤 §10.3부터 다시 시작한다"
  echo "      이 세션에서는 삭제·정지·deregister가 전부 게이트에 막히고 잔존은 §10.9 (A)로 기록된다"
fi

# RDS (시크릿 ARN도 여기서 도출한다)
# 조회 결과와 오류를 **버리지 않고** 잡는다(§10.4가 이 값으로 삭제 여부를 판정한다)
DESC_OUT=$(aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" \
  --query 'DBInstances[0].[DBInstanceStatus,MasterUserSecret.SecretArn,DBInstanceArn]' --output text 2>&1)
DESC_RC=$?
echo "rc=$DESC_RC  $DESC_OUT"
if [ "$DESC_RC" -eq 0 ]; then
  SECRET_ARN_FOUND=$(printf %s "$DESC_OUT" | awk '{print $2}')
  DB_ARN_FOUND=$(printf %s "$DESC_OUT" | awk '{print $3}')
  # ⚠️ **고정 식별자는 다른 실행의 드릴 DB일 수 있다.** 소유권을 먼저 세우지 않고 ARN을 채택하면
  #    남의 DB 마스터 시크릿이 DRILL_SECRET이 되어 §10.8이 그것을 삭제하려 든다.
  # 태그 조회 실패는 불일치가 아니다(§2.7 OWNED ②) → 30초 간격 **총 3회** 재조회한다.
  # 단발 조회로 두면 한 번의 스로틀링이 OWN_STATE != ours 로 굳어 DRILL_SECRET을 채택하지 못하고,
  # §10.8이 시크릿을 식별할 수 없어 정상 정리가 §10.9 (A)③으로 뒤집힌다.
  OWN_STATE=unknown
  OWN_TAG_STATE=unknown; OWN_PURPOSE=""; OWN_TOKEN=""
  for OWN_TRY in 1 2 3; do
    OWN_TAGS=$(aws rds list-tags-for-resource --resource-name "$DB_ARN_FOUND" \
      --query 'TagList[?Key==`purpose`||Key==`drill-token`].[Key,Value]' --output text 2>&1)
    OWN_TAGS_RC=$?
    if [ "$OWN_TAGS_RC" -eq 0 ]; then
      OWN_PURPOSE=$(printf %s "$OWN_TAGS" | awk '$1=="purpose"{print $2}')
      OWN_TOKEN=$(printf %s "$OWN_TAGS" | awk '$1=="drill-token"{print $2}')
      OWN_TAG_STATE=ok
      break
    fi
    echo "소유권 태그 조회 실패($OWN_TRY/3) rc=$OWN_TAGS_RC $OWN_TAGS"
    if [ "$OWN_TRY" -lt 3 ]; then sleep 30; fi
  done
  # `[ -n "$TOKEN" ]`이 토큰 비교 **앞**에 온다(§2.7 ④). 재진입 템플릿(§2.1)은 `TOKEN=""`로 시작하고,
  # `purpose`만 달리고 `drill-token`이 없는 RDS를 만나면 `awk`가 뽑는 `OWN_TOKEN`도 빈 문자열이라
  # `"" = ""`로 소유권이 성립한다 → **남의(또는 토큰 불명의) DB 마스터 시크릿을 `DRILL_SECRET`으로
  # 채택**하고 §10.8이 그것을 지우려 든다(바로 위 ⚠️가 경고한 경로). §10.4의 삭제 게이트는 별도의
  # 같은 비교를 갖고 있어 거기에도 같은 가드를 둔다.
  if [ "$OWN_TAG_STATE" = ok ] && [ "$OWN_PURPOSE" = rds-restore-drill ] \
     && [ -n "$TOKEN" ] && [ "$OWN_TOKEN" = "$TOKEN" ] \
     && { [ -z "$RESTORE_EXPECTED_ARN" ] || [ "$DB_ARN_FOUND" = "$RESTORE_EXPECTED_ARN" ]; }; then
    OWN_STATE=ours
  fi
  if [ "$OWN_STATE" = ours ] && [ -n "$SECRET_ARN_FOUND" ] && [ "$SECRET_ARN_FOUND" != None ] \
     && [ "$SECRET_ARN_FOUND" != "$PROD_SECRET" ]; then
    # 소유권이 섰으므로 원장을 채운다(§2.7) — 원장 ARN이 비어 있었다면 여기서 확정한다.
    # 이렇게 해야 아래 튜플이 `SECRET_OWNER_DB_ARN = RESTORE_EXPECTED_ARN`으로 **내부 정합**을 갖는다.
    [ -z "$RESTORE_EXPECTED_ARN" ] && RESTORE_EXPECTED_ARN="$DB_ARN_FOUND"
    DRILL_SECRET="$SECRET_ARN_FOUND"                           # 재진입 세션에서도 ARN을 되찾는다
    SECRET_OWNER_DB_ARN="$DB_ARN_FOUND"
    SECRET_PROVENANCE_OK=1
    SECRET_LIFECYCLE=arn-known
  elif [ "$OWN_STATE" != ours ]; then
    echo "STOP: 고정 식별자의 DB가 이번 드릴 소유로 확인되지 않았다(purpose=$OWN_PURPOSE token=$OWN_TOKEN tag_state=$OWN_TAG_STATE)"
    echo "      → 시크릿 ARN을 채택하지 않는다. §10.4도 이 DB를 삭제하지 않으며 (A)로 남는다"
    echo "      → tag_state=unknown이면 '남의 것'이 아니라 **못 봤다**이다. 원인 해소 후 §10.3부터 재진입한다"
  fi
  echo "SECRET_LIFECYCLE=$SECRET_LIFECYCLE DRILL_SECRET=$DRILL_SECRET own=$OWN_STATE prov=$SECRET_PROVENANCE_OK"
fi
# 인스턴스가 이미 지워졌으면 이 조회로는 ARN을 얻을 수 없다 → **원장의 `(SECRET_OWNER_DB_ARN, DRILL_SECRET)` 튜플과
# `SECRET_PROVENANCE_OK`** 를 붙여넣는다. 그 튜플은 소유권이 **확인됐던 시점**(§7)에 기록된 것이므로,
# DB가 사라진 뒤에도 §10.8이 "이 시크릿이 우리 것이었다"를 말할 수 있는 유일한 근거다.

# IAM
aws iam get-role --role-name "$DRILL_ROLE" --query 'Role.Arn' --output text 2>&1 | tail -1   # NoSuchEntity = 없음

# ECS 태스크 — **두 경로로 나눠야 한다.** 그리고 조회 결과를 **화면에만 뿌리지 않고 합집합 변수로 만든다**
# (§10.5가 순회할 대상이 여기서 만들어지지 않으면 stop-task 경로 자체가 존재하지 않는다 — TD의 `REVS_UNION`과 같은 이유).
TASKS_FOUND=""
TASK_DISCOVERY_STATE=ok      # ok | unknown — 조회 실패를 "대상 없음"으로 축소하지 않는다
[ "${SHELL_OK:-0}" = 1 ] || TASK_DISCOVERY_STATE=unknown   # 셸이 다르면 아래 순회·분할이 달라진다(§10.3 ⓪-0)
if [ -z "${ACCOUNT_ID:-}" ]; then
  TASK_DISCOVERY_STATE=unknown
  echo "STOP: ACCOUNT_ID가 비었다 — ARN 형식 대조의 기준값이 없어 전건이 불일치가 된다(§0 원장 입력)"
fi

# ⓐ **대사되지 않은 전송 시도가 있으면 성공한 빈 목록을 종결 근거로 쓰지 않는다.**
#    ECS는 결과적 일관성이라 `run-task` 직후의 태스크가 두 목록 **모두에서 아직 안 보일 수 있고**
#    (ADR 0005 "ECS — 결과적 일관성" 인용), 응답을 잃은 시도는 원장에 taskArn조차 없다. 그 조합에서
#    "성공한 빈 조회"를 완결로 읽으면 `TASKS_TOTAL=0` → `TASK_LEDGER_COMPLETE=1`이 되어,
#    뒤늦게 기동한 **운영 마스터 비밀번호를 쥔 컨테이너**가 있는데도 (d)가 PASS로 기록된다.
#    미대사 상태는 **사람이 세는 숫자가 아니라 `$DRILL_DIR/dispatch/` 파일**이다(§2.6) — §3.3·§8.3이
#    AWS 호출 **전에** 만들고 대사 뒤에 지우므로 터미널이 죽어도 남고, 같은 client token 재전송은
#    같은 파일명이라 이중 계상되지 않는다. **`$DRILL_DIR`가 앵커다.**
#    **앵커는 경로 존재가 아니라 manifest 일치다** — 경로만 보면 다른 드릴의 디렉터리나 빈 `$HOME`도
#    통과하고, 잃어버린 `DRILL_DIR`를 아래 STOPPED scan의 `mkdir -p`가 **다시 만들어** 다음 재진입에는
#    "유효한 빈 앵커"로 보인다(codex-cli r21 실측). manifest의 토큰이 이번 드릴과 같을 때만 원장을 읽는다.
DISPATCH_DIR="$DRILL_DIR/dispatch"
DISPATCH_ANCHOR_OK=0
if [ -z "${DRILL_DIR:-}" ] || [ ! -d "$DRILL_DIR" ] || [ -L "$DRILL_DIR" ] \
   || [ ! -d "$DISPATCH_DIR" ] || [ -L "$DISPATCH_DIR" ]; then
  # 아래 네 자식 경로와 같은 규율로 **상위 두 경로도 symlink를 거부한다** — `-d`는 링크를 따라가므로
  # 상위만 링크로 바꿔치기하면 자식 검사가 전부 남의 디렉터리에서 통과한다.
  TASK_DISCOVERY_STATE=unknown
  echo "STOP: DRILL_DIR/dispatch가 없거나 경로가 틀리거나 symlink다('${DRILL_DIR:-미설정}') — 위험 호출 원장을 읽지 못했다(§0 입력구)"
elif [ -L "$DISPATCH_DIR/manifest" ] || [ ! -s "$DISPATCH_DIR/manifest" ] \
     || [ ! -r "$DISPATCH_DIR/manifest" ]; then
  TASK_DISCOVERY_STATE=unknown
  echo "STOP: dispatch/manifest가 없거나 읽을 수 없거나 symlink다 — 이 디렉터리가 이번 드릴의 원장이라는 증거가 없다"
  echo "      (§2.1이 드릴 시작 때 봉인한다. 없다면 원장을 잃었거나 다른 경로를 가리키고 있다)"
elif [ -z "$TOKEN" ] \
     || [ "$(awk -F= '$1=="token"{print $2}' "$DISPATCH_DIR/manifest")" != "$TOKEN" ]; then
  TASK_DISCOVERY_STATE=unknown
  echo "STOP: manifest의 토큰이 이번 드릴과 다르거나 이 세션의 TOKEN이 비었다 — 이번 드릴의 원장이라는 증거가 없다"
  awk -F= '$1=="token"{print "      manifest="$2}' "$DISPATCH_DIR/manifest"; echo "      TOKEN=$TOKEN"
elif ! { [ -d "$DISPATCH_DIR/task-pending" ] && [ ! -L "$DISPATCH_DIR/task-pending" ] \
         && [ -r "$DISPATCH_DIR/task-pending" ] && [ -x "$DISPATCH_DIR/task-pending" ] \
         && [ -d "$DISPATCH_DIR/td-pending" ] && [ ! -L "$DISPATCH_DIR/td-pending" ] \
         && [ -r "$DISPATCH_DIR/td-pending" ] && [ -x "$DISPATCH_DIR/td-pending" ] \
         && [ -f "$DISPATCH_DIR/task-arns" ] && [ -f "$DISPATCH_DIR/td-arns" ] \
         && [ ! -L "$DISPATCH_DIR/task-arns" ] && [ ! -L "$DISPATCH_DIR/td-arns" ] \
         && [ -r "$DISPATCH_DIR/task-arns" ] && [ -r "$DISPATCH_DIR/td-arns" ]; }; then
  # **부분 손상을 "정상 빈 원장"으로 읽지 않는다.** §2.1이 네 경로를 **전부 미리** 만들므로, 하나라도
  # 없거나 못 읽으면 그것은 "아직 호출 안 함"이 아니라 **원장 일부를 잃은 것**이다. 예컨대 `task-arns`만
  # 지워지면 회수한 ARN이 합집합에서 빠진 채 `TASKS_TOTAL=0`으로 닫힌다(codex-cli r22 실측).
  # **정리 코드가 재생성하지 않는다** — 만들어 주면 손실이 그 순간 은폐된다.
  TASK_DISCOVERY_STATE=unknown
  echo "STOP: dispatch 원장 네 경로 중 일부가 없거나 읽을 수 없다 — 부분 손실이다(빈 원장이 아니다)"
  ls -la "$DISPATCH_DIR" 2>&1 | sed 's/^/      /'
else
  DISPATCH_ANCHOR_OK=1
fi

# (1) 비종결: --started-by 는 **단독 필터**로만 쓴다("it must be the only filter that you use").
#     --cluster 는 필터가 아니라 조회 범위라 함께 써도 된다. 기본 desired status가 RUNNING이라 살아 있는 시도가 잡힌다.
#     한 번의 스로틀링이 탐색 전체를 unknown으로 굳히지 않도록 **30초 간격 총 3회**(이 문서 공통 관용구).
TASKS_RUNNING=""
RUN_LIST_OK=0
for RUN_TRY in 1 2 3; do
  TASKS_RUNNING=$(aws ecs list-tasks --cluster "$CLUSTER" --started-by "$TOKEN" \
    --query 'taskArns[]' --output text 2>&1)
  if [ $? -eq 0 ]; then RUN_LIST_OK=1; break; fi
  echo "비종결 태스크 조회 실패($RUN_TRY/3): $TASKS_RUNNING"
  if [ "$RUN_TRY" -lt 3 ]; then sleep 30; fi
done
if [ "$RUN_LIST_OK" = 1 ]; then
  echo "비종결: $TASKS_RUNNING"
  for t in $TASKS_RUNNING; do TASKS_FOUND="$TASKS_FOUND $t"; done   # 서버가 이미 startedBy로 걸렀다
else
  TASK_DISCOVERY_STATE=unknown
  echo "비종결 태스크 조회 3회 실패 → 탐색 불완전"
fi

# (2) 종료: 두 필터를 함께 못 쓰므로 서버 필터링이 불가능하다 →
#     list-tasks 를 **페이지 끝까지** 돌려 ARN을 모으고(--max-items/--no-paginate 쓰지 않는다),
#     모은 ARN을 **100개씩 잘라** describe-tasks 에 넘긴 뒤 startedBy 를 클라이언트에서 대조한다.
# **각 단계(폴더·목록·기록·분할·대사)를 개별로 확인한다** — 하나라도 반환값을 안 보면 STOPPED 후보를
# 건너뛴 채 탐색이 ok로 남는다.
STOPPED_STATE=fail          # ok | fail
STOPPED_OUT=""
# **앵커가 서지 않았으면 scan 폴더를 만들지 않는다** — `mkdir -p`가 잃어버린 `$DRILL_DIR`를 되살려
# 다음 재진입에 "유효한 빈 앵커"로 보이게 만든다(codex-cli r21 실측: 첫 확인 unknown → scan 뒤 ok).
SCAN="$DRILL_DIR/scan-$(date -u +%H%M%S)"      # 실행마다 새 폴더 = 이전 chunk 재처리 방지
if [ "$DISPATCH_ANCHOR_OK" != 1 ]; then
  echo "STOPPED 대사 생략 — 앵커가 서지 않았다(원장을 되살리지 않는다)"
elif ! mkdir -p "$SCAN"; then
  echo "STOPPED 대사 폴더 생성 실패: $SCAN"
else
  # 파이프의 종료 코드는 **마지막 명령의 것**이라 `aws | tr > file` 형태로는 조회 실패를 못 잡는다
  # (= §10.4가 인용하는 "조회 실패를 부재로 읽는" 안티패턴). 그래서 출력을 먼저 받고 rc를 본 뒤 변환한다.
  for STOP_TRY in 1 2 3; do
    STOPPED_OUT=$(aws ecs list-tasks --cluster "$CLUSTER" --desired-status STOPPED \
      --query 'taskArns[]' --output text 2>&1)
    if [ $? -eq 0 ]; then STOPPED_STATE=ok; break; fi
    echo "STOPPED 목록 조회 실패($STOP_TRY/3): $STOPPED_OUT"
    if [ "$STOP_TRY" -lt 3 ]; then sleep 30; fi
  done
  [ "$STOPPED_STATE" = ok ] || echo "STOPPED 목록 조회 3회 실패"
fi
# **성공한 빈 결과와 기록 실패를 구분한다.** `printf '%s\n' ""` 는 개행 1바이트 파일을 만들어 `-s` 가 참이 되고,
# 빈 chunk가 `describe-tasks --tasks` 에 들어가 **정상적인 "STOPPED 0건"이 unknown으로 뒤집힌다.**
# 그래서 **유효한 task ARN 행만** 남기고, 0건은 정상 빈 집합으로 읽는다.
if [ "$STOPPED_STATE" = ok ]; then
  if ! printf '%s\n' "$STOPPED_OUT" | tr '\t' '\n' \
       | awk -v p="arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task/" 'index($0, p) == 1' > "$SCAN/stopped-arns.txt"; then
    STOPPED_STATE=fail
    echo "STOPPED ARN 파일 기록 실패: $SCAN/stopped-arns.txt"
  fi
fi
if [ "$STOPPED_STATE" = ok ] && [ ! -s "$SCAN/stopped-arns.txt" ]; then
  echo "STOPPED 유효 ARN 0건 = 정상 빈 집합(조회는 성공했다)"
elif [ "$STOPPED_STATE" = ok ]; then
  if split -l 100 "$SCAN/stopped-arns.txt" "$SCAN/chunk-"; then   # describe-tasks 는 요청당 ARN 100개 상한
    for c in "$SCAN"/chunk-*; do
      CHUNK_OUT=$(aws ecs describe-tasks --cluster "$CLUSTER" --tasks $(cat "$c") \
        --query "tasks[?startedBy=='$TOKEN'].[taskArn,lastStatus,desiredStatus,containers[0].exitCode]" \
        --output text 2>&1)
      if [ $? -ne 0 ]; then
        STOPPED_STATE=fail
        echo "STOPPED 대사 실패 chunk=$c: $CHUNK_OUT"
        continue
      fi
      printf '%s\n' "$CHUNK_OUT"                              # 관측 이력(§2.6)에 그대로 옮겨 적는다
      # 첫 칸이 taskArn이다. 종결 태스크도 (A)/(가) 판정 대상이라 합집합에 넣는다.
      for t in $(printf '%s\n' "$CHUNK_OUT" | awk 'NF{print $1}'); do TASKS_FOUND="$TASKS_FOUND $t"; done
    done
  else
    STOPPED_STATE=fail
    echo "STOPPED ARN 100개 분할 실패: $SCAN"
  fi
fi
if [ "$STOPPED_STATE" != ok ]; then
  TASK_DISCOVERY_STATE=unknown
  echo "STOPPED 경로 미완 → 탐색 불완전"
fi

# ⓑ **원장이 없는데 이번 토큰 태스크가 보이면 원장을 신뢰할 수 없다.** 로그 스트림 접두는 드릴 토큰이고
#    CloudWatch 보존이 태스크 보관 창보다 길어 **재탐색이 놓친 태스크의 증거**가 된다(§2.5 ⑤의 `logs:FilterLogEvents`).
#    **이 검사가 유일한 근거인 상태에서는 조회 실패를 건너뛰지 않는다** — 원장이 없을 때 이 조회마저 실패하면
#    "증거 없음"이 아니라 "못 봤다"이므로 fail-closed다. 원장이 정상이면 이 값은 보조 증거일 뿐이라 건너뛴다.
#    한계: `filter-log-events`는 **이벤트**를 찾으므로, 스트림만 만들고 한 줄도 못 남긴 태스크는 못 잡는다
#    (`DescribeLogStreams`는 §2.5 권한 목록에 없어 쓰지 않는다).
# ⓑ-2 **미대사 시도를 태그로 자동 대사한다.** `run-task`가 붙인 `drill-client-token` 태그가 taskArn과
#    client token을 잇는 **유일한 상관 키**다(`Task` 응답에는 client token 필드가 없고, `startedBy`는
#    드릴 전체가 같은 값이라 시도를 구분하지 못한다). 재탐색으로 잡힌 태스크의 태그를 읽어 그 시도를 닫으면
#    응답 유실이 사람 판단 없이 해소된다. 태그가 없는 옛 태스크는 아래 ⓒ에서 정리 후보로만 기록하며,
#    client token과의 결속을 증명할 수 없으므로 pending을 닫지 않는다.
if [ "$DISPATCH_ANCHOR_OK" = 1 ]; then
  for t in $TASKS_FOUND; do
    TAG_OUT=$(aws ecs describe-tasks --cluster "$CLUSTER" --tasks "$t" --include TAGS \
      --query 'tasks[0].tags[?key==`drill-client-token`].value|[0]' --output text 2>&1)
    [ $? -eq 0 ] || { echo "태그 조회 실패(자동 대사 생략) $t: $TAG_OUT"; continue; }
    case "$TAG_OUT" in ''|None) continue ;; esac
    # **AWS에서 되읽은 값을 경로에 그대로 쓰지 않는다.** 태그 값에는 `/`·`.`이 허용되므로
    # `TAG_OUT=../manifest` 같은 값이 오면 `rm -f`가 원장 밖 파일을 지운다(codex-cli r22 실측).
    # §2.6의 **허용된 시도 토큰 패턴**으로 제한하고, 어긋나면 조용히 넘기지 않는다 —
    # 이번 토큰 태스크에 우리가 붙이지 않은 태그가 있다는 뜻이라 원격 상태를 신뢰할 수 없다.
    case "$TAG_OUT" in
      "$TOKEN"-tda-[12]|"$TOKEN"-tdb-[123]) ;;
      *) TASK_DISCOVERY_STATE=unknown
         echo "STOP: drill-client-token 태그가 이번 드릴의 시도 토큰 형식이 아니다 $t: '$TAG_OUT'"
         continue ;;
    esac
    [ -f "$DISPATCH_DIR/task-pending/$TAG_OUT" ] || continue
    grep -Fxq -- "$t" "$DISPATCH_DIR/task-arns" 2>/dev/null \
      || printf '%s\n' "$t" >> "$DISPATCH_DIR/task-arns"
    if grep -Fxq -- "$t" "$DISPATCH_DIR/task-arns" 2>/dev/null; then
      rm -f "$DISPATCH_DIR/task-pending/$TAG_OUT"
      echo "미대사 시도 자동 대사: $TAG_OUT → $t"
    else
      echo "STOP: $t 을 원장에 기록하지 못해 $TAG_OUT 을 닫지 않는다"
    fi
  done
fi

# ⓑ-3 **자동 대사 뒤에** 미대사 판정을 한다 — 앞에 두면 방금 회수한 시도까지 unknown으로 굳는다
#      (TD 쪽에서 r20에 같은 순서 문제를 이미 겪었다).
TASK_PENDING_LIST=""
TASK_PENDING_ENUM_OK=0
if [ "$DISPATCH_ANCHOR_OK" = 1 ]; then
  if TASK_PENDING_RAW=$(find "$DISPATCH_DIR/task-pending" -mindepth 1 -maxdepth 1 -print 2>&1); then
    TASK_PENDING_ENUM_OK=1
    for k in $TASK_PENDING_RAW; do TASK_PENDING_LIST="$TASK_PENDING_LIST $(basename "$k")"; done
  else
    TASK_DISCOVERY_STATE=unknown
    echo "STOP: task-pending 열거 실패 — 미대사 파일 유무를 확정할 수 없다: $TASK_PENDING_RAW"
  fi
fi

# 원장 파일·pending이 **둘 다 비었을 때만** 로그/재탐색 증거와 대조한다. 디렉터리는 앵커가 항상
# 존재하게 하므로 `! -d task-pending`은 영원히 참이 될 수 없는 잘못된 빈 원장 판정이다.
TASK_LEDGER_ABSENT=0
TASK_LOG_SEEN=""
if [ "$DISPATCH_ANCHOR_OK" = 1 ] && [ "$TASK_PENDING_ENUM_OK" = 1 ]; then
  [ -z "$TASK_PENDING_LIST" ] && [ ! -s "$DISPATCH_DIR/task-arns" ] && TASK_LEDGER_ABSENT=1
  LOG_OUT=$(aws logs filter-log-events --log-group-name "$LOG_GROUP" \
    --log-stream-name-prefix "$TOKEN" --max-items 1 \
    --query 'events[0].logStreamName' --output text 2>&1)
  LOG_RC=$?
  if [ "$LOG_RC" -eq 0 ]; then
    case "$LOG_OUT" in ''|None) ;; *) TASK_LOG_SEEN="$LOG_OUT" ;; esac
  elif [ "$TASK_LEDGER_ABSENT" = 1 ]; then
    TASK_DISCOVERY_STATE=unknown
    echo "STOP: 원장이 비었는데 로그 조회까지 실패했다 — 태스크를 띄웠는지 확인할 근거가 없다: $LOG_OUT"
  else
    echo "이번 토큰 로그 스트림 조회 실패(원장이 있으므로 보조 증거) — 이 검사는 건너뛴다: $LOG_OUT"
  fi
  if [ "$TASK_LEDGER_ABSENT" = 1 ] && { [ -n "$TASKS_FOUND" ] || [ -n "$TASK_LOG_SEEN" ]; }; then
    TASK_DISCOVERY_STATE=unknown
    echo "STOP: dispatch 원장이 비었는데 이번 토큰 태스크의 증거가 있다 → DRILL_DIR를 확인한다"
    echo "      재탐색:$TASKS_FOUND  로그스트림:$TASK_LOG_SEEN"
  fi
else
  echo "로그-원장 대조 생략 — 앵커 또는 pending 열거가 불완전하다"
fi
if [ -n "$TASK_PENDING_LIST" ]; then
  TASK_DISCOVERY_STATE=unknown
  echo "STOP: taskArn도 확정 failures[]도 못 받은 전송 시도가 남아 있다:$TASK_PENDING_LIST"
  echo "      같은 client token으로 §3.3·§8.3 (1)(2)를 다시 실행해 멱등 원응답(taskArn 또는 확정 failures[])을 회수한다"
fi

# **원장 ∪ 재탐색을 실제로 만든다.** 원장 ARN은 재탐색에 안 나올 수 있다(보관 만료) — 그때도 (A)/(가) 판정 대상이다.
# 입력은 셋이다: ⓐ `dispatch/task-arns`(코드가 회수해 적은 것 — 단일 소스) ⓑ §0에 붙여 넣은 종이 원장
# (`DRILL_DIR`까지 잃었을 때의 백업) ⓒ 재탐색 결과.
# **형식 불일치를 조용히 제외하지 않는다** — 그것은 "그 태스크가 없다"가 아니라 **원장이 깨졌다**는 뜻이라,
# 제외하면 정리 대상이 사라진 채 탐색만 ok로 남는다.
TASK_ARNS_FILE=""
if [ "$DISPATCH_ANCHOR_OK" = 1 ]; then
  # TD 쪽은 여기서 건드리지 않는다 — `TD_DISCOVERY_STATE`는 아래 TD 블록이 무조건 다시 대입하므로
  # 여기서 올려도 죽은 대입이고(의도 오독을 부른다), TD 원장은 `td-arns`라 이 실패와 무관하다.
  # `task-arns` 읽기 실패는 `TASK_DISCOVERY_STATE=unknown`만으로 §10.9 (A)⑥에 걸린다.
  TASK_ARNS_FILE=$(awk 'NF' "$DISPATCH_DIR/task-arns") \
    || { TASK_DISCOVERY_STATE=unknown; echo "STOP: task-arns 원장을 읽지 못했다"; }
  TASK_ARNS_FILE=$(printf '%s ' $TASK_ARNS_FILE)
fi
TASKS_UNION=""
for t in $TASK_ARNS_FILE $TASK_ARNS_LEDGER $TDA_TASK_ARN $TDB_TASK_ARN $TASKS_FOUND; do
  if [ "${t%%/*}" != "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task" ]; then
    TASK_DISCOVERY_STATE=unknown
    echo "STOP: 이번 계정·리전의 task ARN이 아니다(원장 손상 가능) → 탐색 불완전: $t"
    continue
  fi
  DUP=0
  for u in $TASKS_UNION; do [ "$t" = "$u" ] && DUP=1 && break; done
  [ "$DUP" = 0 ] && TASKS_UNION="$TASKS_UNION $t"
done
TASKS_TOTAL=0
for t in $TASKS_UNION; do TASKS_TOTAL=$((TASKS_TOTAL + 1)); done
echo "TASKS_UNION=$TASKS_UNION"
echo "TASKS_TOTAL=$TASKS_TOTAL TASK_DISCOVERY_STATE=$TASK_DISCOVERY_STATE  # ← §10.5가 이 값을 그대로 쓴다"

# **등록 응답이 유실된 revision을 되찾는다.** 원장의 TD_A_ARN/TD_B_ARN만 믿으면, 서비스가 revision을 만들었는데
# CLI 응답이나 세션을 잃은 경우 family는 present라 재등록은 막히고, 정리는 exact ARN이 없어 아무것도 못 한다
# → 이번 실행의 ACTIVE TD가 그대로 (A) 잔존으로 남는다. 이번 drill-token 태그가 붙은 revision만 골라낸다
#   (토큰은 §2.1에서 난수를 포함하므로 다른 실행의 revision이 섞이지 않는다).
REVS_FOUND=""
TD_ARNS_FILE=""
REVS_LEDGER=""
REVS_UNION=""
TD_PENDING_LIST=""
# ok | unknown — 조회 실패를 "대상 없음"으로 축소하지 않는다. **셸이 Bash가 아니면 아래 루프의 단어 분할이
# 달라져 결과가 틀린 채로 나오므로**, 그 세션의 탐색은 "성공"이 아니라 unknown(= §10.9 (A)⑥)에서 시작한다.
if [ "${SHELL_OK:-0}" = 1 ]; then TD_DISCOVERY_STATE=ok; else TD_DISCOVERY_STATE=unknown; fi
[ -n "${ACCOUNT_ID:-}" ] || TD_DISCOVERY_STATE=unknown   # 형식 대조의 기준값이 없으면 전건이 불일치가 된다
# **태스크와 같은 규율·같은 파일 원장** — 등록 응답을 잃어 revision ARN을 못 적은 시도가 남아 있으면,
# 성공한 빈 목록을 "등록된 revision 없음"으로 확정하지 않는다(INACTIVE 목록은 참조가 끊기면 누락되기까지 한다).
# 아래 "원장 복구 규칙"이 태그 재탐색으로 revision을 찾으면 그 family의 미대사 파일을 **자동으로 닫는다**.
# **태스크와 같은 앵커를 요구한다.** 여기서 `DRILL_DIR` 존재만 보면, 다른 드릴을 가리킨 상태에서도
# 아래가 그 경로의 `td-arns`/`td-pending`을 읽고 쓰고 지운다 — 남의 원장을 변조하는 경로다(codex-ide r22).
reconcile_td_dispatch() {
if [ "$DISPATCH_ANCHOR_OK" != 1 ]; then
  TD_DISCOVERY_STATE=unknown
  echo "STOP: reconcile_td_dispatch 직접 호출 거부 — dispatch 앵커가 서지 않았다"
  return 1
fi
for F in "$TD_A" "$TD_B"; do
  for S in ACTIVE INACTIVE DELETE_IN_PROGRESS; do
    # 한 번의 스로틀링이 탐색 전체를 unknown으로 굳히지 않도록 **30초 간격 총 3회**(태스크 목록과 같은 관용구).
    ARNS=""; TD_LIST_OK=0
    for TD_LIST_TRY in 1 2 3; do
      ARNS=$(aws ecs list-task-definitions --family-prefix "$F" --status "$S" \
        --query 'taskDefinitionArns[]' --output text 2>&1)
      if [ $? -eq 0 ]; then TD_LIST_OK=1; break; fi
      echo "TD 목록 조회 실패($TD_LIST_TRY/3) family=$F status=$S: $ARNS"
      if [ "$TD_LIST_TRY" -lt 3 ]; then sleep 30; fi
    done
    if [ "$TD_LIST_OK" != 1 ]; then
      TD_DISCOVERY_STATE=unknown
      echo "TD 목록 조회 3회 실패 family=$F status=$S → 탐색 불완전"
      continue
    fi
    for a in $ARNS; do
      [ "${a%:*}" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task-definition/$F" ] || continue
      TAGS_T=""; TD_TAG_OK=0
      for TD_TAG_TRY in 1 2 3; do
        TAGS_T=$(aws ecs describe-task-definition --task-definition "$a" --include TAGS \
          --query '[tags[?key==`purpose`].value|[0],tags[?key==`drill-token`].value|[0]]' \
          --output text 2>&1)
        if [ $? -eq 0 ]; then TD_TAG_OK=1; break; fi
        echo "TD 태그 조회 실패($TD_TAG_TRY/3) $a: $TAGS_T"
        if [ "$TD_TAG_TRY" -lt 3 ]; then sleep 30; fi
      done
      if [ "$TD_TAG_OK" != 1 ]; then
        TD_DISCOVERY_STATE=unknown
        echo "TD 태그 조회 3회 실패 $a → 소유 여부 불명"
        continue
      fi
      [ -n "$TOKEN" ] || continue                                   # 빈 토큰은 소유권 근거가 아니다(§2.7 ④)
      [ "$(printf %s "$TAGS_T" | awk '{print $1}')" = rds-restore-drill ] || continue
      [ "$(printf %s "$TAGS_T" | awk '{print $2}')" = "$TOKEN" ] || continue
      REVS_FOUND="$REVS_FOUND $a"
    done
  done
done

# **원장 ∪ 재탐색을 여기서 실제로 만든다.** 주석으로 "이어 붙인다"라고만 두면 아무도 안 붙인다.
# 원장 ARN은 목록에 안 나올 수 있다 — INACTIVE 목록은 참조가 끊기면 누락되므로(위 help 인용) 반드시 합쳐야 한다.
# **형식 불일치를 조용히 제외하지 않는다**(태스크 합집합과 대칭) — 비어 있지 않은데 형식이 틀린 원장 값은
# "그 revision이 없다"가 아니라 **원장이 깨졌다**는 뜻이고, 제외하면 실제 INACTIVE revision이 양쪽에서 사라진다.
TD_ARNS_FILE=$(awk 'NF' "$DISPATCH_DIR/td-arns") \
  || { TASK_DISCOVERY_STATE=unknown; TD_DISCOVERY_STATE=unknown; echo "STOP: td-arns 원장을 읽지 못했다"; }
TD_ARNS_FILE=$(printf '%s ' $TD_ARNS_FILE)
for a in $TD_ARNS_FILE $TD_A_ARN $TD_B_ARN; do
  case "${a%:*}" in
    "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task-definition/$TD_A"|"arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task-definition/$TD_B")
      REVS_LEDGER="$REVS_LEDGER $a" ;;
    *)
      TD_DISCOVERY_STATE=unknown
      echo "STOP: 이번 계정·리전의 드릴 TD revision ARN이 아니다(원장 손상 가능) → 탐색 불완전: $a" ;;
  esac
done
for a in $REVS_LEDGER $REVS_FOUND; do
  DUP=0
  for b in $REVS_UNION; do [ "$a" = "$b" ] && DUP=1 && break; done
  [ "$DUP" = 0 ] && REVS_UNION="$REVS_UNION $a"
done
echo "REVS_LEDGER=$REVS_LEDGER"
echo "REVS_FOUND=$REVS_FOUND"
echo "REVS_UNION=$REVS_UNION          # ← §10.6이 이 값을 그대로 쓴다"
echo "TD_DISCOVERY_STATE=$TD_DISCOVERY_STATE  # unknown이면 §10.9 (A)⑥로 남긴다(탐색 불완전)"

# 원장 복구 규칙: family마다 이번 토큰 revision이 **정확히 1건**이고 원장 칸이 비어 있으면
# 그 ARN을 TD_A_ARN/TD_B_ARN에 **실제로 대입**한 뒤 §3.2·§8.2의 읽기 전용 reconciliation으로 readiness를 재도출한다.
# 2건 이상이면 원장을 손대지 않는다(어느 것이 검증에 쓰인 revision인지 증명할 수 없다) — 정리 대상에는 전건이 들어간다.
for F in "$TD_A" "$TD_B"; do
  N=0; ONE=""
  for a in $REVS_FOUND; do
    [ "${a%:*}" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task-definition/$F" ] || continue
    N=$((N + 1)); ONE="$a"
  done
  if [ "$N" = 1 ]; then
    if [ "$F" = "$TD_A" ] && [ -z "$TD_A_ARN" ]; then TD_A_ARN="$ONE"; echo "원장 복구: TD_A_ARN=$TD_A_ARN"; fi
    if [ "$F" = "$TD_B" ] && [ -z "$TD_B_ARN" ]; then TD_B_ARN="$ONE"; echo "원장 복구: TD_B_ARN=$TD_B_ARN"; fi
    # **회수가 미대사 상태를 실제로 닫는다.** 이 family의 등록 응답을 잃었더라도 이번 토큰 revision을
    # 정확히 1건 찾았으면 그 시도는 대사된 것이다 — 파일을 지우지 않으면 회수에 성공해도 탐색이 열리지 않는다.
    if [ -n "${DRILL_DIR:-}" ] && [ -f "$DISPATCH_DIR/td-pending/$F" ]; then
      grep -Fxq -- "$ONE" "$DISPATCH_DIR/td-arns" 2>/dev/null \
        || printf '%s\n' "$ONE" >> "$DISPATCH_DIR/td-arns"
      if grep -Fxq -- "$ONE" "$DISPATCH_DIR/td-arns" 2>/dev/null; then
        rm -f "$DISPATCH_DIR/td-pending/$F"        # ARN 기록 확인 뒤에만 닫는다
        echo "미대사 등록 회수: $F → $ONE"
      else
        echo "STOP: $ONE 을 원장에 기록하지 못해 $F 의 미대사 파일을 닫지 않는다"
      fi
    fi
  fi
  echo "$F: 이번 토큰 revision ${N}건 ${ONE:+대표=$ONE}"
done

# **미대사 등록이 남아 있으면 탐색을 닫지 않는다** — 위 회수까지 끝난 뒤에 판정해야
# "회수했는데도 unknown"이 되지 않는다(회수 전에 판정하면 정상 복구가 영구 FAIL이 된다).
if TD_PENDING_RAW=$(find "$DISPATCH_DIR/td-pending" -mindepth 1 -maxdepth 1 -print 2>&1); then
  for k in $TD_PENDING_RAW; do TD_PENDING_LIST="$TD_PENDING_LIST $(basename "$k")"; done
else
  TD_DISCOVERY_STATE=unknown
  echo "STOP: td-pending 열거 실패 — 미대사 파일 유무를 확정할 수 없다: $TD_PENDING_RAW"
fi
if [ -n "$TD_PENDING_LIST" ]; then
  TD_DISCOVERY_STATE=unknown
  echo "STOP: revision ARN을 못 받은 등록 시도가 남아 있다:$TD_PENDING_LIST — 태그 재탐색으로도 회수되지 않았다"
fi
}

if [ "${DISPATCH_ANCHOR_OK:-0}" != 1 ]; then
  TD_DISCOVERY_STATE=unknown
  echo "STOP: dispatch 앵커가 서지 않았다 — TD 원장을 읽지도 쓰지도 않는다(위 §10.3 앵커 검사 참조)"
else
  reconcile_td_dispatch
fi
unset -f reconcile_td_dispatch
echo "TD_DISCOVERY_STATE=$TD_DISCOVERY_STATE (회수 후 최종)"

# ⓒ **태그 없는 태스크의 수동 후보 기록 — pending을 종결하지 않는 진단 전용 폴백이다.**
# exact ARN·cluster·startedBy는 "이번 드릴 태스크"라는 소유권만 증명하고, 어느 client token 요청의 결과인지는
# 증명하지 못한다. 같은 ARN 하나를 여러 token에 붙이면 아직 보이지 않는 다른 태스크를 누락할 수 있으므로,
# 이 블록은 정리 합집합에 후보 ARN을 보태기만 하고 `task-pending/*`를 삭제하지 않는다.
# pending을 닫는 권위 있는 경로는 둘뿐이다: (1) 같은 client token 멱등 재전송의 원응답(taskArn/확정 failures[]),
# (2) `drill-client-token` 태그가 직접 결속한 위 ⓑ-2 자동 대사. 둘 다 없으면 §10.9 (A)⑥으로 남겨 에스컬레이션한다.
T_CANDIDATE_ARN=""   # ← 태그 없는 것으로 관측한 이번 드릴 taskArn. 비워 두면 아무 것도 기록하지 않는다.
if [ "$DISPATCH_ANCHOR_OK" != 1 ]; then
  echo "STOP: 앵커가 서지 않았다 — 후보를 기록하지 않는다"
elif [ -z "$T_CANDIDATE_ARN" ]; then
  echo "진단 후보 없음 — 미대사 token은 같은 client token 재전송으로만 닫는다"
elif [ "${T_CANDIDATE_ARN%%/*}" != "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task" ]; then
  echo "STOP: taskArn 형식이 이번 계정·리전과 다르다: $T_CANDIDATE_ARN"
elif ! T_CANDIDATE_OWN=$(aws ecs describe-tasks --cluster "$CLUSTER" --tasks "$T_CANDIDATE_ARN" \
       --query 'tasks[0].[taskArn,clusterArn,startedBy]' --output text 2>&1) \
     || [ "$(printf %s "$T_CANDIDATE_OWN" | awk '{print $1}')" != "$T_CANDIDATE_ARN" ] \
     || [ "$(printf %s "$T_CANDIDATE_OWN" | awk '{print $2}')" != "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:cluster/linkpulse-prod-cluster" ] \
     || [ -z "$TOKEN" ] \
     || [ "$(printf %s "$T_CANDIDATE_OWN" | awk '{print $3}')" != "$TOKEN" ]; then
  echo "STOP: 이 taskArn이 이번 드릴 소유로 확인되지 않았다(보관 만료 MISSING 포함): $T_CANDIDATE_OWN"
else
  grep -Fxq -- "$T_CANDIDATE_ARN" "$DISPATCH_DIR/task-arns" 2>/dev/null \
    || printf '%s\n' "$T_CANDIDATE_ARN" >> "$DISPATCH_DIR/task-arns"
  if grep -Fxq -- "$T_CANDIDATE_ARN" "$DISPATCH_DIR/task-arns" 2>/dev/null; then
    echo "정리 후보 기록: $T_CANDIDATE_ARN — pending은 보존한다. 같은 client token으로 원응답을 회수한다"
  else
    echo "STOP: 후보 ARN 원장 기록 실패 — pending과 기존 원장을 그대로 둔다"
  fi
fi
```

> **STOPPED 태스크는 일정 시간만 조회 가능하게 보관**되므로 **재탐색은 드릴 종료 직후 수행**한다. 그 창을 놓쳐 회수하지 못한 STOPPED 태스크는 **이미 종결된 것이라 (A)가 아니다**(비용·권한 없음) — 증거 완결성만 손해다. 다만 **원장의 `taskArn`을 조회했을 때의 `failures[].reason=MISSING`은 곧바로 종결이 아니다**(§10.5).

### 10.4 ① 드릴 DB 삭제 **호출** — 과금 시계를 가장 먼저 멈춘다

여기서는 **호출만** 하고 기다리지 않는다 — 완료 대기(waiter)는 §10.8 ⑤에서 한 번에 모은다.

**세 갈래를 구분한다: 부재(멱등 성공) / 조회 실패(= 상태 불명 → (A)) / 존재(삭제 호출).** 조회 실패를 부재로 읽으면 **과금 중인 인스턴스를 남긴 채 "정리 완료"로 기록**된다 — 같은 레포의 `scripts/full-destroy-prod.sh`가 최근 고친 것이 정확히 이 안티패턴(`2>/dev/null | grep -q`로 실패를 "리소스 없음"으로 오독)이다.

```bash
DEL_STATE=unknown        # unknown | absent | present
OUT=$(aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" \
      --query 'DBInstances[0].[DBInstanceStatus,DBInstanceArn]' --output text 2>&1)
RC=$?
if [ "$RC" -eq 0 ]; then
  DEL_STATE=present; DRILL_STATUS=$(printf %s "$OUT" | awk '{print $1}'); DRILL_ARN=$(printf %s "$OUT" | awk '{print $2}')
elif printf %s "$OUT" | grep -q "DBInstanceNotFound"; then
  DEL_STATE=absent
else
  DEL_STATE=unknown      # 자격증명 만료·리전 오설정·스로틀링·네트워크 등 — 부재로 읽지 않는다
fi
echo "DEL_STATE=$DEL_STATE  rc=$RC  $OUT"

case "$DEL_STATE" in
  absent)  echo "드릴 DB 없음(DBInstanceNotFound) = 성공(멱등)" ;;
  unknown) echo "STOP: 조회 실패 = 상태 불명 → (A) 잔존으로 기록하고 원인 해소 후 재진입(§10.9)" ;;
  present)
    if [ "$DRILL_STATUS" = "deleting" ]; then
      # 재진입 경로: 앞선 정리에서 이미 접수됐다. 재호출하면 InvalidDBInstanceState 만 쌓인다.
      echo "이미 deleting = 삭제 접수됨 → 재호출하지 않고 §10.8 ⑤ waiter로 간다"
    else
      # 태그 값까지 정확히 확인한다(키 존재만으로는 부족). 조회 실패는 불일치가 아니라 unknown → 30초×3 재조회.
      DEL_TAG_STATE=unknown; TAGV=""; TAG_TOKEN=""
      for TAG_TRY in 1 2 3; do
        DEL_TAGS=$(aws rds list-tags-for-resource --resource-name "$DRILL_ARN" \
          --query 'TagList[?Key==`purpose`||Key==`drill-token`].[Key,Value]' --output text 2>&1)
        DEL_TAGS_RC=$?
        if [ "$DEL_TAGS_RC" -eq 0 ]; then
          TAGV=$(printf %s "$DEL_TAGS" | awk '$1=="purpose"{print $2}')
          TAG_TOKEN=$(printf %s "$DEL_TAGS" | awk '$1=="drill-token"{print $2}')
          DEL_TAG_STATE=ok
          break
        fi
        echo "태그 조회 실패($TAG_TRY/3) rc=$DEL_TAGS_RC $DEL_TAGS"
        if [ "$TAG_TRY" -lt 3 ]; then sleep 30; fi
      done
      # 소유권은 **§2.7의 단일 정의**를 그대로 쓴다: 난수 토큰 태그가 1차 근거이고, 원장 ARN은 있을 때만 강화 조건.
      # `RESTORE_CALL_STATE`는 조건에 넣지 않는다 — 전송 불명·뒤늦은 접수·재시도에서 호출 결과와 실제 소유권이
      # 갈라지는데, 거기서 삭제를 막으면 **우리가 만든 과금 중인 인스턴스에 정리 경로가 사라진다.**
      RESTORE_OWNER_OK=0
      if [ -z "$RESTORE_EXPECTED_ARN" ] || [ "$DRILL_ARN" = "$RESTORE_EXPECTED_ARN" ]; then
        RESTORE_OWNER_OK=1
      fi
      echo "target=$DRILL_DB status=$DRILL_STATUS purpose=$TAGV token=$TAG_TOKEN(기대 '${TOKEN:-빈값}') tag_state=$DEL_TAG_STATE"
      echo "  call_state=$RESTORE_CALL_STATE(참고) expected_arn=$RESTORE_EXPECTED_ARN owner_ok=$RESTORE_OWNER_OK  (운영=$SRC_DB — 이 값이면 안 된다)"
      if [ "${SHELL_OK:-0}" = 1 ] \
         && [ "$DRILL_DB" != "$SRC_DB" ] && [ "$DRILL_DB" = "linkpulse-restore-drill" ] \
         && [ "$DEL_TAG_STATE" = ok ] \
         && [ "$TAGV" = "rds-restore-drill" ] && [ -n "$TOKEN" ] && [ "$TAG_TOKEN" = "$TOKEN" ] \
         && [ "$RESTORE_OWNER_OK" = 1 ]; then
        date -u +%Y-%m-%dT%H:%M:%SZ                           # ← 삭제 **요청** 시각(원장)
        aws rds delete-db-instance --db-instance-identifier "$DRILL_DB" \
          --skip-final-snapshot --delete-automated-backups \
          --query 'DBInstance.DBInstanceStatus' --output text  # ⚠️💸 deleting 이 나오면 접수됨
      else
        echo "STOP: 셸(SHELL_OK=${SHELL_OK:-미설정})·식별자/태그/복원 소유권 확인 실패 — 삭제를 호출하지 않았다. (A)로 남기고 사람이 판단"
      fi
    fi ;;
esac
```

- **§2.7 OWNED의 다섯 조건 + "운영 식별자 아님"이 전부 참일 때만** 삭제 호출에 도달한다. 하나라도 어긋나면 **호출하지 않고 (A)** 로 남긴다 — 변수 오염 상태에서 남의 DB를 지우는 것보다 잔존이 낫다.
- **난수 토큰 태그는 락이 아니라 소유권 방어다.** 다른 세션이 만든 고정 식별자가 보이면 그 리소스의 `drill-token`은 이번 `$TOKEN`과 달라 조건 ④에서 배제된다. 그러나 두 세션이 생성 전 부재 조회를 함께 통과하는 레이스는 막지 못하므로, §2.2·§2.5의 단일 변경 작업 락이 없으면 sentinel을 쓰거나 리소스를 만들지 않는다.
- **여기에 `RESTORE_CALL_STATE`를 더 얹지 않는 이유**(§2.7): 응답 유실 뒤 최초 요청이 뒤늦게 접수되면 재호출은 `AlreadyExists`를 받고, 호출 상태를 소유권 조건으로 쓰면 **바로 그 순간 우리 인스턴스가 고아가 된다** — 태그는 우리 것인데 삭제는 거부되는 상태다. 소유권 판정은 리소스에 붙은 증거(태그·ARN)로만 한다.
- **`creating`·`modifying` 등 전이 상태에서 호출이 거부되면**(`InvalidDBInstanceState`) 오류를 기록하고 **30초 뒤 재조회해 다시 판단**한다(최초 포함 총 3회). **이미 `deleting`이면 재호출하지 않는다** — 위 블록이 그 상태를 따로 분기해 §10.8 ⑤의 waiter로 넘긴다(재진입에서 불필요한 실패 기록이 쌓이지 않게).
- 드릴 인스턴스는 `--no-deletion-protection`·`--backup-retention-period 0`으로 만들었으므로 삭제 보호·최종 스냅샷 요구에 걸리지 않는다. **운영(`linkpulse-prod-pg`)은 `deletion_protection=true`라 실수로 이 명령을 맞아도 거부된다** — 그래도 식별자를 눈으로 확인하는 것이 1차 방어다.
- 삭제는 비동기다. **여기서 통과했다고 과금이 끝난 것이 아니다** — 종료 판정은 §10.8 ⑤의 `wait db-instance-deleted`다.

### 10.5 ② 태스크 — 위에서부터 먼저 맞는 것 하나로 확정(우선순위 규칙)

**비종결의 분류 키는 `desiredStatus` 하나다.** (`desiredStatus`와 `lastStatus`를 섞으면 `desiredStatus=STOPPED ∧ lastStatus=RUNNING`이 (다)로 떨어져 **중복 `stop-task`** 를 부른다.)

| 우선순위 | 조건 | 조치 |
| -------- | ---- | ---- |
| **(가) 종결** | `lastStatus` ∈ {`STOPPED`, `DELETED`} | **조치 없음** |
| **(나) 비종결 · 이미 정지 요청됨** | `desiredStatus == STOPPED` (`lastStatus`가 `RUNNING`·`DEACTIVATING`·`STOPPING`·`DEPROVISIONING` 무엇이든 **묻지 않는다**) | **`wait tasks-stopped`만** (`stop-task` 재호출 금지) |
| **(다) 비종결 · 정지 요청 없음** | `desiredStatus == RUNNING` | **`stop-task` → `wait tasks-stopped`** |
| **(별도)** | `describe-tasks`가 `failures[].reason=MISSING` | 아래 MISSING 규율 |

대표 조합(상태명이 늘어도 분기를 놓치지 않게):

| `desiredStatus` / `lastStatus` | 분류 | 조치 |
| ------------------------------ | ---- | ---- |
| `STOPPED` / `STOPPED` | (가) | 없음 |
| `STOPPED` / `RUNNING` | **(나)** | waiter만 (정지 요청은 이미 나갔다 — waiter 상한을 넘겨 빠져나온 뒤 재진입하면 정확히 이 상태다) |
| `STOPPED` / `STOPPING`·`DEPROVISIONING` | (나) | waiter만 |
| `RUNNING` / `RUNNING`·`PENDING`·`ACTIVATING` | (다) | `stop-task` → waiter |

**대상은 §10.3이 만든 `TASKS_UNION` 전건이다.** 아래 블록은 그 전건을 순회하며 ARN마다 종착 상태를 원장 파일에 남긴다 — TD(§10.6)와 같은 형태다. 순회 없이 단일 ARN만 처리하면 **실패한 이전 시도나 응답 유실 뒤 재탐색된 `RUNNING` 태스크가 정지되지 않은 채** 남는다(운영 마스터 비밀번호를 쥔 컨테이너).

```bash
EXPECTED_CLUSTER_ARN="arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:cluster/linkpulse-prod-cluster"
TASK_GATE_OK=0
TASK_LEDGER_CONFLICT=0
TASK_LEDGER_COMPLETE=0
TASK_SETTLED_COUNT=0
TASK_BLOCKED_COUNT=0
if [ "${SHELL_OK:-0}" = 1 ] && [ "${TASK_DISCOVERY_STATE:-}" = ok ]; then
  TASK_GATE_OK=1
else
  echo "STOP: 셸이 Bash가 아니거나(SHELL_OK=${SHELL_OK:-미설정}) §10.3 태스크 재탐색이 완료되지 않았다(TASK_DISCOVERY_STATE=${TASK_DISCOVERY_STATE:-미설정}) — bash 로 §10.3부터 실행한다"
  echo "      → 아래 블록은 아무 것도 바꾸지 않고 TASK_LEDGER_COMPLETE=0으로 끝나 §10.9 (A)④에 남는다"
fi
if [ "$TASK_GATE_OK" = 1 ]; then
  TASK_DIR="$DRILL_DIR/task-stop"
  mkdir -p "$TASK_DIR"
  TASK_SETTLED_FILE="$TASK_DIR/settled"
  TASK_BLOCKED_FILE="$TASK_DIR/blocked"
  TASK_CONFLICT_FILE="$TASK_DIR/conflict"
  : > "$TASK_SETTLED_FILE"
  : > "$TASK_BLOCKED_FILE"
  : > "$TASK_CONFLICT_FILE"
fi

# TD의 `record_td_result`와 같은 규율 — 같은 ARN을 두 번 기록하면 원장 충돌 = (A).
# 충돌은 **파일에도** 남긴다: 아래 완결성 판정이 독립 블록이라(붙여넣기 재실행 가능) 셸 변수만으로는
# 앞선 실행의 충돌을 못 본다.
record_task_result() {
  local result="$1"
  local arn="$2"
  if grep -Fxq -- "$arn" "$TASK_SETTLED_FILE" || grep -Fxq -- "$arn" "$TASK_BLOCKED_FILE"; then
    echo "STOP: $arn 종착 상태를 두 번 기록하려 했다 — 태스크 원장 충돌 = (A)"
    printf '%s\n' "$arn" >> "$TASK_CONFLICT_FILE"
    TASK_LEDGER_CONFLICT=1
    return 1
  fi
  case "$result" in
    settled) printf '%s\n' "$arn" >> "$TASK_SETTLED_FILE" ;;
    blocked) printf '%s\n' "$arn" >> "$TASK_BLOCKED_FILE" ;;
    *) echo "STOP: 알 수 없는 태스크 판정 '$result' = (A)"
       printf '%s\n' "$arn" >> "$TASK_CONFLICT_FILE"; TASK_LEDGER_CONFLICT=1; return 1 ;;
  esac
}

[ "$TASK_GATE_OK" = 1 ] || TASKS_UNION=""      # 게이트가 닫힌 세션은 순회 자체를 하지 않는다
for T in $TASKS_UNION; do
  TASK_TARGET_OK=0
  T_STATE=miss              # ok | mismatch | late | miss
  T_OUT=""; T_RC=0; T_DESIRED=""; T_LAST=""
  # **MISSING 규율 1을 코드로 집행한다** — 결과적 일관성·전이 상태·스로틀링 한 번이
  # 정상 종결된 태스크를 곧바로 blocked로 굳히지 않도록 **30초 간격 총 3회**(최초 포함) 재조회한다.
  # 반대로 **소유권 불일치는 시간이 지나도 바뀌지 않으므로 재조회하지 않는다**(성격이 다른 실패다).
  for T_TRY in 1 2 3; do
    T_OUT=$(aws ecs describe-tasks --cluster "$CLUSTER" --tasks "$T" \
      --query 'tasks[0].[taskArn,clusterArn,startedBy,desiredStatus,lastStatus]' --output text 2>&1)
    T_RC=$?
    if [ "$T_RC" -eq 0 ] && [ -n "$T_OUT" ] && [ "$T_OUT" != None ]; then
      T_ARN=$(printf %s "$T_OUT" | awk '{print $1}')
      T_CLUSTER=$(printf %s "$T_OUT" | awk '{print $2}')
      T_STARTED_BY=$(printf %s "$T_OUT" | awk '{print $3}')
      T_DESIRED=$(printf %s "$T_OUT" | awk '{print $4}')
      T_LAST=$(printf %s "$T_OUT" | awk '{print $5}')
      if [ "$CLUSTER" = "linkpulse-prod-cluster" ] && [ "$T_ARN" = "$T" ] \
         && [ "$T_CLUSTER" = "$EXPECTED_CLUSTER_ARN" ] \
         && [ -n "$TOKEN" ] && [ "$T_STARTED_BY" = "$TOKEN" ]; then
        TASK_TARGET_OK=1; T_STATE=ok
      else
        T_STATE=mismatch
      fi
      break
    fi
    echo "== $T  describe 실패/MISSING($T_TRY/3) rc=$T_RC $T_OUT"
    if [ "$T_TRY" -lt 3 ]; then sleep 30; fi
  done
  # **MISSING 규율 2를 코드로 집행한다** — 3회 뒤에도 못 봤으면 `--started-by` 단독 호출로 대사한다.
  # 여기서 잡히면 서버가 **클러스터·startedBy를 이미 대조**한 것이므로 우리 태스크가 살아 있다는 뜻이다
  # (describe만 전파가 늦다) → blocked로 적고 끝내면 비밀번호를 쥔 컨테이너를 그대로 두는 것이라,
  # 활성 필터의 의미대로 desiredStatus=RUNNING으로 보고 (다) 경로에서 정지시킨다.
  if [ "$T_STATE" = miss ]; then
    MISS_OUT=$(aws ecs list-tasks --cluster "$CLUSTER" --started-by "$TOKEN" \
      --query 'taskArns[]' --output text 2>&1)
    MISS_RC=$?
    # `ok` 분기와 **같은 클러스터 리터럴 검사**를 건다 — 같은 게이트가 두 벌로 갈라지지 않게 한다.
    if [ "$MISS_RC" -eq 0 ] && [ "$CLUSTER" = "linkpulse-prod-cluster" ]; then
      for m in $MISS_OUT; do
        if [ "$m" = "$T" ]; then
          TASK_TARGET_OK=1; T_STATE=late; T_DESIRED=RUNNING; T_LAST=""
          break
        fi
      done
    fi
    echo "== $T  startedBy 대사 rc=$MISS_RC → state=$T_STATE"
  fi
  echo "== $T  TASK_TARGET_OK=$TASK_TARGET_OK state=$T_STATE rc=$T_RC $T_OUT"

  if [ "$TASK_TARGET_OK" != 1 ]; then
    # 재조회·대사까지 거치고도 MISSING이거나 소유권이 어긋난 것은 **상태 불명**이다 → (A).
    # 종결 관측 이력이 있는 MISSING만 아래 규율 3에서 사람이 settled로 옮긴다.
    record_task_result blocked "$T"
    echo "STOP: taskArn·clusterArn·startedBy 대조 실패 또는 3회+대사 후에도 MISSING — stop-task를 호출하지 않고 (A)로 남긴다"
  elif [ "$T_LAST" = STOPPED ] || [ "$T_LAST" = DELETED ]; then
    record_task_result settled "$T"
    echo "이미 종결 = 조치 없음"
  elif [ "$T_DESIRED" = STOPPED ]; then
    if aws ecs wait tasks-stopped --cluster "$CLUSTER" --tasks "$T"; then   # (나): **ARN 하나씩**
      record_task_result settled "$T"
    else
      record_task_result blocked "$T"
      echo "STOP: waiter 미통과(상한 초과 등) — 마지막 관측으로 (A)에 남긴다"
    fi
  elif [ "$T_DESIRED" = RUNNING ]; then
    # 전이 상태 거부는 시간이 흡수한다 → **30초 간격 총 3회**(규율 문장과 같은 횟수).
    STOP_OK=0
    for S_TRY in 1 2 3; do
      if aws ecs stop-task --cluster "$CLUSTER" --task "$T" --reason "restore drill cleanup"; then
        STOP_OK=1; break
      fi
      echo "stop-task 실패($S_TRY/3) $T"
      if [ "$S_TRY" -lt 3 ]; then sleep 30; fi
    done
    if [ "$STOP_OK" != 1 ]; then
      record_task_result blocked "$T"
      echo "STOP: stop-task 3회 실패 — 비종결로 두고 (A)로 남긴다"
    elif aws ecs wait tasks-stopped --cluster "$CLUSTER" --tasks "$T"; then # (다): **ARN 하나씩**
      record_task_result settled "$T"
    else
      record_task_result blocked "$T"
      echo "STOP: stop-task 접수 후 waiter 미통과 — (A)로 남긴다"
    fi
  else
    record_task_result blocked "$T"
    echo "STOP: 예상 밖 desiredStatus=$T_DESIRED — 변경하지 않고 (A)로 남긴다"
  fi
done
```

**완결성 판정은 아래 독립 블록이 파일 원장만 읽어 도출한다.** 위 순회 블록과 분리한 이유는 MISSING 규율 3(종결 관측 이력이 있는 ARN을 사람이 `blocked`→`settled`로 옮기는 자리)이 **판정에 도달해야 하기 때문**이다 — 위 블록을 다시 붙여 넣으면 `: >` 가 두 파일을 비워 그 이동이 사라진다. 이동한 뒤에는 **이 블록만** 다시 붙여 넣는다(§4.3 sentinel-B·§7 reconciliation과 같은 "생성/판정 분리" 관용구). `TASK_LEDGER_COMPLETE`를 손으로 `1`로 적지 않는다 — 그 순간 확인이 아니라 선언이 된다(§2.1).

```bash
# 완결성: **전건이 정확히 한 번씩** 종착했을 때만 원장이 닫힌다(TD의 `TD_LEDGER_COMPLETE`와 같은 형태).
TASK_LEDGER_COMPLETE=0
TASK_SETTLED_COUNT=0
TASK_BLOCKED_COUNT=0
TASK_LEDGER_CONFLICT=0
if [ "${TASK_GATE_OK:-0}" != 1 ]; then
  echo "STOP: 태스크 게이트가 닫힌 세션이다 — TASK_LEDGER_COMPLETE=0으로 §10.9 (A)④에 남는다"
elif [ ! -f "${TASK_SETTLED_FILE:-}" ] || [ ! -f "${TASK_BLOCKED_FILE:-}" ]; then
  echo "STOP: 태스크 원장 파일이 없다 — 위 순회 블록을 먼저 실행한다"
else
  [ -s "$TASK_CONFLICT_FILE" ] && TASK_LEDGER_CONFLICT=1     # 순회 중 기록된 충돌(세션을 넘겨 읽는다)
  TASK_SETTLED_COUNT=$(awk 'NF' "$TASK_SETTLED_FILE" | wc -l | tr -d ' ')
  TASK_BLOCKED_COUNT=$(awk 'NF' "$TASK_BLOCKED_FILE" | wc -l | tr -d ' ')
  # 원장 행이 **합집합 전건과 정확히 1:1**인지 파일에서 다시 확인한다 — 수동 이동이 ARN을 잘못 옮겨 적었거나
  # 양쪽에 남겨 뒀을 수 있다. 이 재확인이 없으면 사람의 이동이 완결성을 만들어 내는 우회로가 된다.
  for a in $(awk 'NF' "$TASK_SETTLED_FILE" "$TASK_BLOCKED_FILE"); do
    IN_UNION=0
    for u in $TASKS_UNION; do [ "$a" = "$u" ] && IN_UNION=1 && break; done
    if [ "$IN_UNION" = 0 ]; then
      TASK_LEDGER_CONFLICT=1
      echo "STOP: 원장에 합집합 밖 ARN이 있다 = (A): $a"
    fi
    SEEN=0
    for b in $(awk 'NF' "$TASK_SETTLED_FILE" "$TASK_BLOCKED_FILE"); do
      [ "$a" = "$b" ] && SEEN=$((SEEN + 1))
    done
    if [ "$SEEN" != 1 ]; then
      TASK_LEDGER_CONFLICT=1
      echo "STOP: 같은 ARN이 원장에 ${SEEN}번 있다 = (A): $a"
    fi
  done
  if [ "$TASK_LEDGER_CONFLICT" = 0 ] && [ "$TASK_BLOCKED_COUNT" = 0 ] \
     && [ "$((TASK_SETTLED_COUNT + TASK_BLOCKED_COUNT))" = "${TASKS_TOTAL:-0}" ]; then
    TASK_LEDGER_COMPLETE=1
  fi
fi
echo "TASK_LEDGER_COMPLETE=$TASK_LEDGER_COMPLETE settled=$TASK_SETTLED_COUNT blocked=$TASK_BLOCKED_COUNT total=${TASKS_TOTAL:-미설정} conflict=$TASK_LEDGER_CONFLICT"
```

- **waiter는 반드시 ARN 하나씩** 건다 — 매처가 "all elements"라 여러 ARN을 한 번에 넘기면 **하나가 MISSING이거나 늦게 내려가는 것만으로 배치 전체가 600초를 소진**한다(대상 5건이면 최악 50분).
- **`stop-task`가 전이 상태 때문에 거부되면** 오류를 기록하고 **30초 뒤 재호출**한다(**최초 포함 총 3회**, 초과하면 비종결로 두고 (A)) — 위 루프의 `S_TRY` 3회가 이 규율을 집행한다.
- **waiter가 600초를 넘겨도 절차를 끊지 않는다** — 마지막 `describe-tasks` 결과로 분류해 잔존 목록 판정에 넘긴다.
- **왜 정지시키는가**: (다)의 태스크는 **운영 마스터 비밀번호를 주입받은 컨테이너**다. "STOPPED 확인"이 아니라 **정지시키고 대기**하는 이유가 이것이다. 실패한 시도의 태스크도 대상.

**MISSING은 종결 근거가 있을 때만 종결이다.** ECS API는 결과적 일관성이라 `run-task` 직후 조회에는 새 태스크가 **아직 안 보일 수 있고**, `MISSING`의 의미는 "찾지 못함"일 뿐이라 **보관 만료 · 전파 지연 · 잘못된 클러스터/리전/ARN을 구분해 주지 않는다.** 응답만 받고 곧바로 중단해 정리 게이트로 들어온 태스크를 종결로 세면, **그 태스크가 뒤늦게 기동해 운영 마스터 비밀번호를 주입받은 채 실행**되는데도 (d)는 PASS로 기록된다.

1. **30초 간격으로 `describe-tasks` 재조회(최초 포함 총 3회)** — 위 루프의 `T_TRY` 3회가 집행한다(소유권 불일치는 시간이 바꾸지 않으므로 재조회 대상이 아니다).
2. 그래도 MISSING이면 **`list-tasks --started-by "$TOKEN"` 단독 호출로 대사**한다(전파가 늦은 태스크가 여기서 잡힐 수 있다) — 위 루프의 `MISS_OUT` 호출이 집행하며, **여기서 잡히면 서버가 클러스터·startedBy를 이미 대조한 것**이므로 살아 있는 우리 태스크로 보고 (다) 경로에서 정지시킨다.
3. 그 뒤에도 MISSING이면 **원장에 그 ARN의 종결 관측 이력**(`STOPPED`/`DELETED`를 한 번이라도 본 기록, 또는 `exitCode` 확인 기록)**이 있는 경우에만 (가) 종결**로 인정하고 발견 시각을 증거에 남긴다(보관 창이 지난 정상 케이스). 위 루프는 MISSING을 일단 `blocked`로 적으므로, **이 경우에만** 사람이 `settled` 파일로 옮기고 근거(관측 이력)를 증거에 남긴다 — 코드가 판정할 수 없는 유일한 자리다. 이동 명령과 재실행 순서는 아래 블록이다.
4. **생성 응답만 있고 종결을 한 번도 관측하지 못한 ARN은 "상태 불명"으로 (A)** → (d) FAIL. **상한 초과를 종결로 승격하지 않는다** — 모르는 것을 끝난 것으로 세지 않는다.

**규율 3의 수동 이동**(그 ARN에 종결 관측 이력이 있을 때만). `grep -Fxv … > tmp && mv` 형태로 쓰지 않는다 — **blocked에 그 한 줄만 있으면 `grep`이 출력 없이 1을 반환해 `mv`가 실행되지 않고**, 같은 ARN이 양쪽에 남아 완결성 블록이 중복 충돌 = (A)로 잡는다. 옮긴 뒤에는 **순회 블록이 아니라 완결성 블록만** 다시 붙여 넣는다(순회 블록은 두 파일을 비우고 같은 ARN을 다시 `blocked`로 적는다).

```bash
T_MOVE=""    # ← 종결 관측 이력이 확인된 taskArn 하나(따옴표 안에). 근거는 증거에 함께 남긴다
if [ "${TASK_GATE_OK:-0}" = 1 ] && [ -n "$T_MOVE" ] && grep -Fxq -- "$T_MOVE" "$TASK_BLOCKED_FILE"; then
  grep -Fxv -- "$T_MOVE" "$TASK_BLOCKED_FILE" > "$TASK_BLOCKED_FILE.tmp"
  MOVE_RC=$?
  # `grep`의 **1(선택된 행 없음)과 2 이상(파일 읽기 오류 등)은 다르다.** 1은 blocked에 그 한 줄만
  # 있던 정상 결과이고, 2 이상에서 그대로 진행하면 **빈 tmp가 원장을 덮어쓰고** ARN은 settled에 들어가
  # 대상이 하나일 때 `settled=1 blocked=0 total=1` → 거짓 `COMPLETE=1`이 된다. 0·1일 때만 진행한다.
  if [ "$MOVE_RC" -le 1 ]; then
    mv "$TASK_BLOCKED_FILE.tmp" "$TASK_BLOCKED_FILE"      # `&&`로 잇지 않는다(rc=1이 정상이라서)
    printf '%s\n' "$T_MOVE" >> "$TASK_SETTLED_FILE"
    echo "이동 완료: $T_MOVE — 이제 완결성 블록만 다시 실행한다"
  else
    rm -f "$TASK_BLOCKED_FILE.tmp"
    echo "STOP: blocked 원장을 읽지 못했다(grep rc=$MOVE_RC) — 원본을 그대로 두고 이동하지 않는다"
  fi
else
  echo "STOP: 이동하지 않았다 — gate=${TASK_GATE_OK:-미설정} arn='${T_MOVE}' (blocked 원장에 있는 ARN만 옮긴다)"
fi
```

- **완결성 판정은 `TASK_LEDGER_COMPLETE`가 단일 소스다**(TD의 `TD_LEDGER_COMPLETE`와 대칭). `TASKS_UNION` 전건이 정확히 한 번씩 `settled`로 닫히고 `blocked`가 0일 때만 1이 된다. `TASKS_TOTAL`과 종착 건수가 어긋나면(루프 중단·중복 기록) 0이므로 §10.9 (A)④에 남는다. 완결성 블록은 **파일을 비우지 않고 다시 세기만** 하며, 원장 행이 합집합 밖이거나 중복이면 규율 3의 수동 이동이라도 충돌로 잡아 0으로 닫는다.

### 10.6 ③ TD 종결은 2단계다

**모든 revision을 각각 `deregister`한 뒤에야 삭제할 수 있다** — _"You must deregister a task definition revision before you delete it."_ TD-A만 deregister하고 둘을 함께 삭제하면 **ACTIVE인 TD-B는 `failures[]`에 남아 (A) 잔존이 확정된다.**

**재진입에서도 멱등이어야 한다** — 앞선 정리에서 이미 삭제가 접수돼 `DELETE_IN_PROGRESS`이거나 물리적으로 사라진 revision에 다시 `deregister`를 걸면 실패가 나는데, 그걸 (A)로 세면 **정상 정리가 (d) FAIL로 뒤집힌다**(§10.1의 "NotFound·이미 종결 상태는 성공" 규칙 위반). 그래서 **현재 상태로 먼저 분류**한다: `ACTIVE`만 deregister 대상, `INACTIVE`만 삭제 대상, `DELETE_IN_PROGRESS`·부재는 **조치 없음(종결)**.

```bash
# 유효한 예시 ARN을 기본값으로 두지 않는다. 정리 대상 = §10.3이 **실제로 합집합을 만들어 둔** `REVS_UNION`
# (= 원장의 정확한 revision ARN ∪ 이번 drill-token 태그로 회수한 것, 중복 제거).
# **§10.3을 건너뛴 실행은 주석이 아니라 게이트로 막는다**(§2.1) — 건너뛰면 REVS가 비어 정리 대상이 통째로
# 사라지고, 그 "0건"은 "없다"가 아니라 "못 봤다"인데도 아래 완결성 판정이 통과해 (d)가 PASS로 기록된다.
TD_GATE_OK=0
if [ "${SHELL_OK:-0}" = 1 ] && [ "${TD_DISCOVERY_STATE:-}" = ok ]; then
  TD_GATE_OK=1
else
  echo "STOP: 셸이 Bash가 아니거나(SHELL_OK=${SHELL_OK:-미설정}) §10.3 재탐색이 완료되지 않았다(TD_DISCOVERY_STATE=${TD_DISCOVERY_STATE:-미설정}) — bash 로 §10.3부터 실행한다"
  echo "      → 아래 블록은 아무 것도 바꾸지 않고 TD_LEDGER_COMPLETE=0으로 끝나 §10.9 (A)⑤·⑥에 남는다"
fi
REVS=""
TD_LEDGER_CONFLICT=0
TD_LEDGER_COMPLETE=0
TD_SETTLED_COUNT=0
TD_BLOCKED_COUNT=0
# revision별 종착 상태의 단일 원장. 삭제 호출 전 검증에서 막힌 대상도 빠짐없이 (A)에 기록한다.
# **파일 준비도 게이트 안에서 한다** — 게이트가 닫힌 세션은 DRILL_DIR가 비어 `mkdir -p "/td-delete"`가
# 실패하는데, 판정은 어차피 (A)로 옳게 끝나므로 그 오류 출력이 원인만 흐린다.
if [ "$TD_GATE_OK" = 1 ]; then
  REVS="$REVS_UNION"
  DEL_DIR="$DRILL_DIR/td-delete"
  mkdir -p "$DEL_DIR"
  TD_SETTLED_FILE="$DEL_DIR/settled"
  TD_BLOCKED_FILE="$DEL_DIR/blocked"
  : > "$TD_SETTLED_FILE"
  : > "$TD_BLOCKED_FILE"
fi

# 이 문서에서 함수를 쓰는 유일한 블록이다(같은 판정을 6곳에서 반복하지 않기 위함).
# 대화형 셸에 붙여 넣으면 `record_td_result`·`is_td_not_found` 정의가 **세션에 남는다** — 재진입에서
# 이전 정의가 그대로 쓰이지 않도록, 문서를 고친 뒤에는 이 블록을 처음부터 다시 붙여 넣는다.
record_td_result() {
  local result="$1"
  local arn="$2"
  if grep -Fxq -- "$arn" "$TD_SETTLED_FILE" || grep -Fxq -- "$arn" "$TD_BLOCKED_FILE"; then
    echo "STOP: $arn 종착 상태를 두 번 기록하려 했다 — TD 원장 충돌 = (A)"
    TD_LEDGER_CONFLICT=1
    return 1
  fi
  case "$result" in
    settled) printf '%s\n' "$arn" >> "$TD_SETTLED_FILE" ;;
    blocked) printf '%s\n' "$arn" >> "$TD_BLOCKED_FILE" ;;
    *) echo "STOP: 알 수 없는 TD 판정 '$result' = (A)"; TD_LEDGER_CONFLICT=1; return 1 ;;
  esac
}

# AWS CLI의 확정적 물리 부재만 멱등 종결로 인정한다. 다른 비영 종료는 모두 조회 실패다.
is_td_not_found() {
  printf %s "$1" | grep -Eqi 'ClientException.*Unable to describe task definition'
}

TO_DELETE=""
for r in $REVS; do
  TD_TARGET_OK=0
  TD_BASE=${r%:*}
  TD_REV=${r##*:}
  if [ "$TD_BASE" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task-definition/linkpulse-restore-drill-a" ] \
     || [ "$TD_BASE" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task-definition/linkpulse-restore-drill-b" ]; then
    case "$TD_REV" in
      ""|*[!0-9]*) TD_TARGET_OK=0 ;;
      *) TD_TARGET_OK=1 ;;
    esac
  fi
  if [ "$TD_TARGET_OK" != 1 ]; then
    record_td_result blocked "$r"
    echo "$r -> STOP: 고정 드릴 family의 숫자 revision ARN이 아니다. 변경하지 않고 (A)"
    continue
  fi

  OUT=$(aws ecs describe-task-definition --task-definition "$r" --include TAGS \
        --query '[taskDefinition.taskDefinitionArn,taskDefinition.status,
          tags[?key==`purpose`].value|[0],tags[?key==`drill-token`].value|[0]]' \
        --output text 2>&1); RC=$?
  if [ "$RC" -ne 0 ]; then
    if is_td_not_found "$OUT"; then
      record_td_result settled "$r"
      echo "$r -> 확정적 부재 = 종결(조치 없음)"   # 이미 물리 삭제됨
    else
      record_td_result blocked "$r"
      echo "$r -> 조회 실패(상태 불명) = (A): rc=$RC $OUT"
    fi
    continue
  fi
  TD_ARN_OBS=$(printf %s "$OUT" | awk '{print $1}')
  TD_STATUS_OBS=$(printf %s "$OUT" | awk '{print $2}')
  TD_PURPOSE_OBS=$(printf %s "$OUT" | awk '{print $3}')
  TD_TOKEN_OBS=$(printf %s "$OUT" | awk '{print $4}')
  if [ "$TD_ARN_OBS" != "$r" ] || [ "$TD_PURPOSE_OBS" != rds-restore-drill ] \
     || [ -z "$TOKEN" ] || [ "$TD_TOKEN_OBS" != "$TOKEN" ]; then
    record_td_result blocked "$r"
    echo "$r -> STOP: exact ARN·purpose·drill-token 소유권 불일치. 변경하지 않고 (A)"
    continue
  fi
  case "$TD_STATUS_OBS" in
    ACTIVE)
      if S=$(aws ecs deregister-task-definition --task-definition "$r" \
              --query 'taskDefinition.status' --output text 2>&1); then
        if [ "$S" = INACTIVE ]; then
          echo "deregister OK  $r -> $S"
          TO_DELETE="$TO_DELETE $r"
        else
          record_td_result blocked "$r"
          echo "deregister 응답 상태 불일치 $r -> '$S' = (A)"
        fi
      else
        record_td_result blocked "$r"
        echo "deregister FAIL $r -> $S = (A)"  # ← ACTIVE인데 실패 = 삭제 불가
      fi ;;
    INACTIVE)            echo "$r -> 이미 INACTIVE = deregister 생략, 삭제 대상"; TO_DELETE="$TO_DELETE $r" ;;
    DELETE_IN_PROGRESS)  record_td_result settled "$r"; echo "$r -> 이미 DELETE_IN_PROGRESS = 종결 확정" ;;
    *)                   record_td_result blocked "$r"; echo "$r -> 예상 밖 상태 '$TD_STATUS_OBS' = (A)" ;;
  esac
done

# deregister와 delete 사이에도 소유권·상태를 다시 읽어 레이스를 닫는다.
SAFE_TO_DELETE=""
for r in $TO_DELETE; do
  DELETE_META=$(aws ecs describe-task-definition --task-definition "$r" --include TAGS \
    --query '[taskDefinition.taskDefinitionArn,taskDefinition.status,
      tags[?key==`purpose`].value|[0],tags[?key==`drill-token`].value|[0]]' \
    --output text 2>&1)
  DELETE_META_RC=$?
  DELETE_ARN=$(printf %s "$DELETE_META" | awk '{print $1}')
  DELETE_STATUS=$(printf %s "$DELETE_META" | awk '{print $2}')
  DELETE_PURPOSE=$(printf %s "$DELETE_META" | awk '{print $3}')
  DELETE_TOKEN=$(printf %s "$DELETE_META" | awk '{print $4}')
  if [ "$DELETE_META_RC" -ne 0 ]; then
    if is_td_not_found "$DELETE_META"; then
      record_td_result settled "$r"
      echo "$r -> delete 직전 확정적 부재 = 종결"
    else
      record_td_result blocked "$r"
      echo "$r -> STOP: delete 직전 조회 실패(상태 불명) = (A): rc=$DELETE_META_RC $DELETE_META"
    fi
  elif [ "$DELETE_ARN" = "$r" ] && [ "$DELETE_STATUS" = INACTIVE ] \
       && [ "$DELETE_PURPOSE" = rds-restore-drill ] \
       && [ -n "$TOKEN" ] && [ "$DELETE_TOKEN" = "$TOKEN" ]; then
    SAFE_TO_DELETE="$SAFE_TO_DELETE $r"
  elif [ "$DELETE_ARN" = "$r" ] && [ "$DELETE_STATUS" = DELETE_IN_PROGRESS ] \
       && [ "$DELETE_PURPOSE" = rds-restore-drill ] \
       && [ -n "$TOKEN" ] && [ "$DELETE_TOKEN" = "$TOKEN" ]; then
    record_td_result settled "$r"
    echo "$r -> 이미 DELETE_IN_PROGRESS = 종결"
  else
    record_td_result blocked "$r"
    echo "$r -> STOP: delete 직전 exact ARN·상태·소유권 재검증 실패 = (A)"
  fi
done
TO_DELETE="$SAFE_TO_DELETE"

# 2단계: 삭제 — **한 요청에 최대 10 revision**이므로 10개씩 나눠 호출한다(재드릴로 revision이 늘어도 안전).
# **판정은 요청이 아니라 revision ARN마다** 응답으로 한다(아래 즉시 게이트 1~4를 그대로 구현한 블록이다).
# 응답을 'D'(taskDefinitions[]) / 'F'(failures[]) 접두 행으로 평탄화해 revision별로 대조한다.
if [ -n "$TO_DELETE" ]; then
  date -u +%Y-%m-%dT%H:%M:%SZ                  # ← 삭제 **요청** 시각(원장)
  PENDING="$TO_DELETE"
  for SEND_TRY in 1 2 3; do                    # 접수 불명은 재전송(최초 포함 총 3회)
    [ -z "$PENDING" ] && break
    if [ "$SEND_TRY" -gt 1 ]; then
      sleep 30; echo "접수 증거 없는 revision 재전송 $SEND_TRY/3:$PENDING"
    fi
    : > "$DEL_DIR/raw-$SEND_TRY.txt"
    set -- $PENDING                            # 위치 인자로 옮겨 10개씩 자른다(같은 셸 안에서 처리)
                                               # ⚠️ 대화형 셸의 기존 위치 인자를 덮는다 — 다른 스크립트를
                                               #    소싱하던 셸이 아니라 드릴 전용 셸에서 붙여 넣는다
    while [ "$#" -gt 0 ]; do
      BATCH=""; N=0
      while [ "$#" -gt 0 ] && [ "$N" -lt 10 ]; do BATCH="$BATCH $1"; shift; N=$((N + 1)); done
      RESP=$(aws ecs delete-task-definitions --task-definitions $BATCH \
        --query "[taskDefinitions[].['D',taskDefinitionArn,status], failures[].['F',arn,reason]][]" \
        --output text 2>&1)
      printf '%s\n' "$RESP" | tee -a "$DEL_DIR/raw-$SEND_TRY.txt"
    done

    NEXT=""
    for r in $PENDING; do
      ROW=$(awk -F'\t' -v a="$r" '$2==a{k=$1;v=$3} END{if(k!="")print k":"v}' "$DEL_DIR/raw-$SEND_TRY.txt")
      case "$ROW" in
        "D:DELETE_IN_PROGRESS")
          record_td_result settled "$r"
          echo "$r -> DELETE_IN_PROGRESS = 종결 확정(이후 stale 조회가 뒤집지 못한다)" ;;
        "F:MISSING")
          # MISSING 뒤 확인은 present / 확정적 부재 / unknown으로 나눈다.
          # AccessDenied·스로틀링·timeout을 부재로 오독하지 않고, unknown만 30초 간격 총 3회 재조회한다.
          TD_MISSING_STATE=unknown
          for MISSING_TRY in 1 2 3; do
            MISSING_OUT=$(aws ecs describe-task-definition --task-definition "$r" \
              --query 'taskDefinition.taskDefinitionArn' --output text 2>&1)
            MISSING_RC=$?
            if [ "$MISSING_RC" -eq 0 ]; then
              TD_MISSING_STATE=present
              break
            elif is_td_not_found "$MISSING_OUT"; then
              TD_MISSING_STATE=absent
              break
            else
              echo "$r -> MISSING 확인 조회 실패 $MISSING_TRY/3 rc=$MISSING_RC $MISSING_OUT"
              if [ "$MISSING_TRY" -lt 3 ]; then sleep 30; fi
            fi
          done
          case "$TD_MISSING_STATE" in
            absent)
              record_td_result settled "$r"
              echo "$r -> MISSING + 확정적 부재 = 멱등 성공(종결)" ;;
            present)
              record_td_result blocked "$r"
              echo "$r -> MISSING인데 describe는 성공 = 모순 → (A)" ;;
            *)
              record_td_result blocked "$r"
              echo "$r -> MISSING 확인이 3회 뒤에도 상태 불명 = (A)" ;;
          esac ;;
        "F:"*)
          record_td_result blocked "$r"
          echo "$r -> failures[] reason=${ROW#F:} = 비멱등 실패 (A)" ;;
        *)
          NEXT="$NEXT $r"
          echo "$r -> 어느 목록에도 없음(접수 불명) → 재전송 대상" ;;
      esac
    done
    PENDING="$NEXT"
  done
  for r in $PENDING; do
    record_td_result blocked "$r"
    echo "$r -> 3회 재전송에도 접수 증거 없음 = (A)"
  done
fi

# REVS_UNION의 모든 revision이 settled 또는 blocked 중 정확히 하나에 있어야 원장이 완결이다.
# 게이트가 닫혔으면 원장 파일 자체가 없다 → 세지 않고 0을 유지한다(위 초기값).
TD_TOTAL=$(printf '%s\n' $REVS | awk 'NF{n++} END{print n+0}')
if [ "$TD_GATE_OK" = 1 ]; then
  TD_SETTLED_COUNT=$(wc -l < "$TD_SETTLED_FILE" | tr -d ' ')
  TD_BLOCKED_COUNT=$(wc -l < "$TD_BLOCKED_FILE" | tr -d ' ')
fi
if [ "$TD_GATE_OK" = 1 ] && [ "$TD_LEDGER_CONFLICT" = 0 ] \
   && [ $((TD_SETTLED_COUNT + TD_BLOCKED_COUNT)) -eq "$TD_TOTAL" ]; then
  TD_LEDGER_COMPLETE=1
else
  echo "STOP: TD 원장 불완전/충돌 — gate=$TD_GATE_OK total=$TD_TOTAL settled=$TD_SETTLED_COUNT blocked=$TD_BLOCKED_COUNT = (A)"
fi
echo "TD total=$TD_TOTAL 종결=$TD_SETTLED_COUNT / (A)=$TD_BLOCKED_COUNT ledger_complete=$TD_LEDGER_COMPLETE  ← §10.9 (A)⑤의 단일 소스"
```

**판정 단위는 "요청"이 아니라 "revision ARN"이다.** HTTP 200 응답 안에 성공한 `taskDefinitions[]`와 실패한 `failures[]`가 **함께** 온다 → CLI 종료 코드나 "요청 성공"을 한 값으로 적으면 **TD-A는 삭제 전이되고 TD-B는 `failures[]`에 있는데도 "삭제 호출 성공"으로 기록**돼 (A)=0으로 오판한다.

**즉시 게이트 — revision ARN마다 위에서부터 먼저 맞는 것 하나로 확정한다:**

1. **응답의 `taskDefinitions[]`에 있고 `status == DELETE_IN_PROGRESS`** → **종결 확정.** 이후 조회가 `INACTIVE`를 반환해도 **뒤집지 않는다**(결과적 일관성에 의한 stale 조회). 관측값은 증거에만 남긴다.
2. **응답의 `failures[]`에 있고 `reason == MISSING`** → `describe-task-definition`으로 직접 대사한다. **확정적인 `ClientException … Unable to describe task definition`만 물리적 부재 = 멱등 성공(종결)**으로 인정한다. 조회 성공은 모순으로 (A), `AccessDenied`·스로틀링·timeout 등 상태 불명은 30초 간격 총 3회 재조회 후에도 같으면 (A)다.
3. **`failures[]`에 있고 `reason`이 `MISSING`이 아님** → **비멱등 실패 = (A)**.
4. **접수 여부 불명**(응답 유실·타임아웃·응답의 어느 목록에도 그 ARN이 없음) → **조회만 반복하지 않는다.** 해당 revision에 **`delete-task-definitions`를 재전송**(30초 간격, **최초 포함 총 3회**)해 revision별 응답을 다시 확보하고 1~3으로 판정한다. **끝까지 접수 증거를 못 얻으면 (A)** — 조회에서 `INACTIVE`로 보이든 아니든 마찬가지다.

- **`deregister` 자체가 실패한 revision은 삭제를 시도할 수 없으므로 (A)** 다.
- 근거: **TD는 과금 대상도, 권한을 보유한 주체도 아니므로** 제어면의 비동기 지연을 기다릴 이유가 없지만, **삭제가 실제로 접수됐는지는 revision별로 확인해야** 한다. `DELETE_IN_PROGRESS`는 _"stay … until all the associated tasks and services have been terminated"_ 이고 **전용 waiter가 없다** → 물리적 부재를 드릴 창 안에서 기다리면 정상 동작을 FAIL로 오판하거나 무기한 대기하게 된다.
- **증거에 시각 2개**: **삭제 요청 시각**과 **물리적 부재 확인 시각**을 따로 남겨, 즉시 차단(권한·과금)과 제어면 지연을 사후에 구분할 수 있게 한다.
- **지연 확인(후속, 권장 익일)**: `aws ecs list-task-definitions --family-prefix <family> --status DELETE_IN_PROGRESS`가 비었는지 **1회** 확인해 증거에 남긴다. **익일인 이유**: 차단 리소스(태스크)가 사라진 뒤에도 _"The task definition deletion can take **up to 1 hour** to complete after the task is stopped."_ ([Deleting a task definition revision](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/delete-task-definition-v2.html)) 미완이면 **(d) FAIL이 아니라 후속 추적 항목**(연결 태스크가 아직 살아 있다는 신호이므로 **태스크 STOPPED를 재확인**한다).

### 10.7 ④ 임시 role — 순서를 지킨다

§10.1의 공통 형태대로 **존재 조회 → 있을 때만 조치**하되, **§10.4와 같은 3갈래**로 나눈다. `get-role`의 모든 실패를 `else`(= 없음)로 보내면 **자격증명 만료·`AccessDenied`·스로틀링·네트워크 오류가 "임시 role 없음 = 성공"으로 기록**돼, **드릴 시크릿 읽기 권한을 가진 role을 남긴 채 (d) PASS**가 된다.

```bash
ROLE_STATE=unknown       # unknown | absent | present
R_OUT=$(aws iam get-role --role-name "$DRILL_ROLE" \
  --query 'Role.[Arn,Tags[?Key==`purpose`].Value|[0],Tags[?Key==`drill-token`].Value|[0]]' \
  --output text 2>&1)
R_RC=$?
if [ "$R_RC" -eq 0 ]; then
  ROLE_STATE=present
  R_ARN=$(printf %s "$R_OUT" | awk '{print $1}')
  R_PURPOSE=$(printf %s "$R_OUT" | awk '{print $2}')
  R_TOKEN=$(printf %s "$R_OUT" | awk '{print $3}')
elif printf %s "$R_OUT" | grep -q "NoSuchEntity"; then
  ROLE_STATE=absent
fi
echo "ROLE_STATE=$ROLE_STATE  rc=$R_RC  $R_OUT"

case "$ROLE_STATE" in
  absent)  echo "임시 role 없음(NoSuchEntity) = 성공(멱등)" ;;
  unknown) echo "STOP: role 조회 실패 = 상태 불명 → (A) 부재 확인 실패로 기록(§10.9 ②). 원인 해소 후 재진입" ;;
  present)
    EXPECTED_ROLE_ARN="arn:aws:iam::${ACCOUNT_ID}:role/$DRILL_ROLE"
    echo "role target=$R_ARN purpose=$R_PURPOSE token=$R_TOKEN"
    # §10.9 (A)②는 **`ROLE_STATE=absent`만 종결**로 읽는다(round-9 반전) → 아래 모든 종착 분기에서
    # 상태를 명시적으로 전이시킨다. 정상 삭제 뒤 상태를 갱신하지 않으면 성공 경로가 (d) FAIL로 뒤집힌다.
    if [ "${SHELL_OK:-0}" != 1 ] || [ "$DRILL_ROLE" != "linkpulse-restore-drill-exec" ] \
       || [ "$R_ARN" != "$EXPECTED_ROLE_ARN" ] \
       || [ "$R_PURPOSE" != "rds-restore-drill" ] \
       || [ -z "$TOKEN" ] || [ "$R_TOKEN" != "$TOKEN" ]; then
      echo "STOP: 셸(SHELL_OK=${SHELL_OK:-미설정})·role ARN·purpose·drill-token 대조 실패 — 정책 제거·role 삭제를 호출하지 않고 (A)로 남긴다"
      # ROLE_STATE=present 유지: 존재하지만 이번 드릴 소유로 확인되지 않았다 = 손대지 않고 (A)
    else
      INLINE_OK=0
      INLINE_OUT=$(aws iam delete-role-policy --role-name "$DRILL_ROLE" \
        --policy-name read-drill-db-secret 2>&1); INLINE_RC=$?
      if [ "$INLINE_RC" -eq 0 ] || printf %s "$INLINE_OUT" | grep -q "NoSuchEntity"; then INLINE_OK=1; fi

      ATTACH_OK=0
      ATTACH_OUT=$(aws iam detach-role-policy --role-name "$DRILL_ROLE" \
        --policy-arn arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy 2>&1); ATTACH_RC=$?
      if [ "$ATTACH_RC" -eq 0 ] || printf %s "$ATTACH_OUT" | grep -q "NoSuchEntity"; then ATTACH_OK=1; fi

      if [ "$INLINE_OK" = 1 ] && [ "$ATTACH_OK" = 1 ]; then
        if aws iam delete-role --role-name "$DRILL_ROLE"; then
          VERIFY_OUT=$(aws iam get-role --role-name "$DRILL_ROLE" 2>&1); VERIFY_RC=$?
          if [ "$VERIFY_RC" -eq 0 ]; then
            # 삭제 호출은 성공했는데 아직 조회된다 → 존재로 남긴다(IAM 전파 지연이면 재진입에서 absent가 된다)
            echo "STOP: delete-role 뒤에도 role이 조회된다 = (A): $VERIFY_OUT"
          elif printf %s "$VERIFY_OUT" | grep -q "NoSuchEntity"; then
            ROLE_STATE=absent          # ← **확정적 부재만** 종결. 이 전이가 없으면 정상 경로가 (A)②로 떨어진다
            echo "임시 role 부재 확인 = 성공 (ROLE_STATE=absent)"
          else
            ROLE_STATE=unknown         # 조회 자체가 실패 = 상태 불명(부재로 읽지 않는다)
            echo "STOP: delete-role 뒤 부재 확인 조회 실패 = 상태 불명 → (A): $VERIFY_OUT"
          fi
        else
          echo "STOP: delete-role 실패 = (A)"   # ROLE_STATE=present 유지
        fi
      else
        echo "STOP: 정책 제거 실패(inline=$INLINE_OK attached=$ATTACH_OK) — role 삭제를 호출하지 않고 (A)로 남긴다"
      fi
    fi ;;
esac
echo "ROLE_STATE=$ROLE_STATE  ← §10.9 (A)②의 단일 소스(absent만 종결)"
```

**인라인 삭제 → 관리형 detach → `delete-role`.** 관리형을 떼지 않으면 `delete-role`이 `DeleteConflict`로 실패한다.

### 10.8 ⑤⑥⑦ 삭제 완료·시크릿·운영 무변경

⑤는 **§10.4에서 낸 삭제 요청의 완료를 확인**하는 단계다(요청을 여기서 처음 내는 것이 아니다 — 드릴 DB가 아직 있는데 §10.4를 건너뛰었다면 거기로 돌아간다).

**waiter의 종료 코드를 반드시 본다.** 이 waiter는 1800초를 넘기면 **exit 255**이고 매처는 `length(DBInstances) == 0`이다 — 종료 코드를 확인하지 않고 다음 줄에서 `date`를 찍으면 **아직 과금 중인 인스턴스에 "물리적 부재 확인" 증거가 만들어지고**, §10.9의 "waiter 미통과 = (A)" 판정이 그 시각 때문에 무력화된다.

```bash
DB_GONE_OK=0
if aws rds wait db-instance-deleted --db-instance-identifier "$DRILL_DB"; then   # ← 통과 = 과금 종료 판정
  DB_GONE_OK=1
  date -u +%Y-%m-%dT%H:%M:%SZ                                          # ← 물리적 부재 확인 시각(원장)
else
  echo "STOP: db-instance-deleted waiter 미통과(1800초 초과·권한 오류 등) — 부재 확인 시각을 찍지 않는다"
  echo "      → §10.9 (A)① 로 기록한다(과금이 끝났다는 증거가 없다). 아래 현재 상태로 재호출/중단 판단"
  aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" \
    --query 'DBInstances[0].DBInstanceStatus' --output text 2>&1 | tail -1
fi

SECRET_STATE=unknown       # unknown | absent | scheduled | present
SECRET_OUT=""
SECRET_RC=""
SECRET_OWNER=""
SECRET_NAME=""

case "$SECRET_LIFECYCLE" in
  not-created)
    if [ -z "${DRILL_SECRET:-}" ] || [ "$DRILL_SECRET" = None ]; then
      SECRET_STATE=absent
      SECRET_OUT="자격증명 전환 전 중단: 논리적 미생성"
    else
      SECRET_OUT="수명주기 not-created인데 ARN이 존재함 — 원장 불일치"
    fi
    ;;
  create-requested)
    # §10.3에서 삭제 전에 ARN을 재탐색했어야 한다. 비어 있으면 생성/부재를 증명할 수 없다.
    if [ -z "${DRILL_SECRET:-}" ] || [ "$DRILL_SECRET" = None ]; then
      SECRET_OUT="생성 요청 이력은 있으나 ARN을 회수하지 못함"
    else
      SECRET_LIFECYCLE=arn-known
    fi
    ;;
  arn-known) ;;
  *) SECRET_OUT="알 수 없는 SECRET_LIFECYCLE=$SECRET_LIFECYCLE" ;;
esac

if [ "$SECRET_LIFECYCLE" = arn-known ]; then
  if [ -z "${DRILL_SECRET:-}" ] || [ "$DRILL_SECRET" = None ] \
     || [ "$DRILL_SECRET" = "$PROD_SECRET" ]; then
    SECRET_OUT="STOP: ARN이 비었거나 운영 ARN과 같다"
  else
    SECRET_OUT=$(aws secretsmanager describe-secret --secret-id "$DRILL_SECRET" \
      --query '[ARN,DeletedDate,OwningService,Name]' --output text 2>&1)
    SECRET_RC=$?
    if [ "$SECRET_RC" -eq 0 ]; then
      SECRET_ARN_OBS=$(printf %s "$SECRET_OUT" | awk '{print $1}')
      SECRET_DELETED=$(printf %s "$SECRET_OUT" | awk '{print $2}')
      SECRET_OWNER=$(printf %s "$SECRET_OUT" | awk '{print $3}')
      SECRET_NAME=$(printf %s "$SECRET_OUT" | awk '{print $4}')
      if [ "$SECRET_ARN_OBS" != "$DRILL_SECRET" ]; then
        SECRET_STATE=unknown
      elif [ -n "$SECRET_DELETED" ] && [ "$SECRET_DELETED" != None ]; then
        SECRET_STATE=scheduled
      else
        SECRET_STATE=present
      fi
    elif printf %s "$SECRET_OUT" | grep -q "ResourceNotFoundException"; then
      SECRET_STATE=absent
    fi
  fi
fi

# **`present`를 곧바로 확정하지 않는다.** RDS는 DB를 삭제할 때 관리 시크릿도 함께 삭제한다(§2.3 ⓔ) →
# waiter 직후의 present는 그 전파가 아직 끝나지 않은 **정상 비동기 진행**일 공산이 크다.
# 여기서 바로 delete-secret을 쏘면 서비스 관리 시크릿이라 거부되고, 정상 동작이 "Support 에스컬레이션"이 된다.
# 이 문서의 다른 비동기 판정(MISSING 30초×3, TD DELETE_IN_PROGRESS)과 같은 규율로 **최초 포함 총 3회** 재조회한다.
if [ "$SECRET_STATE" = present ]; then
  for SEC_TRY in 2 3; do
    sleep 30
    SECRET_OUT=$(aws secretsmanager describe-secret --secret-id "$DRILL_SECRET" \
      --query '[ARN,DeletedDate,OwningService,Name]' --output text 2>&1)
    SECRET_RC=$?
    if [ "$SECRET_RC" -eq 0 ]; then
      SECRET_ARN_OBS=$(printf %s "$SECRET_OUT" | awk '{print $1}')
      SECRET_DELETED=$(printf %s "$SECRET_OUT" | awk '{print $2}')
      SECRET_OWNER=$(printf %s "$SECRET_OUT" | awk '{print $3}')
      SECRET_NAME=$(printf %s "$SECRET_OUT" | awk '{print $4}')
      if [ "$SECRET_ARN_OBS" != "$DRILL_SECRET" ]; then SECRET_STATE=unknown; break; fi
      if [ -n "$SECRET_DELETED" ] && [ "$SECRET_DELETED" != None ]; then SECRET_STATE=scheduled; break; fi
      echo "시크릿 재조회 $SEC_TRY/3: 아직 present — RDS 자동 정리 전파 대기"
    elif printf %s "$SECRET_OUT" | grep -q "ResourceNotFoundException"; then
      SECRET_STATE=absent; break
    else
      SECRET_STATE=unknown; break
    fi
  done
fi

# 3회 뒤에도 present면 RDS 쪽 자동 정리가 동작하지 않은 것이다 → 안전 게이트를 통과한 경우 7일 복구 창으로 삭제 예약한다.
if [ "$SECRET_STATE" = present ]; then
  # 튜플은 **관계까지** 검증한다 — boolean 하나와 "비어 있지 않음"만 보면, 재진입에서 이전 드릴의
  # `SECRET_PROVENANCE_OK=1`·옛 DB ARN이 이번 `DRILL_SECRET`과 섞여도 통과한다.
  # AWS가 서비스 관리 시크릿 삭제를 거부해 주는 것에 안전을 기대지 않는다(사후 거부는 게이트가 아니다).
  if [ "${SHELL_OK:-0}" = 1 ] && [ "$SECRET_LIFECYCLE" = arn-known ] && [ -n "$DRILL_SECRET" ] \
     && [ "$SECRET_PROVENANCE_OK" = 1 ] \
     && [ -n "$RESTORE_EXPECTED_ARN" ] && [ "$SECRET_OWNER_DB_ARN" = "$RESTORE_EXPECTED_ARN" ] \
     && [ "$SECRET_ARN_OBS" = "$DRILL_SECRET" ] \
     && [ -n "$PROD_SECRET" ] && [ "$PROD_SECRET" != None ] \
     && [ "$DRILL_SECRET" != "$PROD_SECRET" ] && [ "$SECRET_OWNER" = rds ]; then
    SECRET_DELETE_OUT=$(aws secretsmanager delete-secret --secret-id "$DRILL_SECRET" \
      --recovery-window-in-days 7 --query '[ARN,DeletionDate]' --output text 2>&1)
    SECRET_DELETE_RC=$?
    echo "delete-secret rc=$SECRET_DELETE_RC $SECRET_DELETE_OUT"
    if [ "$SECRET_DELETE_RC" -eq 0 ]; then
      SECRET_VERIFY=$(aws secretsmanager describe-secret --secret-id "$DRILL_SECRET" \
        --query '[ARN,DeletedDate,OwningService]' --output text 2>&1)
      SECRET_VERIFY_RC=$?
      VERIFY_ARN=$(printf %s "$SECRET_VERIFY" | awk '{print $1}')
      VERIFY_DELETED=$(printf %s "$SECRET_VERIFY" | awk '{print $2}')
      VERIFY_OWNER=$(printf %s "$SECRET_VERIFY" | awk '{print $3}')
      if [ "$SECRET_VERIFY_RC" -eq 0 ] && [ "$VERIFY_ARN" = "$DRILL_SECRET" ] \
         && [ -n "$VERIFY_DELETED" ] && [ "$VERIFY_DELETED" != None ] \
         && [ "$VERIFY_OWNER" = rds ]; then
        SECRET_STATE=scheduled
      else
        SECRET_STATE=unknown
        echo "STOP: 삭제 예약 뒤 exact ARN·DeletedDate·owner 재확인 실패"
      fi
    else
      # 서비스 관리 시크릿(OwningService=rds)의 직접 삭제는 API가 거부할 수 있다. 반복하지 않고 (A)로 올린다.
      # 여기까지 왔다는 것은 **DB 삭제 후에도 RDS가 시크릿을 정리하지 않았다**는 뜻이므로 정상 경로가 아니다.
      SECRET_STATE=present
      echo "STOP: RDS 관리 시크릿 삭제 예약 거부 — 재시도하지 말고 AWS Support/운영자에게 에스컬레이션"
    fi
  else
    echo "STOP: 셸(SHELL_OK=${SHELL_OK:-미설정})·튜플 정합(SECRET_OWNER_DB_ARN=$SECRET_OWNER_DB_ARN vs RESTORE_EXPECTED_ARN=$RESTORE_EXPECTED_ARN)·exact drill ARN·운영 ARN 불일치·OwningService=rds 게이트 미통과 — 삭제하지 않는다"
  fi
fi

# 생성 요청 이력만 있고 ARN을 끝내 회수하지 못한 경우: **DB 부재가 확정됐다면** 남아 있을 드릴 시크릿은 없다.
# (i) 요청이 닿지 않아 애초에 안 만들어졌거나, (ii) 만들어졌다면 DB 삭제와 함께 RDS가 지웠다(§2.3 ⓔ) — 둘 다 부재로 수렴한다.
# 이 규칙이 없으면 이름조차 모르는 시크릿 때문에 (d)가 영구히 거짓 FAIL이 된다.
if [ "$SECRET_LIFECYCLE" = create-requested ] && [ "$DB_GONE_OK" = 1 ] \
   && { [ -z "${DRILL_SECRET:-}" ] || [ "$DRILL_SECRET" = None ]; }; then
  SECRET_LIFECYCLE=not-created
  SECRET_STATE=absent
  SECRET_OUT="ARN 미회수 + DB 부재 확정 → 잔존할 드릴 시크릿 없음(논리적 수렴)"
fi
echo "SECRET_LIFECYCLE=$SECRET_LIFECYCLE SECRET_STATE=$SECRET_STATE owner=$SECRET_OWNER name=$SECRET_NAME rc=${SECRET_RC:-미호출} $SECRET_OUT"

aws rds describe-db-instances --db-instance-identifier "$SRC_DB" --query 'DBInstances[0].{
  status:DBInstanceStatus,pg:DBParameterGroups,secret:MasterUserSecret.SecretArn,
  pub:PubliclyAccessible,sgs:VpcSecurityGroups[].VpcSecurityGroupId}' --output json
# ⑦ 운영은 §2.5 ①에서 찍어 둔 값과 **동일**해야 한다(설정·파라미터그룹·시크릿 ARN 불변).
```

드릴 시크릿은 원장의 **수명주기**(`not-created` → `create-requested` → `arn-known`)와 조회 결과의 네 상태를 함께 판정한다:

1. **`SECRET_LIFECYCLE=not-created` + 빈 ARN** 또는 **`SECRET_STATE=absent`**(ResourceNotFound) → 종결. §7 전 중단은 정상적인 논리적 미생성이고, §7의 `modify-db-instance`가 **AWS의 확정 거부**를 받은 경우도 같다(요청이 실행되지 않았으므로 시크릿이 없다). **ARN을 못 찾은 채 DB 부재가 확정된 경우**도 여기로 수렴한다 — 안 만들어졌거나, 만들어졌다면 DB와 함께 지워졌다.
2. **`SECRET_STATE=scheduled`**(`DeletedDate` 있음, 삭제 예약·복구 창 대기) → **(B) 정상 비동기 진행.** 예약된 시크릿은 값 조회가 이미 거부되고 임시 role도 지워진 뒤라 **권한·과금이 없다.**
3. **`SECRET_STATE=present`** → **먼저 30초 간격으로 총 3회 재조회한다.** RDS가 DB 삭제와 함께 시크릿을 지우므로(§2.3 ⓔ) waiter 직후의 present는 대개 전파 지연이다. 3회 뒤에도 present면 소유권 튜플(`SECRET_PROVENANCE_OK` + `SECRET_OWNER_DB_ARN`) ∧ exact 원장 ARN ∧ 운영 ARN 아님 ∧ `OwningService=rds`일 때만 **7일 복구 창 삭제 예약** 후 `DeletedDate`를 재확인한다. 서비스 관리 시크릿이라 거부되면 반복하지 않고 (A)로 남겨 에스컬레이션한다 — **그 지점은 정상 경로가 아니라 RDS 자동 정리가 동작하지 않았다는 신호다.**
4. **`SECRET_STATE=unknown`**(조회 실패·대상 미확정) → 부재로 읽지 않고 **(A)** 로 남겨 원인 해소 후 재진입한다.

> **운영 시크릿 오삭제는 지연 장애형 사고다.** 운영 시크릿을 지우면 **다음 태스크 기동 시 `DB_PASSWORD` 주입이 실패**해(운영 `ecs.tf`의 `secrets`) 드릴이 끝난 한참 뒤 배포·재기동 시점에 터진다. 삭제 대상 ARN은 **반드시 드릴 인스턴스 조회로 도출**한 값만 쓴다.

### 10.9 ⑧ 잔존 목록 2분류 — **(d) 판정의 단일 소스**

§1의 (d) 축·§2.2 부수효과 표·§9는 전부 **여기를 참조**하며 같은 판정을 각자 다시 서술하지 않는다.

**(A) 조치 필요 잔존 — 하나라도 있으면 (d) FAIL**

> **조건은 전부 "종결 값이 아니면 (A)"의 형태로 읽는다.** 특정 값(`unknown` 등)에만 반응하게 쓰면 **해당 §10.x 블록을 아예 실행하지 않아 변수가 미설정인 세션**이 조용히 통과한다 — 그것이야말로 이 문서가 닫아 온 "0건으로 보이는 것 vs 못 본 것"의 혼동이다. §2.1이 세 변수를 `unknown`으로 초기화하는 것도 같은 이유다.

1. 드릴 DB(**`wait db-instance-deleted` 미통과 포함** — waiter가 성공하지 않았으면 부재 확인 시각도 없다)
2. 임시 role — **§10.7의 `ROLE_STATE`가 `absent`가 아닌 모든 경우**(`present`·`unknown`·미설정). `NoSuchEntity`로 확인한 `absent`만 종결이다
3. 드릴 시크릿 — **§10.8의 `SECRET_STATE`가 `absent`도 `scheduled`도 아닌 모든 경우**(`present`·`unknown`·미설정). `scheduled`는 아래 (B)다
4. **드릴 태스크 중 §10.5 원장의 `blocked`에 기록된 것 또는 `TASK_LEDGER_COMPLETE != 1` 전부** — `PROVISIONING`·`PENDING`·`ACTIVATING`·`RUNNING`·`DEACTIVATING`·`STOPPING`·`DEPROVISIONING`, **종결 관측 이력 없이 MISSING인 "상태 불명" ARN**, waiter 미통과, 그리고 **§10.5를 아예 실행하지 않아 원장이 비어 있는 세션**(미설정 = 0건이 아니라 "못 봤다")
   - `DEPROVISIONING`을 (A)에 두는 것은 **의도적 보수성**이다(이미 컨테이너가 멈추고 ENI를 회수하는 중이라 "비밀번호를 쥔 컨테이너"는 아니지만, 상태별 예외를 두면 기준이 다시 갈라진다 → **종결 아니면 (A)** 한 줄로 유지. waiter 상한 600초를 감안하면 이 상태로 ⑧까지 오는 경우는 드물다)
5. **TD revision 중 §10.6 원장의 `blocked`에 기록된 것 또는 `TD_LEDGER_COMPLETE != 1` 전부** — **`ACTIVE`인데 `deregister`가 실패**한 revision, target·소유권·delete 직전 재검증이 실패한 revision, `failures[]`에 **`MISSING` 이외의 사유**로 잡힌 revision, **재전송 3회까지 접수 증거를 못 얻은** revision, **상태 조회 자체가 실패한**(확정적 부재 응답이 아닌) revision
   - **이미 `DELETE_IN_PROGRESS`이거나 물리적으로 사라진 revision은 (A)가 아니다** — 재진입에서 그 둘에 `deregister`를 다시 걸어 나온 실패는 §10.1의 "이미 종결 상태는 성공" 규칙대로 무시한다(§10.6이 상태로 먼저 분류하는 이유).
   - **판정 단위는 요청이 아니라 revision ARN**이고, **판정 축은 삭제 응답이지 조회 상태가 아니다** — 응답으로 접수가 확인된 revision은 조회에 `INACTIVE`가 보여도 (A)가 아니며(stale), 접수 증거가 없으면 조회 상태와 무관하게 (A)다.
6. **§10.3의 `TD_DISCOVERY_STATE`·`TASK_DISCOVERY_STATE`가 `ok`가 아닌 모든 경우** — 잔존이 0건으로 보여도 그것은 "없다"가 아니라 "못 봤다"이다. `unknown`이 되는 사유는 다섯이고 **태스크·TD 양쪽이 대칭**이다: ⓐ 목록·태그·태스크 조회가 **3회 재시도 뒤에도** 실패 ⓑ **`ACCOUNT_ID` 미설정**으로 ARN 형식 대조의 기준값이 없음 ⓒ **원장 ARN의 형식 불일치**(제외가 아니라 "원장이 깨졌다"로 읽는다) ⓓ **`$DRILL_DIR/dispatch/`에 미대사 파일이 남아 있음**(`task-pending/<client token>`·`td-pending/<family>`) — 응답을 잃어 taskArn·revision ARN을 못 회수한 시도가 있다는 뜻이고, 그 파일은 AWS 호출 **전에** 쓰이므로 터미널이 죽어도 남는다 ⓔ **`DRILL_DIR`가 없거나 경로가 틀림**(원장 자체를 못 읽음), 또는 **dispatch 원장이 없는데 이번 토큰 태스크·로그 스트림이 관측됨**(다른 드릴을 가리키거나 런북 밖에서 띄운 것). 미설정 = **§10.3을 실행하지 않음**. 원인 해소 후 §10.3부터 재진입한다. §10.6·§10.5는 각각 이 값이 `ok`일 때만 실행되며(`TD_GATE_OK`·`TASK_GATE_OK`), 아니면 아무 것도 바꾸지 않고 ⑤·④도 함께 (A)로 남는다.

**(B) 정상 비동기 진행 — 잔존으로 세지 않으며 (d) PASS**: TD **`DELETE_IN_PROGRESS`**, 드릴 시크릿의 **삭제 예약(복구 창 대기)**. 지연 확인 대상으로만 남긴다.

> 둘을 섞으면 정상 정리가 FAIL로 기록되고, 반대로 **(A)를 좁게 잡으면 미완 정리가 PASS로 기록된다.** 어느 쪽도 드릴의 결론을 오염시킨다.

**정리 대상이 아닌 것(증거로 남긴다)**: sentinel 링크 행(삭제 API 없음), 드릴 로그 스트림.

---

## 11. (j) 스냅샷 복원 — 이번 드릴에서는 실행하지 않는다

별도 복원 검증에서 PITR 대신 특정 스냅샷 시점을 검증해야 할 때 이 절을 쓴다. **구성 게이트(§6)·자격증명 전환(§7)·검증 접속(§8)·정리 게이트(§10)는 PITR과 완전히 동일**하므로 참조하고, 다른 것만 적는다. 이 경로도 운영 cutover나 사고 복구를 수행하지 않는다.

**두 모드를 잇는 것은 공통 읽기 전용 플래그 2개다** — §8은 `B_MARGIN_OK`(PITR 전용)가 아니라 **`SENTINEL_BOUNDARY_OK`** 를 보고, 집계 부등식은 **`BASELINE_COMPARABLE_OK`** 가 1일 때만 (b) 판정에 들어간다. PITR은 §4.3 끝에서, 스냅샷은 아래 2-b)에서 같은 이름으로 세운다. 이 분리가 없으면 스냅샷으로 복원한 유료 인스턴스가 **PITR 전용 게이트에 막혀 검증 태스크조차 뜨지 않는다.** sentinel-B POST의 `SENTINEL_B_CREATE_ALLOWED`는 상태 변경 전 일회성 가드라 이 둘에 포함하지 않는다.

```bash
# 1) 스냅샷 식별자 선정 — Status=available 인 것만
aws rds describe-db-snapshots --db-instance-identifier "$SRC_DB" \
  --query 'reverse(sort_by(DBSnapshots[?Status==`available`],&SnapshotCreateTime))[:10].[DBSnapshotIdentifier,SnapshotCreateTime,SnapshotType]' \
  --output table

# 2) 선택값 검증 — 예시는 빈 문자열로 시작하고, 조회 성공 + 출처·상태·암호화를 모두 대조한다.
#    식별자는 **반드시 따옴표 안 변수**로 둔다. `--db-snapshot-identifier <스냅샷 ID>` 처럼 적으면
#    셸이 `<`를 **입력 리다이렉션**으로 읽어 AWS 호출 전에 죽는다(`bash -n`은 통과하므로 눈으로 못 잡는다).
# **원장 값을 덮지 않는다.** 재진입에서 이 줄이 빈 문자열로 초기화하면 §5 공통 reconciliation의
# source 대조(`RESTORE_REQUEST_SOURCE = SNAPSHOT_ID`)가 실패해 `SOURCE_LEDGER_OK=0`이 되고,
# **이미 과금 중인 스냅샷 복원본이 `RESTORE_OK`를 다시 얻지 못해** §6 이후로 진행할 수 없다.
SNAPSHOT_ID="${SNAPSHOT_ID:-}"   # ← 신규 선택일 때만 위 1)에서 고른 실제 식별자를 넣는다(따옴표 안에)
SNAPSHOT_OK=0
if [ -z "$SNAPSHOT_ID" ]; then
  echo "STOP: SNAPSHOT_ID가 비었다 — 목록에서 실제 값을 고른다"
else
  SNAP_OUT=$(aws rds describe-db-snapshots --db-snapshot-identifier "$SNAPSHOT_ID" \
    --query 'DBSnapshots[0].[DBSnapshotIdentifier,Status,DBInstanceIdentifier,Encrypted,SnapshotCreateTime]' \
    --output text 2>&1)
  SNAP_RC=$?
  if [ "$SNAP_RC" -eq 0 ]; then
    SNAP_ID_OBS=$(printf %s "$SNAP_OUT" | awk '{print $1}')
    SNAP_STATUS=$(printf %s "$SNAP_OUT" | awk '{print $2}')
    SNAP_SOURCE=$(printf %s "$SNAP_OUT" | awk '{print $3}')
    SNAP_ENCRYPTED=$(printf %s "$SNAP_OUT" | awk '{print $4}')
    SNAP_CREATED=$(printf %s "$SNAP_OUT" | awk '{print $5}')
    if [ "$SNAP_ID_OBS" = "$SNAPSHOT_ID" ] && [ "$SNAP_STATUS" = available ] \
       && [ "$SNAP_SOURCE" = "$SRC_DB" ] && [ "$SNAP_ENCRYPTED" = True ] \
       && [ -n "$SNAP_CREATED" ] && [ "$SNAP_CREATED" != None ]; then
      SNAPSHOT_OK=1
    fi
  fi
fi
echo "SNAPSHOT_OK=$SNAPSHOT_OK rc=${SNAP_RC:-미호출} $SNAP_OUT"
```

**2-a) sentinel-B 생성 허용 — 상태 변경 전 게이트.** 스냅샷과 sentinel-A의 경계만 먼저 확인한다. `SENTINEL_B_CREATE_ALLOWED=1`일 때 §4.3 **(1-a) 예약 → 원장 기입 → (1-b) 전송**만 실행해 B를 원장에 기록한 뒤 아래 2-b)로 돌아온다. 이 플래그는 삭제할 수 없는 POST만 허용하며 복원 후 재진입에서는 쓰지 않는다.

```bash
SENTINEL_B_CREATE_ALLOWED=0
SENTINEL_B_CREATE_MODE=""
B_CREATE_SLOT_OK=0
case "$SENTINEL_B_CREATE_STATE:$SENTINEL_B_TRY" in
  not-used:1|retry-authorized:2) B_CREATE_SLOT_OK=1 ;;
esac
A_CREATED_MAX="${A_CREATED_MAX:-}"   # sentinel-A 중 가장 늦은 created_at(DB 시계)
SNAP_PRE_EPOCHS=$(python3 -c 'import datetime as d,re,sys
def e(s):
    assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s)
    x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
    assert x.utcoffset() is not None
    return int(x.timestamp())
print(" ".join(str(e(a)) for a in sys.argv[1:]))' \
  "$SNAP_CREATED" "$A_CREATED_MAX" 2>/dev/null)
SNAP_PRE_EPOCH=$(printf %s "$SNAP_PRE_EPOCHS" | awk '{print $1}')
A_PRE_EPOCH=$(printf %s "$SNAP_PRE_EPOCHS" | awk '{print $2}')
case "$SNAP_PRE_EPOCH:$A_PRE_EPOCH" in
  ""|:*|*:|*::*|*[!0-9:]*)
    echo "STOP: SnapshotCreateTime 또는 A_CREATED_MAX 형식 오류 — sentinel-B를 만들지 않는다"
    ;;
  *)
    if [ "${DRILL_MUTATION_OK:-0}" = 1 ] \
       && lock_live \
       && [ "$SNAPSHOT_OK" = 1 ] && [ "${BASELINE_RESULT_OK:-0}" = 1 ] \
       && [ "$RESTORE_CALL_STATE" = not-started ] && [ -n "$A_CODES" ] \
       && [ "$B_CREATE_SLOT_OK" = 1 ] \
       && [ "$SNAP_PRE_EPOCH" -ge $((A_PRE_EPOCH + 60)) ]; then
      SENTINEL_B_CREATE_ALLOWED=1
      SENTINEL_B_CREATE_MODE=snapshot
    else
      echo "STOP: 변경 자격(DRILL_MUTATION_OK=${DRILL_MUTATION_OK:-미설정} 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)) 또는 스냅샷 모드 sentinel-B 생성 전제(A가 스냅샷보다 60초 이상 앞섬)를 만족하지 않는다"
    fi
    ;;
esac
echo "SENTINEL_B_CREATE_ALLOWED=$SENTINEL_B_CREATE_ALLOWED mode=$SENTINEL_B_CREATE_MODE state=$SENTINEL_B_CREATE_STATE try=$SENTINEL_B_TRY"
```

**2-b) 스냅샷 모드 경계 재도출 — 읽기 전용.** PITR의 restore point 대신 관측한 `SnapshotCreateTime`이 기준이다. sentinel A/B 브래킷과 baseline 집계 비교 가능성을 분리한다. 복원 후 재진입에서는 §5 reconciliation 뒤 **먼저 2) 선택값 검증을 원장 `SNAPSHOT_ID`로 다시 실행해 `SNAPSHOT_OK`·`SNAP_CREATED`를 재도출하고, 이어서 2-b)를 실행한다.** 2-a와 §4.3 (1-a)/(1-b)는 다시 실행하지 않는다.

```bash
SENTINEL_BOUNDARY_OK=0
BASELINE_COMPARABLE_OK=0
BASELINE_LEDGER_OK=0
BASELINE_EVIDENCE_OK=0
A_CREATED_MAX="${A_CREATED_MAX:-}"
B_CREATED_AT="${B_CREATED_AT:-}"
T_BASELINE="${T_BASELINE:-}"
BASELINE_CAPTURED_AT="${BASELINE_CAPTURED_AT:-}"

SNAP_BOUNDARY_EPOCHS=$(python3 -c 'import datetime as d,re,sys
def e(s):
    assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s)
    x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
    assert x.utcoffset() is not None
    return int(x.timestamp())
print(" ".join(str(e(a)) for a in sys.argv[1:]))' \
  "$SNAP_CREATED" "$A_CREATED_MAX" "$B_CREATED_AT" 2>/dev/null)
SNAP_EPOCH=$(printf %s "$SNAP_BOUNDARY_EPOCHS" | awk '{print $1}')
A_MAX_EPOCH=$(printf %s "$SNAP_BOUNDARY_EPOCHS" | awk '{print $2}')
B_EPOCH=$(printf %s "$SNAP_BOUNDARY_EPOCHS" | awk '{print $3}')

SNAPSHOT_SOURCE_OK=0
if [ "$RESTORE_CALL_STATE" = not-started ] && [ "$SNAPSHOT_OK" = 1 ]; then
  SNAPSHOT_SOURCE_OK=1
elif [ "${RESTORE_OK:-0}" = 1 ] && [ "$RESTORE_REQUEST_KIND" = snapshot ] \
     && [ -n "$RESTORE_REQUEST_SOURCE" ] \
     && [ "$RESTORE_REQUEST_SOURCE" = "$SNAPSHOT_ID" ]; then
  SNAPSHOT_SOURCE_OK=1
elif [ "$RESTORE_CALL_STATE" = ambiguous ] && [ "${RESTORE_RETRY_ALLOWED:-0}" = 1 ] \
     && [ "$RESTORE_RETRY_STATE" = authorized ] && [ "$RESTORE_REQUEST_KIND" = snapshot ] \
     && [ -n "$RESTORE_REQUEST_SOURCE" ] \
     && [ "$RESTORE_REQUEST_SOURCE" = "$SNAPSHOT_ID" ]; then
  SNAPSHOT_SOURCE_OK=1
fi

# PITR와 같은 봉인 대조(§4.3) — 호출이 나간 뒤에는 원장의 B가 봉인값과 정확히 같아야 한다.
B_SEAL_OK=1
if [ "$RESTORE_CALL_STATE" != not-started ]; then
  if [ "$RESTORE_PRECHECK_STATE" = ready ] \
     && [ -n "$RESTORE_PRECHECK_B_CODE" ] && [ -n "$RESTORE_PRECHECK_B_CREATED_AT" ] \
     && [ "$RESTORE_PRECHECK_B_CODE" = "$B_CODE" ] \
     && [ "$RESTORE_PRECHECK_B_CREATED_AT" = "$B_CREATED_AT" ]; then
    B_SEAL_OK=1
  else
    B_SEAL_OK=0
    echo "STOP: 원장의 sentinel-B가 최초 호출 직전 봉인값과 다르다 — T0 이후 B로 경계를 세우지 않는다"
    echo "      봉인 code=$RESTORE_PRECHECK_B_CODE created=$RESTORE_PRECHECK_B_CREATED_AT / 현재 code=$B_CODE created=$B_CREATED_AT"
  fi
fi

SNAP_B_MARGIN_RELATION=unknown
case "$SNAP_EPOCH:$A_MAX_EPOCH:$B_EPOCH" in
  ""|:*|*:|*::*|*[!0-9:]*)
    echo "STOP: SnapshotCreateTime·A_CREATED_MAX·B_CREATED_AT 중 하나가 RFC3339로 파싱되지 않았다"
    ;;
  *)
    # A는 스냅샷보다 60초 이상 앞서야 "복원본에 존재해야 함"이 성립하고, B는 60초 이상 뒤여야 "부재해야 함"이 성립한다.
    if [ "$SNAPSHOT_SOURCE_OK" = 1 ] && [ -n "$A_CODES" ] && [ -n "$B_CODE" ] \
       && [ "$B_SEAL_OK" = 1 ] \
       && [ "$SENTINEL_B_CREATE_STATE" = accepted ] \
       && [ "$SNAP_EPOCH" -ge $((A_MAX_EPOCH + 60)) ]; then
      if [ "$B_EPOCH" -ge $((SNAP_EPOCH + 60)) ]; then
        SENTINEL_BOUNDARY_OK=1
        SNAP_B_MARGIN_RELATION=sufficient
      else
        SNAP_B_MARGIN_RELATION=insufficient
      fi
    else
      echo "STOP: 'A 전건이 스냅샷보다 60초 이상 앞섬 ∧ B가 60초 이상 뒤'라는 증거가 없다(b_seal_ok=$B_SEAL_OK)"
      echo "      → 더 최신 스냅샷을 고르거나, sentinel-A를 만든 뒤에 생성된 스냅샷을 쓴다. 복원을 호출하지 않는다"
    fi
    ;;
esac
if [ "$SNAP_B_MARGIN_RELATION" = insufficient ] \
   && [ "$SENTINEL_B_CREATE_STATE" = accepted ] && [ "$SENTINEL_B_TRY" = 1 ] \
   && [ "$RESTORE_CALL_STATE" = not-started ]; then
  SENTINEL_B_CREATE_STATE=retry-authorized
  SENTINEL_B_TRY=2
  echo "스냅샷 sentinel-B 첫 유효 응답의 마진 미달 확인 → state=$SENTINEL_B_CREATE_STATE try=$SENTINEL_B_TRY ← 원장 기입"
fi

# baseline은 sentinel 브래킷이 아니라 집계 부등식에만 쓴다.
BASE_EPOCH=$(python3 -c 'import datetime as d,re,sys
s=sys.argv[1]
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s)
x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
assert x.utcoffset() is not None
print(int(x.timestamp()))' "$T_BASELINE" 2>/dev/null)
BASE_CAPTURE_EPOCH=$(python3 -c 'import datetime as d,re,sys
s=sys.argv[1]
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z",s)
print(int(d.datetime.fromisoformat(s.replace("Z","+00:00")).timestamp()))' \
  "$BASELINE_CAPTURED_AT" 2>/dev/null)
BASE_MAX_EPOCH=$(python3 -c 'import datetime as d,re,sys
s=sys.argv[1]
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s)
x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
assert x.utcoffset() is not None
print(int(x.timestamp()))' "$BASELINE_MAX_CREATED_AT" 2>/dev/null)
SNAP_RESTORE_T0_EPOCH=$(python3 -c 'import datetime as d,re,sys
s=sys.argv[1]
assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})",s)
x=d.datetime.fromisoformat(s.replace("Z","+00:00"))
assert x.utcoffset() is not None
print(int(x.timestamp()))' "$RESTORE_T0" 2>/dev/null)

SNAP_BASELINE_TUPLE_OK=1
case "$BASELINE_ROW_COUNT" in ""|*[!0-9]*) SNAP_BASELINE_TUPLE_OK=0 ;; esac
case "$BASELINE_CLICKS_SUM" in ""|*[!0-9]*) SNAP_BASELINE_TUPLE_OK=0 ;; esac
case "$A_EXPECTED_COUNT:$BASELINE_A_HITS" in 1:1|2:2|3:3) ;; *) SNAP_BASELINE_TUPLE_OK=0 ;; esac
case "$BASE_EPOCH:$BASE_CAPTURE_EPOCH:$BASE_MAX_EPOCH:$SNAP_RESTORE_T0_EPOCH" in
  ""|:*|*:|*::*|*[!0-9:]*) SNAP_BASELINE_TUPLE_OK=0 ;;
esac
if [ "$SNAP_BASELINE_TUPLE_OK" = 1 ] \
   && [ "$TDA_RESULT_READ_STATE" = read ] \
   && [ "$BASE_CAPTURE_EPOCH" -ge $((BASE_EPOCH - 60)) ] \
   && [ "$BASE_CAPTURE_EPOCH" -le "$SNAP_RESTORE_T0_EPOCH" ] \
   && [ -n "$BASELINE_SOURCE_TASK_ARN" ] \
   && [ "${BASELINE_SOURCE_TASK_ARN%%/*}" = "arn:aws:ecs:$AWS_REGION:${ACCOUNT_ID}:task" ] \
   && [ "$BASELINE_SOURCE_TASK_ARN" = "$TDA_TASK_ARN" ] \
   && [ "$RESTORE_PRECHECK_STATE" = ready ] \
   && [ "$RESTORE_PRECHECK_BASELINE_TASK_ARN" = "$BASELINE_SOURCE_TASK_ARN" ] \
   && [ -n "$RESTORE_REQUEST_SOURCE" ] \
   && [ "$RESTORE_PRECHECK_KIND" = "$RESTORE_REQUEST_KIND" ] \
   && [ "$RESTORE_PRECHECK_SOURCE" = "$RESTORE_REQUEST_SOURCE" ] \
   && { [ "$RESTORE_CALL_STATE" = accepted ] || [ "$RESTORE_CALL_STATE" = ambiguous ]; }; then
  BASELINE_LEDGER_OK=1
fi

if [ "${BASELINE_RESULT_OK:-0}" = 1 ] && [ -n "$BASE_EPOCH" ]; then
  BASELINE_EVIDENCE_OK=1
elif [ "${RESTORE_OK:-0}" = 1 ] && [ "$BASELINE_LEDGER_OK" = 1 ]; then
  BASELINE_EVIDENCE_OK=1
fi
if [ "$SNAPSHOT_SOURCE_OK" = 1 ] && [ "$BASELINE_EVIDENCE_OK" = 1 ] \
   && [ -n "$SNAP_EPOCH" ] && [ "$SNAP_EPOCH" -ge $((BASE_EPOCH + 60)) ]; then
  BASELINE_COMPARABLE_OK=1
else
  echo "주의: baseline 증거가 없거나 스냅샷이 T_baseline보다 앞선다 → 집계 부등식은 판정에서 제외한다"
fi
echo "BASELINE_LEDGER_OK=$BASELINE_LEDGER_OK BASELINE_EVIDENCE_OK=$BASELINE_EVIDENCE_OK SENTINEL_BOUNDARY_OK=$SENTINEL_BOUNDARY_OK BASELINE_COMPARABLE_OK=$BASELINE_COMPARABLE_OK (mode=snapshot)"
echo "SNAP_B_MARGIN_RELATION=$SNAP_B_MARGIN_RELATION B_state=$SENTINEL_B_CREATE_STATE B_try=$SENTINEL_B_TRY"
```

**3) 복원.** 옵션은 PITR과 동일(식별자·서브넷그룹·SG·파라미터그룹·비공개·Single-AZ·삭제보호 해제·보관 0·태그)하며, 안전 가드도 §5와 같다. 생성 호출과 reconciliation을 분리하고 최초 `T0`/source를 원장에 보존한다.

```bash
RESTORE_OK=0
TARGET_ABSENT_OK=0
TARGET_STATE=unknown
TARGET_OUT=$(aws rds describe-db-instances --db-instance-identifier "$DRILL_DB" \
  --query 'DBInstances[0].DBInstanceArn' --output text 2>&1)
TARGET_RC=$?
if [ "$TARGET_RC" -eq 0 ]; then
  TARGET_STATE=present
elif printf %s "$TARGET_OUT" | grep -q "DBInstanceNotFound"; then
  TARGET_STATE=absent
  TARGET_ABSENT_OK=1
fi
echo "스냅샷 호출 직전 TARGET_STATE=$TARGET_STATE TARGET_ABSENT_OK=$TARGET_ABSENT_OK rc=$TARGET_RC"

# 자격 규칙은 §5와 동일하다: 영속 상태 `authorized` + 일회성 플래그를 **함께** 요구하고,
# 재시도 여부는 호출 로컬 `IS_RETRY`에 보존한다(셸 플래그만 보면 인터럽트 뒤 두 번 전송된다).
RESTORE_CALL_ALLOWED=0
IS_RETRY=0
if [ "$RESTORE_CALL_STATE" = not-started ]; then
  RESTORE_CALL_ALLOWED=1
elif [ "$RESTORE_CALL_STATE" = ambiguous ] && [ "$RESTORE_RETRY_ALLOWED" = 1 ] \
     && [ "$RESTORE_RETRY_STATE" = authorized ] \
     && [ "$RESTORE_REQUEST_KIND" = snapshot ] \
     && [ -n "$RESTORE_REQUEST_SOURCE" ] \
     && [ "$RESTORE_REQUEST_SOURCE" = "$SNAPSHOT_ID" ]; then
  RESTORE_CALL_ALLOWED=1
  IS_RETRY=1
fi

INITIAL_RESTORE_GUARD_OK=0
RETRY_RESTORE_GUARD_OK=0
if [ "$IS_RETRY" = 0 ] \
   && [ "${BASELINE_RESULT_OK:-0}" = 1 ] && [ "$SNAPSHOT_OK" = 1 ] \
   && [ "$SENTINEL_BOUNDARY_OK" = 1 ] && [ "$SENTINEL_B_CREATE_STATE" = accepted ]; then
  INITIAL_RESTORE_GUARD_OK=1
elif [ "$IS_RETRY" = 1 ] \
     && [ "${BASELINE_LEDGER_OK:-0}" = 1 ] && [ "${SNAPSHOT_SOURCE_OK:-0}" = 1 ] \
     && [ "$SNAPSHOT_OK" = 1 ] && [ "$SENTINEL_BOUNDARY_OK" = 1 ] \
     && [ "$SENTINEL_B_CREATE_STATE" = accepted ] \
     && [ "$RESTORE_PRECHECK_STATE" = ready ] && [ "$RESTORE_PRECHECK_KIND" = snapshot ] \
     && [ -n "$RESTORE_PRECHECK_SOURCE" ] \
     && [ "$RESTORE_PRECHECK_SOURCE" = "$RESTORE_REQUEST_SOURCE" ] \
     && [ "$RESTORE_PRECHECK_BASELINE_TASK_ARN" = "$BASELINE_SOURCE_TASK_ARN" ]; then
  RETRY_RESTORE_GUARD_OK=1
fi

if [ "$RESTORE_CALL_ALLOWED" = 1 ] \
   && { [ "$INITIAL_RESTORE_GUARD_OK" = 1 ] || [ "$RETRY_RESTORE_GUARD_OK" = 1 ]; } \
   && [ "${DRILL_MUTATION_OK:-0}" = 1 ] \
   && lock_live \
   && [ "$VALUES_OK" = 1 ] && [ "$TARGET_ABSENT_OK" = 1 ] \
   && [ -n "$SUBNET_GROUP" ] \
   && [ -n "$DATA_SG" ] && [ -n "$PROD_PG" ] \
   && [ "$DRILL_DB" != "$SRC_DB" ] && [ "$DRILL_DB" = "linkpulse-restore-drill" ]; then

  if [ "$IS_RETRY" = 1 ]; then
    # PITR과 같은 계보 규칙: 최초 T0·snapshot ID·ambiguous 상태는 보존하고 재시도 시각만 원장에 병기한다.
    # 소진 기록도 §5와 같은 자리다 — **재호출 직전**에 dispatched를 적고, 일회성 플래그도 함께 닫는다.
    RESTORE_RETRY_STATE=dispatched
    RESTORE_RETRY_ALLOWED=0
    echo "RESTORE_RETRY_STATE=$RESTORE_RETRY_STATE (allowed=$RESTORE_RETRY_ALLOWED)   ← **호출 전에** 원장에 적는다"
    echo "스냅샷 재시도 호출(1회): 최초 T0=$RESTORE_T0 유지 / 재시도 시각=$(date -u +%Y-%m-%dT%H:%M:%SZ) ← 원장 병기"
  else
    RESTORE_PRECHECK_STATE=ready
    RESTORE_PRECHECK_KIND=snapshot
    RESTORE_PRECHECK_SOURCE="$SNAPSHOT_ID"
    RESTORE_PRECHECK_BASELINE_TASK_ARN="$BASELINE_SOURCE_TASK_ARN"
    RESTORE_PRECHECK_B_CODE="$B_CODE"
    RESTORE_PRECHECK_B_CREATED_AT="$B_CREATED_AT"
    echo "RESTORE_PRECHECK_STATE=$RESTORE_PRECHECK_STATE kind=$RESTORE_PRECHECK_KIND source=$RESTORE_PRECHECK_SOURCE baseline_task=$RESTORE_PRECHECK_BASELINE_TASK_ARN b_code=$RESTORE_PRECHECK_B_CODE b_created=$RESTORE_PRECHECK_B_CREATED_AT ← 최초 호출 전에 원장 기입"
    RESTORE_T0=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    RESTORE_REQUEST_KIND=snapshot
    RESTORE_REQUEST_SOURCE="$SNAPSHOT_ID"
    RESTORE_EXPECTED_ARN=""
    RESTORE_INSTANCE_CREATED=""
    RESTORE_CALL_STATE=ambiguous
    echo "RESTORE_CALL_STATE=$RESTORE_CALL_STATE RESTORE_T0=$RESTORE_T0 kind=$RESTORE_REQUEST_KIND source=$RESTORE_REQUEST_SOURCE"
  fi

  RESTORE_OUT=$(aws rds restore-db-instance-from-db-snapshot \
    --db-snapshot-identifier "$SNAPSHOT_ID" \
    --db-instance-identifier "$DRILL_DB" \
    --db-subnet-group-name "$SUBNET_GROUP" \
    --vpc-security-group-ids "$DATA_SG" \
    --db-parameter-group-name "$PROD_PG" \
    --no-publicly-accessible --no-multi-az --no-deletion-protection \
    --backup-retention-period 0 \
    --tags Key=purpose,Value=rds-restore-drill Key=drill-token,Value="$TOKEN" \
    --query 'DBInstance.[DBInstanceIdentifier,DBInstanceArn,DBInstanceStatus,InstanceCreateTime]' \
    --output text 2>&1)
  RESTORE_RC=$?
  echo "스냅샷 복원 호출 rc=$RESTORE_RC $RESTORE_OUT"
  if [ "$RESTORE_RC" -eq 0 ]; then
    RESPONSE_ID=$(printf %s "$RESTORE_OUT" | awk '{print $1}')
    RESPONSE_ARN=$(printf %s "$RESTORE_OUT" | awk '{print $2}')
    RESPONSE_CREATED=$(printf %s "$RESTORE_OUT" | awk '{print $4}')
    if [ "$RESPONSE_ID" = "$DRILL_DB" ] && [ -n "$RESPONSE_ARN" ] \
       && [ -n "$RESPONSE_CREATED" ] && [ "$RESPONSE_CREATED" != None ]; then
      RESTORE_CALL_STATE=accepted
      RESTORE_EXPECTED_ARN="$RESPONSE_ARN"
      RESTORE_INSTANCE_CREATED="$RESPONSE_CREATED"
    else
      RESTORE_CALL_STATE=ambiguous
    fi
  elif printf %s "$RESTORE_OUT" | grep -Eqi \
    "RequestTimeout|Read timeout|Connect timeout|Connection reset|Could not connect to the endpoint|EndpointConnectionError|TLS handshake timeout"; then
    RESTORE_CALL_STATE=ambiguous
  elif printf %s "$RESTORE_OUT" | grep -Eq \
    "DBInstanceAlreadyExists|DBSnapshotNotFound|InvalidDBSnapshotState|InvalidParameterValue|InvalidParameterCombination|InvalidDBInstanceState|DBSubnetGroupNotFound|DBParameterGroupNotFound|InstanceQuotaExceeded|KMSKeyNotAccessible|AccessDenied|UnauthorizedOperation|OptInRequired|ValidationError"; then
    # AWS가 의미를 명시해 거부한 응답만 rejected 로 분류한다.
    RESTORE_CALL_STATE=rejected
  else
    # 미상 5xx·처음 보는 오류는 거부의 증거가 아니다. 공통 reconciliation이 후보를 확인하게 한다.
    RESTORE_CALL_STATE=ambiguous
    echo "주의: 분류되지 않은 스냅샷 복원 오류 → ambiguous 로 보수 처리(제어면 결과 재확인)"
  fi
  # 재시도는 accepted 응답만 상태를 상향한다. 그 외 결과는 최초 ambiguous 요청의 계보를 보존한다.
  # 판정에는 호출 로컬 `IS_RETRY`를 쓴다(§5와 동일 — 일회성 플래그는 호출 전에 닫혔다).
  if [ "$IS_RETRY" = 1 ] && [ "$RESTORE_CALL_STATE" != accepted ]; then
    echo "스냅샷 재시도 결과 '$RESTORE_CALL_STATE' → 최초 ambiguous 계보 보존을 위해 ambiguous 로 되돌린다"
    RESTORE_CALL_STATE=ambiguous
  fi
  echo "RESTORE_CALL_STATE=$RESTORE_CALL_STATE RESTORE_T0=$RESTORE_T0 source=$RESTORE_REQUEST_SOURCE expected_arn=$RESTORE_EXPECTED_ARN created=$RESTORE_INSTANCE_CREATED"
else
  # §5와 같은 규율 — 막은 값을 전부 찍는다(위 `if`는 12개 조건을 본다).
  echo "STOP: 스냅샷 복원 호출 안 함 — state=$RESTORE_CALL_STATE target=$TARGET_STATE initial_guard=$INITIAL_RESTORE_GUARD_OK retry_guard=$RETRY_RESTORE_GUARD_OK"
  echo "      자격: DRILL_MUTATION_OK=${DRILL_MUTATION_OK:-미설정}(락=${EXCLUSIVE_DRILL_LOCK_OK:-미설정} 도구=${TOOLS_OK:-미설정} 주체=${SESSION_AWS_OK:-미설정} 원장=${MUTATION_CONTEXT_OK:-미설정}) 락만료epoch=${LOCK_EXPIRES_EPOCH:-0} 현재=$(date -u +%s)"
  echo "      값: VALUES_OK=${VALUES_OK:-미설정} TARGET_ABSENT_OK=${TARGET_ABSENT_OK:-미설정} SNAPSHOT_ID='${SNAPSHOT_ID:-미설정}' SUBNET_GROUP='${SUBNET_GROUP:-미설정}' DATA_SG='${DATA_SG:-미설정}' PROD_PG='${PROD_PG:-미설정}' DRILL_DB='${DRILL_DB:-미설정}'"
fi
# 이 명령의 --manage-master-user-password 도 **Oracle 전용**이다 → §7의 modify 흐름을 그대로 쓴다.
```

호출 뒤에는 §5의 **공통 읽기 전용 reconciliation 블록**을 실행한다. 그 블록은 `RESTORE_REQUEST_KIND=snapshot`일 때 원장의 `SNAPSHOT_ID`·최초 `RESTORE_T0`·정확한 ARN·`InstanceCreateTime`·이번 토큰 태그를 대조한다. 최초 호출의 명시적 `rejected`는 승인하지 않는다. 전송 불명 뒤 세 조회가 모두 `DBInstanceNotFound`이면 공통 블록이 연 재시도 창을 **이 절의 3) 블록**에서만 소비하며, 재시도의 `AlreadyExists`는 `ambiguous` 계보를 유지한 채 태그로 대사한다.

**PITR ↔ 스냅샷 판정 조건 대조표**:

| 항목 | **PITR** | **스냅샷 복원** |
| ---- | -------- | --------------- |
| 복구 지점의 출처 | `--restore-time`에 **지정한 값**(제어면) | **`SnapshotCreateTime` 관측값**(제어면) — 시점을 고를 수 없다 |
| 복구 지점 지연 | `T0 − restore point` | `T0 − SnapshotCreateTime` (**PITR보다 크게 나오는 것이 정상**) |
| baseline 기준 시각 | `T_baseline`(드릴이 만든다) | **스냅샷 시각 기준으로 재정의**하거나, baseline을 쓰지 않는다 |
| sentinel-A 조건 | `restore point`보다 **이전**에 생성 → **존재해야** 함 | **`SnapshotCreateTime` 이전**에 심은 것만 존재해야 함 |
| sentinel-B 조건 | `restore point` 이후·T0 이전 생성 → **부재해야** 함 | `SnapshotCreateTime` 이후 생성 → **부재해야** 함 |
| 경계 검증 플래그 | `B_MARGIN_OK` → `SENTINEL_BOUNDARY_OK`(§4.3) | 2-b)가 `A_CREATED_MAX + 60초 ≤ SnapshotCreateTime ≤ B − 60초`를 확인해 **같은 이름의 플래그**를 세운다 |
| 집계 부등식 | 복원본 `count(*) ≥ baseline` (복원 지점 > `T_baseline`이므로) | **성립하지 않을 수 있다** — 스냅샷이 baseline보다 앞서면 `count(*) < baseline`이 정상이다. 부등식을 그대로 쓰면 **정상 복원을 FAIL로 오판**한다 → `BASELINE_COMPARABLE_OK=0`이면 **판정에서 빼고 숫자만 기록**한다 |

---

## 12. (k) 진단 — 태스크가 `exitCode != 0`이거나 기동 실패일 때

**로그를 읽기 전에** `describe-tasks`의 `stoppedReason`·`containers[].reason`을 본다(로그가 비어 있는 실패를 "결과 없음"으로 오판하지 않기 위함).

| 증상 | 흔한 원인 | 확인·대응 |
| ---- | --------- | --------- |
| `ResourceInitializationError: unable to pull secrets or registry auth` | 시크릿 ARN 오타, 실행 role에 그 ARN 권한 없음, NAT egress 문제 | TD의 `valueFrom`(`:password::` 포함) 확인, TD-B는 **임시 role**을 쓰는지 확인 |
| `unable to assume role` / `AccessDenied`(run-task 시점) | **IAM 전파 지연**, `iam:PassRole` 없음 | 30초 뒤 **다음 토큰**으로 재시도(총 3회), 그래도면 권한 확인 |
| `ResourceInitializationError: failed to create log stream` 계열 | 로그그룹 미존재(`awslogs-create-group` 사용) | `/ecs/linkpulse-prod-app` 재사용으로 고정돼 있는지 확인 |
| `PlatformTaskDefinitionIncompatibilityException` / 시크릿 키가 통째로 들어옴 | 플랫폼 버전 미지정 | `--platform-version 1.4.0` |
| `CannotPullContainerError` | Docker Hub rate limit, digest 오타, 아치 불일치 | 같은 digest로 ECR 미러링 폴백, `runtimePlatform.cpuArchitecture` 확인 |
| psql `exitCode=2`, 로그에 timeout | SG 경로(app SG 아님), 엔드포인트 오타 | `app` SG·app 서브넷인지, `PGHOST`가 드릴 인스턴스인지 |
| psql `FATAL: no pg_hba.conf entry … no encryption` | `PGSSLMODE` 누락 | `require` 확인(운영 파라미터그룹의 `rds.force_ssl=1`이 비TLS를 거부한다 — **정상 동작**) |
| `InvalidParameterValue`(복원 호출) | `--restore-time`이 `LatestRestorableTime` 이상(§4.1이 상한을 관측값보다 앞으로 잡는 이유) | **이 드릴에서는 재호출하지 않는다.** ① §5는 이 응답을 `rejected`로 적는데, reconciliation은 `accepted|ambiguous`만, 재시도 창은 `ambiguous`만 받으므로 `rejected`를 되돌리는 전이가 없다. ② restore point를 다시 계산하면 값이 **뒤로** 이동해 최초 호출 직전에 봉인한 sentinel-B의 마진(`B_CREATED_AT > restore point + 60초`)이 무효화될 수 있다. → §10 정리로 종료하고, 필요하면 **§2.1부터 새 드릴**로 다시 시작한다 — 새 `TOKEN`·`DRILL_DIR`과 `RESTORE_CALL_STATE=not-started`, 새 sentinel-A·baseline이 있어야 §5 호출 자격이 열린다. **같은 셸에서 §4.1만 되풀이하면 `rejected`가 그대로 남아 호출이 열리지 않는다** |
| `ConflictException`(run-task) | client token을 **다른 파라미터**로 재사용 | 응답 `resourceIds`의 taskArn을 원장에 옮기고 **다음 토큰**으로 시도 |
| `STOP: … 치환/형식 오류`가 **값은 정상으로 보이는데** §4.1·§4.3·§5·§11에서 반복 | `python3` 부재·실행 실패(macOS Xcode CLT 미설치 등). 시각·JSON 파싱 호출이 전부 `2>/dev/null`이라 진짜 원인이 가려진다 | §2.5 ⓪-1 `TOOLS_OK` 블록을 실행해 stderr를 본다. **드릴 시작 전이면 `DRILL_MUTATION_OK=0`으로 여기서 막힌다**(이 증상은 프리플라이트를 건너뛴 세션에서만 나온다) |

---

## 13. 정리 명령 요약 (재게시) — 끝나면 반드시 여기로

§10을 그대로 실행한다. 요약 순서:

| 단계 | 내용 | 절 |
| ---- | ---- | -- |
| ⓪ | 재탐색 대사(원장 ∪ 실제 상태) | §10.3 |
| ① | 드릴 DB `delete-db-instance` **호출만**(부재/조회실패/존재 3갈래) | §10.4 |
| ② | 태스크 우선순위 3분류 → 정지·waiter | §10.5 |
| ③ | TD: 상태별 분류 → `ACTIVE`만 deregister → `INACTIVE`만 삭제(10개씩) | §10.6 |
| ④ | 임시 role: 인라인 삭제 → 관리형 detach → `delete-role` | §10.7 |
| ⑤⑥⑦ | `wait db-instance-deleted`(**과금 종료**) · 시크릿 3분기 · 운영 무변경 | §10.8 |
| ⑧ | 잔존 목록 2분류 출력 | §10.9 |

- 각 항목은 **존재 조회 후에만 조치**하고 **NotFound는 성공**, 실패해도 **다음 항목을 계속**한다.
- **(d) PASS = §10.9의 (A) 잔존 0.** (B)(TD `DELETE_IN_PROGRESS`·시크릿 삭제 예약)는 세지 않는다.
- 증거(`url`·비밀번호 제외)는 `docs/postmortems/evidence/rds-restore-<날짜>/`(gitignore·로컬)에 스냅샷하고, 측정값은 [ADR 0005](../adr/0005-backup-restore-dr.md)의 측정 절에 확정 기입한다.
- **[후속·권장 익일]** `list-task-definitions --family-prefix <family> --status DELETE_IN_PROGRESS`가 비었는지 1회 확인.
