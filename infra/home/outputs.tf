output "k3s_ip" {
  description = "k3s VM 고정 IP. kubeconfig의 서버 주소와 pg_hba의 허용 주소가 된다."
  value       = var.k3s_ip
}

output "pg_ip" {
  description = "Postgres VM 고정 IP. infra/home-k8s의 DB_HOST."
  value       = var.pg_ip
}

# ansible 인벤토리. 파일로 받아 쓴다(집 IP가 들어 있어 git에 넣지 않는다):
#   terraform -chdir=infra/home output -raw ansible_inventory > infra/home/ansible/inventory.yml
output "ansible_inventory" {
  description = "infra/home/ansible 인벤토리(YAML)."
  value = yamlencode({
    all = {
      vars = {
        ansible_user = var.vm_user
        # pg VM ufw가 SSH(22)를 이 대역에서만 받는다.
        lan_cidr = "${cidrhost("${var.gateway}/${var.lan_prefix_length}", 0)}/${var.lan_prefix_length}"
      }
      children = {
        k3s      = { hosts = { "lp-k3s-1" = { ansible_host = var.k3s_ip } } }
        postgres = { hosts = { "lp-pg-1" = { ansible_host = var.pg_ip } } }
      }
    }
  })
}
