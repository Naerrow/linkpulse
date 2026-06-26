locals {
  name_prefix = "${var.project}-${var.environment}" # 예: linkpulse-prod

  # PostgreSQL 메이저 버전. 파라미터 그룹 family("postgres16" 등)와 RDS 인스턴스의 정합을
  # 자동으로 맞춘다. var.postgres_version이 "16"이든 "16.4"든 메이저만 뽑는다.
  postgres_major = split(".", var.postgres_version)[0]

  # 가용영역 2개를 동적으로 선택한다(하드코딩 회피). ALB·RDS 다중 AZ 요건을 충족한다.
  azs = slice(data.aws_availability_zones.available.names, 0, 2)
}
