package httpapi

import "net/http"

// NewRouter는 애플리케이션의 전체 HTTP 라우팅을 구성해 공통 미들웨어로 감싼 핸들러를 반환한다.
//
// 라우팅은 표준 net/http.ServeMux를 쓴다(외부 의존성 0). Go 1.22+의 메서드·경로 패턴을
// 사용하며, 더 구체적인 패턴이 우선하므로 이후 추가될 리다이렉트용 와일드카드 "GET /{code}"가
// 여기 등록된 "/healthz" 등 예약 경로를 가리지 않는다.
func NewRouter() http.Handler {
	mux := http.NewServeMux()

	// 헬스 체크. DB 등 의존성 검사는 도입 시점에 readyz에 연결한다.
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", handleReadyz)

	return withMiddleware(mux)
}
