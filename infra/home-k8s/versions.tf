terraform {
  required_version = ">= 1.10.0"

  # infra/k8s와 같은 공급자다. AWS 공급자는 없다 — AWS 쪽 값은 infra/home-aws의 원격 state에서 읽는다(main.tf).
  required_providers {
    helm = {
      source  = "hashicorp/helm"
      version = "~> 3.3"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 3.3"
    }
  }
}
