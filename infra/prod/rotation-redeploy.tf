# P4(d)-2 Step 1 — RDS 비밀번호 회전 완료 시 자동 재배포 (plan 0009)
#
# 왜: 앱은 기동 시 주입된 DB_PASSWORD를 프로세스 수명 내내 재사용하므로(app/internal/db/db.go),
# 회전 후 최대 5분 뒤 신규 커넥션이 전부 28P01로 죽는다. 네 번 재발했고(66h / 24h / 118h / 13h)
# 네 번 모두 복구 동작 자체는 2~6분이면 끝났다 — 다운 시간을 정한 것은 "사람이 시작하기까지"였다.
# 이 파일은 그 시작을 기계에 넘긴다. 앱 코드는 바뀌지 않는다(근본 대응은 Step 2).
#
# 경로: Secrets Manager가 AWSCURRENT를 옮길 때 발행하는 네이티브 Secret Label Updated 이벤트
#   -> EventBridge rule -> Lambda -> ecs:UpdateService(forceNewDeployment).
# CloudTrail RotationSucceeded를 쓰지 않는 이유: 실이벤트 조회 결과 Resources=[]라 top-level
# resources 정확 매칭 선례(eventbridge.tf)가 적용되지 않고, CloudTrail 경유는 best-effort다.
# Lambda가 필요한 이유: event bus rule의 ECS 타깃은 RunTask용뿐이고 UpdateService 범용 타깃이
# 없다(universal target은 EventBridge Scheduler의 기능이지 event rule의 기능이 아니다).
#
# data.aws_caller_identity.current 는 github_oidc.tf 에 선언돼 있어 재사용한다(재선언 금지).

locals {
  # RDS가 관리하는 마스터 비밀번호 시크릿. iam.tf가 execution role에 쓰는 것과 같은 값이다.
  rotation_secret_arn = aws_db_instance.main.master_user_secret[0].secret_arn
}

# =====================================================================
# 광역 관찰 rule — 좁은 rule의 미매칭은 무증상이라, 실이벤트 원문을 따로 남긴다.
# detail 필터 없이 대상 시크릿의 모든 Secrets Manager 이벤트를 CloudWatch Logs에 적재한다.
# 좁은 rule이 침묵할 때 "이벤트가 안 온 것인지 패턴이 틀린 것인지"를 이 로그가 가른다.
# =====================================================================
resource "aws_cloudwatch_event_rule" "secret_events_observe" {
  name        = "${local.name_prefix}-secret-events-observe"
  description = "Observe all Secrets Manager events for the RDS master secret (raw payload for pattern forensics)"

  event_pattern = jsonencode({
    source    = ["aws.secretsmanager"]
    resources = [local.rotation_secret_arn]
  })

  tags = { Name = "${local.name_prefix}-secret-events-observe" }
}

# /aws/events/ 접두사는 EventBridge Logs 타깃의 관례다. retention을 명시하지 않으면 무기한이라
# logs.tf와 같은 var.log_retention_days를 쓴다.
resource "aws_cloudwatch_log_group" "secret_events" {
  name              = "/aws/events/${local.name_prefix}-secret-events"
  retention_in_days = var.log_retention_days
  tags              = { Name = "${local.name_prefix}-secret-events" }
}

# EventBridge의 CloudWatch Logs 타깃은 IAM 실행 role을 쓰지 않는다(SNS 타깃과 다른 점이다 —
# eventbridge.tf의 role_arn 패턴을 여기 복사하면 안 된다). 필요한 것은 로그 그룹의
# resource-based policy이고, AWS 공식 예제 형태를 그대로 쓴다:
#   principal 둘(events / delivery.logs), action 둘(CreateLogStream / PutLogEvents),
#   resource는 stream 범위(:*)까지. 하나라도 좁으면 첫 실이벤트가 FailedInvocations로 끝난다.
# aws_cloudwatch_log_group.arn 은 provider가 :* 접미사를 떼고 주므로 여기서 다시 붙인다.
#
# confused deputy 경계: 로그 그룹 ARN 한정은 "어디에 쓰는가"만 좁히고 "누구를 대신해 호출하는가"는
# 묶지 않는다. AWS의 ECS lifecycle events 예제가 **같은 두 principal에 aws:SourceAccount와
# rule ARN의 aws:SourceArn을 함께** 걸므로(round-1 codex-ide#2로 1차 출처 확인), 그 형태를 따른다.
# DLQ queue policy와 같은 경계를 여기에도 적용하는 것이다.
data "aws_iam_policy_document" "secret_events_logs" {
  statement {
    sid    = "AllowEventBridgeToWriteLogs"
    effect = "Allow"

    principals {
      type        = "Service"
      identifiers = ["events.amazonaws.com", "delivery.logs.amazonaws.com"]
    }

    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.secret_events.arn}:*"]

    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }

    condition {
      test     = "ArnEquals"
      variable = "aws:SourceArn"
      values   = [aws_cloudwatch_event_rule.secret_events_observe.arn]
    }
  }
}

resource "aws_cloudwatch_log_resource_policy" "secret_events" {
  policy_name     = "${local.name_prefix}-secret-events"
  policy_document = data.aws_iam_policy_document.secret_events_logs.json
}

# rule과 target은 별도 리소스다(eventbridge.tf의 선례와 같다). 빠뜨리면 describe-rule과
# resource policy 검사는 전부 통과하는데 target이 0개라 원문을 한 건도 얻지 못한다.
resource "aws_cloudwatch_event_target" "secret_events_logs" {
  rule = aws_cloudwatch_event_rule.secret_events_observe.name
  arn  = aws_cloudwatch_log_group.secret_events.arn

  depends_on = [aws_cloudwatch_log_resource_policy.secret_events]
}

# =====================================================================
# 좁은 rule — 실제 동작용. 대상 시크릿의 AWSCURRENT 라벨 이동만 잡는다.
# 주의: event pattern의 비교 값은 배열이다. 이벤트 *본문*의 labelUpdated는 문자열이고
# *패턴*은 배열이어야 한다(AWS 공식 예제도 "labelUpdated": ["AWSCURRENT"]).
# Secrets Manager는 AWSPENDING·AWSPREVIOUS에는 이 이벤트를 발행하지 않고,
# detail.versionId는 라벨이 붙은 새 버전 ID다 — 이 설계 전체가 그 계약에 기댄다.
# =====================================================================
resource "aws_cloudwatch_event_rule" "rotation_label_updated" {
  name        = "${local.name_prefix}-rotation-label-updated"
  description = "RDS master secret AWSCURRENT moved (rotation finished) trigger ECS force new deployment"

  event_pattern = jsonencode({
    source        = ["aws.secretsmanager"]
    "detail-type" = ["Secret Label Updated"]
    resources     = [local.rotation_secret_arn]
    detail = {
      labelUpdated = ["AWSCURRENT"]
    }
  })

  tags = { Name = "${local.name_prefix}-rotation-label-updated" }
}

# 계층 1 — EventBridge가 Lambda 비동기 큐에 "전달하지 못한" 경우를 받는다.
# handler 안에서 난 오류는 여기로 오지 않는다(그건 계층 2다).
resource "aws_sqs_queue" "rotation_events_dlq" {
  name = "${local.name_prefix}-rotation-events-dlq"
  # 기본 보존은 4일인데 3차 장애가 118시간(약 5일)이었다 — 기본값이면 증거가 사라진다.
  message_retention_seconds = 1209600 # 14일
  # 저장소의 첫 SQS다. 메시지에 비밀값은 없지만(시크릿 ARN·versionId뿐) 여기서 관례를 세운다.
  sqs_managed_sse_enabled = true

  tags = { Name = "${local.name_prefix}-rotation-events-dlq" }
}

# Lambda 리소스 정책은 InvokeFunction을 rule ARN으로 제한하는데(아래), DLQ에 같은 경계가
# 없으면 계정 밖 EventBridge도 이 큐에 쓸 수 있다(confused deputy). AWS 공식 예제대로
# SourceArn을 해당 rule ARN으로 제한한다.
data "aws_iam_policy_document" "rotation_events_dlq" {
  statement {
    sid    = "AllowRotationRuleToSendToDlq"
    effect = "Allow"

    principals {
      type        = "Service"
      identifiers = ["events.amazonaws.com"]
    }

    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.rotation_events_dlq.arn]

    condition {
      test     = "ArnEquals"
      variable = "aws:SourceArn"
      values   = [aws_cloudwatch_event_rule.rotation_label_updated.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }
}

resource "aws_sqs_queue_policy" "rotation_events_dlq" {
  queue_url = aws_sqs_queue.rotation_events_dlq.id
  policy    = data.aws_iam_policy_document.rotation_events_dlq.json
}

# retry_policy를 명시하지 않으면 기본이 최대 24시간·185회 재시도다. 그러면 24시간 뒤 도착한
# 전달이 다음 회전 세대에 걸쳐 "엉뚱한 세대의 이벤트로 재배포"할 수 있다. 회전은 7일 1회라
# 1시간이면 일시적 스로틀·전파 지연을 넘기기 충분하고, 그보다 오래된 이벤트는 오히려 위험하다.
resource "aws_cloudwatch_event_target" "rotation_redeploy_lambda" {
  rule = aws_cloudwatch_event_rule.rotation_label_updated.name
  arn  = aws_lambda_function.rotation_redeploy.arn

  retry_policy {
    maximum_retry_attempts       = 2
    maximum_event_age_in_seconds = 3600
  }

  dead_letter_config {
    arn = aws_sqs_queue.rotation_events_dlq.arn
  }

  # 권한이 붙기 전에 target이 생기면 첫 전달이 조용히 실패한다(eventbridge.tf와 같은 이유).
  depends_on = [aws_lambda_permission.rotation_redeploy_events]
}

# =====================================================================
# Lambda — 이 저장소의 첫 Lambda다.
# Python 3.13 + boto3: boto3가 런타임에 내장돼 의존성 패키징·빌드 단계가 불필요하다.
# Go를 택하면 CI에 새 빌드 단계가 필요한데(.github/workflows는 앱 이미지 빌드만 한다)
# 기한이 짧아 그 작업량을 지지 않는다. 트레이드오프: 관리형 런타임의 boto3 버전은 AWS가
# 고정해 주지 않는다 — handler가 쓰는 API는 update_service/describe_secret/get_item/put_item
# 넷이고 전부 오래된 안정 API라 실질 위험은 낮다.
# =====================================================================

# source_code_hash가 없으면 handler.py를 고쳐도 Terraform이 변경을 감지하지 못해 plan에
# 아무것도 안 뜨고 옛 코드가 계속 돈다(드릴에서 결함을 고칠 때 조용히 막히는 함정).
# source_dir이 아니라 source_file인 이유: 같은 디렉터리의 test_handler.py를 패키징하지 않는다.
data "archive_file" "rotation_redeploy" {
  type        = "zip"
  source_file = "${path.module}/lambda/rotation_redeploy/handler.py"
  output_path = "${path.module}/build/rotation-redeploy.zip"
}

# /aws/lambda/<fn>은 첫 호출 때 자동 생성되고 retention이 무기한이며 Terraform 관리 밖에
# 남는다. 1-7의 사후 증거가 이 로그에 의존하므로 보존 기간을 명시한다.
resource "aws_cloudwatch_log_group" "rotation_redeploy" {
  name              = "/aws/lambda/${local.name_prefix}-rotation-redeploy"
  retention_in_days = var.log_retention_days
  tags              = { Name = "${local.name_prefix}-rotation-redeploy" }
}

# 계층 2 — handler의 AccessDenied·타임아웃·예외를 받는다. 계층 1로는 가지 않는다.
# 이 큐에는 queue policy가 필요 없다: 메시지를 넣는 주체가 Lambda 서비스가 아니라
# "함수의 execution role"이므로 필요한 것은 아래 role의 sqs:SendMessage뿐이다.
# 쓰이지 않는 신뢰 관계를 만들지 않는다.
resource "aws_sqs_queue" "rotation_redeploy_failures" {
  name                      = "${local.name_prefix}-rotation-redeploy-failures"
  message_retention_seconds = 1209600 # 14일 — 위 DLQ와 같은 이유
  sqs_managed_sse_enabled   = true

  tags = { Name = "${local.name_prefix}-rotation-redeploy-failures" }
}

data "aws_iam_policy_document" "rotation_redeploy_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "rotation_redeploy" {
  name               = "${local.name_prefix}-rotation-redeploy"
  assume_role_policy = data.aws_iam_policy_document.rotation_redeploy_assume.json
  tags               = { Name = "${local.name_prefix}-rotation-redeploy" }
}

# 권한을 조각으로 나눠 각각 대상 ARN 한정으로 준다.
# 주지 않는 것을 함께 적어 둔다:
#   - secretsmanager:GetSecretValue — 비밀번호 원문을 함수 메모리로 끌어올 이유가 없다.
#     라벨 확인은 DescribeSecret의 VersionIdsToStages만으로 끝난다(암호화된 값을 반환하지 않는다).
#   - ecs:DescribeServices — handler는 호출하지 않는다. 재배포 제출 판정은 UpdateService
#     응답 자체로 끝나고, 별도 조회는 ECS 최종 일관성 때문에 오판을 만든다.
#   - dynamodb:UpdateItem — 조건부 PutItem과 GetItem 둘이면 충분하다.
# GetItem이 빠지면 첫 중복 이벤트가 AccessDenied로 끝나는데 그때까지 아무 증상도 없다.
data "aws_iam_policy_document" "rotation_redeploy" {
  statement {
    sid       = "ForceNewDeployment"
    actions   = ["ecs:UpdateService"]
    resources = [aws_ecs_service.app.arn]
  }

  statement {
    sid       = "CheckRotationLabel"
    actions   = ["secretsmanager:DescribeSecret"]
    resources = [local.rotation_secret_arn]
  }

  statement {
    sid       = "IdempotencyMarker"
    actions   = ["dynamodb:GetItem", "dynamodb:PutItem"]
    resources = [aws_dynamodb_table.rotation_idempotency.arn]
  }

  statement {
    sid       = "OnFailureDestination"
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.rotation_redeploy_failures.arn]
  }

  # 로그 그룹은 Terraform이 만드므로 CreateLogGroup은 주지 않는다.
  statement {
    sid       = "WriteLogs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.rotation_redeploy.arn}:*"]
  }
}

resource "aws_iam_role_policy" "rotation_redeploy" {
  name   = "${local.name_prefix}-rotation-redeploy"
  role   = aws_iam_role.rotation_redeploy.id
  policy = data.aws_iam_policy_document.rotation_redeploy.json
}

# timeout 기본값은 3초다. 그대로 두면 60초짜리 라벨 확인 bounded wait가 시작되자마자 매 호출이
# 강제 종료돼 UpdateService에 한 번도 도달하지 못하고 Step 1이 통째로 죽는다.
# 120초의 근거: handler 내부 deadline 105초(= 8 GetItem + 60 라벨확인 + 20 UpdateService
# + 8 PutItem + 9 여유) + 15초. 상한 합계가 timeout과 같으면 반환 전에 종료돼 로그가 안 남는다.
# 단발 구간 예산이 최악 실행시간(6/18/6)보다 큰 이유는 handler.py의 예산 상수 주석에 있다.
#
# reserved_concurrent_executions = 1 은 defense-in-depth다(동시 GetItem miss 창을 실질적으로
# 닫는다). 필수 게이트는 아니다 — marker 유일성은 조건부 쓰기가 보장하고, 예약이 불가능하면
# 동시 중복 재배포는 at-least-once 잔여 위험으로 수용한다(대가는 롤링 한 번).
# 2026-09-07 get-account-settings 확인: UnreservedConcurrentExecutions=400 이라 예약 후에도
# 399가 남아 하한 100을 넘긴다 -> 예약한다.
resource "aws_lambda_function" "rotation_redeploy" {
  function_name = "${local.name_prefix}-rotation-redeploy"
  description   = "Force a new ECS deployment after the RDS master secret rotates (AWSCURRENT moved)"
  role          = aws_iam_role.rotation_redeploy.arn

  filename         = data.archive_file.rotation_redeploy.output_path
  source_code_hash = data.archive_file.rotation_redeploy.output_base64sha256
  handler          = "handler.handler"
  runtime          = "python3.13"

  timeout                        = 120
  memory_size                    = 256
  reserved_concurrent_executions = 1

  # 이벤트는 시크릿 ARN·라벨·versionId만 준다 — ECS 대상은 없다. 값은 전부 non-secret이고
  # handler는 넷 중 하나라도 비면 호출 진입 시, 첫 AWS 호출 전에 실패한다(빈 cluster로 호출하면
  # default 클러스터를 보고 ServiceNotFoundException이 나 진단이 흐려진다).
  environment {
    variables = {
      ECS_CLUSTER_ARN        = aws_ecs_cluster.main.arn
      ECS_SERVICE_NAME       = aws_ecs_service.app.name
      IDEMPOTENCY_TABLE_NAME = aws_dynamodb_table.rotation_idempotency.name
      SECRET_ARN             = local.rotation_secret_arn
    }
  }

  # 로그 그룹을 Terraform이 만들기 전에 함수가 호출되면 Lambda가 같은 이름으로 자동 생성해
  # 이후 apply가 "already exists"로 깨진다.
  depends_on = [aws_cloudwatch_log_group.rotation_redeploy]

  tags = { Name = "${local.name_prefix}-rotation-redeploy" }
}

resource "aws_lambda_permission" "rotation_redeploy_events" {
  statement_id  = "AllowInvokeFromRotationRule"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.rotation_redeploy.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.rotation_label_updated.arn
}

# 계층 2의 재시도·보관 상한. 계층 1(EventBridge target)의 값과 같지만 서로 다른 계층의
# 독립 상한이라 최악은 합성된다 — end-to-end 상한은 약 2시간으로 본다(멱등 TTL 7일의 근거).
resource "aws_lambda_function_event_invoke_config" "rotation_redeploy" {
  function_name                = aws_lambda_function.rotation_redeploy.function_name
  maximum_retry_attempts       = 2
  maximum_event_age_in_seconds = 3600

  destination_config {
    on_failure {
      destination = aws_sqs_queue.rotation_redeploy_failures.arn
    }
  }
}

# =====================================================================
# 멱등 테이블 — 키는 이벤트 detail.versionId(모든 전달·재시도에서 같은 값).
# 시각 비교로 멱등을 구현하지 않는다: 호출마다 달라지는 값을 기준으로 삼으면 어떤 중복 억제도
# 성립하지 않는다. 회전 7일 1회 x (강한 일관성 읽기 1 + 조건부 쓰기 1)이라 온디맨드로 사실상 $0.
# 잔여 위험: 파티션 키가 versionId 하나라 "같은 버전에 AWSCURRENT를 다시 부여하는 운영 롤백"은
# 아이템이 실제로 삭제되기 전까지 중복으로 간주돼 skip된다. 이 plan 범위에는 그런 롤백이 없다.
# =====================================================================
resource "aws_dynamodb_table" "rotation_idempotency" {
  name         = "${local.name_prefix}-rotation-idempotency"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "versionId"

  attribute {
    name = "versionId"
    type = "S"
  }

  # 속성명을 handler의 PutItem 항목과 반드시 일치시킨다 — 다르면 TTL이 설정만 되고
  # 아무것도 만료되지 않는다. 삭제는 만료 즉시가 아니라 통상 수일 내이므로 보존 정리용이다.
  ttl {
    attribute_name = "expiresAt"
    enabled        = true
  }

  tags = { Name = "${local.name_prefix}-rotation-idempotency" }
}

# =====================================================================
# 알람 7개 — 자동 복구 경로가 조용히 죽는 것이 최악 실패 모드다.
# 전부 임계 > 0: 이 지표들은 정상 시 값이 없거나 0이고, 한 건이라도 나오면 그 자체가 신호다.
# 회전이 7일 1회라 노이즈가 없다. 희소 지표라 기본값이면 INSUFFICIENT_DATA에 앉으므로
# 7개 모두 treat_missing_data = notBreaching.
# alarm_actions/ok_actions는 기존 alarms 토픽(monitoring.tf)을 쓴다 — 새 토픽을 만들지 않는다.
# action이 없으면 상태만 바뀌고 Slack 카드도 수동 브릿지도 시작하지 않는다.
# =====================================================================

# 1. 광역 rule 전달 실패 — 이게 울리면 원문 적재 장치 자체가 죽은 것이다.
resource "aws_cloudwatch_metric_alarm" "secret_events_observe_failed" {
  alarm_name          = "${local.name_prefix}-secret-events-observe-failed"
  alarm_description   = "EventBridge failed to deliver observed Secrets Manager events to CloudWatch Logs"
  namespace           = "AWS/Events"
  metric_name         = "FailedInvocations"
  dimensions          = { RuleName = aws_cloudwatch_event_rule.secret_events_observe.name }
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-secret-events-observe-failed" }
}

# 2. 좁은 rule 전달 실패.
resource "aws_cloudwatch_metric_alarm" "rotation_rule_failed" {
  alarm_name          = "${local.name_prefix}-rotation-rule-failed"
  alarm_description   = "EventBridge failed to invoke the rotation redeploy Lambda"
  namespace           = "AWS/Events"
  metric_name         = "FailedInvocations"
  dimensions          = { RuleName = aws_cloudwatch_event_rule.rotation_label_updated.name }
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-rotation-rule-failed" }
}

# 3. handler 오류 — 라벨 확인 소진·UpdateService 실패·권한 오류가 전부 여기로 나온다.
resource "aws_cloudwatch_metric_alarm" "rotation_redeploy_errors" {
  alarm_name          = "${local.name_prefix}-rotation-redeploy-errors"
  alarm_description   = "Rotation redeploy Lambda returned an error (auto recovery did not complete)"
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = aws_lambda_function.rotation_redeploy.function_name }
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-rotation-redeploy-errors" }
}

# 4·5. "DLQ로 보내는 동작 자체가 실패한 경우"는 depth로 못 본다 — 그때 depth는 계속 0이다.
# 권한 오류가 바로 이 실패를 만든다.
resource "aws_cloudwatch_metric_alarm" "rotation_rule_dlq_send_failed" {
  alarm_name          = "${local.name_prefix}-rotation-rule-dlq-send-failed"
  alarm_description   = "EventBridge could not write a failed rotation event to its DLQ (evidence is being lost)"
  namespace           = "AWS/Events"
  metric_name         = "InvocationsFailedToBeSentToDlq"
  dimensions          = { RuleName = aws_cloudwatch_event_rule.rotation_label_updated.name }
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-rotation-rule-dlq-send-failed" }
}

resource "aws_cloudwatch_metric_alarm" "rotation_redeploy_destination_failed" {
  alarm_name          = "${local.name_prefix}-rotation-redeploy-destination-failed"
  alarm_description   = "Lambda could not deliver a failed invocation to its on-failure queue (evidence is being lost)"
  namespace           = "AWS/Lambda"
  metric_name         = "DestinationDeliveryFailures"
  dimensions          = { FunctionName = aws_lambda_function.rotation_redeploy.function_name }
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-rotation-redeploy-destination-failed" }
}

# 6·7. DLQ depth는 확인용이다 — SQS는 6시간 넘게 비활성이면 지표 전송을 중단하고, 비활성 큐가
# 활성화될 때 지표에 최대 15분 지연이 있다. 그래서 period=300이고 최대 15분 늦게 뜬다.
# 회전이 7일 1회라 두 큐는 정상 시 메시지도 API 접근도 없어 missing이면 영구
# INSUFFICIENT_DATA에 상주한다 -> notBreaching. 빠른 신호는 위 1~5(1분 지표)다.
resource "aws_cloudwatch_metric_alarm" "rotation_events_dlq_depth" {
  alarm_name          = "${local.name_prefix}-rotation-events-dlq-depth"
  alarm_description   = "Rotation event delivery DLQ is not empty (EventBridge could not reach the Lambda)"
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  dimensions          = { QueueName = aws_sqs_queue.rotation_events_dlq.name }
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-rotation-events-dlq-depth" }
}

resource "aws_cloudwatch_metric_alarm" "rotation_redeploy_failures_depth" {
  alarm_name          = "${local.name_prefix}-rotation-redeploy-failures-depth"
  alarm_description   = "Rotation redeploy on-failure queue is not empty (handler exhausted its retries)"
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  dimensions          = { QueueName = aws_sqs_queue.rotation_redeploy_failures.name }
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-rotation-redeploy-failures-depth" }
}
