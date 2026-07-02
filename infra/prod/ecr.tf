resource "aws_ecr_repository" "app" {
  name                 = "${var.project}/app"
  image_tag_mutability = "MUTABLE" # CI는 git sha 태그를 쓴다(사실상 불변). 동일 태그는 preflight로 재푸시 skip(멱등)이라 MUTABLE 유지.
  force_delete         = var.ecr_force_delete

  image_scanning_configuration {
    scan_on_push = true # push 시 취약점 스캔
  }

  tags = { Name = "${var.project}/app" }
}

# 오래된 이미지는 자동 정리(최근 30개 보관 — CI sha 태그 롤백 여유).
resource "aws_ecr_lifecycle_policy" "app" {
  repository = aws_ecr_repository.app.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep only the 30 most recent images"
      selection = {
        tagStatus   = "any"
        countType   = "imageCountMoreThan"
        countNumber = 30
      }
      action = { type = "expire" }
    }]
  })
}
