# VM 설정·변수 검증·인벤토리 단언(plan 0017 A3). Proxmox에 닿지 않는다(가짜 공급자).
#   cd infra/home && terraform init -backend=false && terraform test
# fixture-key.pub는 이 시험에만 쓰는 공개 키다(개인 키는 버렸다).
# 단언은 VM을 키로 하나씩 가리킨다. VM 객체 전체(민감 속성 password가 든)를 반복하면 단언이 실패할 때
# Terraform이 값을 출력하다 패닉한다("value has marks", ops-gotchas G-23).

mock_provider "proxmox" {}

variables {
  proxmox_endpoint    = "https://192.168.0.10:8006/"
  node_name           = "pve"
  gateway             = "192.168.0.1"
  k3s_ip              = "192.168.0.50"
  pg_ip               = "192.168.0.51"
  ssh_public_key_path = "tests/fixture-key.pub"
}

run "vms" {
  command = plan

  assert {
    condition     = proxmox_virtual_environment_vm.vm["lp-k3s-1"].cpu[0].type == "x86-64-v2-AES" && proxmox_virtual_environment_vm.vm["lp-pg-1"].cpu[0].type == "x86-64-v2-AES"
    error_message = "VM cpu.type이 x86-64-v2-AES가 아니다(공급자 기본 qemu64는 AES-NI가 빠진다)"
  }
  assert {
    condition = (proxmox_virtual_environment_vm.vm["lp-k3s-1"].cpu[0].cores == 4 && proxmox_virtual_environment_vm.vm["lp-k3s-1"].memory[0].dedicated == 8192 &&
      proxmox_virtual_environment_vm.vm["lp-pg-1"].cpu[0].cores == 2 && proxmox_virtual_environment_vm.vm["lp-pg-1"].memory[0].dedicated == 2048 &&
    proxmox_virtual_environment_vm.vm["lp-k3s-1"].disk[0].size == 40 && proxmox_virtual_environment_vm.vm["lp-pg-1"].disk[0].size == 20)
    error_message = "VM 크기가 설계(k3s 4c/8G/40G, pg 2c/2G/20G)와 다르다"
  }
  assert {
    condition = (proxmox_virtual_environment_vm.vm["lp-k3s-1"].initialization[0].ip_config[0].ipv4[0].address == "192.168.0.50/24" &&
      proxmox_virtual_environment_vm.vm["lp-pg-1"].initialization[0].ip_config[0].ipv4[0].address == "192.168.0.51/24" &&
      proxmox_virtual_environment_vm.vm["lp-k3s-1"].initialization[0].ip_config[0].ipv4[0].gateway == "192.168.0.1" &&
    proxmox_virtual_environment_vm.vm["lp-pg-1"].initialization[0].ip_config[0].ipv4[0].gateway == "192.168.0.1")
    error_message = "고정 IP·게이트웨이가 변수와 다르다"
  }
  # 에이전트를 켜되 IP를 기다리지 않는다(ansible 전에는 에이전트가 없어 apply가 15분 막힌다).
  assert {
    condition = (proxmox_virtual_environment_vm.vm["lp-k3s-1"].agent[0].enabled && proxmox_virtual_environment_vm.vm["lp-k3s-1"].agent[0].wait_for_ip[0].disabled && proxmox_virtual_environment_vm.vm["lp-k3s-1"].stop_on_destroy &&
    proxmox_virtual_environment_vm.vm["lp-pg-1"].agent[0].enabled && proxmox_virtual_environment_vm.vm["lp-pg-1"].agent[0].wait_for_ip[0].disabled && proxmox_virtual_environment_vm.vm["lp-pg-1"].stop_on_destroy)
    error_message = "에이전트 IP 대기를 끄지 않았거나 stop_on_destroy가 아니다"
  }
  assert {
    condition = (length(proxmox_virtual_environment_vm.vm["lp-k3s-1"].initialization[0].user_account[0].keys) == 1 && length(proxmox_virtual_environment_vm.vm["lp-pg-1"].initialization[0].user_account[0].keys) == 1 &&
    nonsensitive(proxmox_virtual_environment_vm.vm["lp-k3s-1"].initialization[0].user_account[0].password == null && proxmox_virtual_environment_vm.vm["lp-pg-1"].initialization[0].user_account[0].password == null))
    error_message = "VM에 SSH 공개 키 하나만 넣어야 한다(비밀번호 없음)"
  }
  assert {
    condition     = proxmox_download_file.ubuntu.checksum_algorithm == "sha256" && length(proxmox_download_file.ubuntu.checksum) == 64 && can(regex("/release-[0-9]{8}/", proxmox_download_file.ubuntu.url))
    error_message = "클라우드 이미지가 날짜 고정 URL·체크섬이 아니다"
  }
  assert {
    condition = (yamldecode(output.ansible_inventory).all.children.k3s.hosts["lp-k3s-1"].ansible_host == "192.168.0.50" &&
      yamldecode(output.ansible_inventory).all.children.postgres.hosts["lp-pg-1"].ansible_host == "192.168.0.51" &&
    yamldecode(output.ansible_inventory).all.vars.lan_cidr == "192.168.0.0/24")
    error_message = "ansible 인벤토리가 VM IP·LAN 대역과 다르다"
  }
}

run "rejects_ip_outside_lan" {
  command = plan
  variables {
    k3s_ip = "10.42.0.5"
  }
  expect_failures = [var.k3s_ip]
}

run "rejects_same_ip" {
  command = plan
  variables {
    pg_ip = "192.168.0.50"
  }
  expect_failures = [var.pg_ip]
}
