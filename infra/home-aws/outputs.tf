# infra/home-k8s가 원격 state로 읽어 사이트 ConfigMap(linkpulse-site)에 넣는다. 계정 ID가 들어 있어 저장소에는 남기지 않는다.
output "trust_anchor_arn" {
  description = "서명 도우미 --trust-anchor-arn."
  value       = aws_rolesanywhere_trust_anchor.home.arn
}

output "profile_arn" {
  description = "서명 도우미 --profile-arn."
  value       = aws_rolesanywhere_profile.app.arn
}

output "role_arn" {
  description = "서명 도우미 --role-arn."
  value       = aws_iam_role.app.arn
}

output "click_queue_url" {
  description = "집 앱 CLICK_QUEUE_URL."
  value       = aws_sqs_queue.clicks.url
}

output "click_dlq_url" {
  description = "집 클릭 DLQ(판정 (b)에서 사람이 0인지 본다)."
  value       = aws_sqs_queue.clicks_dlq.url
}
