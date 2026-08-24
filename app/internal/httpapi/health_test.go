package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Naerrow/linkpulse/app/internal/links"
)

// newTestReadyRouter는 레디니스 점검 함수를 주입한 라우터를 만든다.
// newTestRouter는 Readiness가 nil이라 실패 경로를 한 번도 실행하지 않는다.
func newTestReadyRouter(check func(ctx context.Context) error) http.Handler {
	svc := links.NewService(links.NewMemoryRepository(), 7)
	return NewRouter(RouterDeps{
		Links:     svc,
		BaseURL:   testBaseURL,
		Readiness: check,
		RateLimit: RateLimitConfig{Disabled: true},
	})
}

// TestHealthz는 라이브니스 엔드포인트가 200과 {"status":"ok"}를 돌려주는지 검증한다.
func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()                               // 가짜 응답기 생성
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil) //

	newTestRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("응답 디코딩 실패: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want %q", body.Status, "ok")
	}
}

// TestReadyz는 레디니스 엔드포인트가 200과 {"status":"ready"}를 돌려주는지 검증한다.
func TestReadyz(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	newTestRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("응답 디코딩 실패: %v", err)
	}
	if body.Status != "ready" {
		t.Errorf("status = %q, want %q", body.Status, "ready")
	}
}

// TestMethodNotAllowed는 등록된 경로라도 다른 메서드면 404/405가 나오는지 확인한다.
// (GET 전용 패턴에 POST가 매칭되지 않아야 한다)
func TestHealthzWrongMethod(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)

	newTestRouter().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("POST /healthz 가 200을 돌려주면 안 된다 (got %d)", rec.Code)
	}
}

// TestReadinessTimeoutFitsRoute53Budget는 레디니스 상한이 Route53 예산 안에 있는지 고정한다.
// Route53 헬스체크는 연결 후 2초 안에 2xx/3xx를 받아야 healthy로 판정하고, canary가
// /readyz를 프로빙한다(ADR 0004 §1-a). 상한이 2초로 회귀하면 앱의 503이 Route53의
// 포기 시점과 겹쳐 판정이 전달되지 않으므로, 값 자체를 테스트로 붙잡는다.
func TestReadinessTimeoutFitsRoute53Budget(t *testing.T) {
	// 두 논리를 구분한다: "왜 2초면 안 되는가"는 판정 일치 문제이고(health.go의 상수 주석),
	// "왜 하필 1초인가"는 크기 문제다 — Route53 예산 2초에 TLS 핸드셰이크가 포함되므로
	// ("within two seconds after connecting") 그 절반을 앱 몫으로 잡았다.
	// 이 테스트가 고정하는 것은 후자이고, 느슨해지면(2초 회귀 포함) 실패한다.
	const maxReadinessTimeout = 1 * time.Second
	if readinessTimeout > maxReadinessTimeout {
		t.Fatalf("readinessTimeout = %v, %v 이하여야 한다", readinessTimeout, maxReadinessTimeout)
	}
}

// TestReadyzUnavailable은 점검 함수가 실패하면 503 + {"status":"unavailable"}이 나가는지 검증한다.
// 이 503이 canary → HealthCheckStatus → canary_down → Slack 경로의 시작점이다.
func TestReadyzUnavailable(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	router := newTestReadyRouter(func(context.Context) error {
		return errors.New("DB 연결 실패")
	})
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var body statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("응답 디코딩 실패: %v", err)
	}
	if body.Status != "unavailable" {
		t.Errorf("status = %q, want %q", body.Status, "unavailable")
	}
}

// TestReadyzAppliesTimeout은 점검 함수가 readinessTimeout 데드라인이 붙은 ctx를 받는지 검증한다.
// 실제로 기다리지 않고 ctx.Deadline()만 읽어 상한 적용 여부를 확인한다.
func TestReadyzAppliesTimeout(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	var remaining time.Duration
	var hasDeadline bool
	router := newTestReadyRouter(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		hasDeadline = ok
		if ok {
			remaining = time.Until(deadline)
		}
		return nil
	})
	router.ServeHTTP(rec, req)

	if !hasDeadline {
		t.Fatal("점검 함수가 데드라인 없는 ctx를 받았다 — 상한이 적용되지 않는다")
	}
	if remaining <= 0 || remaining > readinessTimeout {
		t.Errorf("남은 시간 = %v, (0, %v] 범위여야 한다", remaining, readinessTimeout)
	}
}
