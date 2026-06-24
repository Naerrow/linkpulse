package httpapi

import (
	"net/http"

	"github.com/Naerrow/linkpulse/app/internal/links"
)

// NewRouter는 애플리케이션의 전체 HTTP 라우팅을 구성해 공통 미들웨어로 감싼 핸들러를 반환한다.
// 링크 서비스와 단축 URL 기준 주소(baseURL)를 주입받는다(의존성 주입).
//
// 라우팅은 표준 net/http.ServeMux를 쓴다(외부 의존성 0). Go 1.22+의 메서드·경로 패턴을
// 사용하며, 더 구체적인 패턴이 우선하므로 리다이렉트용 와일드카드 "GET /{code}"가
// "/healthz"·"/api/..." 같은 예약 경로를 가리지 않는다.
func NewRouter(svc *links.Service, baseURL string) http.Handler {
	mux := http.NewServeMux()
	lh := &linkHandler{svc: svc, baseURL: baseURL}

	// 헬스 체크. DB 등 의존성 검사는 도입 시점에 readyz에 연결한다.
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", handleReadyz)

	// 링크 API.
	mux.HandleFunc("POST /api/links", lh.create)
	mux.HandleFunc("GET /api/links/{code}", lh.stats)

	// 단축 코드 리다이렉트. 가장 덜 구체적인 패턴이라 위 예약 경로들이 우선한다.
	mux.HandleFunc("GET /{code}", lh.redirect)

	return withMiddleware(mux)
}
