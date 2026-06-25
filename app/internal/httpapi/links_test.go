package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// createLink은 POST /api/links를 호출해 생성된 링크 응답을 돌려주는 테스트 헬퍼다.
func createLink(t *testing.T, router http.Handler, url string) linkResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader(`{"url":"`+url+`"}`))
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var body linkResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("응답 디코딩 실패: %v", err)
	}
	return body
}

// TestCreateLink는 정상 단축 생성이 201과 올바른 응답 DTO를 돌려주는지 확인한다.
func TestCreateLink(t *testing.T) {
	router := newTestRouter()
	const url = "https://example.com/page?q=1"

	link := createLink(t, router, url)
	if link.Code == "" {
		t.Error("code가 비어 있음")
	}
	if link.URL != url {
		t.Errorf("url = %q, want %q", link.URL, url)
	}
	if link.ShortURL != testBaseURL+"/"+link.Code {
		t.Errorf("short_url = %q, want %q", link.ShortURL, testBaseURL+"/"+link.Code)
	}
	if link.Clicks != 0 {
		t.Errorf("clicks = %d, want 0", link.Clicks)
	}
}

// TestCreateLinkInvalidJSON은 깨진 본문에 400을 돌려주는지 확인한다.
func TestCreateLinkInvalidJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader("not json"))
	newTestRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestCreateLinkInvalidURL은 허용되지 않는 스킴에 400을 돌려주는지 확인한다.
func TestCreateLinkInvalidURL(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader(`{"url":"ftp://example.com"}`))
	newTestRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestRedirect는 GET /{code}가 302와 Location 헤더로 원본 URL을 돌려주는지 확인한다.
func TestRedirect(t *testing.T) {
	router := newTestRouter()
	const url = "https://example.com/landing"
	link := createLink(t, router, url)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+link.Code, nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != url {
		t.Errorf("Location = %q, want %q", loc, url)
	}
}

// TestRedirectNotFound는 없는 코드에 404를 돌려주는지 확인한다.
func TestRedirectNotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	newTestRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestStatsCountsClicks는 리다이렉트 후 stats의 clicks가 증가하는지 확인한다.
func TestStatsCountsClicks(t *testing.T) {
	router := newTestRouter()
	link := createLink(t, router, "https://example.com")

	// 두 번 리다이렉트 → 클릭 2.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/"+link.Code, nil)
		router.ServeHTTP(rec, req)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/links/"+link.Code, nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body linkResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("응답 디코딩 실패: %v", err)
	}
	if body.Clicks != 2 {
		t.Errorf("clicks = %d, want 2", body.Clicks)
	}
}
