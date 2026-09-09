terraform {
  required_version = ">= 1.10.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }

    # rotation-redeploy.tf가 handler.py를 인라인 zip으로 패키징하는 데 쓴다
    # (S3 버킷·CI 아티팩트를 새로 만들지 않기 위해).
    archive = {
      source  = "hashicorp/archive"
      version = "~> 2.0"
    }
  }
}
