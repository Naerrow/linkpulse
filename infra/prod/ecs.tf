resource "aws_ecs_cluster" "main" {
  name = "${local.name_prefix}-cluster"

  setting {
    name  = "containerInsights"
    value = var.enable_container_insights ? "enabled" : "disabled"
  }

  tags = { Name = "${local.name_prefix}-cluster" }
}

# skip_destroy: 새 revision을 등록할 때 이전 revision을 INACTIVE로 만들지 않는다.
# Step 2가 task definition에 env를 추가하면 replacement가 일어나는데, 그때 이전 revision이
# INACTIVE가 되면 롤백 대상이 사라진다(직전 ACTIVE revision을 다시 가리키는 것이 유일한 백스톱이다 —
# Step 1의 force-new-deployment는 같은 이미지를 다시 띄울 뿐이라 코드 결함을 되돌리지 못한다).
resource "aws_ecs_task_definition" "app" {
  family                   = "${local.name_prefix}-app"
  skip_destroy             = true
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
      environment = concat([
        { name = "APP_PORT", value = "8080" },
        # 운영 모드: DB 미설정 시 인메모리 폴백을 막고 기동을 중단시킨다(app/internal/config).
        { name = "APP_ENV", value = "production" },
        { name = "LOG_LEVEL", value = var.log_level },
        { name = "PUBLIC_BASE_URL", value = "https://${var.domain_name}" },
        { name = "SHORT_CODE_LENGTH", value = tostring(var.short_code_length) },
        { name = "DB_HOST", value = aws_db_instance.main.address },
        { name = "DB_PORT", value = tostring(aws_db_instance.main.port) },
        { name = "DB_NAME", value = aws_db_instance.main.db_name },
        { name = "DB_USER", value = aws_db_instance.main.username },
        { name = "DB_SSLMODE", value = "require" },

        # P4(d)-2 Step 2 (plan 0009 2-2b). 둘 다 비밀이 아니다.
        # DB_SECRET_ARN: 앱이 회전 후 현재 비밀번호를 다시 읽을 대상. SDK 리전의 source of
        #   truth이기도 하다 — ECS Fargate는 Lambda와 달리 AWS_REGION을 자동 주입하지 않고
        #   SDK v2에는 기본 리전이 없어서, ARN을 파싱해 얻은 리전을 WithRegion에 명시한다.
        # AWS_REGION: SDK를 동작시키는 값이 아니라 배포 시점 교차검사다. task definition이
        #   의도한 리전으로 만들어졌는지를 배포 전에 거른다(2-5의 게이트가 이 둘을 본다).
        #   ARN의 리전과 다르면 앱이 기동 오류로 막는다.
        { name = "DB_SECRET_ARN", value = local.rotation_secret_arn },
        { name = "AWS_REGION", value = var.region },
        ],
        # plan 0011: 관리자 토큰의 SHA-256 해시. 토큰 원문이 아니라 해시라서 평문 env로 둔다 —
        #   토큰이 256비트 난수라 해시로 역산할 수 없고, 그래서 Secrets Manager가 필요 없다(비용 0).
        #   sensitive로 두지 않는 이유: 그러면 container_definitions 전체가 plan에서 가려져 env diff를 검토할 수 없다.
        #   값이 없으면 env 자체를 넣지 않는다(빈 문자열 env는 ECS 응답에서 빠져 매 plan마다 교체가 뜰 수 있다).
        #   그 경우 앱은 관리자 기능(링크 생성·요청 승인)을 끈다(fail-closed).
        var.admin_token_sha256 == "" ? [] : [
          { name = "ADMIN_TOKEN_SHA256", value = lower(var.admin_token_sha256) },
      ])

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
