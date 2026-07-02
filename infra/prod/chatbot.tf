# P3 관측성 — SNS 알람 통지를 Slack으로 보내는 AWS Chatbot(Amazon Q Developer in chat applications).
# Slack workspace OAuth 승인은 콘솔에서 사람이 1회 하고, 그 team/channel ID를 tfvars에 넣는다.
# 두 값이 다 있을 때만 role/attachment/config를 만든다(없으면 알람+SNS만 = 중간 마일스톤).
# Chatbot API는 4개 리전(us-east-2/us-west-2/ap-southeast-1/eu-west-1)에만 있다 — ap-northeast-2엔
# 엔드포인트가 없어(CLI 실측: 연결 실패) 기본 리전 생성 시 apply가 깨진다. 설정 데이터는 글로벌
# 저장소라 config 리소스에만 region=us-east-2를 명시한다(IAM은 글로벌, SNS는 ap-northeast-2 유지).

locals {
  # 공백만 있는 값은 "없음"으로 취급(trimspace). validation(variables.tf)이 한쪽만 채우는 실수를 막는다.
  slack_enabled = trimspace(var.slack_team_id) != "" && trimspace(var.slack_channel_id) != ""
}

# Chatbot이 assume하는 채널 role. 신뢰 주체는 chatbot.amazonaws.com(서비스링크드롤 아님).
resource "aws_iam_role" "chatbot" {
  count = local.slack_enabled ? 1 : 0
  name  = "${local.name_prefix}-chatbot"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "chatbot.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })

  tags = { Name = "${local.name_prefix}-chatbot" }
}

# 읽기 전용 권한(알람 카드 렌더 + Slack "Show logs" 지원). 단, 계정 전역 CloudWatch·Logs 읽기라
# 진짜 최소권한은 아니며, user_authorization_required=false와 결합해 채널 멤버 전원이 이 role을
# 공유한다 → 운영 전용 private 채널 유지가 전제. 스코프 축소는 P4(IAM 최소권한)에서 재검토.
resource "aws_iam_role_policy_attachment" "chatbot_cw_ro" {
  count      = local.slack_enabled ? 1 : 0
  role       = aws_iam_role.chatbot[0].name
  policy_arn = "arn:aws:iam::aws:policy/CloudWatchReadOnlyAccess"
}

# Slack 채널 설정. 인자명은 Terraform 기준(CFN의 UserRoleRequired/GuardrailPolicies가 아님).
# guardrail을 지정하지 않으면 기본이 AdministratorAccess라, 명시해서 상한을 CloudWatch 읽기로 낮춘다.
resource "aws_chatbot_slack_channel_configuration" "alarms" {
  count = local.slack_enabled ? 1 : 0
  # Chatbot 엔드포인트가 있는 리전으로 고정(기본 리전엔 API 없음 → 없으면 apply 실패).
  # 4개 지원 리전 중 어디든 동작(데이터 글로벌·통지 경로 무관) — 실측 검증된 us-east-2 선택.
  region             = "us-east-2"
  configuration_name = "${local.name_prefix}-slack"
  iam_role_arn       = aws_iam_role.chatbot[0].arn
  slack_team_id      = trimspace(var.slack_team_id)
  slack_channel_id   = trimspace(var.slack_channel_id)

  sns_topic_arns              = [aws_sns_topic.alarms.arn]
  guardrail_policy_arns       = ["arn:aws:iam::aws:policy/CloudWatchReadOnlyAccess"]
  user_authorization_required = false
  logging_level               = "ERROR"

  # role ARN만 참조하면 정책 부착 완료 전에 config가 생성될 수 있다(role ARN 존재 ≠ 정책 부착 완료).
  # 부착 완료 후 생성되도록 명시 — 권한 전파 타이밍·teardown 순서 안전.
  depends_on = [aws_iam_role_policy_attachment.chatbot_cw_ro]

  tags = { Name = "${local.name_prefix}-slack" }
}
