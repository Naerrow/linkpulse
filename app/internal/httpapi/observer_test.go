package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Naerrow/linkpulse/app/internal/links"
)

// observed는 지표 훅이 받은 요청 하나다.
type observed struct {
	route, method string
	status        int
}

// fakeObserver는 RequestObserver 호출을 모은다.
type fakeObserver struct {
	mu   sync.Mutex
	seen []observed
}

func (f *fakeObserver) ObserveRequest(route, method string, status int, _ time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, observed{route, method, status})
}

func (f *fakeObserver) last(t *testing.T) observed {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		t.Fatal("지표 훅이 불리지 않았다")
	}
	return f.seen[len(f.seen)-1]
}

// newObservedRouter는 지표 훅을 끼운 인메모리 라우터다.
func newObservedRouter(obs RequestObserver, rl RateLimitConfig) http.Handler {
	repo := links.NewMemoryRepository()
	return NewRouter(RouterDeps{
		Links:            links.NewService(repo, 7, &recordedClicks{}),
		Requests:         links.NewRequestService(repo, 7),
		AdminTokenSHA256: testAdminTokenHash(),
		BaseURL:          testBaseURL,
		RateLimit:        rl,
		Observer:         obs,
	})
}

// TestRouteLabelThroughMiddleware는 미들웨어 체인을 다 지난 요청의 route 라벨이 mux 패턴인지 본다(plan 0015).
// 바깥 requestLogger가 next 뒤에 r.Pattern을 읽는 전제를 지킨다 — 체인 중간에서 요청을 복제하면 모두 none이 되어 실패한다.
// 원시 경로(코드·요청 ID)가 라벨에 들어가지 않는 것도 본다.
func TestRouteLabelThroughMiddleware(t *testing.T) {
	obs := &fakeObserver{}
	router := newObservedRouter(obs, RateLimitConfig{Disabled: true})
	link := createLink(t, router, "https://example.com")

	cases := []struct {
		method, path string
		want         observed
	}{
		{http.MethodGet, "/" + link.Code, observed{"GET /{code}", "GET", http.StatusFound}},
		{http.MethodGet, "/api/links/" + link.Code, observed{"GET /api/links/{code}", "GET", http.StatusOK}},
		{http.MethodGet, "/api/requests/req-secret-id", observed{"GET /api/requests/{id}", "GET", http.StatusNotFound}},
		{http.MethodGet, "/healthz", observed{"GET /healthz", "GET", http.StatusOK}},
		// mux가 맞추지 못한 요청: 패턴 없음(404)과 메서드 불일치(405).
		{http.MethodGet, "/a/b/c", observed{"none", "GET", http.StatusNotFound}},
		{http.MethodDelete, "/api/links", observed{"none", "DELETE", http.StatusMethodNotAllowed}},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		got := obs.last(t)
		if got != c.want {
			t.Errorf("%s %s → %+v, want %+v", c.method, c.path, got, c.want)
		}
		if strings.Contains(got.route, link.Code) || strings.Contains(got.route, "req-secret-id") {
			t.Errorf("%s %s: 원시 경로가 route 라벨에 들어갔다: %q", c.method, c.path, got.route)
		}
	}
}

// TestRouteLabelRateLimited는 mux 앞에서 끝난 요청(429)이 none으로 세어지는지 본다.
func TestRouteLabelRateLimited(t *testing.T) {
	obs := &fakeObserver{}
	router := newObservedRouter(obs, RateLimitConfig{ReadPerMin: 60, ReadBurst: 1})

	doReq(router, http.MethodGet, "/somecode", "", "198.51.100.7")
	rec := doReq(router, http.MethodGet, "/somecode", "", "198.51.100.7")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("두 번째 요청 status = %d, want 429", rec.Code)
	}
	if got, want := obs.last(t), (observed{"none", "GET", http.StatusTooManyRequests}); got != want {
		t.Errorf("429 → %+v, want %+v", got, want)
	}
}

// TestPublicRouterHasNoMetrics는 공개 핸들러가 지표·pprof를 내지 않는지 본다(plan 0015 판정 (a)).
// 지표는 내부 포트(9090)에만 있다 — ALB가 8080만 보므로 이것이 밖으로 새지 않는 이유다.
func TestPublicRouterHasNoMetrics(t *testing.T) {
	router := newObservedRouter(&fakeObserver{}, RateLimitConfig{Disabled: true})
	for _, path := range []string{"/metrics", "/debug/pprof/", "/debug/pprof/profile"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		body := rec.Body.String()
		if rec.Code == http.StatusOK || strings.Contains(body, "# HELP") || strings.Contains(body, "go_goroutines") || strings.Contains(body, "profile") {
			t.Errorf("공개 %s = %d, 본문에 지표·pprof가 있다: %.80q", path, rec.Code, body)
		}
	}
}
