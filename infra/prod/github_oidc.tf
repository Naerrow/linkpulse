# GitHub Actions가 OIDC로 AWS에 접근해 ECR push + ECS 배포를 수행하기 위한 IAM(P2 CI/CD).
# 장기 액세스 키 없이 GitHub가 발급한 단기 OIDC 토큰을 신뢰한다(가드레일 #2: 비밀 미보관).
# 인프라는 Terraform(사람 apply), 앱 이미지 배포는 이 role로 CI가 aws CLI로 수행한다.
# Terraform <-> CI 경계는 docs/adr/0001-cicd-terraform-ci-boundary.md 참고.

data "aws_caller_identity" "current" {}

# GitHub OIDC 공급자. 계정당 token.actions.githubusercontent.com 하나만 존재할 수 있으므로,
# 다른 스택에서 이미 만들었으면 var.existing_github_oidc_provider_arn로 ARN을 주입하고
# 새로 만들지 않는다(EntityAlreadyExists 회피).
resource "aws_iam_openid_connect_provider" "github" {
  count          = var.existing_github_oidc_provider_arn == "" ? 1 : 0
  url            = "https://token.actions.githubusercontent.com"
  client_id_list = ["sts.amazonaws.com"]
  # aws provider 6.x는 GitHub OIDC 신뢰 앵커를 자동 관리하므로 thumbprint_list를 지정하지 않는다.

  tags = { Name = "${local.name_prefix}-github-oidc" }
}

locals {
  github_oidc_provider_arn = var.existing_github_oidc_provider_arn != "" ? var.existing_github_oidc_provider_arn : one(aws_iam_openid_connect_provider.github[*].arn)
}

# 배포 role의 신뢰 정책: main 브랜치에서 실행된 워크플로의 OIDC 토큰만 assume 허용.
data "aws_iam_policy_document" "github_actions_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [local.github_oidc_provider_arn]
    }

    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }

    # main 브랜치 워크플로만 허용(최소권한). sub claim 형식은 GitHub의 environment/OIDC
    # 커스터마이즈 설정에 따라 달라질 수 있으니, 트리거 정책을 바꾸면 이 값도 함께 갱신한다.
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["repo:Naerrow/linkpulse:ref:refs/heads/main"]
    }
  }
}

resource "aws_iam_role" "github_actions_deploy" {
  name               = "${local.name_prefix}-gha-deploy"
  assume_role_policy = data.aws_iam_policy_document.github_actions_assume.json

  tags = { Name = "${local.name_prefix}-gha-deploy" }
}

# 배포 권한(최소권한). 리소스를 가능한 한 우리 ECR repo / ECS service / 2개 role로 한정한다.
data "aws_iam_policy_document" "github_actions_deploy" {
  # ECR 레지스트리 로그인 토큰. 리소스 한정이 불가능해 * 가 강제된다.
  statement {
    sid       = "EcrAuthToken"
    effect    = "Allow"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }

  # 이미지 push/pull + 배포 preflight(태그 존재 확인)를 우리 repo로 한정.
  statement {
    sid    = "EcrPushPull"
    effect = "Allow"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:InitiateLayerUpload",
      "ecr:UploadLayerPart",
      "ecr:CompleteLayerUpload",
      "ecr:PutImage",
      "ecr:BatchGetImage",
      "ecr:GetDownloadUrlForLayer",
      "ecr:DescribeImages",
    ]
    resources = [aws_ecr_repository.app.arn]
  }

  # 새 task definition 등록을 우리 family로 한정.
  # (AWS Service Authorization Reference: RegisterTaskDefinition은 task-definition 리소스 타입 지원)
  statement {
    sid       = "EcsRegisterTaskDefinition"
    effect    = "Allow"
    actions   = ["ecs:RegisterTaskDefinition"]
    resources = ["arn:aws:ecs:${var.region}:${data.aws_caller_identity.current.account_id}:task-definition/${local.name_prefix}-app:*"]
  }

  # DescribeTaskDefinition은 리소스 레벨 권한을 지원하지 않아 * 가 강제된다.
  statement {
    sid       = "EcsDescribeTaskDefinition"
    effect    = "Allow"
    actions   = ["ecs:DescribeTaskDefinition"]
    resources = ["*"]
  }

  # 서비스 갱신/조회는 우리 서비스 ARN으로 한정(롤링 배포 + 안정화 대기).
  statement {
    sid    = "EcsDeployService"
    effect = "Allow"
    actions = [
      "ecs:UpdateService",
      "ecs:DescribeServices",
    ]
    resources = [aws_ecs_service.app.arn]
  }

  # register-task-definition이 taskdef에 execution/task role을 넣으므로, 그 두 role에만 PassRole 허용.
  statement {
    sid       = "PassEcsRoles"
    effect    = "Allow"
    actions   = ["iam:PassRole"]
    resources = [aws_iam_role.ecs_execution.arn, aws_iam_role.ecs_task.arn]

    condition {
      test     = "StringEquals"
      variable = "iam:PassedToService"
      values   = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role_policy" "github_actions_deploy" {
  name   = "deploy"
  role   = aws_iam_role.github_actions_deploy.id
  policy = data.aws_iam_policy_document.github_actions_deploy.json
}
