# ---- 일반 ----
variable "region" {
  description = "AWS 리전."
  type        = string
  default     = "ap-northeast-2"
}

variable "project" {
  description = "리소스 이름 접두에 쓰는 프로젝트명."
  type        = string
  default     = "linkpulse"
}

variable "environment" {
  description = "환경명(리소스 접두·태그)."
  type        = string
  default     = "prod"
}

# ---- 도메인 / DNS ----
variable "domain_name" {
  description = "서비스 도메인(apex). ACM 인증서·ALB alias·PUBLIC_BASE_URL에 쓰인다."
  type        = string
  default     = "lpulse.live"
}

variable "hosted_zone_id" {
  description = "기존 Route53 호스팅 영역 ID. 비우면 domain_name으로 조회한다(동일 이름 zone이 여러 개면 ID 지정)."
  type        = string
  default     = ""
}

# ---- 네트워크 ----
variable "vpc_cidr" {
  description = "VPC CIDR."
  type        = string
  default     = "10.0.0.0/16"
}

variable "public_subnet_cidrs" {
  description = "퍼블릭 서브넷 CIDR(AZ 수와 같은 길이). ALB·NAT 배치."
  type        = list(string)
  default     = ["10.0.0.0/24", "10.0.1.0/24"]
}

variable "app_subnet_cidrs" {
  description = "앱(프라이빗) 서브넷 CIDR. EKS 노드·파드(VPC CNI가 파드 IP를 여기서 받는다)·컨트롤 플레인 ENI 배치."
  type        = list(string)
  default     = ["10.0.10.0/24", "10.0.11.0/24"]
}

variable "data_subnet_cidrs" {
  description = "데이터(격리) 서브넷 CIDR. RDS 배치(인터넷 경로 없음)."
  type        = list(string)
  default     = ["10.0.20.0/24", "10.0.21.0/24"]
}

# ---- EKS (plan 0014) ----
variable "eks_version" {
  description = "EKS 쿠버네티스 버전. 표준 지원 중 최신으로 고정한다 — apply 전 `aws eks describe-cluster-versions`의 status가 STANDARD_SUPPORT인지 확인한다. 바꾸면 노드·애드온도 따라 바뀐다."
  type        = string
  default     = "1.37"
}

variable "node_instance_type" {
  description = "관리형 노드 인스턴스 타입. ARM64(Graviton)여야 한다 — 노드 그룹 ami_type이 AL2023_ARM_64_STANDARD다."
  type        = string
  default     = "t4g.medium"
}

variable "node_count" {
  description = "노드 수(최소·최대·희망이 모두 이 값). 오토스케일러가 없어 고정이다. 앱 최대 7개·시스템 파드·kube-prometheus-stack이 3대에 들어간다(ADR 0007 노드 수용, plan 0015)."
  type        = number
  default     = 3
}

variable "log_retention_days" {
  description = "CloudWatch 로그 보관 일수."
  type        = number
  default     = 14
}

variable "enable_alb_access_logs" {
  description = "ALB 접근 로그를 켠다. 켜면 로그 버킷(7일 만료)을 이 스택이 만든다. 측정 apply에서만 -var enable_alb_access_logs=true로 켠다(plan 0015 502 분류)."
  type        = bool
  default     = false
}

variable "alarm_sms_number" {
  description = "알람 SMS 수신 번호(E.164, 예: +821012345678). 비우면 SMS 구독을 만들지 않는다. Slack 경로가 죽었을 때의 두 번째 경로 — 2026-08-30 장애(118시간)에서 Slack 카드는 도착했는데 사람에게 닿지 않았다."
  type        = string
  default     = ""
  sensitive   = true
}

# ---- RDS ----
variable "db_name" {
  description = "초기 데이터베이스 이름."
  type        = string
  default     = "linkpulse"
}

variable "db_username" {
  description = "마스터 사용자명. 비밀번호는 RDS가 Secrets Manager에 생성·관리한다(코드에 두지 않음)."
  type        = string
  default     = "linkpulse"
}

variable "db_instance_class" {
  description = "RDS 인스턴스 클래스."
  type        = string
  default     = "db.t4g.micro"
}

variable "db_allocated_storage" {
  description = "RDS 할당 스토리지(GB, gp3 최소 20)."
  type        = number
  default     = 20
}

variable "db_max_allocated_storage" {
  description = "스토리지 자동확장 상한(GB). 0이면 자동확장 비활성."
  type        = number
  default     = 0
}

variable "postgres_version" {
  description = "PostgreSQL 메이저 버전(마이너는 RDS가 선택)."
  type        = string
  default     = "16"
}

variable "db_backup_retention" {
  description = "자동 백업 보관 일수."
  type        = number
  default     = 7
}

variable "db_deletion_protection" {
  description = "RDS 삭제 보호. 운영은 true 유지."
  type        = bool
  default     = true
}

variable "db_skip_final_snapshot" {
  description = "삭제 시 최종 스냅샷 생략. 운영은 false(스냅샷 남김)."
  type        = bool
  default     = false
}

# ---- 모니터링 / Slack 알림 (P3) ----
# CloudWatch 알람 → SNS → AWS Chatbot(Amazon Q Developer in chat applications) → Slack.
# 두 값은 콘솔에서 Slack workspace를 OAuth 승인한 뒤 확보하는 "식별자"다(비밀값 아님).
# 둘 다 비우면 알람+SNS만 만들고 Slack 배선은 스킵한다(중간 마일스톤).
variable "slack_team_id" {
  description = "Slack workspace(team) ID. 콘솔 Amazon Q Developer in chat applications에서 OAuth 승인 후 확보. 비우면 Slack 통지 스킵."
  type        = string
  default     = ""
}

variable "slack_channel_id" {
  description = "통지받을 Slack 채널 ID(채널명 아님, 예: C0123ABCDEF). 채널에서 `/invite @Amazon Q` 필요."
  type        = string
  default     = ""

  # slack_team_id/slack_channel_id는 둘 다 채우거나 둘 다 비워야 한다(한쪽만=실수).
  # count 게이트가 부분 생성은 이미 막지만, 조용한 스킵 대신 apply 전 명확한 에러를 낸다.
  validation {
    condition     = (trimspace(var.slack_team_id) == "") == (trimspace(var.slack_channel_id) == "")
    error_message = "Set both slack_team_id and slack_channel_id, or neither."
  }
}
