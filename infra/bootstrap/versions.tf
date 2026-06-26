# bootstrap 스택: 본 인프라(infra/prod)의 Terraform state를 보관할 S3 버킷만 만든다.
# 이 스택 자신의 state는 로컬(terraform.tfstate)에 둔다 — 원격 state를 만들기 위한
# 스택이 그 원격 state를 쓸 수는 없는 닭/달걀 문제 때문이다. 분실 시 import로 복구한다.
terraform {
  required_version = ">= 1.10.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}
