resource "aws_ecr_repository" "app" {
  name                 = "${var.project}/app"
  image_tag_mutability = "MUTABLE" # P1 수동 배포 편의. P2 CI에서 sha 불변 태그로 전환.
  force_delete         = false

  image_scanning_configuration {
    scan_on_push = true # push 시 취약점 스캔
  }

  tags = { Name = "${var.project}/app" }
}

# 오래된 이미지는 자동 정리(최근 10개만 보관).
resource "aws_ecr_lifecycle_policy" "app" {
  repository = aws_ecr_repository.app.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep only the 10 most recent images"
      selection = {
        tagStatus   = "any"
        countType   = "imageCountMoreThan"
        countNumber = 10
      }
      action = { type = "expire" }
    }]
  })
}
