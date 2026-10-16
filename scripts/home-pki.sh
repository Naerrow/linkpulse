#!/usr/bin/env bash
# 집 사이트 Roles Anywhere용 PKI (plan 0017 설계 4, ADR 0010).
#
#   scripts/home-pki.sh                     CA·말단이 없으면 만들고, 있으면 그대로 두고 검사만 한다
#   scripts/home-pki.sh --renew-leaf        말단(app.crt·app.key)만 다시 발급한다(만료 전 갱신, 런북 home-site.md)
#   scripts/home-pki.sh --test-leaf <CN>    같은 CA로 다른 CN의 시험 말단을 만든다(판정 (c): 기대 결과 AccessDenied)
#   scripts/home-pki.sh --check-ca <crt>    CA 인증서 조건만 검사한다
#   scripts/home-pki.sh --check-leaf <crt> <ca.crt>   말단 인증서 조건만 검사한다
#
# 디렉터리는 저장소 밖 ~/.linkpulse-home/pki다(PKI_DIR로 바꿀 수 있다). 개인 키는 화면에 찍지 않는다.
#   ca.key·ca.crt   EC P-256, 5년, CA:true, keyCertSign·cRLSign. CA 키는 맥 밖으로 내보내지 않는다(사람이 오프라인 백업).
#   app.key·app.crt EC P-256, 1년, CA:false, digitalSignature, CN linkpulse-home-app. 키는 k8s Secret으로만 클러스터에 들어간다.
# 조건은 Roles Anywhere 신뢰 모델(userguide trust-model.html)이다. 맞지 않으면 CreateSession이 거부되므로 만들 때 미리 막는다.
set -euo pipefail

PKI_DIR="${PKI_DIR:-$HOME/.linkpulse-home/pki}"
APP_CN="linkpulse-home-app" # infra/home-aws variables.tf app_cert_cn과 같아야 한다
CA_DAYS=1825
LEAF_DAYS=365

die() { echo "❌ $*" >&2; exit 1; }

# 말단은 여기서 만들고 검사를 통과한 뒤에만 제자리로 옮긴다. 실패로 끝나도 개인 키가 남지 않게 지운다.
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# genpkey의 PKCS#8 EC 키와 아래 출력 형식은 OpenSSL 3 이상 기준이다. macOS 기본 openssl(LibreSSL)은 쓰지 않는다.
openssl version | grep -qE '^OpenSSL [3-9]\.' || die "OpenSSL 3 이상이 필요하다(지금: $(openssl version)). brew install openssl@3 후 PATH 앞에 둔다"

# 인증서 공통 조건: X.509v3, 서명이 SHA-256 이상(MD5·SHA1 거부), Subject에 CN.
check_common() {
  local txt=$1 name=$2
  grep -q "Version: 3 (0x2)" <<<"$txt" || die "$name: X.509v3가 아니다"
  local sig
  sig=$(sed -n 's/^ *Signature Algorithm: //p' <<<"$txt" | head -1)
  grep -qiE 'sha(256|384|512)' <<<"$sig" || die "$name: 서명 알고리즘이 SHA-256 이상이 아니다($sig)"
  grep -qE '^ *Subject: .*CN *= *[^,]+' <<<"$txt" || die "$name: Subject에 CN이 없다"
}

# "X509v3 <확장>:" 다음 줄(값)을 꺼낸다.
ext_value() { grep -A1 "X509v3 $2:" <<<"$1" | tail -1 | sed 's/^ *//'; }

check_ca() {
  local crt=$1 txt
  txt=$(openssl x509 -in "$crt" -noout -text) || die "$crt: 인증서를 읽지 못했다"
  check_common "$txt" "CA $crt"
  [[ $(ext_value "$txt" "Basic Constraints") == CA:TRUE* ]] || die "CA $crt: Basic Constraints가 CA:TRUE가 아니다"
  ext_value "$txt" "Key Usage" | grep -q "Certificate Sign" || die "CA $crt: Key Usage에 Certificate Sign이 없다"
  echo "✅ CA 조건 통과: $crt (만료 $(openssl x509 -in "$crt" -noout -enddate | cut -d= -f2))"
}

check_leaf() {
  local crt=$1 ca=$2 txt
  txt=$(openssl x509 -in "$crt" -noout -text) || die "$crt: 인증서를 읽지 못했다"
  check_common "$txt" "말단 $crt"
  local bc
  bc=$(ext_value "$txt" "Basic Constraints")
  # 확장이 없어도 Roles Anywhere는 받지만, 이 스크립트가 만드는 말단은 늘 CA:FALSE를 적는다.
  [[ $bc == CA:FALSE* ]] || die "말단 $crt: Basic Constraints가 CA:FALSE가 아니다($bc)"
  ext_value "$txt" "Key Usage" | grep -q "Digital Signature" || die "말단 $crt: Key Usage에 Digital Signature가 없다"
  openssl verify -CAfile "$ca" "$crt" >/dev/null 2>&1 || die "말단 $crt: ${ca}가 서명한 인증서가 아니다"
  echo "✅ 말단 조건 통과: $crt ($(openssl x509 -in "$crt" -noout -subject), 만료 $(openssl x509 -in "$crt" -noout -enddate | cut -d= -f2))"
}

# 키와 인증서가 짝인지(공개 키 비교). 다르면 도우미가 서명은 해도 Roles Anywhere가 거부한다.
check_pair() {
  [[ "$(openssl pkey -in "$1" -pubout)" == "$(openssl x509 -in "$2" -noout -pubkey)" ]] || die "$1와 $2가 짝이 아니다"
}

new_key() { (umask 077 && openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$1" 2>/dev/null); }

make_ca() {
  new_key "$PKI_DIR/ca.key"
  openssl req -new -x509 -key "$PKI_DIR/ca.key" -sha256 -days "$CA_DAYS" -subj "/CN=linkpulse-home-ca" \
    -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" \
    -addext "subjectKeyIdentifier=hash" -out "$PKI_DIR/ca.crt"
}

# 말단 하나를 임시 디렉터리에 만들고 검사를 통과한 뒤에만 제자리로 옮긴다(갱신 중 실패해도 옛 말단이 남는다).
# 일련번호는 OpenSSL 3 이상의 기본값(난수)이다 — Roles Anywhere가 세션 이름에 쓴다.
make_leaf() {
  local cn=$1 key=$2 crt=$3 tmp="$TMP/leaf"
  rm -rf "$tmp" && mkdir "$tmp"
  new_key "$tmp/leaf.key"
  openssl req -new -key "$tmp/leaf.key" -subj "/CN=$cn" -out "$tmp/leaf.csr"
  printf '%s\n' "basicConstraints=critical,CA:FALSE" "keyUsage=critical,digitalSignature" \
    "subjectKeyIdentifier=hash" "authorityKeyIdentifier=keyid" >"$tmp/ext.cnf"
  openssl x509 -req -in "$tmp/leaf.csr" -CA "$PKI_DIR/ca.crt" -CAkey "$PKI_DIR/ca.key" \
    -sha256 -days "$LEAF_DAYS" -extfile "$tmp/ext.cnf" -out "$tmp/leaf.crt" 2>/dev/null
  check_leaf "$tmp/leaf.crt" "$PKI_DIR/ca.crt" >/dev/null
  check_pair "$tmp/leaf.key" "$tmp/leaf.crt"
  mv "$tmp/leaf.key" "$key"
  mv "$tmp/leaf.crt" "$crt"
  check_leaf "$crt" "$PKI_DIR/ca.crt"
}

case "${1:-}" in
  --check-ca) check_ca "${2:?CA 인증서 경로}"; exit 0 ;;
  --check-leaf) check_leaf "${2:?말단 인증서 경로}" "${3:?CA 인증서 경로}"; exit 0 ;;
esac

(umask 077 && mkdir -p "$PKI_DIR")

if [[ -e "$PKI_DIR/ca.key" ]]; then
  echo "CA가 이미 있다 — 덮어쓰지 않는다($PKI_DIR/ca.crt)"
else
  [[ -e "$PKI_DIR/ca.crt" ]] && die "ca.key 없이 ca.crt만 있다 — 손으로 정리한 뒤 다시 실행한다"
  # CA는 인자 없는 실행에서만 만든다. --renew-leaf·--test-leaf가 새 CA를 만들면 그 말단은 트러스트 앵커에 없는 CA가 서명해
  # CreateSession이 거부된다(맥을 바꿨거나 PKI_DIR이 틀렸거나 백업에서 ca.*가 빠진 경우).
  [[ -z "${1:-}" ]] || die "CA가 없다($PKI_DIR) — $1 모드는 트러스트 앵커에 올린 기존 CA로만 발급한다. PKI_DIR·백업을 확인한다"
  make_ca
  echo "CA를 만들었다. ca.key를 비밀번호 관리자 등 오프라인에 백업한다(분실하면 트러스트 앵커를 새 CA로 바꿔야 한다)"
fi
check_ca "$PKI_DIR/ca.crt"
check_pair "$PKI_DIR/ca.key" "$PKI_DIR/ca.crt"

case "${1:-}" in
  "")
    if [[ -e "$PKI_DIR/app.key" ]]; then
      echo "말단이 이미 있다 — 덮어쓰지 않는다(갱신은 --renew-leaf)"
      check_leaf "$PKI_DIR/app.crt" "$PKI_DIR/ca.crt"
      check_pair "$PKI_DIR/app.key" "$PKI_DIR/app.crt"
    else
      make_leaf "$APP_CN" "$PKI_DIR/app.key" "$PKI_DIR/app.crt"
    fi
    ;;
  --renew-leaf)
    make_leaf "$APP_CN" "$PKI_DIR/app.key" "$PKI_DIR/app.crt"
    echo "다음: 클러스터 Secret linkpulse-ra-cert 교체 → kubectl rollout restart(런북 home-site.md '인증서 갱신')"
    ;;
  --test-leaf)
    cn="${2:?시험 말단 CN}"
    [[ "$cn" != "$APP_CN" ]] || die "시험 말단 CN은 ${APP_CN}과 달라야 한다(신뢰 정책의 CN 조건을 시험한다)"
    [[ "$cn" =~ ^[A-Za-z0-9.-]+$ ]] || die "CN은 영문·숫자·.·-만 쓴다: $cn"
    make_leaf "$cn" "$PKI_DIR/test-$cn.key" "$PKI_DIR/test-$cn.crt"
    echo "시험 말단: $PKI_DIR/test-$cn.crt (판정 (c)가 끝나면 지운다)"
    ;;
  *) die "모르는 인자: $1 (사용법은 파일 머리 주석)" ;;
esac
