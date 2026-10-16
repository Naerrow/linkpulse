variable "region" {
  description = "AWS 리전. 집 클릭 큐와 Roles Anywhere 트러스트 앵커가 여기에 있다."
  type        = string
  default     = "ap-northeast-2"
}

variable "project" {
  description = "리소스 이름 접두에 쓰는 프로젝트명(infra/prod와 같게)."
  type        = string
  default     = "linkpulse"
}

variable "ca_cert_path" {
  description = "트러스트 앵커로 올릴 자체 CA 인증서 경로(scripts/home-pki.sh가 만든 ca.crt). 공개 정보지만 경로가 사람마다 달라 로컬 tfvars로 받는다. CA 개인 키가 아니다."
  type        = string
  default     = "~/.linkpulse-home/pki/ca.crt"
}

variable "app_cert_cn" {
  description = "집 앱 말단 인증서의 CN. 역할 신뢰 정책이 이 CN만 받는다(scripts/home-pki.sh의 말단 CN과 같아야 한다)."
  type        = string
  default     = "linkpulse-home-app"
}
