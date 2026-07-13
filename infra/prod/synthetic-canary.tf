# P4(c) 무트래픽 방어선 canary — Route53 헬스체크로 lpulse.live/healthz를 외부에서 상시 프로빙한다.
# 두 가지를 동시에 해결한다:
#   (a) 실사용 트래픽 0인 시간대에도 ALB로 실제 요청이 흘러(무트래픽 다운이면 ALB가 503 → 기존
#       alb-elb-5xx가 무트래픽에서도 발화), (b) 헬스체크 자신의 HealthCheckStatus 지표(1=정상/0=다운)로
#       트래픽·5xx 카운트와 무관한 결정론적 liveness 신호를 준다. 배경·트레이드오프는 docs/adr/0004.
#
# 크로스리전 함정: Route53 헬스체크의 CloudWatch 지표는 us-east-1에만 발행된다(글로벌 서비스 규약).
# 그래서 알람은 provider=aws.use1(us-east-1)로 만들고, CloudWatch 알람 액션은 자기 리전의 SNS로만
# 갈 수 있어 us-east-1 전용 토픽을 둔다. 이 토픽을 기존 Chatbot config(chatbot.tf)에 크로스리전으로
# 추가 바인딩해 같은 Slack 채널로 통지한다(ADR 0002가 검증한 패턴). 헬스체크 리소스 자체는 글로벌이라
# 기존 route53_acm.tf와 동일하게 기본 provider(ap-northeast-2)로 만든다.
# data.aws_caller_identity.current 는 github_oidc.tf 에 선언돼 있어 재사용한다(재선언 금지).

# ---- us-east-1 전용 SNS 토픽 (canary 알람 통지 팬아웃) ----
# 기본 provider(ap-northeast-2)로 만들면 us-east-1 토픽 ARN에 ap-northeast-2 API로 접근해 apply가
# 깨지고, validate는 provider·리전·ARN 불일치를 못 잡는다 → 두 리소스 모두 provider=aws.use1 명시.
resource "aws_sns_topic" "canary_use1" {
  provider = aws.use1
  name     = "${local.name_prefix}-canary-alarms"
  tags     = { Name = "${local.name_prefix}-canary-alarms" }
}

# CloudWatch 알람만 이 토픽에 publish 허용 + confused-deputy 방지(SourceArn/SourceAccount).
# monitoring.tf의 aws_sns_topic_policy.alarms 미러 — 단 SourceArn 리전 리터럴이 us-east-1이다
# (알람이 us-east-1에 있으므로). 계정 ID는 글로벌이라 기본 provider의 caller_identity를 그대로 쓴다.
resource "aws_sns_topic_policy" "canary_use1" {
  provider = aws.use1
  arn      = aws_sns_topic.canary_use1.arn

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowCloudWatchAlarmsPublish"
      Effect    = "Allow"
      Principal = { Service = "cloudwatch.amazonaws.com" }
      Action    = "sns:Publish"
      Resource  = aws_sns_topic.canary_use1.arn
      Condition = {
        ArnLike      = { "aws:SourceArn" = "arn:aws:cloudwatch:us-east-1:${data.aws_caller_identity.current.account_id}:alarm:*" }
        StringEquals = { "aws:SourceAccount" = data.aws_caller_identity.current.account_id }
      }
    }]
  })
}

# ---- Route53 헬스체크 (상시 synthetic canary) ----
# HTTPS로 lpulse.live/healthz를 30초 주기 프로빙. string-match/measure_latency 미사용(비용 — 각 +$1/월):
# string match 없이 Route53는 2xx/3xx 응답을 healthy로 판정하고(/healthz=200이라 정상), 3회 연속 실패 시
# unhealthy로 전환한다. enable_sni=true는 ALB가 SNI로 인증서를 고르므로 필수. 리소스는 글로벌이라
# 기본 provider로 만든다("Route 53 is a global service, so you don't specify the region" — ADR 0004).
# ALB SG는 443을 0.0.0.0/0에 개방(security_groups.tf)해 글로벌 헬스체커가 도달 가능 — SG 변경 불요.
resource "aws_route53_health_check" "canary" {
  type              = "HTTPS"
  fqdn              = var.domain_name
  port              = 443
  resource_path     = "/healthz"
  enable_sni        = true
  request_interval  = 30
  failure_threshold = 3

  tags = { Name = "${local.name_prefix}-canary" }
}

# ---- HealthCheckStatus 알람 (결정론적 무트래픽 down 신호) ----
# HealthCheckStatus는 1=정상/0=다운(AWS/Route53, 차원 HealthCheckId, 통계 Minimum 유효 — ADR 0004).
# Minimum<1을 3분(60s×3) 보면 ALARM. 헬스체크 존재 시 지표는 상시 발행되나, 헬스체크 삭제/일시 공백을
# 안전하게 down으로 보려 treat_missing_data=breaching(최초 발행 전 공백의 오탐 레이스는 리스크·preflight로
# 관리 — plan 리스크 절). alarm_actions·ok_actions 둘 다 us-east-1 토픽(기존 12알람과 동일: 상태 전이 통지 →
# 복구 시 OK 카드). provider=aws.use1 필수(지표가 us-east-1에만 있음). 헬스체크 id는 글로벌이라 크로스
# provider 참조가 정상이다.
resource "aws_cloudwatch_metric_alarm" "canary_down" {
  provider          = aws.use1
  alarm_name        = "${local.name_prefix}-canary-down"
  alarm_description = "Route53 health check for ${var.domain_name}/healthz is DOWN for 3min (no-traffic liveness backstop)"
  namespace         = "AWS/Route53"
  metric_name       = "HealthCheckStatus"
  dimensions        = { HealthCheckId = aws_route53_health_check.canary.id }

  statistic           = "Minimum"
  period              = 60
  evaluation_periods  = 3
  threshold           = 1
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching"
  alarm_actions       = [aws_sns_topic.canary_use1.arn]
  ok_actions          = [aws_sns_topic.canary_use1.arn]
  tags                = { Name = "${local.name_prefix}-canary-down" }
}
