data "aws_iam_policy_document" "ecs_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

# 실행 역할(execution role): ECS 에이전트가 이미지 pull·로그 전송·시크릿 주입에 쓴다.
resource "aws_iam_role" "ecs_execution" {
  name               = "${local.name_prefix}-ecs-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
}

# 관리형 정책: ECR pull + CloudWatch Logs put.
resource "aws_iam_role_policy_attachment" "ecs_execution_managed" {
  role       = aws_iam_role.ecs_execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# DB 비밀번호 시크릿 읽기 권한(해당 시크릿 ARN으로만 한정).
# RDS 관리 시크릿은 AWS 관리형 KMS 키(aws/secretsmanager)로 암호화되며, secretsmanager
# 권한이 있는 동일 리전 주체의 복호화를 허용하므로 별도 kms 정책은 필요 없다.
data "aws_iam_policy_document" "ecs_execution_secrets" {
  statement {
    sid       = "ReadDBPassword"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_db_instance.main.master_user_secret[0].secret_arn]
  }
}

resource "aws_iam_role_policy" "ecs_execution_secrets" {
  name   = "read-db-secret"
  role   = aws_iam_role.ecs_execution.id
  policy = data.aws_iam_policy_document.ecs_execution_secrets.json
}

# 태스크 역할(task role): 앱이 런타임에 호출하는 AWS API용. 현재 앱은 AWS SDK를
# 쓰지 않으므로 권한 없는 빈 역할이다(향후 필요 시 정책을 붙인다).
resource "aws_iam_role" "ecs_task" {
  name               = "${local.name_prefix}-ecs-task"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
}
