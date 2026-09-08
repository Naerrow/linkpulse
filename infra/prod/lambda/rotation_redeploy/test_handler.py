"""handler.py 단위 테스트 (plan 0009 1-2).

fake DynamoDB/Secrets Manager/ECS 클라이언트로 확정 흐름과 at-least-once 계약을 고정한다.
프로덕션 직접 invoke가 첫 로직 시험이 되지 않게 하는 것이 목적이다.
boto3는 import하지 않는다 — handler가 기본 factory 안에서만 지연 import하므로
fake를 넘기는 이 테스트는 수집 단계에서 boto3를 요구하지 않는다.
"""

import json
import os
import unittest
from unittest import mock

import handler as h

SECRET_ARN = "arn:aws:secretsmanager:ap-northeast-2:123456789012:secret:rds-pg-a1b2c3"
CLUSTER_ARN = "arn:aws:ecs:ap-northeast-2:123456789012:cluster/linkpulse-prod-cluster"
SERVICE_NAME = "linkpulse-prod-app"
TABLE_NAME = "linkpulse-prod-rotation-idempotency"
VERSION_ID = "a1b2c3d4-5678-90ab-cdef-EXAMPLE11111"

ENV = {
    "ECS_CLUSTER_ARN": CLUSTER_ARN,
    "ECS_SERVICE_NAME": SERVICE_NAME,
    "IDEMPOTENCY_TABLE_NAME": TABLE_NAME,
    "SECRET_ARN": SECRET_ARN,
}


def make_event(version_id=VERSION_ID, label="AWSCURRENT", resources=None):
    """AWS 문서의 Secret Label Updated 이벤트 구조를 그대로 쓴다(labelUpdated는 문자열)."""
    return {
        "version": "0",
        "id": "6a7e8feb-0000-0000-0000-000000000000",
        "detail-type": "Secret Label Updated",
        "source": "aws.secretsmanager",
        "time": "2026-09-14T00:12:34Z",
        "resources": [SECRET_ARN] if resources is None else resources,
        "detail": {"name": "rds-pg", "labelUpdated": label, "versionId": version_id},
    }


class FakeClock:
    """단조 시계 + 함수 잔여 시간. 60초짜리 bounded wait를 실시간으로 태우지 않기 위해.

    ⚠️ monotonic()은 읽을 때마다 아주 조금 전진한다. 정지 시계로 두면 단계 deadline을 잡은
    직후의 게이트 검사가 "잔여 == 예산"이라는 실제로는 불가능한 조건에서 돌아,
    "예산 == 최악 실행시간"이라 항상 닫히는 게이트를 통과시켜 버린다(round-1 결함 1).
    """

    TICK = 0.001

    def __init__(self, remaining=120.0):
        self.now = 1000.0
        self._deadline = self.now + remaining

    def monotonic(self):
        self.now += self.TICK
        return self.now

    def sleep(self, seconds):
        self.now += seconds

    def remaining_ms(self):
        return max(0.0, self._deadline - self.now) * 1000.0


class FakeContext:
    def __init__(self, clock):
        self._clock = clock

    def get_remaining_time_in_millis(self):
        return self._clock.remaining_ms()


class ConditionalCheckFailed(Exception):
    pass


class FakeDynamoDB:
    class exceptions:
        ConditionalCheckFailedException = ConditionalCheckFailed

    def __init__(self):
        self.items = {}
        self.get_item_calls = []
        self.put_item_calls = []
        self.get_item_error = None
        self.put_item_error = None
        # 동시 GetItem miss 창을 재현한다(순차 실행으로는 만들 수 없다).
        self.force_get_miss = False

    def get_item(self, **kwargs):
        self.get_item_calls.append(kwargs)
        if self.get_item_error:
            raise self.get_item_error
        if self.force_get_miss:
            return {}
        key = kwargs["Key"]["versionId"]["S"]
        return {"Item": self.items[key]} if key in self.items else {}

    def put_item(self, **kwargs):
        self.put_item_calls.append(kwargs)
        if self.put_item_error:
            raise self.put_item_error
        key = kwargs["Item"]["versionId"]["S"]
        if "attribute_not_exists" in kwargs.get("ConditionExpression", "") and key in self.items:
            raise ConditionalCheckFailed("marker already exists")
        self.items[key] = kwargs["Item"]


class FakeSecretsManager:
    def __init__(self, stages=None, clock=None, seconds_per_call=0.0):
        self.stages = stages if stages is not None else {VERSION_ID: ["AWSCURRENT"]}
        self.calls = []
        self._clock = clock
        self._seconds_per_call = seconds_per_call

    def describe_secret(self, **kwargs):
        self.calls.append(kwargs)
        if self._clock and self._seconds_per_call:
            self._clock.sleep(self._seconds_per_call)
        response = {"VersionIdsToStages": self.stages}
        if getattr(self, "leak", None):
            # 실 API는 값을 반환하지 않는다. 응답을 통째로 로그에 찍는 회귀를 잡기 위한 미끼다.
            response["SecretString"] = self.leak
        return response


class FakeECS:
    def __init__(self, deployments=None, error=None):
        self.deployments = (
            deployments
            if deployments is not None
            else [{"id": "ecs-svc/1111", "status": "PRIMARY", "rolloutState": "IN_PROGRESS"}]
        )
        self.error = error
        self.calls = []

    def update_service(self, **kwargs):
        self.calls.append(kwargs)
        if self.error:
            raise self.error
        return {"service": {"deployments": self.deployments}}


class HandlerTestCase(unittest.TestCase):
    def setUp(self):
        self.clock = FakeClock()
        self.context = FakeContext(self.clock)
        self.db = FakeDynamoDB()
        self.sm = FakeSecretsManager()
        self.ecs = FakeECS()
        self.clients = {"dynamodb": self.db, "secretsmanager": self.sm, "ecs": self.ecs}

        self.logs = []
        for patcher in (
            mock.patch.dict(os.environ, ENV, clear=False),
            mock.patch.object(h, "_monotonic", self.clock.monotonic),
            mock.patch.object(h, "_sleep", self.clock.sleep),
            mock.patch.object(h, "_log", lambda **f: self.logs.append(f)),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)

    def last_log(self):
        return self.logs[-1]

    def assertNoAwsCalls(self):
        self.assertEqual(self.db.get_item_calls, [])
        self.assertEqual(self.db.put_item_calls, [])
        self.assertEqual(self.sm.calls, [])
        self.assertEqual(self.ecs.calls, [])

    def invoke(self, event=None):
        return h.handler(event or make_event(), self.context, self.clients)


class TestNormalPath(HandlerTestCase):
    def test_a_first_delivery_redeploys_and_writes_marker(self):
        """(a) marker 없음 -> 라벨 확인 후 UpdateService 1회 + PutItem 1회."""
        result = self.invoke()
        self.assertEqual(result["outcome"], "redeploy_submitted")
        self.assertEqual(len(self.ecs.calls), 1)
        self.assertEqual(len(self.db.put_item_calls), 1)
        self.assertIn(VERSION_ID, self.db.items)

    def test_j_update_service_request_contract(self):
        """(j) 환경변수에서 읽은 cluster/service/forceNewDeployment가 그대로 전달된다."""
        self.invoke()
        self.assertEqual(
            self.ecs.calls[0],
            {"cluster": CLUSTER_ARN, "service": SERVICE_NAME, "forceNewDeployment": True},
        )

    def test_marker_carries_ttl_attribute(self):
        """TTL 속성명은 ttl 블록과 같은 expiresAt이어야 한다(다르면 아무것도 만료되지 않는다)."""
        self.invoke()
        item = self.db.put_item_calls[0]["Item"]
        self.assertIn("expiresAt", item)
        self.assertGreater(int(item["expiresAt"]["N"]), 0)

    def test_i_in_progress_rollout_still_writes_marker(self):
        """(i) rolloutState=IN_PROGRESS여도 redeploy_submitted로 marker를 쓴다.

        marker는 "제출됨"이지 "복구됨"이 아니다 — 같은 versionId의 후속 이벤트는 skip된다.
        """
        first = self.invoke()
        self.assertEqual(first["outcome"], "redeploy_submitted")
        self.assertEqual(first["rollout_state"], "IN_PROGRESS")

        second = self.invoke()
        self.assertEqual(second["outcome"], "skipped_duplicate")
        self.assertEqual(len(self.ecs.calls), 1)


class TestDuplicateSuppression(HandlerTestCase):
    def test_b_sequential_duplicate_skips_without_redeploy(self):
        """(b) 완료 후 도착한 순차 중복은 UpdateService를 부르지 않는다. 억제의 유일한 증거."""
        self.invoke()
        self.ecs.calls.clear()

        result = self.invoke()
        self.assertEqual(result["outcome"], "skipped_duplicate")
        self.assertEqual(self.ecs.calls, [])
        self.assertEqual(len(self.db.put_item_calls), 1)

    def test_b2_get_item_uses_consistent_read(self):
        """(b-2) 최종 일관성 읽기면 억제가 확률적으로만 동작한다."""
        self.invoke()
        self.assertTrue(self.db.get_item_calls[0]["ConsistentRead"])

    def test_b3_conditional_check_failed_is_success_with_raced_outcome(self):
        """(b-3) 동시 miss 이후 늦게 진 쪽 — 성공 반환하되 이미 재배포를 제출했다."""
        self.db.items[VERSION_ID] = {"versionId": {"S": VERSION_ID}}
        self.db.force_get_miss = True

        result = self.invoke()
        self.assertEqual(result["outcome"], "redeploy_submitted_raced_marker")
        self.assertEqual(len(self.ecs.calls), 1)
        self.assertEqual(self.last_log()["outcome"], "redeploy_submitted_raced_marker")

    def test_e_concurrent_miss_both_redeploy_exactly_one_marker(self):
        """(e) 둘 다 GetItem miss -> 둘 다 UpdateService, marker를 쓴 쪽은 정확히 하나.

        성공 기준은 "재배포 1회"가 아니다 — 선행 조회는 강한 일관성이어도 원자적이지 않아
        이 설계가 줄 수 없는 보장이다. 진 쪽도 오류가 아닌 성공으로 끝난다.
        """
        self.db.force_get_miss = True

        first = self.invoke()
        second = self.invoke()

        self.assertEqual(first["outcome"], "redeploy_submitted")
        self.assertEqual(second["outcome"], "redeploy_submitted_raced_marker")
        self.assertEqual(len(self.ecs.calls), 2)
        self.assertEqual(len(self.db.items), 1)


class TestFailOpen(HandlerTestCase):
    def test_g_update_service_failure_retries_redeploy(self):
        """(g) UpdateService 실패 -> 오류 반환. 기록이 없으므로 재시도가 재배포를 다시 건다."""
        self.ecs.error = RuntimeError("ThrottlingException")
        with self.assertRaises(h._Failed) as caught:
            self.invoke()
        self.assertEqual(caught.exception.outcome, "failed_update_service")
        self.assertEqual(self.db.put_item_calls, [])

        self.ecs.error = None
        result = self.invoke()
        self.assertEqual(result["outcome"], "redeploy_submitted")
        self.assertEqual(len(self.ecs.calls), 2)

    def test_f_putitem_failure_leaves_no_marker_so_retry_redeploys_again(self):
        """(f) UpdateService 성공 뒤 PutItem 실패 -> 오류. 재시도가 재배포를 한 번 더 건다.

        중복 재배포는 at-least-once 계약 안이다. 반대(재배포 소실)가 압도적으로 나쁘다.
        """
        self.db.put_item_error = RuntimeError("ProvisionedThroughputExceededException")
        with self.assertRaises(h._Failed) as caught:
            self.invoke()
        self.assertEqual(caught.exception.outcome, "failed_putitem")
        self.assertEqual(self.db.items, {})

        self.db.put_item_error = None
        result = self.invoke()
        self.assertEqual(result["outcome"], "redeploy_submitted")
        self.assertEqual(len(self.ecs.calls), 2)

    def test_getitem_failure_is_an_error(self):
        """GetItem 실패는 조건 실패가 아니다 — 삼키면 fail-closed가 되살아난다."""
        self.db.get_item_error = RuntimeError("InternalServerError")
        with self.assertRaises(h._Failed) as caught:
            self.invoke()
        self.assertEqual(caught.exception.outcome, "failed_getitem")
        self.assertEqual(self.ecs.calls, [])


class TestPreconditions(HandlerTestCase):
    def test_c_label_not_moved_exhausts_bounded_wait_and_errors(self):
        """(c) versionId가 아직 AWSCURRENT에 없음 -> 60초 소진 후 오류. marker는 안 쓴다."""
        self.sm.stages = {"other-version": ["AWSCURRENT"]}
        with self.assertRaises(h._Failed) as caught:
            self.invoke()
        self.assertEqual(caught.exception.outcome, "failed_label_wait")
        self.assertEqual(self.ecs.calls, [])
        self.assertEqual(self.db.items, {})
        # 5초 간격 폴링이 60초 예산 안에서만 돈다.
        self.assertEqual(len(self.sm.calls), 12)

    def test_d_missing_primary_deployment_is_an_error(self):
        """(d) 응답에 PRIMARY deployment가 없으면 오류를 반환한다."""
        self.ecs.deployments = [{"id": "ecs-svc/0", "status": "ACTIVE"}]
        with self.assertRaises(h._Failed) as caught:
            self.invoke()
        self.assertEqual(caught.exception.outcome, "failed_update_service")
        self.assertEqual(self.db.items, {})


class TestDeadline(HandlerTestCase):
    def test_h1_insufficient_remaining_time_skips_sdk_call(self):
        """(h)① 진입 시 남은 시간이 부족하면 SDK 호출을 아예 시작하지 않는다.

        시작한 재시도는 끊을 수 없어 120초에 강제 종료되면 실패 지점조차 로그에 안 남는다.
        """
        self.clock = FakeClock(remaining=16.0)
        self.context = FakeContext(self.clock)
        with mock.patch.object(h, "_monotonic", self.clock.monotonic):
            with self.assertRaises(h._Failed) as caught:
                self.invoke()
        self.assertEqual(caught.exception.outcome, "failed_deadline")
        self.assertEqual(self.db.get_item_calls, [])

    def test_h2_slow_sdk_breaks_the_stage_monotonic_deadline(self):
        """(h)② 느린 SDK가 단계 deadline을 넘기면 루프가 깨진다.

        진입 시 잔여 시간만 보는 구현은 이 경로에서 통과하지 못한다.
        """
        self.sm = FakeSecretsManager(
            stages={"other-version": ["AWSCURRENT"]}, clock=self.clock, seconds_per_call=25.0
        )
        self.clients["secretsmanager"] = self.sm

        with self.assertRaises(h._Failed) as caught:
            self.invoke()
        self.assertEqual(caught.exception.outcome, "failed_label_wait")
        # 25 + 5(sleep) + 25 = 55초 -> 단계 잔여 5초 이하라 다음 sleep 전에 끝난다 -> 정확히 2회.
        self.assertEqual(len(self.sm.calls), 2)
        self.assertEqual(self.ecs.calls, [])


class TestValidation(HandlerTestCase):
    def test_j_missing_environment_variable_fails_before_any_aws_call(self):
        """(j) 네 환경변수 중 하나라도 비면 호출 진입 시, 첫 AWS 호출 전에 실패한다."""
        for key in h.ENV_KEYS:
            with self.subTest(key=key):
                with mock.patch.dict(os.environ, {key: ""}):
                    with self.assertRaises(ValueError):
                        self.invoke()
                self.assertNoAwsCalls()
                self.assertEqual(self.last_log()["outcome"], "failed_validation")

    def test_event_for_another_secret_is_rejected(self):
        """직접 invoke는 rule의 resources 필터를 우회하므로 handler가 다시 대조한다."""
        other = "arn:aws:secretsmanager:ap-northeast-2:123456789012:secret:other-x1y2z3"
        with self.assertRaises(ValueError):
            self.invoke(make_event(resources=[other]))
        self.assertNoAwsCalls()
        self.assertEqual(self.last_log()["outcome"], "failed_validation")

    def test_non_awscurrent_label_is_rejected(self):
        with self.assertRaises(ValueError):
            self.invoke(make_event(label="AWSPREVIOUS"))
        self.assertEqual(self.db.get_item_calls, [])

    def test_label_as_array_is_accepted(self):
        """rule 패턴은 본문이 배열이어도 매칭한다 — handler가 더 좁으면 거짓 음성이 된다."""
        result = self.invoke(make_event(label=["AWSCURRENT"]))
        self.assertEqual(result["outcome"], "redeploy_submitted")
        self.assertEqual(len(self.ecs.calls), 1)

    def test_array_without_awscurrent_is_rejected(self):
        with self.assertRaises(ValueError):
            self.invoke(make_event(label=["AWSPREVIOUS"]))
        self.assertNoAwsCalls()

    def test_missing_version_id_is_rejected(self):
        with self.assertRaises(ValueError):
            self.invoke(make_event(version_id=""))
        self.assertEqual(self.db.get_item_calls, [])


class TestRealClock(unittest.TestCase):
    """⚠️ _monotonic·_sleep을 패치하지 **않는다** — 정지 시계 fake가 가리는 결함을 잡는 유일한 테스트.

    "단계 예산 == 최악 실행시간"이면 진입 게이트가 구조적으로 항상 닫혀, 정상 이벤트도 AWS 호출
    전에 failed_deadline으로 끝나고 재배포가 0회가 된다(round-1 codex-cli#1 / claude-ide#1).
    라벨이 이미 AWSCURRENT라 폴링이 1회로 끝나므로 실시계로도 수 ms에 끝난다.

    ⚠️ 이 클래스에는 폴링이 도는 케이스(라벨 미이동)를 넣지 않는다 — _sleep도 패치하지 않으므로
    실제로 5초씩 잔다. 그 경로는 FakeClock을 쓰는 TestPreconditions·TestDeadline이 덮는다.
    """

    class Context:
        def get_remaining_time_in_millis(self):
            return 120000

    def run_handler(self, clients, event=None):
        with mock.patch.dict(os.environ, ENV, clear=False):
            with mock.patch.object(h, "_log", lambda **f: self.logs.append(f)):
                return h.handler(event or make_event(), self.Context(), clients)

    def setUp(self):
        self.logs = []

    def test_normal_path_reaches_update_service(self):
        db, ecs = FakeDynamoDB(), FakeECS()
        clients = {"dynamodb": db, "secretsmanager": FakeSecretsManager(), "ecs": ecs}

        result = self.run_handler(clients)

        self.assertEqual(result["outcome"], "redeploy_submitted")
        self.assertEqual(len(ecs.calls), 1)
        self.assertEqual(len(db.put_item_calls), 1)

    def test_secret_material_never_reaches_the_log(self):
        """handler는 GetSecretValue를 부르지 않지만, DescribeSecret 응답을 통째로 찍는
        회귀가 들어와도 잡히게 고정한다."""
        sm = FakeSecretsManager()
        sm.stages = {VERSION_ID: ["AWSCURRENT"]}
        sm.leak = "super-secret-password"
        clients = {"dynamodb": FakeDynamoDB(), "secretsmanager": sm, "ecs": FakeECS()}

        self.run_handler(clients)

        rendered = json.dumps(self.logs, ensure_ascii=False, default=str)
        self.assertNotIn("super-secret-password", rendered)
        self.assertNotIn("SecretString", rendered)


if __name__ == "__main__":
    unittest.main()
