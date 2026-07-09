package httpapi

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// 레이트리밋 운영 기본값(상수 확정 — 자주 튜닝할 값 아님, env로 노출하지 않는다).
// 한도는 per-IP·per-분. 인스턴스별 인메모리라 태스크 N개면 실질 한도가 N배가 되는 한계가 있다
// (분산 리밋은 후속 과제 P4-f/Redis).
const (
	defaultWritePerMin = 20  // POST /api/links (쓰기: 열거·스팸 생성 억제)
	defaultWriteBurst  = 10  //
	defaultStatsPerMin = 60  // GET /api/links/{code} (통계: 데이터 노출 엔드포인트)
	defaultStatsBurst  = 20  //
	defaultReadPerMin  = 300 // GET /{code} 등 (읽기·리다이렉트: NAT 공유 IP 오탐 최소화 위해 넉넉히)
	defaultReadBurst   = 100 //

	// rlTTL은 유휴 클라이언트 버킷을 메모리에서 비우는 기준 시간이다.
	rlTTL = 10 * time.Minute
	// rlSweepInterval은 스윕을 실행하는 최소 간격이다(새 키 삽입 시 기회적으로만 실행).
	rlSweepInterval = time.Minute
	// rlTrustedHops는 X-Forwarded-For 최우측에서 신뢰하는 프록시 홉 수다(clientIP의 선택 인덱스로 사용).
	// 현재 토폴로지는 클라이언트 → ALB → ECS로 ALB 단일 홉이라 1. CloudFront 등 홉이 추가되면 이 값만 올린다.
	rlTrustedHops = 1
)

// tier는 요청을 한도 그룹으로 분류한 것이다. 키 접두어로도 쓰인다.
type tier string

const (
	tierExempt tier = "exempt" // 무제한(헬스체크)
	tierWrite  tier = "write"
	tierStats  tier = "stats"
	tierRead   tier = "read"
)

// RateLimitConfig는 3티어 레이트리밋의 운영 파라미터다(RouterDeps로 주입).
//
// zero-value는 곧 운영 기본값으로 해석된다(안전 기본 — 설정을 깜빡해도 prod가 보호됨).
// 각 필드가 0이면 위 default* 상수로 채워진다(withDefaults). 테스트는 낮은 한도·Disabled·
// 주입 clock으로 override한다.
type RateLimitConfig struct {
	Disabled bool // true면 리밋을 완전히 끈다(핸들러 단위 테스트가 리밋과 결합하지 않도록).

	WritePerMin int
	WriteBurst  int
	StatsPerMin int
	StatsBurst  int
	ReadPerMin  int
	ReadBurst   int

	// now는 TTL 스윕(lastSeen 판정)용 클럭이다. nil이면 time.Now.
	// 주의: 토큰 리필은 rate.Limiter가 내부적으로 time.Now를 쓰므로 이 clock의 영향을 받지 않는다.
	now func() time.Time
}

// withDefaults는 0인 필드를 운영 기본값으로 채운 복사본을 돌려준다.
func (c RateLimitConfig) withDefaults() RateLimitConfig {
	if c.WritePerMin == 0 {
		c.WritePerMin = defaultWritePerMin
	}
	if c.WriteBurst == 0 {
		c.WriteBurst = defaultWriteBurst
	}
	if c.StatsPerMin == 0 {
		c.StatsPerMin = defaultStatsPerMin
	}
	if c.StatsBurst == 0 {
		c.StatsBurst = defaultStatsBurst
	}
	if c.ReadPerMin == 0 {
		c.ReadPerMin = defaultReadPerMin
	}
	if c.ReadBurst == 0 {
		c.ReadBurst = defaultReadBurst
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// bucket은 한 클라이언트 키의 토큰버킷 리미터와 마지막 관측 시각이다.
type bucket struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// rateLimit은 클라이언트 키(티어+IP)별 토큰버킷을 보관하는 인메모리 리미터다.
// NewRouter 시점에 1회 생성돼 모든 요청이 상태 맵을 공유한다.
// 백그라운드 janitor 고루틴을 두지 않고, 새 키 삽입 시 기회적으로 오래된 항목을 스윕한다
// (테스트마다 라우터를 새로 만들어도 고루틴 누수가 없다).
type rateLimit struct {
	cfg RateLimitConfig // withDefaults 적용 완료본

	mu        sync.Mutex // buckets/lastSweep 보호(*rate.Limiter 자체는 동시성 안전하나 맵 변형은 락 필요)
	buckets   map[string]*bucket
	lastSweep time.Time
}

// newRateLimit은 기본값을 채운 리미터를 만든다(상태 맵 1회 생성).
func newRateLimit(cfg RateLimitConfig) *rateLimit {
	cfg = cfg.withDefaults()
	return &rateLimit{
		cfg:       cfg,
		buckets:   make(map[string]*bucket),
		lastSweep: cfg.now(),
	}
}

// middleware는 요청을 티어로 분류해 한도를 적용한다. 초과 시 실제 핸들러 실행 전에 429로 거부한다.
func (rl *rateLimit) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rl.cfg.Disabled {
			next.ServeHTTP(w, r)
			return
		}

		t := classify(r)
		if t == tierExempt {
			next.ServeHTTP(w, r)
			return
		}

		perMinLimit, burst := rl.tierParams(t)
		key := string(t) + "|" + clientIP(r)
		if !rl.limiterFor(key, perMin(perMinLimit), burst).Allow() {
			// Retry-After는 writeError(내부에서 WriteHeader 호출)보다 먼저 설정해야 반영된다.
			// 값은 리미터 상태를 소비하지 않게 산출한다(토큰 1개 리필 주기 = ceil(60/perMin)초).
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter(perMinLimit)))
			writeError(w, http.StatusTooManyRequests, "rate_limited", "요청이 너무 많습니다. 잠시 후 다시 시도해 주세요")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// classify는 메서드·경로로 요청의 한도 티어를 정한다.
// 미들웨어는 mux 바깥이라 PathValue가 없으므로 경로를 직접 매칭한다.
// 헬스체크 예외는 ALB가 쓰는 GET에 한정한다(비-GET 헬스 경로에 무제한 우회로를 만들지 않도록).
func classify(r *http.Request) tier {
	p := r.URL.Path
	switch {
	case (p == "/healthz" || p == "/readyz") && r.Method == http.MethodGet:
		return tierExempt
	case r.Method == http.MethodPost && p == "/api/links":
		return tierWrite
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/api/links/"):
		return tierStats
	default:
		return tierRead
	}
}

// tierParams는 티어별 (분당 요청 한도, 버스트)를 돌려준다.
func (rl *rateLimit) tierParams(t tier) (int, int) {
	switch t {
	case tierWrite:
		return rl.cfg.WritePerMin, rl.cfg.WriteBurst
	case tierStats:
		return rl.cfg.StatsPerMin, rl.cfg.StatsBurst
	default: // tierRead
		return rl.cfg.ReadPerMin, rl.cfg.ReadBurst
	}
}

// limiterFor는 키에 해당하는 리미터를 반환한다(없으면 생성). 새 키 삽입 시 기회적으로 스윕한다.
// 맵 조회·삽입·스윕을 동일 뮤텍스 아래에서 수행한다.
func (rl *rateLimit) limiterFor(key string, limit rate.Limit, burst int) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.cfg.now()
	if b, ok := rl.buckets[key]; ok {
		b.lastSeen = now // 활성 클라이언트는 스윕 대상에서 갱신.
		return b.lim
	}

	rl.sweep(now)
	lim := rate.NewLimiter(limit, burst)
	rl.buckets[key] = &bucket{lim: lim, lastSeen: now}
	return lim
}

// sweep은 마지막 스윕 후 rlSweepInterval이 지났을 때만 TTL 초과 항목을 제거한다.
// 호출자가 rl.mu를 잡고 있어야 한다.
func (rl *rateLimit) sweep(now time.Time) {
	if now.Sub(rl.lastSweep) < rlSweepInterval {
		return
	}
	rl.lastSweep = now
	for k, b := range rl.buckets {
		if now.Sub(b.lastSeen) > rlTTL {
			delete(rl.buckets, k)
		}
	}
}

// clientIP는 레이트리밋 키로 쓸 클라이언트 IP를 정한다.
//
// 토폴로지: 클라이언트 → ALB → ECS (CDN 없음, 신뢰 프록시 홉 = rlTrustedHops = 1).
// ALB는 관측한 소스 IP를 X-Forwarded-For "맨 뒤"에 append한다. 따라서 최우측에서 신뢰 홉 수
// (rlTrustedHops)만큼 안쪽으로 들어간 항목이 ALB가 본 실제 소스이며 위조 불가다. 그 좌측 값은
// 클라이언트가 위조할 수 있으므로 무시한다. XFF가 없으면(로컬·직접 접속) RemoteAddr host로 폴백한다.
// CloudFront 등 신뢰 프록시가 늘면 rlTrustedHops만 올리면 선택 인덱스가 따라 이동한다.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		// 최우측에서 rlTrustedHops만큼 안쪽 = 가장 바깥 신뢰 프록시가 관측한 소스.
		idx := len(parts) - rlTrustedHops
		if idx < 0 {
			idx = 0
		}
		if ip := canonicalIP(parts[idx]); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return canonicalIP(r.RemoteAddr) // 포트가 없으면 그대로 정규화.
	}
	return canonicalIP(host)
}

// canonicalIP는 "ip" 또는 "ip:port" 형태를 받아 정규화된 IP 문자열을 돌려준다.
// IPv6 축약·대소문자를 net.ParseIP로 정규화해 같은 클라이언트가 항상 같은 키로 묶이게 한다.
// 파싱 실패 시 공백만 제거한 원본을 쓴다(알 수 없는 형식이라도 일관 키가 되도록).
func canonicalIP(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host // "ip:port"·"[ipv6]:port" → host만.
	}
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}
	return s
}

// perMin은 분당 요청 수를 rate.Limit(초당 토큰 리필 속도)으로 변환한다.
func perMin(n int) rate.Limit {
	return rate.Limit(float64(n) / 60.0)
}

// retryAfter는 429 응답의 Retry-After(초)를 산출한다. 토큰 1개 리필 주기 = ceil(60/perMin)초를
// 정수 산술로 구해 부동소수점 오차(예: 20/min에서 3이 4로 반올림) 없이 최소 1초를 보장한다.
// 리미터 상태를 건드리지 않아 거절된 요청이 다음 허용 시점을 밀지 않는다.
func retryAfter(perMinLimit int) int {
	if perMinLimit <= 0 {
		return 1
	}
	sec := (60 + perMinLimit - 1) / perMinLimit // ceil(60/perMinLimit)
	if sec < 1 {
		return 1
	}
	return sec
}
