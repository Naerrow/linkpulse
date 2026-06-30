# state용 S3 버킷. 버킷 이름은 전역 유일해야 하므로 계정 ID를 접미로 붙인다.
data "aws_caller_identity" "current" {}

locals {
  bucket_name = "lpulse-tfstate-${data.aws_caller_identity.current.account_id}-apne2"
}

resource "aws_s3_bucket" "tfstate" {
  bucket = local.bucket_name

  # state는 절대 잃으면 안 되는 자산이다. 실수로 강제 삭제되지 않게 잠근다.
  force_destroy = false

  lifecycle {
    prevent_destroy = true
  }
}

# 버전 관리: state를 덮어써도 이전 버전을 보관해 복구할 수 있게 한다.
resource "aws_s3_bucket_versioning" "tfstate" {
  bucket = aws_s3_bucket.tfstate.id
  versioning_configuration {
    status = "Enabled"
  }
}

# 저장 시 암호화(SSE-S3 / AES256).
resource "aws_s3_bucket_server_side_encryption_configuration" "tfstate" {
  bucket = aws_s3_bucket.tfstate.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# 퍼블릭 접근 전면 차단.
resource "aws_s3_bucket_public_access_block" "tfstate" {
  bucket                  = aws_s3_bucket.tfstate.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}
