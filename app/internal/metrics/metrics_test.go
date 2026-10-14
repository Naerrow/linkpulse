package metrics

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql "pgx" 드라이버(연결은 하지 않는다)
	dto "github.com/prometheus/client_model/go"

	"github.com/Naerrow/linkpulse/app/internal/clicks"
)

// find는 이름과 라벨이 맞는 지표 하나를 돌려준다. 없으면 nil이다.
func find(t *testing.T, m *Metrics, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("Gather 실패: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	next:
		for _, metric := range f.GetMetric() {
			got := map[string]string{}
			for _, l := range metric.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if len(got) != len(labels) {
				continue
			}
			for k, v := range labels {
				if got[k] != v {
					continue next
				}
			}
			return metric
		}
	}
	return nil
}

// TestClickCountersMatchStats는 각 지표가 맞는 Stats 필드를 내는지 본다(plan 0015 판정 (b)가 이 값을 정수로 비교한다).
// 필드마다 다른 값을 넣어, 카운터 하나를 다른 필드에 묶으면 실패한다.
func TestClickCountersMatchStats(t *testing.T) {
	s := &clicks.Stats{}
	s.Recorded.Store(101)
	s.Dropped.Store(102)
	s.PublishedBatches.Store(103)
	s.PublishedClicks.Store(104)
	s.PublishErrors.Store(105)
	s.AppliedBatches.Store(106)
	s.AppliedClicks.Store(107)
	s.DuplicateBatches.Store(108)
	s.UnknownClicks.Store(109)
	s.ApplyErrors.Store(110)
	m := New()
	m.RegisterClicks(s, func() int { return 42 })

	want := map[string]float64{
		"linkpulse_clicks_recorded_total":          101,
		"linkpulse_clicks_dropped_total":           102,
		"linkpulse_clicks_published_batches_total": 103,
		"linkpulse_clicks_published_total":         104,
		"linkpulse_clicks_publish_errors_total":    105,
		"linkpulse_clicks_applied_batches_total":   106,
		"linkpulse_clicks_applied_total":           107,
		"linkpulse_clicks_duplicate_batches_total": 108,
		"linkpulse_clicks_unknown_total":           109,
		"linkpulse_clicks_apply_errors_total":      110,
	}
	if len(want) != len(clickCounters) {
		t.Fatalf("Stats 카운터 %d개 중 %d개만 확인한다", len(clickCounters), len(want))
	}
	for name, v := range want {
		metric := find(t, m, name, nil)
		if metric == nil {
			t.Errorf("%s가 없다", name)
			continue
		}
		if got := metric.GetCounter().GetValue(); got != v {
			t.Errorf("%s = %v, want %v", name, got, v)
		}
	}
	if metric := find(t, m, "linkpulse_clicks_pending_codes", nil); metric == nil || metric.GetGauge().GetValue() != 42 {
		t.Errorf("linkpulse_clicks_pending_codes = %v, want 42", metric)
	}

	// 긁을 때마다 현재 값을 읽는다(한 번 읽고 굳지 않는다).
	s.Recorded.Add(1)
	if got := find(t, m, "linkpulse_clicks_recorded_total", nil).GetCounter().GetValue(); got != 102 {
		t.Errorf("증가 뒤 recorded = %v, want 102", got)
	}
}

// TestObserveRequest는 요청 하나가 카운터와 히스토그램에 들어가고, 모르는 메서드는 other로 묶이는지 본다.
func TestObserveRequest(t *testing.T) {
	m := New()
	m.ObserveRequest("GET /{code}", "GET", 302, 100*time.Millisecond)
	m.ObserveRequest("none", "XYZZY", 405, time.Millisecond)

	if metric := find(t, m, "linkpulse_http_requests_total", map[string]string{"route": "GET /{code}", "method": "GET", "code": "302"}); metric == nil || metric.GetCounter().GetValue() != 1 {
		t.Errorf("리다이렉트 카운터 = %v, want 1", metric)
	}
	if metric := find(t, m, "linkpulse_http_requests_total", map[string]string{"route": "none", "method": "other", "code": "405"}); metric == nil {
		t.Error("모르는 메서드가 other로 묶이지 않았다")
	}

	// 지연 SLO 경계(100ms)가 버킷에 있고, 정확히 100ms는 그 버킷에 든다(le는 이하).
	h := find(t, m, "linkpulse_http_request_duration_seconds", map[string]string{"route": "GET /{code}"})
	if h == nil {
		t.Fatal("히스토그램이 없다")
	}
	var found bool
	for _, b := range h.GetHistogram().GetBucket() {
		if b.GetUpperBound() == 0.1 {
			found = true
			if b.GetCumulativeCount() != 1 {
				t.Errorf("le=0.1 누적 = %d, want 1", b.GetCumulativeCount())
			}
		}
	}
	if !found {
		t.Error("히스토그램에 SLO 경계 0.1초 버킷이 없다")
	}
}

// TestRegisterDB는 풀 통계가 db_name 라벨로 나가는지 본다. 연결은 만들지 않는다(Stats만 읽는다).
func TestRegisterDB(t *testing.T) {
	pool, err := sql.Open("pgx", "postgres://u@127.0.0.1:1/x")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pool.SetMaxOpenConns(11)
	m := New()
	m.RegisterDB(pool)
	if metric := find(t, m, "go_sql_max_open_connections", map[string]string{"db_name": "linkpulse"}); metric == nil || metric.GetGauge().GetValue() != 11 {
		t.Errorf("go_sql_max_open_connections = %v, want 11", metric)
	}
}

// TestHandler는 내부 포트 핸들러가 지표·런타임 수집기·pprof를 내는지 본다.
func TestHandler(t *testing.T) {
	h := New().Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d", rec.Code)
	}
	// 실험 E1·E4가 쓰는 두 지표.
	for _, name := range []string{"go_sched_gomaxprocs_threads", "process_cpu_seconds_total", "linkpulse_http_connections_opened_total"} {
		if !strings.Contains(body, "\n"+name+" ") {
			t.Errorf("/metrics에 %s가 없다", name)
		}
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "profile") {
		t.Errorf("/debug/pprof/ = %d", rec.Code)
	}

	// 실험 E2가 받는 CPU 프로파일. 이 경로가 빠지면 Index가 받아 "Unknown profile" 404를 낸다.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/profile?seconds=1", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/octet-stream" {
		t.Errorf("/debug/pprof/profile = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}
