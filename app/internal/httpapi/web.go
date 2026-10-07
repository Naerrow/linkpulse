package httpapi

import (
	"io/fs"
	"net/http"

	"github.com/Naerrow/linkpulse/app/internal/web"
)

// pageCSP는 화면 응답의 Content-Security-Policy다(plan 0011).
// 관리자 토큰이 브라우저 localStorage에 있으므로 스크립트 출처를 이 서버로 못박는다 —
// 인라인 스크립트·외부 스크립트·다른 사이트의 프레임 삽입이 전부 막힌다.
const pageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// staticTypes는 /static/에서 내보내는 파일과 그 Content-Type이다. 목록에 없는 이름은 404다.
var staticTypes = map[string]string{
	"app.js":    "text/javascript; charset=utf-8",
	"style.css": "text/css; charset=utf-8",
}

// webHandler는 내장 화면 파일을 서빙한다.
type webHandler struct {
	files fs.FS
}

func newWebHandler() webHandler {
	return webHandler{files: web.Files}
}

// index는 GET / — 화면 한 장을 돌려준다.
func (h webHandler) index(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, "index.html", "text/html; charset=utf-8")
}

// static은 GET /static/{file} — 화면의 스크립트·스타일을 돌려준다.
func (h webHandler) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	contentType, ok := staticTypes[name]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "존재하지 않는 파일입니다")
		return
	}
	h.serve(w, r, name, contentType)
}

func (h webHandler) serve(w http.ResponseWriter, r *http.Request, name, contentType string) {
	data, err := fs.ReadFile(h.files, name)
	if err != nil {
		writeInternalError(w, r, err) // 내장 파일이라 빌드가 됐다면 일어나지 않는다.
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", contentType)
	hdr.Set("Content-Security-Policy", pageCSP)
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Referrer-Policy", "no-referrer")
	// 배포하면 바로 새 화면이 보이도록 매번 다시 확인하게 한다(파일이 작아 비용이 없다).
	hdr.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
