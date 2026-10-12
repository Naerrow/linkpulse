# plan 0013: 리다이렉트의 클릭 배치를 나르는 큐.
# 앱 태스크가 모은 클릭을 1초마다 배치로 보내고(태스크당 초당 메시지 1개 이하, 빈 배치는 보내지 않는다),
# 같은 태스크 안의 워커가 받아 DB에 묶어 반영한다. 중복 반영은 앱이 배치 ID로 거른다(click_batches).
#
# 표준 큐를 쓴다. FIFO는 순서가 필요 없는 여기서 처리량 한도만 더할 뿐이고, 최소 한 번 전달의 중복은 앱이 거른다.
resource "aws_sqs_queue" "clicks" {
  name = "${local.name_prefix}-clicks"
  # 워커가 반영에 실패하면(Ack 없음) 30초 뒤 다시 받는다. 정상 반영은 1초 안쪽이다.
  visibility_timeout_seconds = 30
  # 4일(SQS 기본값). DB 장애 때 자동으로 버티는 시간은 이것이 아니라 가시성 30초 × 재전달 5회 ≈ 2.5분이다.
  # 그 뒤에는 DLQ로 옮겨지고 사람이 재처리한다(docs/runbooks/alarm-response.md).
  message_retention_seconds = 345600
  # 롱폴링. 앱도 20초로 요청하지만 큐 기본값으로도 둔다 — 메시지가 없을 때 빈 요청 수를 줄인다.
  receive_wait_time_seconds = 20
  sqs_managed_sse_enabled   = true

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.clicks_dlq.arn
    maxReceiveCount     = 5
  })

  tags = { Name = "${local.name_prefix}-clicks" }
}

# 반영에 5번 실패했거나 본문이 깨진 배치가 모인다.
# 보존 14일: 표준 큐는 DLQ로 옮겨도 처음 넣은 시각 기준으로 만료되므로 본 큐(4일)보다 길게 둔다(AWS 권고).
# 앱의 click_batches 보존(21일)은 이 14일 + 재처리 뒤 본 큐 4일을 덮도록 잡았다(app/internal/clicks/worker.go).
resource "aws_sqs_queue" "clicks_dlq" {
  name                      = "${local.name_prefix}-clicks-dlq"
  message_retention_seconds = 1209600
  sqs_managed_sse_enabled   = true

  tags = { Name = "${local.name_prefix}-clicks-dlq" }
}

# 이 DLQ는 클릭 큐 전용이다. 다른 큐가 실수로 이 DLQ를 가리키지 못하게 막는다.
resource "aws_sqs_queue_redrive_allow_policy" "clicks_dlq" {
  queue_url = aws_sqs_queue.clicks_dlq.id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.clicks.arn]
  })
}

# DLQ에 하나라도 들어오면 사람이 봐야 한다 — 그 배치의 클릭은 아직 DB에 없다.
resource "aws_cloudwatch_metric_alarm" "clicks_dlq_depth" {
  alarm_name          = "${local.name_prefix}-clicks-dlq-depth"
  alarm_description   = "Click batch DLQ is not empty (a batch failed to apply 5 times or could not be decoded)"
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  dimensions          = { QueueName = aws_sqs_queue.clicks_dlq.name }
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-clicks-dlq-depth" }
}

# 가장 오래된 메시지가 5분을 넘으면 워커가 멈춘 것이다(정상은 수 초 안에 비워진다).
# 메시지가 없을 때는 지표가 비므로 notBreaching이다. 3번 넘게 받고도 지워지지 않은 메시지는 SQS가 이 지표에서
# 빼므로(표준 큐, maxReceiveCount > 3), 깨진 메시지는 이 알람이 아니라 위 DLQ 알람으로 잡힌다.
resource "aws_cloudwatch_metric_alarm" "clicks_oldest_age" {
  alarm_name          = "${local.name_prefix}-clicks-oldest-age"
  alarm_description   = "Oldest click batch has waited over 5 minutes (click worker stalled)"
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateAgeOfOldestMessage"
  dimensions          = { QueueName = aws_sqs_queue.clicks.name }
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 300
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-clicks-oldest-age" }
}
