# 집 사이트 바탕 (plan 0017, ADR 0010): Proxmox VM 두 대. 설치·설정은 ansible/(k3s·PostgreSQL)이 한다.
#
#   lp-k3s-1  4 vCPU·8GiB·40GiB  k3s 서버 하나(단일 노드). EKS 노드 3대의 요청 합(약 1.75 vCPU·3GiB)과 HPA 최대 6이 들어간다.
#   lp-pg-1   2 vCPU·2GiB·20GiB  PostgreSQL 16. 클러스터 밖 VM이라 클러스터를 다시 만들어도 데이터가 남는다(RDS와 같은 자리).
#   합 6 vCPU·10GiB — 16GB 서버에서 Proxmox 호스트에 여유를 남긴다.
#
# 순서: 이 스택 → ansible(common → postgres → k3s) → infra/home-k8s. 지울 때는 역순(런북 home-site.md).

locals {
  vms = {
    "lp-k3s-1" = { role = "k3s", cores = 4, memory_mib = 8192, disk_gib = 40, ip = var.k3s_ip }
    "lp-pg-1"  = { role = "postgres", cores = 2, memory_mib = 2048, disk_gib = 20, ip = var.pg_ip }
  }
}

# Ubuntu 24.04 LTS 클라우드 이미지. 날짜를 고정한 URL과 SHA256SUMS의 체크섬이다(2026-09-26 릴리스, 2주 규칙).
# 이미지 이름은 .img지만 내용은 qcow2다. import 형식은 확장자로 형식을 알아서 .qcow2로 저장한다(공급자 문서).
resource "proxmox_download_file" "ubuntu" {
  node_name          = var.node_name
  datastore_id       = var.image_datastore_id
  content_type       = "import"
  url                = "https://cloud-images.ubuntu.com/releases/noble/release-20260926/ubuntu-24.04-server-cloudimg-amd64.img"
  file_name          = "ubuntu-24.04-server-cloudimg-amd64-20260926.qcow2"
  checksum           = "6a81c37564db9b1ee84e141922625e1d7c5b389b99bb3c572e0243607d5bb4d2"
  checksum_algorithm = "sha256"
  # 약 600MB. 기본 10분이 집 회선에서 모자랄 수 있다.
  upload_timeout = 1800
}

resource "proxmox_virtual_environment_vm" "vm" {
  for_each = local.vms

  name        = each.key
  description = "linkpulse home site ${each.value.role} (infra/home, plan 0017)"
  node_name   = var.node_name
  # Proxmox가 늘 정렬해서 돌려준다 — 정렬해 두지 않으면 plan마다 차이가 난다.
  tags    = sort(["linkpulse", each.value.role])
  on_boot = true

  # 게스트 에이전트는 ansible common이 설치·시작한다. 그 전에는 에이전트가 없으므로:
  #   - IP를 기다리지 않는다(고정 IP라 출력은 변수에서 온다) — 기다리면 apply가 15분 막힌다.
  #   - 지울 때는 종료(에이전트 필요) 대신 정지한다.
  # ansible 전에 Proxmox 화면에서 Shutdown을 누르면 에이전트를 기다리다 시간이 초과된다(그때는 Stop).
  agent {
    enabled = true
    wait_for_ip {
      disabled = true
    }
  }
  stop_on_destroy = true

  # 공급자 기본값 qemu64는 AES-NI 등이 빠진다. x86-64-v2-AES는 Proxmox 8의 기본값이다.
  cpu {
    cores = each.value.cores
    type  = "x86-64-v2-AES"
  }

  memory {
    dedicated = each.value.memory_mib
  }

  scsi_hardware = "virtio-scsi-single"
  disk {
    datastore_id = var.vm_datastore_id
    import_from  = proxmox_download_file.ubuntu.id
    interface    = "scsi0"
    size         = each.value.disk_gib
    iothread     = true
    discard      = "on"
  }

  network_device {
    bridge = var.network_bridge
  }

  operating_system {
    type = "l26"
  }

  # 클라우드 이미지는 직렬 콘솔로 부팅 메시지를 낸다(Proxmox 위키 Cloud-Init Support). SSH가 안 될 때 Proxmox 콘솔로 본다.
  serial_device {}
  vga {
    type = "serial0"
  }

  # cloud-init: 고정 IP와 SSH 공개 키만 넣는다(API로 된다 — 스니펫 파일·SSH가 필요 없다).
  # DNS는 적지 않는다 — Proxmox가 호스트의 DNS 설정을 넣는다.
  initialization {
    datastore_id = var.vm_datastore_id
    ip_config {
      ipv4 {
        address = "${each.value.ip}/${var.lan_prefix_length}"
        gateway = var.gateway
      }
    }
    user_account {
      username = var.vm_user
      keys     = [trimspace(file(pathexpand(var.ssh_public_key_path)))]
    }
    # 첫 부팅 패키지 업그레이드를 끈다 — 이미지가 날짜로 고정돼 있고, 켜면 다시 만들기(판정 (f)) 시간이 들쭉날쭉하다.
    # 보안 업데이트는 Ubuntu 기본 unattended-upgrades가 한다.
    upgrade = false
  }
}
