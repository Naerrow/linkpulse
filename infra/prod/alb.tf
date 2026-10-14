resource "aws_lb" "main" {
  name               = "${local.name_prefix}-alb"
  load_balancer_type = "application"
  internal           = false
  security_groups    = [aws_security_group.alb.id]
  subnets            = aws_subnet.public[*].id

  # 접근 로그는 측정 동안만 켠다(plan 0015 — 502·타임아웃을 error_reason으로 분류한다). 버킷은 아래에서 만든다.
  dynamic "access_logs" {
    for_each = var.enable_alb_access_logs ? [1] : []
    content {
      bucket  = aws_s3_bucket.alb_logs[0].id
      enabled = true
    }
  }

  # ALB는 접근 로그를 켤 때 버킷에 시험 파일(ELBAccessLogTestFile)을 써서 권한을 확인한다.
  # 버킷 정책보다 먼저 켜지면 apply가 Access Denied로 실패한다 — bucket 참조만으로는 정책 순서가 보장되지 않는다.
  depends_on = [aws_s3_bucket_policy.alb_logs]

  tags = { Name = "${local.name_prefix}-alb" }
}

# ---- 접근 로그 버킷 (enable_alb_access_logs일 때만) ----
# 측정 창 동안만 있는 임시 버킷이다. 측정이 끝나면 destroy 전에 내려받는다(load/k6/README.md).
# - force_destroy: 인프라를 매일 지운다. 로그가 남아 있어도 destroy가 멈추지 않게 한다.
# - SSE-S3: ALB 접근 로그가 지원하는 유일한 서버 쪽 암호화다(ELB 사용자 가이드 enable-access-logging, Step 1).
# tfsec 지적 중 받아들인 것: 고객 관리 키(위 이유로 불가), 버전 관리(7일 뒤 지우는 임시 로그), 버킷 자체의 접근 로그(로그의 로그).
#tfsec:ignore:aws-s3-encryption-customer-key
#tfsec:ignore:aws-s3-enable-versioning
#tfsec:ignore:aws-s3-enable-bucket-logging
resource "aws_s3_bucket" "alb_logs" {
  count = var.enable_alb_access_logs ? 1 : 0
  # 버킷 이름은 전역에서 유일해야 해서 계정 ID를 붙인다.
  bucket        = "${local.name_prefix}-alb-logs-${data.aws_caller_identity.current.account_id}"
  force_destroy = true

  tags = { Name = "${local.name_prefix}-alb-logs" }
}

resource "aws_s3_bucket_public_access_block" "alb_logs" {
  count                   = var.enable_alb_access_logs ? 1 : 0
  bucket                  = aws_s3_bucket.alb_logs[0].id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

#tfsec:ignore:aws-s3-encryption-customer-key
resource "aws_s3_bucket_server_side_encryption_configuration" "alb_logs" {
  count  = var.enable_alb_access_logs ? 1 : 0
  bucket = aws_s3_bucket.alb_logs[0].id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "alb_logs" {
  count  = var.enable_alb_access_logs ? 1 : 0
  bucket = aws_s3_bucket.alb_logs[0].id
  rule {
    id     = "expire-7d"
    status = "Enabled"
    filter {}
    expiration {
      days = 7
    }
  }
}

# 로그 전달 주체는 서비스 주체(logdelivery.elasticloadbalancing.amazonaws.com)다. ELB 사용자 가이드 enable-access-logging
# "Bucket policy"가 권하는 형태다 — 리전별 ELB 계정 ID(서울 600734575887)를 쓰는 방식은 "legacy"로 적혀 있다(2026-10-14 확인).
# 경로에 이 계정 ID를, 조건에 이 계정·리전의 ALB만(aws:SourceArn)을 넣는다(같은 문서 Security best practices). 접두는 두지 않는다.
data "aws_iam_policy_document" "alb_logs" {
  count = var.enable_alb_access_logs ? 1 : 0
  statement {
    sid       = "AllowELBLogDelivery"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.alb_logs[0].arn}/AWSLogs/${data.aws_caller_identity.current.account_id}/*"]
    principals {
      type        = "Service"
      identifiers = ["logdelivery.elasticloadbalancing.amazonaws.com"]
    }
    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values   = ["arn:aws:elasticloadbalancing:${var.region}:${data.aws_caller_identity.current.account_id}:loadbalancer/*"]
    }
  }
}

resource "aws_s3_bucket_policy" "alb_logs" {
  count  = var.enable_alb_access_logs ? 1 : 0
  bucket = aws_s3_bucket.alb_logs[0].id
  policy = data.aws_iam_policy_document.alb_logs[0].json

  # 공개 차단 설정과 정책 쓰기가 겹치면 S3가 거부할 수 있어 차단 설정 뒤에 쓴다.
  depends_on = [aws_s3_bucket_public_access_block.alb_logs]
}

resource "aws_lb_target_group" "app" {
  name        = "${local.name_prefix}-tg"
  port        = 8080
  protocol    = "HTTP"
  vpc_id      = aws_vpc.main.id
  target_type = "ip" # 파드 IP(VPC CNI). LB 컨트롤러가 TargetGroupBinding으로 등록한다(infra/k8s)

  # 헬스체크는 liveness(/healthz)만 본다. DB 의존성(/readyz)을 ALB가 보면 DB 일시
  # 장애에 전 태스크가 한꺼번에 unhealthy가 되어 장애가 증폭된다.
  # 간격 10초·healthy 2회(plan 0014): 파드의 readiness gate가 ALB healthy를 기다리므로, 이 값이 새 파드가
  # 트래픽을 받기까지의 시간(약 20초)이다. 30초·3회면 60~90초라 HPA로 늘린 파드가 측정 단계 안에 일하지 못한다.
  health_check {
    path                = "/healthz"
    matcher             = "200"
    interval            = 10
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  # 앱의 graceful shutdown(10s)에 맞춘 연결 드레이닝.
  deregistration_delay = 30

  tags = { Name = "${local.name_prefix}-tg" }
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.main.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = aws_acm_certificate_validation.main.certificate_arn

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.app.arn
  }
}

resource "aws_lb_listener" "http_redirect" {
  load_balancer_arn = aws_lb.main.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type = "redirect"
    redirect {
      port        = "443"
      protocol    = "HTTPS"
      status_code = "HTTP_301"
    }
  }
}
