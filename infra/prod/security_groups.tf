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

# RDS: 앱(EKS 노드·파드)에서만 5432 수신, 아웃바운드 없음
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

# 앱 파드로 전달. VPC CNI라 파드는 노드의 SG(EKS 클러스터 SG)를 쓴다(plan 0014).
resource "aws_vpc_security_group_egress_rule" "alb_to_app" {
  security_group_id            = aws_security_group.alb.id
  description                  = "To app pods on 8080 (EKS cluster SG)"
  referenced_security_group_id = local.cluster_sg_id
  from_port                    = 8080
  to_port                      = 8080
  ip_protocol                  = "tcp"
}

# ---- EKS 클러스터 SG (plan 0014) ----
# EKS가 만든 SG라 리소스 자체는 Terraform이 소유하지 않고 규칙만 더한다.
# ECS 시절 app SG의 아웃바운드 제한(HTTPS/DNS/DB만)은 옮기지 않는다 — 클러스터 SG는 모든 egress를 허용한다.
# 노드 전용 SG로 제한을 지키려면 컨트롤 플레인·노드 간·웹훅 규칙을 직접 관리해야 해서, 노드 2대 구성에서는
# 전체 허용을 받아들였다(ADR 0007). 수신은 여전히 ALB에서 8080만 더한다(나머지는 EKS 기본 규칙 = 같은 SG끼리).
resource "aws_vpc_security_group_ingress_rule" "cluster_from_alb" {
  security_group_id            = local.cluster_sg_id
  description                  = "From ALB on 8080 only (app pods)"
  referenced_security_group_id = aws_security_group.alb.id
  from_port                    = 8080
  to_port                      = 8080
  ip_protocol                  = "tcp"
}

# ---- data SG ----
# 앱(EKS 클러스터 SG = 노드·파드)에서만 5432 수신
resource "aws_vpc_security_group_ingress_rule" "data_from_app" {
  security_group_id            = aws_security_group.data.id
  description                  = "From app (EKS cluster SG) on 5432 only"
  referenced_security_group_id = local.cluster_sg_id
  from_port                    = 5432
  to_port                      = 5432
  ip_protocol                  = "tcp"
}
# data SG 아웃바운드 규칙 없음 — RDS는 능동 아웃바운드가 필요 없다.
