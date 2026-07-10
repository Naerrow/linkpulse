# P4 배포 실패 알림 — ECS 롤링 배포 실패(서킷브레이커 롤백)를 EventBridge로 잡아
# 기존 SNS(alarms) -> AWS Chatbot -> Slack 경로로 흘린다. (docs/adr/0003-deploy-failure-alerts.md)
#
# 커버리지: ECS단 배포 실패(SERVICE_DEPLOYMENT_FAILED)만. GitHub Actions단 실패(빌드/테스트 실패,
# 러너 미획득)는 ECS까지 도달하지 않아 이 경로로 못 잡는다(ADR §커버리지 갭).
# data.aws_caller_identity.current 는 github_oidc.tf 에 선언돼 있어 재사용한다(재선언 금지).

# 필터: aws.ecs 의 "ECS Deployment State Change" 중 SERVICE_DEPLOYMENT_FAILED 이며,
# 대상 서비스 ARN(정확 매칭)인 것만. clusterArn은 이 이벤트 타입에 없으므로 쓰지 않고,
# 서비스 ARN은 top-level resources에서 매칭한다(fixtures/README.md 수기 대조).
# ARN은 수기 조립 대신 provider가 평가한 aws_ecs_service.app.arn을 직접 쓴다 — 파티션/포맷
# drift를 원천 차단하고 서비스에 대한 명시적 의존성이 생긴다. ECS long-ARN은 2021년부터 계정
# 강제라 이 값이 실이벤트 resources[0]와 동일 포맷이다. (실제 serviceArn 대조는 사람 preflight.)
resource "aws_cloudwatch_event_rule" "deploy_failed" {
  name        = "${local.name_prefix}-deploy-failed"
  description = "ECS rolling deploy failed (circuit breaker rollback) notify via SNS alarms to Slack"

  event_pattern = jsonencode({
    source        = ["aws.ecs"]
    "detail-type" = ["ECS Deployment State Change"]
    detail = {
      eventName = ["SERVICE_DEPLOYMENT_FAILED"]
    }
    resources = [aws_ecs_service.app.arn]
  })

  tags = { Name = "${local.name_prefix}-deploy-failed" }
}

# EventBridge가 assume해 SNS로 publish하는 실행 role.
# SNS 타깃은 IAM 실행 role(target role_arn)을 지원한다(eb-use-resource-based.html).
# 이 방식이 우월한 이유: SNS 토픽 정책엔 EventBridge용 Condition을 걸 수 없어(같은 문서)
# confused-deputy 방지를 토픽 정책으로는 못 한다. role trust엔 걸 수 있어 여기서 처리한다
# (cross-service-confused-deputy-prevention.html: SourceArn=규칙 ARN). 기존
# aws_sns_topic_policy.alarms(cloudwatch용)는 손대지 않는다.
resource "aws_iam_role" "deploy_events" {
  name = "${local.name_prefix}-deploy-events"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "events.amazonaws.com" }
      Action    = "sts:AssumeRole"
      Condition = {
        ArnLike      = { "aws:SourceArn" = aws_cloudwatch_event_rule.deploy_failed.arn }
        StringEquals = { "aws:SourceAccount" = data.aws_caller_identity.current.account_id }
      }
    }]
  })

  tags = { Name = "${local.name_prefix}-deploy-events" }
}

# 이 role은 alarms 토픽으로의 sns:Publish만 가진다(대상 ARN 한정 = 최소권한).
resource "aws_iam_role_policy" "deploy_events_publish" {
  name = "${local.name_prefix}-deploy-events-publish"
  role = aws_iam_role.deploy_events.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "sns:Publish"
      Resource = aws_sns_topic.alarms.arn
    }]
  })
}

# 타깃: alarms 토픽. input transformer로 Chatbot custom notification을 만든다.
# source는 반드시 리터럴 "custom"(이벤트 원본 aws.ecs를 넣으면 렌더 실패), version="1.0",
# content.description 필수. escaping 위험을 줄이려 description엔 reason 단독만 넣고
# 서비스 ARN/이벤트명/시각은 title로 분리한다(reason 특수문자는 6단계 실발화로 확인).
resource "aws_cloudwatch_event_target" "deploy_failed_sns" {
  rule     = aws_cloudwatch_event_rule.deploy_failed.name
  arn      = aws_sns_topic.alarms.arn
  role_arn = aws_iam_role.deploy_events.arn

  # role ARN 참조만으론 publish 정책 부착 전에 target이 생길 수 있다(재생성/롤백 후 순서 안전).
  # chatbot.tf와 같은 패턴 — 정책 부착 완료 후 target 생성. (meta-argument라 plan diff 없음.)
  depends_on = [aws_iam_role_policy.deploy_events_publish]

  input_transformer {
    input_paths = {
      svc    = "$.resources[0]"
      ev     = "$.detail.eventName"
      reason = "$.detail.reason"
      t      = "$.time"
    }

    # 한 줄 리터럴 JSON — 개행 삽입으로 template이 깨질 여지를 줄인다.
    input_template = "{ \"version\": \"1.0\", \"source\": \"custom\", \"content\": { \"title\": \"ECS deploy failed <ev> on <svc> at <t>\", \"description\": \"<reason>\" } }"
  }
}
