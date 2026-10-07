package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// adminAuth는 관리자 토큰 검사기다(plan 0011).
//
// 서버는 토큰 원문이 아니라 SHA-256 해시만 가진다. 토큰은 운영자가 외우는 8자 이상의 비밀번호이고,
// 해시는 운영자의 AWS 계정(태스크 정의·tfstate)과 로컬 tfvars에만 있다. 온라인 대입은 레이트리밋이 막는다.
// 그래서 Secrets Manager 없이 평문 env로 줄 수 있다(비용 0).
// 해시가 비어 있으면 관리자 기능 전체가 꺼진다(fail-closed) — 설정이 빠졌을 때 "아무나 관리자"가 되지 않는다.
type adminAuth struct {
	hash []byte // sha256(토큰). nil이면 꺼짐
}

// enabled는 관리자 기능이 켜져 있는지 알려 준다.
func (a adminAuth) enabled() bool {
	return len(a.hash) == sha256.Size
}

// allowed는 요청의 Bearer 토큰이 관리자 토큰인지 확인한다. 비교는 상수 시간이다.
func (a adminAuth) allowed(r *http.Request) bool {
	if !a.enabled() {
		return false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(sum[:], a.hash) == 1
}

// require는 관리자만 통과시키는 핸들러 래퍼다. 실패 사유(토큰 없음·틀림·기능 꺼짐)는 구분하지 않는다.
func (a adminAuth) require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.allowed(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="linkpulse-admin"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "관리자 토큰이 필요합니다")
			return
		}
		next(w, r)
	}
}
