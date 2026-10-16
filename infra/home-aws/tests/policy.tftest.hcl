# 역할 신뢰 정책·권한·큐 설정 단언(plan 0017 A2).
#   cd infra/home-aws && terraform init -backend=false && terraform test
# AWS에 닿지 않는다: 관리 리소스를 모두 덮어써(override_resource) apply가 API를 부르지 않고,
# 정책 문서(aws_iam_policy_document)는 공급자가 로컬에서 만든다. 자격증명은 가짜다 — 덮어쓰기를 빠뜨리면 인증 오류로 멈춘다.
# fixture-ca.crt는 이 시험에만 쓰는 공개 인증서다(개인 키는 버렸다). 실제 CA와 무관하다.

provider "aws" {
  region                      = "ap-northeast-2"
  access_key                  = "test"
  secret_key                  = "test"
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
}

variables {
  ca_cert_path = "tests/fixture-ca.crt"
}

override_resource {
  target = aws_rolesanywhere_trust_anchor.home
  values = {
    arn = "arn:aws:rolesanywhere:ap-northeast-2:000000000000:trust-anchor/00000000-0000-0000-0000-000000000000"
  }
}

override_resource {
  target = aws_iam_role.app
  values = {
    arn = "arn:aws:iam::000000000000:role/linkpulse-home-app"
  }
}

override_resource {
  target = aws_iam_role_policy.app
}

override_resource {
  target = aws_rolesanywhere_profile.app
}

override_resource {
  target = aws_sqs_queue.clicks
  values = {
    arn = "arn:aws:sqs:ap-northeast-2:000000000000:linkpulse-home-clicks"
  }
}

override_resource {
  target = aws_sqs_queue.clicks_dlq
  values = {
    arn = "arn:aws:sqs:ap-northeast-2:000000000000:linkpulse-home-clicks-dlq"
  }
}

override_resource {
  target = aws_sqs_queue_redrive_allow_policy.clicks_dlq
}

run "trust_policy" {
  # 세 동작, 주체 rolesanywhere, 조건 두 개(트러스트 앵커 ARN·CN). 하나라도 빠지면 다른 앵커·다른 CN 인증서로 역할을 맡을 수 있다.
  assert {
    condition = alltrue([
      for s in jsondecode(data.aws_iam_policy_document.app_assume.json).Statement :
      s.Effect == "Allow" && s.Principal.Service == "rolesanywhere.amazonaws.com" &&
      toset(s.Action) == toset(["sts:AssumeRole", "sts:TagSession", "sts:SetSourceIdentity"]) &&
      try(s.Condition.ArnEquals["aws:SourceArn"], "") == "arn:aws:rolesanywhere:ap-northeast-2:000000000000:trust-anchor/00000000-0000-0000-0000-000000000000" &&
      try(s.Condition.StringEquals["aws:PrincipalTag/x509Subject/CN"], "") == "linkpulse-home-app"
    ])
    error_message = "신뢰 정책이 세 동작·rolesanywhere 주체·SourceArn·CN 조건을 모두 갖추지 않았다"
  }
  assert {
    condition     = length(jsondecode(data.aws_iam_policy_document.app_assume.json).Statement) == 1
    error_message = "신뢰 정책 문장은 하나여야 한다(조건 없는 문장이 끼면 조건이 무력해진다)"
  }
  assert {
    condition     = aws_iam_role.app.assume_role_policy == data.aws_iam_policy_document.app_assume.json
    error_message = "역할이 이 신뢰 정책 문서를 쓰지 않는다"
  }

  # 권한: 집 큐 하나에 세 동작만.
  assert {
    condition = alltrue([
      for s in jsondecode(data.aws_iam_policy_document.app.json).Statement :
      toset(s.Action) == toset(["sqs:SendMessage", "sqs:ReceiveMessage", "sqs:DeleteMessage"]) &&
      s.Resource == "arn:aws:sqs:ap-northeast-2:000000000000:linkpulse-home-clicks"
    ]) && length(jsondecode(data.aws_iam_policy_document.app.json).Statement) == 1
    error_message = "앱 권한이 집 큐 하나의 세 동작(Send·Receive·Delete)과 다르다"
  }

  # 큐 설정은 AWS 큐(infra/prod sqs.tf)와 같다.
  assert {
    condition = (aws_sqs_queue.clicks.visibility_timeout_seconds == 30 && aws_sqs_queue.clicks.message_retention_seconds == 345600 &&
      aws_sqs_queue.clicks.receive_wait_time_seconds == 20 && aws_sqs_queue.clicks.sqs_managed_sse_enabled &&
      jsondecode(aws_sqs_queue.clicks.redrive_policy).maxReceiveCount == 5 &&
    jsondecode(aws_sqs_queue.clicks.redrive_policy).deadLetterTargetArn == aws_sqs_queue.clicks_dlq.arn)
    error_message = "집 클릭 큐 설정이 AWS 큐와 다르다"
  }
  assert {
    condition     = aws_sqs_queue.clicks_dlq.message_retention_seconds == 1209600 && aws_sqs_queue.clicks_dlq.sqs_managed_sse_enabled
    error_message = "집 DLQ 설정이 AWS DLQ와 다르다"
  }
  assert {
    condition     = aws_rolesanywhere_profile.app.duration_seconds == 3600 && aws_iam_role.app.max_session_duration == 3600 && aws_rolesanywhere_profile.app.role_arns == toset([aws_iam_role.app.arn])
    error_message = "프로필이 이 역할 하나·세션 1시간이 아니다"
  }
}
