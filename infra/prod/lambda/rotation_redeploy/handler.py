"""RDS 마스터 비밀번호 회전이 끝나면 ECS 서비스를 강제 재배포한다.

Secrets Manager가 AWSCURRENT 라벨을 새 버전으로 옮길 때 발행하는 네이티브
`Secret Label Updated` 이벤트를 EventBridge가 이 함수로 전달한다. 앱은 기동 시 주입된
DB_PASSWORD를 프로세스 수명 내내 재사용하므로(app/internal/db/db.go), 회전 후에는
재배포만이 새 비밀번호를 태스크에 넣는다.

계약 (plan 0009 Step 1):

- 멱등 키는 detail.versionId다. 부작용 *앞*의 GetItem(강한 일관성)이 완료 후 도착한 순차
  중복을 거르고, 부작용 *뒤*의 조건부 PutItem은 marker 유일성만 담당한다(fail-open).
  기록을 UpdateService 앞에 두면 "기록은 됐는데 재배포는 실패"한 상태가 이후 모든 전달을
  skip시켜 자동 복구가 조용히 봉인된다.
- marker는 "재배포를 제출했다"는 뜻이지 "복구됐다"는 뜻이 아니다. rollout은 추적하지 않는다
  — 롤링은 수 분이고 이 함수의 내부 예산은 105초라 원리적으로 불가능하다. rollout 실패는
  ECS 배포 서킷브레이커 + 기존 ALB/ECS 알람 + canary 탐지 → 수동 브릿지가 받는다.
- 계약은 at-least-once다. 동시 GetItem miss와 "UpdateService 성공 → PutItem 실패" 두 창에서
  재배포가 한 번 더 돌 수 있다. UpdateService에는 clientToken이 없어 exactly-once는 불가능하고,
  중복 재배포(롤링 한 번)보다 재배포 소실(서비스가 내려간 채 알람도 없음)이 압도적으로 나쁘다.
- 라벨 확인(DescribeSecret)은 비교 기준이 아니라 UpdateService의 선행 조건이다. 전파 지연
  중에 재배포하면 새 태스크가 옛 AWSCURRENT를 주입받아 그대로 실패한다.

boto3는 기본 client factory 안에서만 지연 import한다 — 단위 테스트가 fake factory를 넘기므로
모듈 수집 단계에서 boto3를 요구하지 않는다(sys.modules monkeypatch도 필요 없다).
"""

import json
import os
import time

# ---- 실행 예산 (초) --------------------------------------------------------
# 함수 timeout 120초가 이 표에서 파생된다:
#   8(GetItem) + 60(라벨 확인) + 20(UpdateService) + 8(PutItem) + 9(여유) = 105 = 내부 deadline
# 상한 합계가 함수 timeout과 같으면 안전 여유가 아니다 — 모든 구간이 상한에 닿으면 반환·직렬화
# 전에 런타임이 종료돼 어떤 실패였는지 로그조차 남지 않는다. 그래서 120 - 105 = 15초를 벌린다.
#
# ⚠️ 단계 예산은 그 단계의 최악 실행시간보다 **커야** 한다. 같으면 진입 게이트가 구조적으로
# 항상 닫힌다 — deadline을 잡은 뒤 게이트가 시계를 다시 읽는 사이에 시간이 지나므로
# "단계 잔여 < 최악"이 언제나 참이 되고, 정상 이벤트도 AWS 호출 전에 failed_deadline으로 끝난다
# (round-1 codex-cli#1 / claude-ide#1 — 실시계 10회 시행에서 10회 모두 차단됐다).
# GetItem·PutItem은 최악 6에 예산 6이라 이 함정에 걸려 있었다. 각 8로 올리고 여유를 13 -> 9로
# 줄여 합계 105를 유지한다. 8은 새 숫자가 아니라 UpdateService(예산 20 / 최악 18)가 이미 쓰던
# "예산 = 최악 + 2" 패턴으로 되돌린 값이다.
#
# ⚠️ 합계 105는 **장부 숫자**다 — 각 단계가 _monotonic() + BUDGET으로 deadline을 새로 잡으므로
# 105초짜리 전역 deadline은 코드에 존재하지 않는다. 실제 강제 장치는 RESERVE_SECONDS(함수 잔여)와
# 단계별 deadline 둘이다. 여유 행을 더 깎아도 런타임은 바뀌지 않으니 이 숫자를 불변식으로 읽지 않는다.
BUDGET_GET_ITEM = 8
BUDGET_LABEL_WAIT = 60
BUDGET_UPDATE_SERVICE = 20
BUDGET_PUT_ITEM = 8
RESERVE_SECONDS = 15

# 각 SDK 호출의 최악 실행시간 = 시도 수 x (connect + read) + backoff 상한(2초).
# connect_timeout/read_timeout은 "한 번의 연결·읽기 시도" 제한이지 API 호출 전체의 제한이
# 아니다. botocore는 timeout·5xx·스로틀을 별도 횟수로 재시도하므로, 시도 수를 함께 고정하고
# 그 최악값을 진입 게이트에서 본다. 시작한 재시도는 끊을 수 없다.
WORST_GET_ITEM = 6  # 2 x (1+1) + 2
WORST_DESCRIBE_SECRET = 4  # 1 x (1+3)
WORST_UPDATE_SERVICE = 18  # 2 x (2+6) + 2
WORST_PUT_ITEM = 6  # 2 x (1+1) + 2

LABEL_POLL_INTERVAL = 5

# marker 보존 7일. 멱등 창의 end-to-end 실질 상한은 약 2시간이다(EventBridge target 재시도
# 1시간과 Lambda async 보관 1시간은 서로 다른 계층의 독립 상한이라 합성된다) — 84배 여유.
# TTL은 claim lease가 아니라 보존 정리용이다: 삭제는 만료 즉시가 아니라 통상 수일 내이고,
# 만료됐지만 아직 삭제되지 않은 아이템은 GetItem에 계속 보인다.
MARKER_TTL_SECONDS = 7 * 24 * 60 * 60

# ECS 대상은 이벤트가 주지 않는다(이벤트에는 시크릿 ARN·라벨·versionId뿐). Terraform이 주입한다.
ENV_KEYS = ("ECS_CLUSTER_ARN", "ECS_SERVICE_NAME", "IDEMPOTENCY_TABLE_NAME", "SECRET_ARN")

# 테스트가 갈아끼울 수 있게 모듈 속성으로 둔다(단계 deadline을 실시간으로 태우지 않기 위해).
_monotonic = time.monotonic
_sleep = time.sleep


class _Failed(Exception):
    """실패 지점을 outcome으로 들고 다니는 예외.

    이 예외로 끝나면 Lambda 호출이 오류로 끝나 재시도 경로(계층 2 on-failure)를 타고,
    로그에는 어느 지점에서 멈췄는지가 남는다.
    """

    def __init__(self, outcome, message, **fields):
        super().__init__(message)
        self.outcome = outcome
        self.fields = fields


def _log(**fields):
    """구조화 로그 한 줄. 1-7의 사후 증거가 이 필드들에 의존한다.

    시크릿 값은 담기지 않는다 — 이 함수는 GetSecretValue를 호출하지 않고,
    DescribeSecret은 암호화된 값을 반환하지 않는다.
    """
    print(json.dumps(fields, ensure_ascii=False, sort_keys=True, default=str))


def _default_clients():
    """boto3를 여기서만 지연 import한다(모듈 수집 시점에 요구하지 않기 위해).

    구간마다 timeout·시도 수가 다르므로 클라이언트도 구간별로 나눠 만든다.
    """
    import boto3
    from botocore.config import Config

    def cfg(connect, read, attempts):
        return Config(
            connect_timeout=connect,
            read_timeout=read,
            retries={"mode": "standard", "total_max_attempts": attempts},
        )

    return {
        "dynamodb": boto3.client("dynamodb", config=cfg(1, 1, 2)),
        "secretsmanager": boto3.client("secretsmanager", config=cfg(1, 3, 1)),
        "ecs": boto3.client("ecs", config=cfg(2, 6, 2)),
    }


def _load_config():
    """네 환경변수를 읽고 하나라도 비면 **호출 진입 시, 첫 AWS 호출 전에** 실패한다.

    빈 값으로 AWS를 호출하면 엉뚱한 오류로 진단이 흐려진다 — 특히 cluster를 생략하면
    default 클러스터를 보고 ServiceNotFoundException이 난다. 상수 하드코딩은 하지 않는다.

    ⚠️ Lambda의 Init 단계(모듈 최상위)가 아니라 Invoke 단계에서 읽으므로 plan의 "기동 시
    fail-fast"를 문자 그대로 만족하지는 않는다(round-1 codex-cli#3 / codex-ide). 안전
    불변식은 같다 — 이벤트 파싱·client 생성·모든 AWS 호출보다 앞서 실패하고 구조화 로그와
    on-failure destination을 그대로 탄다. 모듈 로드를 환경변수에 묶지 않아 단위 테스트가
    환경변수 없이 이 모듈을 import할 수 있다는 이점이 있어 이 시점을 택했다.
    """
    config = {key: os.environ.get(key, "").strip() for key in ENV_KEYS}
    missing = sorted(key for key, value in config.items() if not value)
    if missing:
        raise ValueError("required environment variables are empty: " + ", ".join(missing))
    return config


def _read_event(event, secret_arn):
    """이벤트에서 versionId를 꺼내고 대상 시크릿·라벨인지 대조한다.

    rule이 resources와 labelUpdated로 이미 거르지만, 직접 invoke(1-4)는 그 필터를 우회한다.
    이벤트 본문의 labelUpdated는 문자열이다(event pattern 쪽만 배열).
    """
    detail = event.get("detail") or {}
    version_id = (detail.get("versionId") or "").strip()
    label = detail.get("labelUpdated")
    # rule 패턴의 labelUpdated = ["AWSCURRENT"]는 본문이 문자열이든 배열이든 매칭한다.
    # 여기서 문자열만 받으면 rule이 통과시킨 실이벤트를 handler가 거부해 거짓 음성이 된다
    # (재배포 0회 + Errors 알람). 폭을 rule에 맞춘다.
    labels = label if isinstance(label, list) else [label]
    if not version_id:
        raise ValueError("event detail.versionId is missing")
    if "AWSCURRENT" not in labels:
        raise ValueError("unexpected detail.labelUpdated: %r" % (label,))
    if secret_arn not in (event.get("resources") or []):
        raise ValueError("event resources do not contain the target secret ARN")
    return version_id


def _gate(stage_deadline, context, worst_case):
    """SDK 호출 진입 게이트. 막혔으면 사유("stage"/"function"), 아니면 None.

    단계 잔여와 함수 잔여(-여유 15초)를 둘 다 본다. 함수 잔여만 보면 16초 남았을 때
    최악 18초짜리 UpdateService에 진입하는 것을 막지 못한다.

    ⚠️ 단발 구간(GetItem/UpdateService/PutItem)은 deadline을 바로 한 문장 앞에서 잡으므로,
    "예산 > 최악"인 한 stage 항이 실질적으로 바인딩되지 않는다 — 그 셋을 지키는 것은 function
    항 하나다. stage 항이 실제로 도는 곳은 라벨 확인 폴링 루프이고 test_h2가 그것을 덮는다.
    """
    if stage_deadline - _monotonic() < worst_case:
        return "stage"
    if context.get_remaining_time_in_millis() / 1000.0 - RESERVE_SECONDS < worst_case:
        return "function"
    return None


def _marker_exists(clients, context, config, version_id):
    """부작용 앞의 강한 일관성 읽기 — 완료 후 도착한 순차 중복을 여기서 거른다.

    ConsistentRead가 없으면 직전에 쓴 marker를 못 볼 수 있어 억제가 확률적으로만 동작한다.
    """
    deadline = _monotonic() + BUDGET_GET_ITEM
    blocked = _gate(deadline, context, WORST_GET_ITEM)
    if blocked:
        raise _Failed("failed_deadline", "no budget for GetItem (%s)" % blocked)
    try:
        response = clients["dynamodb"].get_item(
            TableName=config["IDEMPOTENCY_TABLE_NAME"],
            Key={"versionId": {"S": version_id}},
            ConsistentRead=True,
        )
    except Exception as exc:
        raise _Failed("failed_getitem", "%s: %s" % (type(exc).__name__, exc)) from exc
    return "Item" in response


def _wait_for_current_label(clients, context, config, version_id):
    """이벤트의 versionId가 AWSCURRENT를 얻을 때까지 상한 안에서 기다린다.

    상한 안에 확인되지 않으면 오류를 반환한다 — marker를 쓰기 전이라 재시도가 안전하다.
    주의: 이것은 창을 좁히는 heuristic이지 폐쇄가 아니다. 여기서 본 것과 ECS execution role이
    태스크 기동 시 하는 읽기는 다른 호출이다. 남는 구간은 탐지 -> 수동 브릿지가 받는다.
    """
    deadline = _monotonic() + BUDGET_LABEL_WAIT
    while True:
        blocked = _gate(deadline, context, WORST_DESCRIBE_SECRET)
        if blocked == "function":
            raise _Failed("failed_deadline", "no function budget left for DescribeSecret")
        if blocked == "stage":
            break
        try:
            response = clients["secretsmanager"].describe_secret(SecretId=config["SECRET_ARN"])
        except Exception as exc:
            raise _Failed("failed_label_wait", "%s: %s" % (type(exc).__name__, exc)) from exc
        stages = (response.get("VersionIdsToStages") or {}).get(version_id) or []
        if "AWSCURRENT" in stages:
            return
        # sleep도 단계 deadline 안에서 돈다 — 넘길 만큼 남았으면 자지 않고 끝낸다.
        if deadline - _monotonic() <= LABEL_POLL_INTERVAL:
            break
        _sleep(LABEL_POLL_INTERVAL)
    raise _Failed(
        "failed_label_wait",
        "AWSCURRENT did not move to %s within %ss" % (version_id, BUDGET_LABEL_WAIT),
    )


def _force_new_deployment(clients, context, config):
    """재배포 제출. 판정 기준은 UpdateService가 200을 반환했다는 사실 하나다.

    DescribeServices를 따로 부르지 않는다 — ECS 최종 일관성 때문에 정상 재배포를 관측 실패로
    오판해 중복 재배포를 만드는 경로가 사라지고, IAM에서 ecs:DescribeServices가 빠진다.
    """
    deadline = _monotonic() + BUDGET_UPDATE_SERVICE
    blocked = _gate(deadline, context, WORST_UPDATE_SERVICE)
    if blocked:
        raise _Failed("failed_deadline", "no budget for UpdateService (%s)" % blocked)
    try:
        response = clients["ecs"].update_service(
            cluster=config["ECS_CLUSTER_ARN"],
            service=config["ECS_SERVICE_NAME"],
            forceNewDeployment=True,
        )
    except Exception as exc:
        raise _Failed("failed_update_service", "%s: %s" % (type(exc).__name__, exc)) from exc

    deployments = (response.get("service") or {}).get("deployments") or []
    primary = next((d for d in deployments if d.get("status") == "PRIMARY"), None)
    if primary is None:
        raise _Failed("failed_update_service", "UpdateService response has no PRIMARY deployment")
    # PRIMARY는 "가장 최근 배포"라는 지위일 뿐 rollout 결과가 아니고, 이 id가 방금 만들어진
    # 것인지 직전 CI 배포의 것인지 판정할 기준도 handler에는 없다 -> 사후 추적용 로그 필드다.
    return {"deployment_id": primary.get("id"), "rollout_state": primary.get("rolloutState")}


def _put_marker(clients, context, config, version_id, deployment):
    """부작용 뒤의 조건부 쓰기. True면 이 호출이 marker를 썼고, False면 동시 경합에서 졌다.

    ConditionalCheckFailedException만 따로 잡는다 — 스로틀·5xx를 조건 실패와 함께 삼키면
    "재배포는 제출됐는데 오류가 아니어서 아무 알람도 울지 않는" fail-closed가 되살아난다.
    """
    deadline = _monotonic() + BUDGET_PUT_ITEM
    blocked = _gate(deadline, context, WORST_PUT_ITEM)
    if blocked:
        raise _Failed("failed_deadline", "no budget for PutItem (%s)" % blocked, **deployment)

    client = clients["dynamodb"]
    item = {
        "versionId": {"S": version_id},
        "outcome": {"S": "redeploy_submitted"},
        "expiresAt": {"N": str(int(time.time()) + MARKER_TTL_SECONDS)},
    }
    if deployment.get("deployment_id"):
        item["deploymentId"] = {"S": deployment["deployment_id"]}
    try:
        client.put_item(
            TableName=config["IDEMPOTENCY_TABLE_NAME"],
            Item=item,
            ConditionExpression="attribute_not_exists(versionId)",
        )
    except client.exceptions.ConditionalCheckFailedException:
        return False
    except Exception as exc:
        raise _Failed("failed_putitem", "%s: %s" % (type(exc).__name__, exc), **deployment) from exc
    return True


def _run(clients, context, config, version_id):
    if _marker_exists(clients, context, config, version_id):
        return "skipped_duplicate", {}
    _wait_for_current_label(clients, context, config, version_id)
    deployment = _force_new_deployment(clients, context, config)
    wrote = _put_marker(clients, context, config, version_id, deployment)
    outcome = "redeploy_submitted" if wrote else "redeploy_submitted_raced_marker"
    return outcome, deployment


def handler(event, context, clients=None):
    """EventBridge -> Lambda 진입점.

    반환값 규칙: GetItem 적중 또는 PutItem ConditionalCheckFailed = 성공 반환,
    그 외 모든 실패 = 오류 반환(재시도 경로). 두 성공은 서로 다른 일이 일어난 것이므로
    outcome으로 구분한다 — 앞은 재배포를 부르지 않았고, 뒤는 이미 제출했다.
    """
    fields = {"event_id": event.get("id"), "event_time": event.get("time")}
    try:
        config = _load_config()
        version_id = _read_event(event, config["SECRET_ARN"])
    except ValueError as exc:
        _log(outcome="failed_validation", error=str(exc), **fields)
        raise

    fields["version_id"] = version_id
    try:
        outcome, extra = _run(clients or _default_clients(), context, config, version_id)
    except _Failed as exc:
        _log(outcome=exc.outcome, error=str(exc), **dict(fields, **exc.fields))
        raise

    _log(outcome=outcome, **dict(fields, **extra))
    return dict(extra, outcome=outcome)
