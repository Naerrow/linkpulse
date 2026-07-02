output "alb_dns_name" {
  description = "ALB 기본 DNS. 도메인 적용 전 임시 접근·디버깅용."
  value       = aws_lb.main.dns_name
}

output "app_url" {
  description = "서비스 공개 URL."
  value       = "https://${var.domain_name}"
}

output "ecr_repository_url" {
  description = "앱 이미지 push 대상 ECR URL."
  value       = aws_ecr_repository.app.repository_url
}

output "rds_address" {
  description = "RDS 엔드포인트 호스트(비밀번호는 Secrets Manager 보관)."
  value       = aws_db_instance.main.address
}

output "ecs_cluster_name" {
  description = "ECS 클러스터 이름."
  value       = aws_ecs_cluster.main.name
}

output "ecs_service_name" {
  description = "ECS 서비스 이름."
  value       = aws_ecs_service.app.name
}

output "cloudwatch_log_group" {
  description = "앱 로그 그룹."
  value       = aws_cloudwatch_log_group.app.name
}

# ---- P2 CI/CD용 (GitHub repo Variables에 등록) ----
output "github_actions_role_arn" {
  description = "GitHub Actions가 OIDC로 assume하는 배포 role ARN. GitHub repo Variable AWS_DEPLOY_ROLE_ARN."
  value       = aws_iam_role.github_actions_deploy.arn
}

output "ecs_task_definition_family" {
  description = "ECS task definition family. CI가 describe-task-definition base로 사용. GitHub Variable ECS_TASK_DEFINITION_FAMILY."
  value       = aws_ecs_task_definition.app.family
}

output "ecr_repository_name" {
  description = "ECR 리포지토리 이름(URL 아님). aws ecr describe-images --repository-name용. GitHub Variable ECR_REPOSITORY_NAME."
  value       = aws_ecr_repository.app.name
}

# ---- P3 관측성/알림 ----
output "sns_alarms_topic_arn" {
  description = "알람 통지 SNS 토픽 ARN. Chatbot/이메일 등 구독 연결용."
  value       = aws_sns_topic.alarms.arn
}

output "slack_alerts_enabled" {
  description = "Slack 통지 배선 설정 여부(slack_team_id/slack_channel_id 둘 다 설정 시 true). 실제 SNS 구독 존재는 list-subscriptions-by-topic로 확인. P3-1 완료 판정 보조."
  value       = local.slack_enabled
}
