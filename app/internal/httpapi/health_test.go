package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
