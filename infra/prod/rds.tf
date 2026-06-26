resource "aws_db_subnet_group" "main" {
  name       = "${local.name_prefix}-db"
  subnet_ids = aws_subnet.data[*].id
  tags       = { Name = "${local.name_prefix}-db-subnet-group" }
}

# TLS 강제(앱의 sslmode=require와 정합). force_ssl=1이면 비암호화 접속을 거부한다.
resource "aws_db_parameter_group" "main" {
  name_prefix = "${local.name_prefix}-pg${local.postgres_major}-"
  family      = "postgres${local.postgres_major}"

  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }

  lifecycle {
    create_before_destroy = true
  }

  tags = { Name = "${local.name_prefix}-pg16" }
}

resource "aws_db_instance" "main" {
  identifier     = "${local.name_prefix}-pg"
  engine         = "postgres"
  engine_version = var.postgres_version
  instance_class = var.db_instance_class

  allocated_storage     = var.db_allocated_storage
  max_allocated_storage = var.db_max_allocated_storage > 0 ? var.db_max_allocated_storage : null
  storage_type          = "gp3"
  storage_encrypted     = true

  db_name  = var.db_name
  username = var.db_username
  # 비밀번호는 RDS가 생성·관리하고 Secrets Manager에 보관한다(코드/state에 평문 미저장).
  manage_master_user_password = true

  multi_az               = false # 균형: Single-AZ. HA 필요 시 true(P4 후보).
  db_subnet_group_name   = aws_db_subnet_group.main.name
  vpc_security_group_ids = [aws_security_group.data.id]
  parameter_group_name   = aws_db_parameter_group.main.name
  publicly_accessible    = false

  backup_retention_period = var.db_backup_retention
  copy_tags_to_snapshot   = true
  deletion_protection     = var.db_deletion_protection
  skip_final_snapshot     = var.db_skip_final_snapshot
  # skip_final_snapshot=false면 최종 스냅샷 이름이 필요하다(이름 충돌 시 철거 실패 → 의도된 보호장치).
  final_snapshot_identifier = var.db_skip_final_snapshot ? null : "${local.name_prefix}-pg-final"

  auto_minor_version_upgrade = true
  apply_immediately          = false

  tags = { Name = "${local.name_prefix}-pg" }
}
