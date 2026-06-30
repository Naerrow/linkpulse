# 원격 state: bootstrap 스택이 만든 S3 버킷에 저장한다.
# 잠금은 DynamoDB 없이 S3 네이티브 lockfile을 쓴다(Terraform 1.10+).
# 버킷 이름엔 계정 ID가 들어가 환경마다 다르므로 bucket 값만 backend.hcl로 주입한다:
#   terraform init -backend-config=backend.hcl
terraform {
  backend "s3" {
    key          = "prod/terraform.tfstate"
    region       = "ap-northeast-2"
    encrypt      = true
    use_lockfile = true
    # bucket = "..."  ← backend.hcl 에서 주입
  }
}
