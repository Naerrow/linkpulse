# 보안그룹은 IP가 아니라 SG 참조 체인으로 연결한다: 인터넷 -> alb -> app -> data.
# 태스크 IP가 바뀌어도 규칙이 유지되고 최소권한이 자연 성립한다.
#
# 주의: AWS는 SG/rule의 description에 ASCII만 허용한다(한글/화살표 등은 apply 시 API가
# 거부하며 validate/plan으로는 안 잡힌다). 그래서 description 값은 ASCII 영어로 두고,
# 한국어 설명은 각 리소스 위 # 주석으로 남긴다.

# ALB: 인터넷에서 443/80 수신, app으로 8080 전달
resource "aws_security_group" "alb" {
  name_prefix = "${local.name_prefix}-alb-"
  description = "ALB: 443/80 in from internet, 8080 out to app"
  vpc_id      = aws_vpc.main.id
  tags        = { Name = "${local.name_prefix}-alb-sg" }
}

# ECS 앱: ALB에서만 8080 수신, 아웃바운드는 HTTPS/DNS/DB로 한정
resource "aws_security_group" "app" {
  name_prefix = "${local.name_prefix}-app-"
  description = "ECS app: 8080 in from ALB, egress HTTPS/DNS/DB only"
  vpc_id      = aws_vpc.main.id
  tags        = { Name = "${local.name_prefix}-app-sg" }
}

# RDS: app에서만 5432 수신, 아웃바운드 없음
resource "aws_security_group" "data" {
  name_prefix = "${local.name_prefix}-data-"
  description = "RDS: 5432 in from app only, no egress"
  vpc_id      = aws_vpc.main.id
  tags        = { Name = "${local.name_prefix}-data-sg" }
}

# ---- ALB SG ----
# 인터넷에서 HTTPS 수신
resource "aws_vpc_security_group_ingress_rule" "alb_https" {
  security_group_id = aws_security_group.alb.id
  description       = "HTTPS from internet"
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = 443
  to_port           = 443
  ip_protocol       = "tcp"
}

# 인터넷에서 HTTP 수신(443으로 리다이렉트)
resource "aws_vpc_security_group_ingress_rule" "alb_http" {
  security_group_id = aws_security_group.alb.id
  description       = "HTTP from internet (redirect to 443)"
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = 80
  to_port           = 80
  ip_protocol       = "tcp"
}

# 앱 컨테이너로 전달
resource "aws_vpc_security_group_egress_rule" "alb_to_app" {
  security_group_id            = aws_security_group.alb.id
  description                  = "To app containers on 8080"
  referenced_security_group_id = aws_security_group.app.id
  from_port                    = 8080
  to_port                      = 8080
  ip_protocol                  = "tcp"
}

# ---- app SG ----
# ALB에서만 8080 수신
resource "aws_vpc_security_group_ingress_rule" "app_from_alb" {
  security_group_id            = aws_security_group.app.id
  description                  = "From ALB on 8080 only"
  referenced_security_group_id = aws_security_group.alb.id
  from_port                    = 8080
  to_port                      = 8080
  ip_protocol                  = "tcp"
}

# HTTPS 아웃바운드(NAT 경유: ECR/Secrets/Logs)
resource "aws_vpc_security_group_egress_rule" "app_https" {
  security_group_id = aws_security_group.app.id
  description       = "HTTPS egress via NAT (ECR/Secrets/Logs)"
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = 443
  to_port           = 443
  ip_protocol       = "tcp"
}

# DNS(UDP) -> VPC 리졸버
resource "aws_vpc_security_group_egress_rule" "app_dns_udp" {
  security_group_id = aws_security_group.app.id
  description       = "DNS UDP to VPC resolver"
  cidr_ipv4         = var.vpc_cidr
  from_port         = 53
  to_port           = 53
  ip_protocol       = "udp"
}

# DNS(TCP) -> VPC 리졸버
resource "aws_vpc_security_group_egress_rule" "app_dns_tcp" {
  security_group_id = aws_security_group.app.id
  description       = "DNS TCP to VPC resolver"
  cidr_ipv4         = var.vpc_cidr
  from_port         = 53
  to_port           = 53
  ip_protocol       = "tcp"
}

# RDS로 5432
resource "aws_vpc_security_group_egress_rule" "app_to_data" {
  security_group_id            = aws_security_group.app.id
  description                  = "To RDS on 5432"
  referenced_security_group_id = aws_security_group.data.id
  from_port                    = 5432
  to_port                      = 5432
  ip_protocol                  = "tcp"
}

# ---- data SG ----
# 앱에서만 5432 수신
resource "aws_vpc_security_group_ingress_rule" "data_from_app" {
  security_group_id            = aws_security_group.data.id
  description                  = "From app on 5432 only"
  referenced_security_group_id = aws_security_group.app.id
  from_port                    = 5432
  to_port                      = 5432
  ip_protocol                  = "tcp"
}
# data SG 아웃바운드 규칙 없음 — RDS는 능동 아웃바운드가 필요 없다.
