package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Naerrow/linkpulse/app/internal/links"
)

// captureLogs는 기본 로거를 버퍼로 바꾸고, 테스트가 끝나면 되돌린다.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// requestLines는 버퍼에 남은 "http request" 줄 수다.
func requestLines(buf *bytes.Buffer) int {
	return strings.Count(buf.String(), `"msg":"http request"`)
}

// statusHandler는 경로로 응답을 정한다. /slow는 늘 남기는 경계(100ms)보다 오래 걸린다.
func statusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
		case "/redirect":
			w.WriteHeader(http.StatusFound)
		case "/unauthorized":
			w.WriteHeader(http.StatusUnauthorized)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/error":
			w.WriteHeader(http.StatusInternalServerError)
		case "/slow":
			time.Sleep(slowRequestLog + 10*time.Millisecond)
			w.WriteHeader(http.StatusOK)
		}
	})
}

// TestRouterAccessLogSample은 라우터가 AccessLogSample을 실제로 쓰는지 본다(plan 0016 단계 2).
// N=1(기본)과 0(설정 안 함)이면 모든 요청이 남는다. N이 아주 크면 빠른 200은 빠지고 404는 남는다
// (200 50개 중 하나라도 뽑힐 확률은 5e-8).
func TestRouterAccessLogSample(t *testing.T) {
	newRouter := func(n int) http.Handler {
		return NewRouter(RouterDeps{
			Links:           links.NewService(links.NewMemoryRepository(), 7, &recordedClicks{}),
			BaseURL:         testBaseURL,
			RateLimit:       RateLimitConfig{Disabled: true},
			AccessLogSample: n,
		})
	}
	for _, n := range []int{0, 1} {
		buf := captureLogs(t)
		router := newRouter(n)
		for range 50 {
			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
		}
		if got := requestLines(buf); got != 50 {
			t.Errorf("AccessLogSample=%d: 요청 50개에 로그 %d줄, want 50", n, got)
		}
	}

	buf := captureLogs(t)
	router := newRouter(1_000_000_000)
	for range 50 {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	}
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/a/b/c", nil))
	if got := requestLines(buf); got != 1 || !strings.Contains(buf.String(), `"status":404`) {
		t.Errorf("AccessLogSample=1e9: 로그 %d줄, want 404 한 줄만 (%s)", got, buf.String())
	}
}

// TestAccessLogAlwaysKeepsErrorsAndSlow는 표본에서 빠지는 경우(keep=false)에도 4xx·5xx·100ms 이상 요청은 남고,
// 빠른 2xx·3xx만 빠지는지 본다. 표본 판정을 주입해 확률에 기대지 않는다.
// 401은 런북 P-3의 grep이, 5xx·느린 요청은 런북 Logs Insights 쿼리가 읽는다.
func TestAccessLogAlwaysKeepsErrorsAndSlow(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/ok", false},
		{"/redirect", false},
		{"/unauthorized", true},
		{"/missing", true},
		{"/error", true},
		{"/slow", true},
	}
	h := requestLogger(nil, func() bool { return false }, statusHandler())
	for _, c := range cases {
		buf := captureLogs(t)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, c.path, nil))
		if got := requestLines(buf) == 1; got != c.want {
			t.Errorf("%s: 로그 남음 = %v, want %v (%s)", c.path, got, c.want, buf.String())
		}
	}
}

// TestAccessLogKeepPicked는 표본으로 뽑히면(keep=true) 빠른 성공 요청도 남는지 본다 — 판정이 실제로 쓰인다.
func TestAccessLogKeepPicked(t *testing.T) {
	buf := captureLogs(t)
	h := requestLogger(nil, func() bool { return true }, statusHandler())
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ok", nil))
	if got := requestLines(buf); got != 1 {
		t.Errorf("뽑힌 200의 로그 %d줄, want 1", got)
	}
}

// TestAccessLogSampledOutStillCounted는 로그에서 빠진 요청도 지표에는 세어지는지 본다.
// 요청 수는 로그가 아니라 지표로 본다(plan 0016) — 지표가 표본을 따라가면 SLO 계산이 틀어진다.
func TestAccessLogSampledOutStillCounted(t *testing.T) {
	captureLogs(t)
	obs := &fakeObserver{}
	h := requestLogger(obs, func() bool { return false }, statusHandler())
	for range 3 {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ok", nil))
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.seen) != 3 {
		t.Errorf("지표 훅 호출 %d번, want 3(로그에서 빠진 요청도 센다)", len(obs.seen))
	}
}

// TestSampleKeeper는 N이 1 이하면 늘 남기고, N=100이면 일부만 남기는지 본다.
// 2,000번 중 한 번도 안 뽑히거나 전부 뽑힐 확률은 각각 약 2e-9·1e-4000이라 사실상 흔들리지 않는다.
func TestSampleKeeper(t *testing.T) {
	for _, n := range []int{0, 1} {
		keep := sampleKeeper(n)
		for range 100 {
			if !keep() {
				t.Fatalf("sampleKeeper(%d)가 false를 냈다 — 1 이하는 전부 남긴다", n)
			}
		}
	}
	keep := sampleKeeper(100)
	kept := 0
	for range 2000 {
		if keep() {
			kept++
		}
	}
	if kept == 0 || kept == 2000 {
		t.Errorf("sampleKeeper(100): 2000번 중 %d번 남김 — 일부만 남아야 한다", kept)
	}
}
