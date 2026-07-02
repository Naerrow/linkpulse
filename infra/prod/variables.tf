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
  description = "앱(프라이빗) 서브넷 CIDR. ECS 태스크 배치."
  type        = list(string)
  default     = ["10.0.10.0/24", "10.0.11.0/24"]
}

variable "data_subnet_cidrs" {
  description = "데이터(격리) 서브넷 CIDR. RDS 배치(인터넷 경로 없음)."
  type        = list(string)
  default     = ["10.0.20.0/24", "10.0.21.0/24"]
}

# ---- 앱 / ECS ----
variable "image_tag" {
  description = "Terraform이 등록하는 baseline/초기 task definition의 이미지 태그. service.task_definition은 ignore_changes라 `terraform apply -var image_tag=<sha>`로는 running 서비스가 이 태그로 옮겨지지 않는다(새 revision만 등록됨). 평상시·비상 배포·롤백은 모두 CI(main push/workflow_dispatch) 또는 직접 `aws ecs update-service`로 한다."
  type        = string
  default     = "bootstrap"
}

variable "service_desired_count" {
  description = "ECS 서비스 목표 태스크 수(운영 정상값 2). 최초 부트스트랩(이미지 없음) 때만 -var service_desired_count=0으로 인프라를 먼저 만든다."
  type        = number
  default     = 2
}

variable "task_cpu" {
  description = "Fargate 태스크 CPU 단위(256 = 0.25 vCPU)."
  type        = string
  default     = "256"
}

variable "task_memory" {
  description = "Fargate 태스크 메모리(MiB)."
  type        = string
  default     = "512"
}

variable "task_cpu_architecture" {
  description = "태스크 CPU 아키텍처. ARM64(Graviton, 저렴) 또는 X86_64. 이미지 빌드 아키텍처와 반드시 일치해야 한다."
  type        = string
  default     = "ARM64"
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

variable "log_retention_days" {
  description = "CloudWatch 로그 보관 일수."
  type        = number
  default     = 14
}

variable "enable_container_insights" {
  description = "ECS Container Insights 활성화(관측성↑, 비용↑). P3에서 켬 — RunningTaskCount 등 태스크 레벨 메트릭 확보. 끄려면 tfvars에서 false."
  type        = bool
  default     = true
}

variable "enable_alb_access_logs" {
  description = "ALB 액세스 로그 S3 저장 활성화. 켜려면 alb_access_logs_bucket 필요."
  type        = bool
  default     = false
}

variable "alb_access_logs_bucket" {
  description = "ALB 액세스 로그를 저장할 기존 S3 버킷 이름(enable_alb_access_logs=true일 때)."
  type        = string
  default     = ""
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

# ---- GitHub OIDC / CI(P2) ----
variable "existing_github_oidc_provider_arn" {
  description = "계정에 이미 있는 GitHub Actions OIDC 공급자(token.actions.githubusercontent.com) ARN. 비우면 새로 만든다. apply 전 `aws iam list-open-id-connect-providers`로 확인해 이미 있으면 그 ARN을 지정한다(중복 생성 EntityAlreadyExists 회피)."
  type        = string
  default     = ""
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
