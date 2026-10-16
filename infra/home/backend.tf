# 원격 state: infra/prod와 같은 S3 버킷, 키만 다르다. 버킷은 infra/prod/backend.hcl을 그대로 쓴다:
#   terraform init -backend-config=../prod/backend.hcl
# 집 리소스지만 state는 AWS S3에 둔다 — 맥을 바꿔도 같은 state를 쓰고, 잠금(use_lockfile)도 같다.
# state에 비밀은 없다(Proxmox 토큰은 환경 변수, VM에는 SSH 공개 키만 넣는다).
terraform {
  backend "s3" {
    key          = "home/terraform.tfstate"
    region       = "ap-northeast-2"
    encrypt      = true
    use_lockfile = true
    # bucket = "..."  ← ../prod/backend.hcl 에서 주입
  }
}
