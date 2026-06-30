output "tfstate_bucket" {
  description = "infra/prod backend가 사용할 state 버킷 이름. backend.hcl의 bucket 값으로 넣는다."
  value       = aws_s3_bucket.tfstate.bucket
}

output "region" {
  description = "버킷 리전."
  value       = "ap-northeast-2"
}
