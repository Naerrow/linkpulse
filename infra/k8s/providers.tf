provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project     = var.project
      Environment = var.environment
      ManagedBy   = "terraform"
    }
  }
}

# 클러스터는 infra/prod가 만든다. 이름으로 찾는다(원격 state를 읽지 않는다 — 두 스택이 이름 규칙으로만 묶인다).
data "aws_eks_cluster" "main" {
  name = local.cluster_name
}

# 인증 토큰은 실행할 때마다 aws CLI로 받는다(exec). 데이터 소스 토큰은 15분 뒤 만료돼 긴 apply가 중간에 끊긴다.
# 이 스택을 돌리는 주체는 클러스터를 만든 운영자다(infra/prod eks.tf의 액세스 엔트리).
provider "kubernetes" {
  host                   = data.aws_eks_cluster.main.endpoint
  cluster_ca_certificate = base64decode(data.aws_eks_cluster.main.certificate_authority[0].data)
  exec {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args        = ["eks", "get-token", "--cluster-name", local.cluster_name, "--region", var.region]
  }
}

provider "helm" {
  kubernetes = {
    host                   = data.aws_eks_cluster.main.endpoint
    cluster_ca_certificate = base64decode(data.aws_eks_cluster.main.certificate_authority[0].data)
    exec = {
      api_version = "client.authentication.k8s.io/v1beta1"
      command     = "aws"
      args        = ["eks", "get-token", "--cluster-name", local.cluster_name, "--region", var.region]
    }
  }
}
