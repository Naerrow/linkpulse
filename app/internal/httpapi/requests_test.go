package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Naerrow/linkpulse/app/internal/links"
)

// do는 라우터에 요청 하나를 보내고 응답을 돌려준다.
func do(router http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// decode는 응답 본문을 v로 해석한다.
func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(v); err != nil {
		t.Fatalf("응답 디코딩 실패: %v (body=%s)", err, rec.Body.String())
	}
}

// submitRequest는 POST /api/requests로 요청을 접수하고 응답 DTO를 돌려준다.
func submitRequest(t *testing.T, router http.Handler, body string) requestResponse {
	t.Helper()
	rec := do(router, httptest.NewRequest(http.MethodPost, "/api/requests", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("접수 status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var out requestResponse
	decode(t, rec, &out)
	return out
}

// TestCreateLinkRequiresAdmin은 링크 생성이 관리자 토큰을 요구하는지 본다.
func TestCreateLinkRequiresAdmin(t *testing.T) {
	router := newTestRouter()
	body := `{"url":"https://example.com"}`

	cases := []struct {
		name, auth string
		want       int
	}{
		{"헤더 없음", "", http.StatusUnauthorized},
		{"틀린 토큰", "Bearer wrong-token", http.StatusUnauthorized},
		{"빈 토큰", "Bearer ", http.StatusUnauthorized},
		{"Bearer 아님", testAdminToken, http.StatusUnauthorized},
		{"맞는 토큰", "Bearer " + testAdminToken, http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader(body))
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := do(router, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401인데 WWW-Authenticate 헤더가 없음")
			}
		})
	}
}

// TestAdminDisabledWhenHashEmpty는 해시가 없으면 맞는 토큰이어도 관리자 기능이 꺼져 있는지 본다(fail-closed).
func TestAdminDisabledWhenHashEmpty(t *testing.T) {
	repo := links.NewMemoryRepository()
	router := NewRouter(RouterDeps{
		Links:     links.NewService(repo, 7),
		Requests:  links.NewRequestService(repo, 7),
		BaseURL:   testBaseURL,
		RateLimit: RateLimitConfig{Disabled: true},
	})
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader(`{"url":"https://example.com"}`)),
		httptest.NewRequest(http.MethodGet, "/api/admin/requests", nil),
	} {
		if rec := do(router, asAdmin(req)); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d, want 401", req.Method, req.URL.Path, rec.Code)
		}
	}
}

// TestRequestApproveFlow는 접수 → 확인 → 관리자 목록 → 승인 → 확인 → 리다이렉트를 끝까지 따라간다.
func TestRequestApproveFlow(t *testing.T) {
	router := newTestRouter()

	submitted := submitRequest(t, router, `{"url":"https://example.com/wanted","note":"블로그용"}`)
	if submitted.Status != "pending" || submitted.StatusURL != testBaseURL+"/#r="+submitted.ID {
		t.Fatalf("접수 응답 = %+v", submitted)
	}

	rec := do(router, httptest.NewRequest(http.MethodGet, "/api/requests/"+submitted.ID, nil))
	var pending requestResponse
	decode(t, rec, &pending)
	if rec.Code != http.StatusOK || pending.ShortURL != "" || pending.Note != "블로그용" {
		t.Fatalf("대기 중 확인 = %d %+v", rec.Code, pending)
	}

	rec = do(router, asAdmin(httptest.NewRequest(http.MethodGet, "/api/admin/requests", nil)))
	var list pendingListResponse
	decode(t, rec, &list)
	if rec.Code != http.StatusOK || len(list.Requests) != 1 || list.Requests[0].ID != submitted.ID {
		t.Fatalf("관리자 목록 = %d %+v", rec.Code, list)
	}

	rec = do(router, asAdmin(httptest.NewRequest(http.MethodPost, "/api/admin/requests/"+submitted.ID+"/approve", nil)))
	var approved approveResponse
	decode(t, rec, &approved)
	if rec.Code != http.StatusOK || approved.Request.Status != "approved" || approved.Link.URL != "https://example.com/wanted" {
		t.Fatalf("승인 = %d %+v", rec.Code, approved)
	}
	if approved.Request.ShortURL != approved.Link.ShortURL || approved.Request.DecidedAt == nil {
		t.Errorf("승인된 요청의 short_url·decided_at이 맞지 않음: %+v", approved.Request)
	}

	rec = do(router, httptest.NewRequest(http.MethodGet, "/api/requests/"+submitted.ID, nil))
	var done requestResponse
	decode(t, rec, &done)
	if done.Status != "approved" || done.ShortURL != approved.Link.ShortURL {
		t.Fatalf("승인 뒤 확인 = %+v", done)
	}

	rec = do(router, httptest.NewRequest(http.MethodGet, "/"+approved.Link.Code, nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://example.com/wanted" {
		t.Errorf("리다이렉트 = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// 이미 결정된 요청은 다시 승인·거절할 수 없다.
	for _, action := range []string{"approve", "reject"} {
		rec = do(router, asAdmin(httptest.NewRequest(http.MethodPost, "/api/admin/requests/"+submitted.ID+"/"+action, nil)))
		if rec.Code != http.StatusConflict {
			t.Errorf("재%s status = %d, want 409", action, rec.Code)
		}
	}
}

// TestRequestReject는 거절 뒤 확인 링크가 rejected를 보여 주는지 본다.
func TestRequestReject(t *testing.T) {
	router := newTestRouter()
	submitted := submitRequest(t, router, `{"url":"https://example.com/no"}`)

	rec := do(router, asAdmin(httptest.NewRequest(http.MethodPost, "/api/admin/requests/"+submitted.ID+"/reject", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("거절 status = %d", rec.Code)
	}
	rec = do(router, httptest.NewRequest(http.MethodGet, "/api/requests/"+submitted.ID, nil))
	var got requestResponse
	decode(t, rec, &got)
	if got.Status != "rejected" || got.ShortURL != "" || got.DecidedAt == nil {
		t.Errorf("거절 뒤 확인 = %+v", got)
	}
}

// TestRequestAdminEndpointsRequireToken은 결정·목록 엔드포인트가 토큰 없이 401인지 본다.
func TestRequestAdminEndpointsRequireToken(t *testing.T) {
	router := newTestRouter()
	submitted := submitRequest(t, router, `{"url":"https://example.com"}`)

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/admin/requests", nil),
		httptest.NewRequest(http.MethodPost, "/api/admin/requests/"+submitted.ID+"/approve", nil),
		httptest.NewRequest(http.MethodPost, "/api/admin/requests/"+submitted.ID+"/reject", nil),
	} {
		if rec := do(router, req); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d, want 401", req.Method, logPath(req.URL.Path), rec.Code)
		}
	}
	// 토큰 없는 승인 시도가 요청을 바꾸지 않았는지도 본다.
	rec := do(router, httptest.NewRequest(http.MethodGet, "/api/requests/"+submitted.ID, nil))
	var got requestResponse
	decode(t, rec, &got)
	if got.Status != "pending" {
		t.Errorf("무인증 시도 뒤 상태 = %s, want pending", got.Status)
	}
}

// TestRequestErrors는 잘못된 입력·없는 요청·대기 상한의 상태코드를 본다.
func TestRequestErrors(t *testing.T) {
	router := newTestRouter()

	cases := []struct {
		name, body string
		want       int
		code       string
	}{
		{"깨진 JSON", "not json", http.StatusBadRequest, "invalid_request"},
		{"위험한 스킴", `{"url":"javascript:alert(1)"}`, http.StatusBadRequest, "invalid_url"},
		{"긴 메모", `{"url":"https://example.com","note":"` + strings.Repeat("가", links.MaxRequestNoteLen+1) + `"}`, http.StatusBadRequest, "note_too_long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(router, httptest.NewRequest(http.MethodPost, "/api/requests", strings.NewReader(tc.body)))
			var e errorResponse
			decode(t, rec, &e)
			if rec.Code != tc.want || e.Error.Code != tc.code {
				t.Errorf("= %d %q, want %d %q", rec.Code, e.Error.Code, tc.want, tc.code)
			}
		})
	}

	missing := "AAAAAAAAAAAAAAAAAAAAAA" // 형식은 맞지만 없는 ID
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/requests/"+missing, nil),
		httptest.NewRequest(http.MethodGet, "/api/requests/not-an-id", nil),
		asAdmin(httptest.NewRequest(http.MethodPost, "/api/admin/requests/"+missing+"/approve", nil)),
		asAdmin(httptest.NewRequest(http.MethodPost, "/api/admin/requests/"+missing+"/reject", nil)),
	} {
		if rec := do(router, req); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404", req.Method, req.URL.Path, rec.Code)
		}
	}
}

// TestRequestPendingCapHTTP는 대기 상한에서 429를 돌려주는지 본다.
func TestRequestPendingCapHTTP(t *testing.T) {
	router := newTestRouter()
	var rec *httptest.ResponseRecorder
	for i := 0; i <= 100; i++ {
		rec = do(router, httptest.NewRequest(http.MethodPost, "/api/requests", strings.NewReader(`{"url":"https://example.com"}`)))
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("101번째 status = %d, want 429", rec.Code)
	}
}

// TestRequestIDNotLogged는 요청 ID가 접근 로그에 남지 않는지 본다(요청 확인 링크의 열쇠).
func TestRequestIDNotLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	router := newTestRouter()
	submitted := submitRequest(t, router, `{"url":"https://example.com"}`)
	do(router, httptest.NewRequest(http.MethodGet, "/api/requests/"+submitted.ID, nil))
	do(router, asAdmin(httptest.NewRequest(http.MethodPost, "/api/admin/requests/"+submitted.ID+"/approve", nil)))

	logs := buf.String()
	if strings.Contains(logs, submitted.ID) {
		t.Fatalf("로그에 요청 ID가 남음:\n%s", logs)
	}
	for _, want := range []string{`"path":"/api/requests/{id}"`, `"path":"/api/admin/requests/{id}/approve"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("로그에 %s가 없음", want)
		}
	}
}

// TestLogPath는 경로 마스킹 규칙을 본다.
func TestLogPath(t *testing.T) {
	cases := map[string]string{
		"/api/requests/abc":               "/api/requests/{id}",
		"/api/admin/requests/abc/approve": "/api/admin/requests/{id}/approve",
		"/api/admin/requests/abc/reject":  "/api/admin/requests/{id}/reject",
		"/api/admin/requests":             "/api/admin/requests",
		"/api/requests":                   "/api/requests",
		"/api/links/abc":                  "/api/links/abc",
		"/abc1234":                        "/abc1234",
	}
	for in, want := range cases {
		if got := logPath(in); got != want {
			t.Errorf("logPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestClassifyNewRoutes는 새 경로가 읽기 한도(가장 느슨함)에 묶이지 않는지 본다.
func TestClassifyNewRoutes(t *testing.T) {
	cases := []struct {
		method, path string
		want         tier
	}{
		{http.MethodPost, "/api/requests", tierWrite},
		{http.MethodPost, "/api/admin/requests/abc/approve", tierWrite},
		{http.MethodPost, "/api/admin/requests/abc/reject", tierWrite},
		{http.MethodPost, "/api/links", tierWrite},
		{http.MethodGet, "/api/requests/abc", tierStats},
		{http.MethodGet, "/api/admin/requests", tierStats},
		{http.MethodGet, "/", tierRead},
		{http.MethodGet, "/static/app.js", tierRead},
	}
	for _, tc := range cases {
		if got := classify(httptest.NewRequest(tc.method, tc.path, nil)); got != tc.want {
			t.Errorf("classify(%s %s) = %s, want %s", tc.method, tc.path, got, tc.want)
		}
	}
}
