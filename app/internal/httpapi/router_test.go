package httpapi

import (
	"net/http"

	"github.com/Naerrow/linkpulse/app/internal/links"
)

// testBaseURL은 테스트에서 short_url을 검증할 때 쓰는 고정 기준 주소다.
const testBaseURL = "http://short.test"

// newTestRouter는 인메모리 저장소를 끼운 라우터를 만든다(테스트 공용 헬퍼).
// Readiness는 nil이라 readyz는 항상 준비됨으로 응답한다.
func newTestRouter() http.Handler {
	svc := links.NewService(links.NewMemoryRepository(), 7)
	return NewRouter(RouterDeps{Links: svc, BaseURL: testBaseURL})
}
