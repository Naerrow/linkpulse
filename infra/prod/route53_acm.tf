# 기존 호스팅 영역을 참조만 한다(생성하지 않음).
# hosted_zone_id가 있으면 그걸로, 없으면 도메인 이름으로 조회한다.
data "aws_route53_zone" "main" {
  zone_id      = var.hosted_zone_id != "" ? var.hosted_zone_id : null
  name         = var.hosted_zone_id != "" ? null : var.domain_name
  private_zone = false
}

# apex 도메인 단일 인증서(SAN 없음). DNS 검증.
resource "aws_acm_certificate" "main" {
  domain_name       = var.domain_name
  validation_method = "DNS"

  lifecycle {
    create_before_destroy = true
  }

  tags = { Name = "${local.name_prefix}-cert" }
}

# 검증용 DNS 레코드를 호스팅 영역에 추가.
resource "aws_route53_record" "cert_validation" {
  for_each = {
    for dvo in aws_acm_certificate.main.domain_validation_options : dvo.domain_name => {
      name   = dvo.resource_record_name
      type   = dvo.resource_record_type
      record = dvo.resource_record_value
    }
  }

  zone_id         = data.aws_route53_zone.main.zone_id
  name            = each.value.name
  type            = each.value.type
  records         = [each.value.record]
  ttl             = 60
  allow_overwrite = true
}

# 검증 완료를 기다린다(HTTPS 리스너가 이 ARN을 참조).
resource "aws_acm_certificate_validation" "main" {
  certificate_arn         = aws_acm_certificate.main.arn
  validation_record_fqdns = [for r in aws_route53_record.cert_validation : r.fqdn]
}

# apex → ALB alias.
resource "aws_route53_record" "alias" {
  zone_id = data.aws_route53_zone.main.zone_id
  name    = var.domain_name
  type    = "A"

  alias {
    name                   = aws_lb.main.dns_name
    zone_id                = aws_lb.main.zone_id
    evaluate_target_health = true
  }
}
