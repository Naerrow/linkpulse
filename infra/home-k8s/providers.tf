# 집 k3s에만 붙는다. kubeconfig는 ansible k3s 역할이 맥에 쓴 파일이고, 컨텍스트 이름을 고정한다 —
# 경로를 잘못 줘서 EKS kubeconfig를 읽으면 이 컨텍스트가 없어 plan이 멈춘다(운영 클러스터를 바꾸지 않는다).
provider "kubernetes" {
  config_path    = pathexpand(var.kubeconfig_path)
  config_context = local.kube_context
}

provider "helm" {
  kubernetes = {
    config_path    = pathexpand(var.kubeconfig_path)
    config_context = local.kube_context
  }
}
