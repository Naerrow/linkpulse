package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIndexPage는 GET /가 화면과 보안 헤더를 돌려주는지 본다.
func TestIndexPage(t *testing.T) {
	rec := do(newTestRouter(), httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp != pageCSP {
		t.Errorf("CSP = %q, want %q", csp, pageCSP)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff 헤더가 없음")
	}
	if !strings.Contains(rec.Body.String(), `<script src="/static/app.js" defer></script>`) {
		t.Error("화면이 /static/app.js를 불러오지 않음")
	}
}

// TestStaticFiles는 목록의 파일만 서빙하고 나머지는 404인지 본다.
func TestStaticFiles(t *testing.T) {
	router := newTestRouter()

	for path, wantType := range map[string]string{
		"/static/app.js":    "text/javascript",
		"/static/style.css": "text/css",
	} {
		rec := do(router, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), wantType) {
			t.Errorf("%s = %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s에 nosniff가 없음", path)
		}
	}
	for _, path := range []string{"/static/index.html", "/static/web.go", "/static/missing.js"} {
		if rec := do(router, httptest.NewRequest(http.MethodGet, path, nil)); rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, rec.Code)
		}
	}
}

// TestAppJSAvoidsInnerHTML은 화면 스크립트가 innerHTML·eval을 쓰지 않는지 본다.
// 관리자 토큰이 localStorage에 있으므로, 방문자가 쓴 문자열이 HTML로 해석되는 순간이 곧 토큰 탈취다.
func TestAppJSAvoidsInnerHTML(t *testing.T) {
	body := do(newTestRouter(), httptest.NewRequest(http.MethodGet, "/static/app.js", nil)).Body.String()
	// 머리 주석의 경고 문장은 빼고 코드만 본다.
	code := body[strings.Index(body, "'use strict';"):]
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
		if strings.Contains(code, banned) {
			t.Errorf("app.js에 %s가 있음", banned)
		}
	}
}
