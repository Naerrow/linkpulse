# RDS 마스터 비밀번호 시크릿의 이벤트 관찰 (plan 0009에서 시작, plan 0014에서 이 파일로 정리)
#
# plan 0009 Step 1(회전 때 ECS 강제 재배포 Lambda)은 plan 0014에서 지웠다. ECS가 없어졌고,
# Step 2(앱이 인증 실패 때 비밀번호를 다시 읽음)가 10-07부터 라이브로 회전을 스스로 견딘다
# (온디맨드 회전 실측: 503 약 61초 뒤 자동 회복). EKS에서의 확인은 측정 창의 회전 드릴이다(ADR 0007).
# 남긴 것은 관찰 장치뿐이다 — 회전 시각의 원문 로그와, 그 적재가 죽었을 때의 알람.

locals {
  # RDS가 관리하는 마스터 비밀번호 시크릿. 앱 파드 역할(iam.tf)과 사이트 ConfigMap(infra/k8s)이 같은 값을 쓴다.
  rotation_secret_arn = aws_db_instance.main.master_user_secret[0].secret_arn
}

# =====================================================================
# 광역 관찰 rule — detail 필터 없이 대상 시크릿의 모든 Secrets Manager 이벤트를 CloudWatch Logs에 적재한다.
# 회전이 언제 시작·끝났는지(AWSCURRENT 이동 시각)를 앱 로그의 auth_failed_observed·credential_recovered와
# 맞춰 보는 원자료다(회전 드릴·장애 회고).
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
# SNS 타깃의 role_arn 패턴을 여기 복사하면 안 된다). 필요한 것은 로그 그룹의
# resource-based policy이고, AWS 공식 예제 형태를 그대로 쓴다:
#   principal 둘(events / delivery.logs), action 둘(CreateLogStream / PutLogEvents),
#   resource는 stream 범위(:*)까지. 하나라도 좁으면 첫 실이벤트가 FailedInvocations로 끝난다.
# aws_cloudwatch_log_group.arn 은 provider가 :* 접미사를 떼고 주므로 여기서 다시 붙인다.
#
# confused deputy 경계: 로그 그룹 ARN 한정은 "어디에 쓰는가"만 좁히고 "누구를 대신해 호출하는가"는
# 묶지 않는다. AWS의 ECS lifecycle events 예제가 **같은 두 principal에 aws:SourceAccount와
# rule ARN의 aws:SourceArn을 함께** 걸므로(round-1 codex-ide#2로 1차 출처 확인), 그 형태를 따른다.
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

# rule과 target은 별도 리소스다. 빠뜨리면 describe-rule과
# resource policy 검사는 전부 통과하는데 target이 0개라 원문을 한 건도 얻지 못한다.
resource "aws_cloudwatch_event_target" "secret_events_logs" {
  rule = aws_cloudwatch_event_rule.secret_events_observe.name
  arn  = aws_cloudwatch_log_group.secret_events.arn

  depends_on = [aws_cloudwatch_log_resource_policy.secret_events]
}

# 관찰 rule 전달 실패 — 이게 울리면 원문 적재 장치 자체가 죽은 것이다.
# 정상 시 값이 없거나 0인 희소 지표라 notBreaching. 알람은 기존 alarms 토픽(monitoring.tf)으로 간다.
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
