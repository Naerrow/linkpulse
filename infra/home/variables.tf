# ---- Proxmox ----
variable "proxmox_endpoint" {
  description = "Proxmox API 주소(https://<서버 IP>:8006/). /api2/json은 붙이지 않는다."
  type        = string
}

variable "proxmox_insecure" {
  description = "Proxmox API 인증서 검증을 건너뛸지. Proxmox 기본 인증서가 자체 서명이라 기본 true다. 집 LAN 안에서만 붙는다."
  type        = bool
  default     = true
}

variable "node_name" {
  description = "Proxmox 노드 이름(서버 셸의 hostname)."
  type        = string
}

variable "image_datastore_id" {
  description = "클라우드 이미지를 받을 저장소. 허용 콘텐츠에 Import가 켜져 있어야 한다(Datacenter > Storage)."
  type        = string
  default     = "local"
}

variable "vm_datastore_id" {
  description = "VM 디스크·cloud-init 디스크를 둘 저장소."
  type        = string
  default     = "local-lvm"
}

variable "network_bridge" {
  description = "VM NIC를 붙일 브리지(집 LAN)."
  type        = string
  default     = "vmbr0"
}

# ---- 집 LAN ----
# 집 LAN(192.168.0.x)은 AWS VPC 10.0.0.0/16, k3s 파드 10.42.0.0/16·서비스 10.43.0.0/16과 겹치지 않는다(0018 라우팅의 전제).
variable "gateway" {
  description = "공유기(게이트웨이) IPv4 주소."
  type        = string

  validation {
    condition     = can(regex("^((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\\.){3}(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])$", var.gateway))
    error_message = "gateway는 IPv4 주소여야 한다."
  }
}

variable "lan_prefix_length" {
  description = "집 LAN 접두 길이(255.255.255.0이면 24)."
  type        = number
  default     = 24
}

# 두 IP는 공유기 DHCP 범위 밖이어야 한다(런북 home-site.md 0단계에서 확인). k3s 노드 IP·pg_hba가 이 주소에 묶인다.
variable "k3s_ip" {
  description = "k3s VM 고정 IPv4(공유기 DHCP 범위 밖)."
  type        = string

  validation {
    condition     = can(cidrhost("${var.k3s_ip}/32", 0)) && cidrhost("${var.k3s_ip}/${var.lan_prefix_length}", 0) == cidrhost("${var.gateway}/${var.lan_prefix_length}", 0)
    error_message = "k3s_ip는 게이트웨이와 같은 LAN의 IPv4 주소여야 한다."
  }
}

variable "pg_ip" {
  description = "Postgres VM 고정 IPv4(공유기 DHCP 범위 밖)."
  type        = string

  validation {
    condition     = can(cidrhost("${var.pg_ip}/32", 0)) && cidrhost("${var.pg_ip}/${var.lan_prefix_length}", 0) == cidrhost("${var.gateway}/${var.lan_prefix_length}", 0) && var.pg_ip != var.k3s_ip
    error_message = "pg_ip는 게이트웨이와 같은 LAN의 IPv4 주소이고 k3s_ip와 달라야 한다."
  }
}

# ---- VM 접속 ----
variable "ssh_public_key_path" {
  description = "VM 사용자에 넣을 SSH 공개 키(맥). 비밀번호 로그인은 만들지 않는다."
  type        = string
  default     = "~/.ssh/id_ed25519.pub"
}

variable "vm_user" {
  description = "cloud-init이 만드는 VM 사용자(sudo 가능). Ansible이 이 사용자로 붙는다."
  type        = string
  default     = "ubuntu"
}
