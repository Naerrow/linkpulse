terraform {
  required_version = ">= 1.10.0"

  required_providers {
    # 0.x라 버전을 정확히 고정한다(2주 규칙: v0.114.0은 2026-09-21 게시, v0.115.0은 10-02라 아직 2주가 안 됐다).
    proxmox = {
      source  = "bpg/proxmox"
      version = "0.114.0"
    }
  }
}
