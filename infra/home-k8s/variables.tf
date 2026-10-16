# ---- 클러스터·state ----
variable "kubeconfig_path" {
  description = "집 k3s kubeconfig(ansible k3s 역할이 맥에 쓴다). 컨텍스트 이름은 linkpulse-home이어야 한다."
  type        = string
  default     = "~/.kube/linkpulse-home.yaml"
}

variable "tfstate_bucket" {
  description = "infra/home·infra/home-aws의 원격 state 버킷(infra/prod/backend.hcl의 bucket과 같은 값). 계정 ID가 들어 있어 로컬 tfvars에만 둔다."
  type        = string
}

variable "region" {
  description = "AWS 리전(state 버킷·집 클릭 큐). 앱 AWS_REGION."
  type        = string
  default     = "ap-northeast-2"
}

# ---- 앱 설정 (사이트 ConfigMap·Secret으로 들어간다) ----
variable "log_level" {
  description = "앱 LOG_LEVEL."
  type        = string
  default     = "info"
}

variable "short_code_length" {
  description = "앱 SHORT_CODE_LENGTH."
  type        = number
  default     = 7
}

variable "admin_token_sha256" {
  description = "관리자 토큰의 SHA-256 해시(16진수 64자). 원문이 아니다. 비우면 관리자 기능이 꺼진다. AWS(infra/k8s)와 같은 값을 쓴다."
  type        = string
  default     = ""
  # 토큰이 외우는 비밀번호라 해시도 비밀로 다룬다 — plan 출력이 공개 PR에 붙는다.
  sensitive = true

  validation {
    condition     = can(regex("^([0-9a-fA-F]{64})?$", var.admin_token_sha256))
    error_message = "admin_token_sha256은 빈 값이거나 SHA-256 해시(16진수 64자)여야 한다. 토큰 원문을 넣지 않았는지 확인한다."
  }
}

# ---- GitOps ----
variable "git_repo_url" {
  description = "ArgoCD가 차트를 읽는 저장소(공개). 인증 없이 읽는다."
  type        = string
  default     = "https://github.com/Naerrow/linkpulse.git"
}

variable "git_revision" {
  description = "ArgoCD가 따라가는 브랜치. 다른 브랜치의 차트를 시험할 때만 바꾼다."
  type        = string
  default     = "main"
}
