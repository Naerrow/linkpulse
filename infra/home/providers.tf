# API 토큰은 환경 변수 PROXMOX_VE_API_TOKEN으로만 준다("<사용자>@<영역>!<토큰 ID>=<비밀>"). tfvars·state에 남지 않는다.
# 입력은 화면에 찍지 않는다: read -rs "PROXMOX_VE_API_TOKEN?토큰: "; export PROXMOX_VE_API_TOKEN(런북 home-site.md).
# 이 스택은 API만 쓴다(SSH 없음) — import_from 디스크 가져오기와 download-url은 API로 된다(공급자 문서 "When is SSH Required?").
provider "proxmox" {
  endpoint = var.proxmox_endpoint
  insecure = var.proxmox_insecure
}
