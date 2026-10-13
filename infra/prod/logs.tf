# 앱 로그 그룹. EKS에서는 Fluent Bit(DaemonSet, infra/k8s)가 앱 파드의 stdout JSON을 여기로 보낸다(plan 0014).
# ECS 시절 그룹(/ecs/linkpulse-prod-app)은 destroy와 함께 사라졌다. 0012·0013 원자료는 load/k6/results/raw에 있다.
resource "aws_cloudwatch_log_group" "app" {
  name              = "/eks/${local.name_prefix}/app"
  retention_in_days = var.log_retention_days
  tags              = { Name = "${local.name_prefix}-app-logs" }
}
