# 원격 state: infra/prod와 같은 S3 버킷, 키만 다르다. 버킷은 infra/prod/backend.hcl을 그대로 쓴다:
#   terraform init -backend-config=../prod/backend.hcl
terraform {
  backend "s3" {
    key          = "home-aws/terraform.tfstate"
    region       = "ap-northeast-2"
    encrypt      = true
    use_lockfile = true
    # bucket = "..."  ← ../prod/backend.hcl 에서 주입
  }
}
