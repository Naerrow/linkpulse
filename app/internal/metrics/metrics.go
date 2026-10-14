// Package metrics는 앱의 Prometheus 지표와 내부 포트 핸들러(/metrics·pprof)를 둔다(plan 0015).
//
// 지표는 공개 포트(8080)가 아니라 내부 포트로만 낸다. ALB·TargetGroupBinding은 8080만 보므로 밖에서 닿지 않는다(ADR 0008).
package metrics

import (
	"database/sql"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Naerrow/linkpulse/app/internal/clicks"
)

// durationBuckets는 요청 시간 히스토그램의 경계(초)다. 지연 SLO 경계 100ms(0.1)가 반드시 들어 있어야 한다 —
// 경계가 없으면 "100ms 이하 비율"을 정확히 셀 수 없다(차트 PrometheusRule의 le="0.1").
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5}

// Metrics는 앱 지표의 레지스트리와 요청·연결 지표다. 전역 기본 레지스트리를 쓰지 않는다 —
// 다른 패키지가 init에서 무엇을 등록하든 /metrics에 섞이지 않게 한다.
type Metrics struct {
	registry    *prometheus.Registry
	requests    *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	connsOpened prometheus.Counter
}

// New는 요청·연결 지표와 Go 런타임·프로세스 수집기를 등록한 Metrics를 만든다.
// Go 수집기 기본값에 go_sched_gomaxprocs_threads가 있다(plan 0015 실험 E4가 실제 GOMAXPROCS를 이것으로 본다).
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "linkpulse_http_requests_total",
			Help: "공개 포트 HTTP 요청 수. route는 ServeMux 패턴이고, mux 앞에서 끝났거나 맞는 패턴이 없으면 none이다.",
		}, []string{"route", "method", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "linkpulse_http_request_duration_seconds",
			Help:    "공개 포트 HTTP 요청 처리 시간(서버 쪽).",
			Buckets: durationBuckets,
		}, []string{"route"}),
		connsOpened: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "linkpulse_http_connections_opened_total",
			Help: "공개 포트에서 새로 열린 TCP 연결 수. 요청 수와 비교하면 ALB의 연결 재사용 정도가 보인다.",
		}),
	}
	m.registry.MustRegister(
		m.requests, m.duration, m.connsOpened,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// ObserveRequest는 httpapi.RequestObserver다. 요청 하나를 세고 처리 시간을 기록한다.
func (m *Metrics) ObserveRequest(route, method string, status int, d time.Duration) {
	m.requests.WithLabelValues(route, methodLabel(method), strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(route).Observe(d.Seconds())
}

// methodLabel은 표준 메서드만 그대로 두고 나머지는 other로 묶는다.
// 메서드는 클라이언트가 아무 문자열이나 보낼 수 있어서, 그대로 라벨에 넣으면 시계열이 끝없이 는다.
func methodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return method
	default:
		return "other"
	}
}

// ConnState는 http.Server.ConnState 훅이다. 새 연결(StateNew)만 센다.
func (m *Metrics) ConnState(_ net.Conn, s http.ConnState) {
	if s == http.StateNew {
		m.connsOpened.Inc()
	}
}

// RegisterDB는 database/sql 풀 통계(go_sql_*{db_name="linkpulse"})를 등록한다. 긁을 때마다 pool.Stats()를 읽는다.
func (m *Metrics) RegisterDB(pool *sql.DB) {
	m.registry.MustRegister(collectors.NewDBStatsCollector(pool, "linkpulse"))
}

// RegisterClicks는 클릭 파이프라인 카운터(clicks.Stats)와 대기 코드 수를 등록한다.
func (m *Metrics) RegisterClicks(s *clicks.Stats, pending func() int) {
	m.registry.MustRegister(&clickCollector{stats: s, pending: pending})
}

// Handler는 내부 포트의 핸들러다: /metrics와 net/http/pprof(/debug/pprof/).
// pprof는 클러스터 안에서만 닿는다. 클러스터 안의 누군가가 프로파일 요청으로 CPU를 쓰게 할 수 있다는 위험은
// 받아들였다(ADR 0008). net/http/pprof는 init에서 DefaultServeMux에도 등록하지만, 공개 서버는 그 mux를 쓰지 않는다.
func (m *Metrics) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}
