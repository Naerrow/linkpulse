resource "aws_ecs_cluster" "main" {
  name = "${local.name_prefix}-cluster"

  setting {
    name  = "containerInsights"
    value = var.enable_container_insights ? "enabled" : "disabled"
  }

  tags = { Name = "${local.name_prefix}-cluster" }
}

resource "aws_ecs_task_definition" "app" {
  family                   = "${local.name_prefix}-app"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.task_cpu
  memory                   = var.task_memory
  execution_role_arn       = aws_iam_role.ecs_execution.arn
  task_role_arn            = aws_iam_role.ecs_task.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = var.task_cpu_architecture
  }

  container_definitions = jsonencode([
    {
      name      = "app"
      image     = "${aws_ecr_repository.app.repository_url}:${var.image_tag}"
      essential = true

      portMappings = [
        { containerPort = 8080, protocol = "tcp" }
      ]

      # 비밀번호를 제외한 설정은 평문 env로. host/port/name/user는 RDS 속성에서 가져온다.
      environment = [
        { name = "APP_PORT", value = "8080" },
        { name = "LOG_LEVEL", value = var.log_level },
        { name = "PUBLIC_BASE_URL", value = "https://${var.domain_name}" },
        { name = "SHORT_CODE_LENGTH", value = tostring(var.short_code_length) },
        { name = "DB_HOST", value = aws_db_instance.main.address },
        { name = "DB_PORT", value = tostring(aws_db_instance.main.port) },
        { name = "DB_NAME", value = aws_db_instance.main.db_name },
        { name = "DB_USER", value = aws_db_instance.main.username },
        { name = "DB_SSLMODE", value = "require" },
      ]

      # 비밀번호만 Secrets Manager에서 주입(가드레일 #2). RDS 관리 시크릿의 password 키 참조.
      secrets = [
        {
          name      = "DB_PASSWORD"
          valueFrom = "${aws_db_instance.main.master_user_secret[0].secret_arn}:password::"
        }
      ]

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.app.name
          "awslogs-region"        = var.region
          "awslogs-stream-prefix" = "app"
        }
      }
    }
  ])

  tags = { Name = "${local.name_prefix}-app" }
}

resource "aws_ecs_service" "app" {
  name            = "${local.name_prefix}-app"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.app.arn
  desired_count   = var.service_desired_count
  launch_type     = "FARGATE"

  network_configuration {
    subnets          = aws_subnet.app[*].id
    security_groups  = [aws_security_group.app.id]
    assign_public_ip = false # 프라이빗 서브넷 + NAT
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.app.arn
    container_name   = "app"
    container_port   = 8080
  }

  # 배포 실패 시 자동 롤백.
  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  health_check_grace_period_seconds = 60

  # 타깃그룹이 리스너에 연결된 뒤 서비스를 생성한다.
  depends_on = [aws_lb_listener.https]

  # 앱 이미지 배포는 CI(P2)가 register-task-definition + update-service로 수행하므로,
  # service가 가리키는 task_definition 리비전 변경을 Terraform이 되돌리지 않게 한다.
  # 함정: Terraform으로 task definition(DB env 등)을 바꿔도 running 서비스엔 곧장 반영되지 않고,
  # 다음 CI 배포가 1회 돌아야 라이브에 적용된다(docs/adr/0001-cicd-terraform-ci-boundary.md 참고).
  lifecycle {
    ignore_changes = [task_definition]
  }

  tags = { Name = "${local.name_prefix}-app" }
}
