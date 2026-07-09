package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Naerrow/linkpulse/app/internal/links"
)

// newRateLimitedRouter는 지정한 리밋 설정으로 인메모리 라우터를 만든다(레이트리밋 테스트 공용).
func newRateLimitedRouter(cfg RateLimitConfig) http.Handler {
	svc := links.NewService(links.NewMemoryRepository(), 7)
	return NewRouter(RouterDeps{Links: svc, BaseURL: testBaseURL, RateLimit: cfg})
}

// doReq는 XFF로 클라이언트 IP를 지정해 요청을 보내고 응답 레코더를 돌려준다.
func doReq(router http.Handler, method, path, body, xff string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// (a) 티어별 한도를 초과하면 429 + Retry-After + 일관된 에러 본문을 돌려준다.
func TestRateLimit_ExceedReturns429(t *testing.T) {
	cfg := RateLimitConfig{
		WritePerMin: 60, WriteBurst: 2,
		StatsPerMin: 60, StatsBurst: 2,
		ReadPerMin: 60, ReadBurst: 2,
	}
	tiers := []struct {
		name, method, path, body string
		burst                    int
	}{
		{"write", http.MethodPost, "/api/links", `{"url":"https://example.com"}`, 2},
		{"stats", http.MethodGet, "/api/links/abc", "", 2},
		{"read", http.MethodGet, "/somecode", "", 2},
	}
	for _, tc := range tiers {
		t.Run(tc.name, func(t *testing.T) {
			router := newRateLimitedRouter(cfg)
			const ip = "203.0.113.7"

			// 버스트 이내는 429가 아니어야 한다.
			for i := 0; i < tc.burst; i++ {
				if rec := doReq(router, tc.method, tc.path, tc.body, ip); rec.Code == http.StatusTooManyRequests {
					t.Fatalf("요청 %d: 버스트 이내인데 429", i+1)
				}
			}

			// 버스트 초과 → 429.
			rec := doReq(router, tc.method, tc.path, tc.body, ip)
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("버스트 초과인데 status = %d, want 429", rec.Code)
			}

			// Retry-After는 양의 정수여야 한다.
			ra := rec.Header().Get("Retry-After")
			if n, err := strconv.Atoi(ra); err != nil || n < 1 {
				t.Errorf("Retry-After = %q, want 양의 정수", ra)
			}

			// 본문은 공통 에러 형식({"error":{"code":"rate_limited"}}).
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("429 본문 디코딩 실패: %v", err)
			}
			if body.Error.Code != "rate_limited" {
				t.Errorf("error.code = %q, want rate_limited", body.Error.Code)
			}
		})
	}
}

// (b) /healthz·/readyz는 한도를 크게 초과해도 무제한이어야 한다(ALB 헬스체크 보호).
func TestRateLimit_HealthExempt(t *testing.T) {
	router := newRateLimitedRouter(RateLimitConfig{ReadPerMin: 60, ReadBurst: 1}) // 읽기 한도 아주 낮게
	const ip = "203.0.113.8"

	for _, path := range []string{"/healthz", "/readyz"} {
		for i := 0; i < 10; i++ { // 버스트(1)를 훨씬 넘겨도 예외라 전부 200
			if rec := doReq(router, http.MethodGet, path, "", ip); rec.Code != http.StatusOK {
				t.Fatalf("%s 요청 %d: status = %d, want 200(예외라 무제한)", path, i+1, rec.Code)
			}
		}
	}
}

// (c) XFF 파싱: 공백·복수 IP·포트 suffix·IPv6·위조 최좌측 무시·RemoteAddr 폴백.
func TestClientIP(t *testing.T) {
	cases := []struct {
		name, xff, remoteAddr, want string
	}{
		{"xff 단일", "203.0.113.5", "10.0.0.1:1234", "203.0.113.5"},
		{"xff 복수는 최우측(ALB 부착)", "203.0.113.5, 70.41.3.18, 150.172.238.178", "10.0.0.1:1234", "150.172.238.178"},
		{"위조된 최좌측 무시", "1.2.3.4, 203.0.113.5", "10.0.0.1:1234", "203.0.113.5"},
		{"공백 트림", "  203.0.113.5  ", "10.0.0.1:1234", "203.0.113.5"},
		{"포트 suffix 제거", "203.0.113.5:5555", "10.0.0.1:1234", "203.0.113.5"},
		{"IPv6 정규화", "2001:DB8::1", "10.0.0.1:1234", "2001:db8::1"},
		{"xff 없으면 RemoteAddr 폴백", "", "203.0.113.9:4444", "203.0.113.9"},
		{"xff 없으면 IPv6 RemoteAddr", "", "[2001:db8::2]:4444", "2001:db8::2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.RemoteAddr = c.remoteAddr
			if c.xff != "" {
				req.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := clientIP(req); got != c.want {
				t.Errorf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}

// (d) 주입 clock으로 시간을 진행하면 TTL 초과 유휴 버킷이 스윕된다.
func TestRateLimit_TTLSweep(t *testing.T) {
	now := time.Unix(1000, 0)
	rl := newRateLimit(RateLimitConfig{
		ReadPerMin: 60, ReadBurst: 5,
		now: func() time.Time { return now },
	})

	rl.limiterFor("read|1.1.1.1", perMin(60), 5)
	rl.limiterFor("read|2.2.2.2", perMin(60), 5)
	if n := bucketLen(rl); n != 2 {
		t.Fatalf("초기 버킷 수 = %d, want 2", n)
	}

	// TTL + sweepInterval 이상 경과시키고 새 키를 넣어 스윕을 유발한다.
	now = now.Add(rlTTL + rlSweepInterval + time.Second)
	rl.limiterFor("read|3.3.3.3", perMin(60), 5)

	if n := bucketLen(rl); n != 1 {
		t.Errorf("스윕 후 버킷 수 = %d, want 1(오래된 2개 제거, 새 1개만 잔존)", n)
	}
}

// bucketLen은 락을 잡고 현재 버킷 수를 읽는다(테스트 전용).
func bucketLen(rl *rateLimit) int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.buckets)
}

// (e) 서로 다른 IP는 독립 버킷이라 한 IP의 소진이 다른 IP에 영향을 주지 않는다.
func TestRateLimit_IndependentIPs(t *testing.T) {
	router := newRateLimitedRouter(RateLimitConfig{WritePerMin: 60, WriteBurst: 2})
	const body = `{"url":"https://example.com"}`

	// IP-A가 버스트를 넘겨 소진(3번째는 429).
	for i := 0; i < 3; i++ {
		doReq(router, http.MethodPost, "/api/links", body, "203.0.113.1")
	}
	// IP-B는 자기 버스트를 온전히 쓸 수 있어야 한다.
	for i := 0; i < 2; i++ {
		if rec := doReq(router, http.MethodPost, "/api/links", body, "203.0.113.2"); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("IP-B 요청 %d: 독립 버킷이어야 하는데 429", i+1)
		}
	}
}

// zero-value RateLimitConfig는 운영 기본값으로 리밋을 켠다(main.go가 의존하는 경로 — 회귀 방지).
func TestRateLimit_ZeroValueUsesProductionDefaults(t *testing.T) {
	router := newRateLimitedRouter(RateLimitConfig{}) // zero-value → withDefaults로 운영 기본값
	const body = `{"url":"https://example.com"}`
	const ip = "203.0.113.20"

	// 쓰기 기본 burst(=defaultWriteBurst)까지는 통과.
	for i := 0; i < defaultWriteBurst; i++ {
		if rec := doReq(router, http.MethodPost, "/api/links", body, ip); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("요청 %d: 기본 버스트 이내인데 429(기본값 미적용?)", i+1)
		}
	}
	// 버스트 초과 → 429(zero-value가 Disabled=false로 리밋을 켰음을 확인).
	if rec := doReq(router, http.MethodPost, "/api/links", body, ip); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("기본 버스트 초과인데 status = %d, want 429", rec.Code)
	}
}

// retryAfter는 정수 산술로 ceil(60/perMin)을 부동소수점 오차 없이 산출해야 한다.
func TestRetryAfter(t *testing.T) {
	cases := []struct{ perMin, want int }{
		{20, 3},  // 쓰기 기본: 60/20=3 (float 경로에서는 3이 4로 반올림되던 값)
		{60, 1},  // 통계 기본
		{300, 1}, // 읽기 기본
		{30, 2},
		{0, 1}, // 방어적: 최소 1초
	}
	for _, c := range cases {
		if got := retryAfter(c.perMin); got != c.want {
			t.Errorf("retryAfter(%d) = %d, want %d", c.perMin, got, c.want)
		}
	}
}
