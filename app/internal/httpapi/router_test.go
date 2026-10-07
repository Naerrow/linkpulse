package httpapi

import (
	"crypto/sha256"
	"net/http"

	"github.com/Naerrow/linkpulse/app/internal/links"
)

// testBaseURL은 테스트에서 short_url을 검증할 때 쓰는 고정 기준 주소다.
const testBaseURL = "http://short.test"

// newTestRouter는 인메모리 저장소를 끼운 라우터를 만든다(테스트 공용 헬퍼).
// Readiness는 nil이라 readyz는 항상 준비됨으로 응답한다.
// 레이트리밋은 비활성으로 명시해 핸들러 단위 테스트가 리밋과 결합하지 않게 한다.
func newTestRouter() http.Handler {
	repo := links.NewMemoryRepository()
	return NewRouter(RouterDeps{
		Links:            links.NewService(repo, 7),
		Requests:         links.NewRequestService(repo, 7),
		AdminTokenSHA256: testAdminTokenHash(),
		BaseURL:          testBaseURL,
		RateLimit:        RateLimitConfig{Disabled: true},
	})
}

// testAdminToken은 테스트 라우터의 관리자 토큰이다. 라우터에는 해시만 들어간다(운영과 같은 형태).
const testAdminToken = "test-admin-token"

func testAdminTokenHash() []byte {
	sum := sha256.Sum256([]byte(testAdminToken))
	return sum[:]
}

// asAdmin은 요청에 관리자 토큰을 붙인다.
func asAdmin(req *http.Request) *http.Request {
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	return req
}
