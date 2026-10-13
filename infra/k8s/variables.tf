# ---- 일반 (infra/prod와 같아야 한다 — 이름 규칙으로 클러스터·RDS·큐를 찾는다) ----
variable "region" {
  description = "AWS 리전."
  type        = string
  default     = "ap-northeast-2"
}

variable "project" {
  description = "리소스 이름 접두에 쓰는 프로젝트명(infra/prod와 같게)."
  type        = string
  default     = "linkpulse"
}

variable "environment" {
  description = "환경명(infra/prod와 같게)."
  type        = string
  default     = "prod"
}

# ---- 앱 설정 (사이트 ConfigMap·Secret으로 들어간다) ----
variable "domain_name" {
  description = "서비스 도메인. 앱 PUBLIC_BASE_URL(https://<이 값>)."
  type        = string
  default     = "lpulse.live"
}

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
  description = "관리자 토큰의 SHA-256 해시(16진수 64자). 원문이 아니다. 비우면 관리자 기능이 꺼진다(plan 0011). infra/prod에서 옮겨 왔다(plan 0014)."
  type        = string
  default     = ""
  # 토큰이 외우는 비밀번호라 해시도 비밀로 다룬다 — plan 출력이 공개 PR에 붙는다.
  sensitive = true

  validation {
    condition     = can(regex("^([0-9a-fA-F]{64})?$", var.admin_token_sha256))
    error_message = "admin_token_sha256은 빈 값이거나 SHA-256 해시(16진수 64자)여야 한다. 토큰 원문을 넣지 않았는지 확인한다."
  }
}

variable "rate_limit_exempt_ips" {
  description = "레이트리밋 예외 IPv4(쉼표 구분). 한계 측정 동안 측정 클라이언트만 풀어 준다. tfvars에 쓰지 않고 측정 apply의 -var로만 준다. 바꾼 뒤에는 kubectl rollout restart deploy/linkpulse -n linkpulse가 필요하다(envFrom은 Secret 변경을 다시 읽지 않는다)."
  type        = string
  default     = ""
  # 측정 클라이언트의 집 IP다. plan 출력이 공개 PR에 붙으므로 값이 찍히지 않게 한다.
  sensitive = true

  # 앱(RATE_LIMIT_EXEMPT_IPS)은 IPv6도 받지만, 측정 IP는 checkip.amazonaws.com의 IPv4라 여기서는 IPv4만 허용한다.
  validation {
    condition = var.rate_limit_exempt_ips == "" || alltrue([
      for ip in split(",", var.rate_limit_exempt_ips) :
      can(regex("^((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\\.){3}(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])$", ip))
    ])
    error_message = "rate_limit_exempt_ips는 빈 값이거나 쉼표로 구분한 IPv4 목록(공백 없이)이어야 한다."
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
