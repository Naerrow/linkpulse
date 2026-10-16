# 집 사이트의 AWS 쪽 (plan 0017, ADR 0010).
#
# 집 k3s의 앱이 AWS에 닿는 데 필요한 것만 둔다: Roles Anywhere(장기 키 없이 X.509 인증서로 임시 자격증명)와 집 전용 클릭 큐.
# infra/prod와 나눈 이유: infra/prod는 시간 과금이라 작업할 때만 켜고 통째로 지운다(full-destroy-prod.sh).
# 여기 있는 것은 시간 과금이 없어 늘 켜 둔다 — infra/prod에 두면 AWS를 끌 때 집도 같이 멈춘다.
# 요금: Roles Anywhere는 추가 요금이 없다(AWS What's New 2023-12, 자체 CA라 Private CA 요금도 없다). SQS는 요청 수만큼이다.
#
# 순서: scripts/home-pki.sh(CA) → 이 스택 → infra/home-k8s(이 스택의 출력을 원격 state로 읽는다).

locals {
  name_prefix = "${var.project}-home"
}

# ---- Roles Anywhere ----
# 트러스트 앵커: 이 CA가 서명한 인증서만 CreateSession을 할 수 있다. CA 인증서는 공개 정보다(개인 키는 맥 밖으로 나가지 않는다).
# CA를 바꾸면(유출·분실) 이 리소스가 새 인증서로 바뀌고, 옛 CA가 서명한 말단은 그 순간 모두 무효가 된다(런북 home-site.md).
resource "aws_rolesanywhere_trust_anchor" "home" {
  name    = local.name_prefix
  enabled = true

  source {
    source_type = "CERTIFICATE_BUNDLE"
    source_data {
      x509_certificate_data = file(pathexpand(var.ca_cert_path))
    }
  }
}

# 신뢰 정책(1차 출처: Roles Anywhere 사용자 가이드 trust-model.html의 예시 형태).
# 세 동작은 Roles Anywhere가 세션을 만들 때 쓴다(소스 ID = 인증서 CN, 세션 태그 = 인증서 필드).
# 조건 두 개가 없으면 이 계정의 어느 트러스트 앵커 인증서로든 이 역할을 맡을 수 있다:
#   - aws:SourceArn: 이 트러스트 앵커로 들어온 요청만(다른 앵커·혼동된 대리인 방지)
#   - x509Subject/CN: 같은 CA가 서명했어도 CN이 다르면 거부(판정 (c)가 --test-leaf로 시험한다)
data "aws_iam_policy_document" "app_assume" {
  statement {
    sid     = "RolesAnywhereHomeApp"
    actions = ["sts:AssumeRole", "sts:TagSession", "sts:SetSourceIdentity"]
    principals {
      type        = "Service"
      identifiers = ["rolesanywhere.amazonaws.com"]
    }
    condition {
      test     = "ArnEquals"
      variable = "aws:SourceArn"
      values   = [aws_rolesanywhere_trust_anchor.home.arn]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:PrincipalTag/x509Subject/CN"
      values   = [var.app_cert_cn]
    }
  }
}

resource "aws_iam_role" "app" {
  name               = "${local.name_prefix}-app"
  description        = "linkpulse home site app via IAM Roles Anywhere (plan 0017)"
  assume_role_policy = data.aws_iam_policy_document.app_assume.json
  # 프로필 세션 1시간과 같다. 서명 도우미가 만료 전에 다시 받는다.
  max_session_duration = 3600
}

# AWS 앱 역할(infra/prod iam.tf pod_app)의 클릭 큐 권한과 같은 세 동작을 집 큐에만 준다.
# 비밀번호는 집 Postgres·file 출처라 Secrets Manager 권한이 없다.
data "aws_iam_policy_document" "app" {
  statement {
    sid       = "HomeClickQueueSendReceiveDelete"
    actions   = ["sqs:SendMessage", "sqs:ReceiveMessage", "sqs:DeleteMessage"]
    resources = [aws_sqs_queue.clicks.arn]
  }
}

resource "aws_iam_role_policy" "app" {
  name   = "app"
  role   = aws_iam_role.app.id
  policy = data.aws_iam_policy_document.app.json
}

# 프로필: 이 역할 하나만 맡을 수 있다. 세션 1시간(기본값)을 적어 둔다.
resource "aws_rolesanywhere_profile" "app" {
  name             = "${local.name_prefix}-app"
  enabled          = true
  role_arns        = [aws_iam_role.app.arn]
  duration_seconds = 3600
}

# ---- 집 클릭 큐 ----
# 사이트마다 큐·워커·DB가 한 묶음이다 — 집 앱이 보낸 클릭은 집 워커가 집 DB에 반영한다.
# 설정은 AWS 큐(infra/prod sqs.tf)와 같다. 근거도 그 파일에 있다(가시성 30초, 재전달 5회, 보존 4일/14일, 롱폴링 20초).
# DLQ·나이 알람은 두지 않는다: 알림 토픽이 infra/prod에 있어 AWS를 끄면 사라지고, 집 알림의 외부 전달은 P7이다.
# 0017은 판정 (b)에서 DLQ 0을 사람이 확인한다.
resource "aws_sqs_queue" "clicks" {
  name                       = "${local.name_prefix}-clicks"
  visibility_timeout_seconds = 30
  message_retention_seconds  = 345600
  receive_wait_time_seconds  = 20
  sqs_managed_sse_enabled    = true

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.clicks_dlq.arn
    maxReceiveCount     = 5
  })

  tags = { Name = "${local.name_prefix}-clicks" }
}

resource "aws_sqs_queue" "clicks_dlq" {
  name                      = "${local.name_prefix}-clicks-dlq"
  message_retention_seconds = 1209600
  sqs_managed_sse_enabled   = true

  tags = { Name = "${local.name_prefix}-clicks-dlq" }
}

resource "aws_sqs_queue_redrive_allow_policy" "clicks_dlq" {
  queue_url = aws_sqs_queue.clicks_dlq.id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.clicks.arn]
  })
}
