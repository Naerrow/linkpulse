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
