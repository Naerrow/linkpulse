provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project     = var.project
      Environment = var.environment
      ManagedBy   = "terraform"
    }
  }
}

# us-east-1 별칭 provider (무트래픽 canary 전용 — synthetic-canary.tf).
# Route53 헬스체크의 CloudWatch 지표(HealthCheckStatus)는 글로벌 서비스 규약상 us-east-1에만
# 발행된다(콘솔에서도 "Change the current region to US East (N. Virginia). Route 53 metrics are
# not available if you select any other region" — 근거는 ADR 0004). CloudWatch 알람은 자기 지표가
# 있는 리전에서 만들어야 하고, 그 알람의 SNS 액션 토픽도 같은 리전이어야 한다 → canary 알람·전용
# SNS 토픽을 us-east-1에 둔다. default_tags는 기본 provider와 동일하게 복제.
provider "aws" {
  alias  = "use1"
  region = "us-east-1"

  default_tags {
    tags = {
      Project     = var.project
      Environment = var.environment
      ManagedBy   = "terraform"
    }
  }
}
