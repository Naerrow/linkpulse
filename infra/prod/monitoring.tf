# P3 관측성 — CloudWatch 알람 + SNS(통지 팬아웃). Slack 배선은 chatbot.tf.
# 감지 경로: CloudWatch Alarm -> SNS topic -> (AWS Chatbot) -> Slack.
# data.aws_caller_identity.current 는 github_oidc.tf 에 이미 선언돼 있어 재사용한다(재선언 금지).
# 임계값은 "초기값"이다 — 실트래픽을 보며 조정한다(ADR 0002 참고).

locals {
  # 스토리지 임계값은 할당량에서 계산한다(db_allocated_storage를 바꿔도 비례 유지).
  rds_free_storage_bytes_low = var.db_allocated_storage * 1024 * 1024 * 1024 / 10 # 할당량의 10%(20GB→2GB)
  rds_freeable_mem_bytes_low = 100 * 1024 * 1024                                  # 100MB(t4g.micro 1GB)
}

# ---- SNS: 알람 통지 팬아웃 ----
# SSE(KMS) 미사용: 알람 메시지에 민감정보 없음 + 암호화 시 KMS 키정책에 CloudWatch 사용권한을
# 따로 열어야 하는 함정 회피. (필요하면 P4에서 CMK로 전환.)
resource "aws_sns_topic" "alarms" {
  name = "${local.name_prefix}-alarms"
  tags = { Name = "${local.name_prefix}-alarms" }
}

# CloudWatch 알람만 이 토픽에 publish 허용 + confused-deputy 방지(SourceArn/SourceAccount).
resource "aws_sns_topic_policy" "alarms" {
  arn = aws_sns_topic.alarms.arn

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowCloudWatchAlarmsPublish"
      Effect    = "Allow"
      Principal = { Service = "cloudwatch.amazonaws.com" }
      Action    = "sns:Publish"
      Resource  = aws_sns_topic.alarms.arn
      Condition = {
        ArnLike      = { "aws:SourceArn" = "arn:aws:cloudwatch:${var.region}:${data.aws_caller_identity.current.account_id}:alarm:*" }
        StringEquals = { "aws:SourceAccount" = data.aws_caller_identity.current.account_id }
      }
    }]
  })
}

# =====================================================================
# ALB 알람 — dimension을 지표별로 정확히 맞춘다.
#   target/health/latency 지표: LoadBalancer + TargetGroup
#   ELB단 5xx: LoadBalancer 만 (TargetGroup을 붙이면 데이터가 없어 영구 INSUFFICIENT_DATA)
# =====================================================================

# 앱(타깃)이 낸 5xx.
resource "aws_cloudwatch_metric_alarm" "alb_target_5xx" {
  alarm_name        = "${local.name_prefix}-alb-target-5xx"
  alarm_description = "ALB target (app) 5xx count >= 5 in 5min"
  namespace         = "AWS/ApplicationELB"
  metric_name       = "HTTPCode_Target_5XX_Count"
  dimensions = {
    LoadBalancer = aws_lb.main.arn_suffix
    TargetGroup  = aws_lb_target_group.app.arn_suffix
  }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 5
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching" # 무트래픽=정상
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-alb-target-5xx" }
}

# ALB 자신이 낸 5xx(502/503/504 등 — 예: 정상 타깃 없음).
resource "aws_cloudwatch_metric_alarm" "alb_elb_5xx" {
  alarm_name          = "${local.name_prefix}-alb-elb-5xx"
  alarm_description   = "ALB-generated 5xx (502/503/504) count >= 5 in 5min"
  namespace           = "AWS/ApplicationELB"
  metric_name         = "HTTPCode_ELB_5XX_Count"
  dimensions          = { LoadBalancer = aws_lb.main.arn_suffix } # LB 만
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 5
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-alb-elb-5xx" }
}

# 헬스체크 실패 타깃 존재.
resource "aws_cloudwatch_metric_alarm" "alb_unhealthy_hosts" {
  alarm_name        = "${local.name_prefix}-alb-unhealthy-hosts"
  alarm_description = "ALB target group has >= 1 unhealthy host for 2min"
  namespace         = "AWS/ApplicationELB"
  metric_name       = "UnHealthyHostCount"
  dimensions = {
    LoadBalancer = aws_lb.main.arn_suffix
    TargetGroup  = aws_lb_target_group.app.arn_suffix
  }
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-alb-unhealthy-hosts" }
}

# p95 응답시간. 저트래픽 샘플부족으로 튀는 것 방지(evaluate_low_sample_count_percentiles).
resource "aws_cloudwatch_metric_alarm" "alb_latency_p95" {
  alarm_name        = "${local.name_prefix}-alb-latency-p95"
  alarm_description = "ALB target p95 response time >= 1s for 3min"
  namespace         = "AWS/ApplicationELB"
  metric_name       = "TargetResponseTime"
  dimensions = {
    LoadBalancer = aws_lb.main.arn_suffix
    TargetGroup  = aws_lb_target_group.app.arn_suffix
  }
  extended_statistic                    = "p95"
  evaluate_low_sample_count_percentiles = "ignore"
  period                                = 60
  evaluation_periods                    = 3
  threshold                             = 1 # 초(second)
  comparison_operator                   = "GreaterThanOrEqualToThreshold"
  treat_missing_data                    = "notBreaching"
  alarm_actions                         = [aws_sns_topic.alarms.arn]
  ok_actions                            = [aws_sns_topic.alarms.arn]
  tags                                  = { Name = "${local.name_prefix}-alb-latency-p95" }
}

# 정상 타깃 0 = 서비스 다운. 무트래픽에서 5xx/unhealthy가 침묵하는 사각지대를 차단한다.
# 데이터가 없으면(타깃 전부 deregister) breaching으로 간주한다.
resource "aws_cloudwatch_metric_alarm" "alb_no_healthy_hosts" {
  alarm_name        = "${local.name_prefix}-alb-no-healthy-hosts"
  alarm_description = "ALB target group has no healthy hosts (service down) for 3min"
  namespace         = "AWS/ApplicationELB"
  metric_name       = "HealthyHostCount"
  dimensions = {
    LoadBalancer = aws_lb.main.arn_suffix
    TargetGroup  = aws_lb_target_group.app.arn_suffix
  }
  statistic           = "Average"
  period              = 60
  evaluation_periods  = 3
  threshold           = 1
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-alb-no-healthy-hosts" }
}

# =====================================================================
# ECS 알람 — CPU/Memory는 AWS/ECS(Insights 불필요), RunningTaskCount는 ECS/ContainerInsights.
# =====================================================================

resource "aws_cloudwatch_metric_alarm" "ecs_cpu_high" {
  alarm_name        = "${local.name_prefix}-ecs-cpu-high"
  alarm_description = "ECS service CPU utilization >= 80% for 3min"
  namespace         = "AWS/ECS"
  metric_name       = "CPUUtilization"
  dimensions = {
    ClusterName = aws_ecs_cluster.main.name
    ServiceName = aws_ecs_service.app.name
  }
  statistic           = "Average"
  period              = 60
  evaluation_periods  = 3
  threshold           = 80
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "missing"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-ecs-cpu-high" }
}

resource "aws_cloudwatch_metric_alarm" "ecs_memory_high" {
  alarm_name        = "${local.name_prefix}-ecs-memory-high"
  alarm_description = "ECS service memory utilization >= 80% for 3min"
  namespace         = "AWS/ECS"
  metric_name       = "MemoryUtilization"
  dimensions = {
    ClusterName = aws_ecs_cluster.main.name
    ServiceName = aws_ecs_service.app.name
  }
  statistic           = "Average"
  period              = 60
  evaluation_periods  = 3
  threshold           = 80
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "missing"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-ecs-memory-high" }
}

# 실행 태스크 수가 목표(desired)보다 적음. 임계값은 소스오브트루스(변수)를 직접 참조한다.
# Container Insights 필요. 활성 직후 초기 공백은 INSUFFICIENT_DATA(정상).
resource "aws_cloudwatch_metric_alarm" "ecs_running_tasks_low" {
  alarm_name        = "${local.name_prefix}-ecs-running-tasks-low"
  alarm_description = "ECS running task count below desired for 3min (needs Container Insights)"
  namespace         = "ECS/ContainerInsights"
  metric_name       = "RunningTaskCount"
  dimensions = {
    ClusterName = aws_ecs_cluster.main.name
    ServiceName = aws_ecs_service.app.name
  }
  statistic           = "Average"
  period              = 60
  evaluation_periods  = 3
  threshold           = var.service_desired_count
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "missing"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-ecs-running-tasks-low" }
}

# =====================================================================
# RDS 알람 — dimension은 DBInstanceIdentifier.
# =====================================================================

resource "aws_cloudwatch_metric_alarm" "rds_cpu_high" {
  alarm_name          = "${local.name_prefix}-rds-cpu-high"
  alarm_description   = "RDS CPU utilization >= 80% for 15min"
  namespace           = "AWS/RDS"
  metric_name         = "CPUUtilization"
  dimensions          = { DBInstanceIdentifier = aws_db_instance.main.identifier }
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 3
  threshold           = 80
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "missing"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-rds-cpu-high" }
}

# 스토리지 소진은 고위험·느린 진행 → 데이터 없으면 breaching으로 간주.
resource "aws_cloudwatch_metric_alarm" "rds_free_storage_low" {
  alarm_name          = "${local.name_prefix}-rds-free-storage-low"
  alarm_description   = "RDS free storage <= 10% of allocated"
  namespace           = "AWS/RDS"
  metric_name         = "FreeStorageSpace"
  dimensions          = { DBInstanceIdentifier = aws_db_instance.main.identifier }
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 1
  threshold           = local.rds_free_storage_bytes_low
  comparison_operator = "LessThanOrEqualToThreshold"
  treat_missing_data  = "breaching"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-rds-free-storage-low" }
}

# FreeableMemory는 워크로드에 따라 상시 낮을 수 있어 초기값 100MB. 만성 발화 시 하향/SwapUsage 병행.
resource "aws_cloudwatch_metric_alarm" "rds_freeable_memory_low" {
  alarm_name          = "${local.name_prefix}-rds-freeable-memory-low"
  alarm_description   = "RDS freeable memory <= 100MB for 15min (tune after observing)"
  namespace           = "AWS/RDS"
  metric_name         = "FreeableMemory"
  dimensions          = { DBInstanceIdentifier = aws_db_instance.main.identifier }
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 3
  threshold           = local.rds_freeable_mem_bytes_low
  comparison_operator = "LessThanOrEqualToThreshold"
  treat_missing_data  = "missing"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-rds-freeable-memory-low" }
}

# 연결 수 상한 근접. t4g.micro 기본 max_connections는 대략 112(파라미터그룹서 실값 확인).
resource "aws_cloudwatch_metric_alarm" "rds_connections_high" {
  alarm_name          = "${local.name_prefix}-rds-connections-high"
  alarm_description   = "RDS connections >= 80 (t4g.micro default max ~112)"
  namespace           = "AWS/RDS"
  metric_name         = "DatabaseConnections"
  dimensions          = { DBInstanceIdentifier = aws_db_instance.main.identifier }
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 2
  threshold           = 80
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "missing"
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  tags                = { Name = "${local.name_prefix}-rds-connections-high" }
}
