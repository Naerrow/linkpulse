package httpapi

import (
	"context"
	"net/http"

	"github.com/Naerrow/linkpulse/app/internal/links"
)

// RouterDeps는 라우터 구성에 필요한 의존성 묶음이다(의존성 주입).
type RouterDeps struct {
	Links   *links.Service
	BaseURL string
	// Readiness는 레디니스 점검 함수다. nil이면 외부 의존성이 없다고 보고 항상 준비됨으로 응답한다.
	Readiness func(ctx context.Context) error
	// RateLimit은 레이트리밋 파라미터다. zero-value면 운영 기본값이 적용된다(RateLimitConfig 참고).
	RateLimit RateLimitConfig
}

// NewRouter는 애플리케이션의 전체 HTTP 라우팅을 구성해 공통 미들웨어로 감싼 핸들러를 반환한다.
//
// 라우팅은 표준 net/http.ServeMux를 쓴다(외부 의존성 0). Go 1.22+의 메서드·경로 패턴을
// 사용하며, 더 구체적인 패턴이 우선하므로 리다이렉트용 와일드카드 "GET /{code}"가
// "/healthz"·"/api/..." 같은 예약 경로를 가리지 않는다.
func NewRouter(d RouterDeps) http.Handler {
	mux := http.NewServeMux()
	lh := &linkHandler{svc: d.Links, baseURL: d.BaseURL}
	rh := &readinessHandler{check: d.Readiness}

	// 헬스 체크. readyz는 주입된 점검 함수로 DB 등 의존성 상태를 반영한다.
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", rh.handle)

	// 링크 API.
	mux.HandleFunc("POST /api/links", lh.create)
	mux.HandleFunc("GET /api/links/{code}", lh.stats)

	// 단축 코드 리다이렉트. 가장 덜 구체적인 패턴이라 위 예약 경로들이 우선한다.
	mux.HandleFunc("GET /{code}", lh.redirect)

	// 리미터 상태 맵은 여기서 1회 생성돼 모든 요청이 공유한다(요청당 재생성 금지).
	return withMiddleware(newRateLimit(d.RateLimit), mux)
}
