package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Naerrow/linkpulse/app/internal/config"
	"github.com/Naerrow/linkpulse/app/internal/metrics"
)

// TestNewServerTimeouts는 newServer가 4개 타임아웃과 주소를 기대값으로 세팅하는지 단언한다.
// 느린/유휴 연결 방어가 회귀로 사라지지 않게 고정한다.
func TestNewServerTimeouts(t *testing.T) {
	cfg := config.Config{Port: "9999"}
	srv := newServer(cfg, http.NotFoundHandler(), nil)

	if srv.Addr != ":9999" {
		t.Errorf("Addr = %q, want %q", srv.Addr, ":9999")
	}

	cases := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout, 5 * time.Second},
		{"ReadTimeout", srv.ReadTimeout, 10 * time.Second},
		{"WriteTimeout", srv.WriteTimeout, 15 * time.Second},
		{"IdleTimeout", srv.IdleTimeout, 75 * time.Second},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	// 값보다 관계가 핵심이다: keep-alive가 ALB 유휴 타임아웃보다 짧거나 같으면 502가 날 수 있다(plan 0015).
	if srv.IdleTimeout <= albIdleTimeout {
		t.Errorf("IdleTimeout %v가 ALB 유휴 타임아웃 %v보다 길지 않다", srv.IdleTimeout, albIdleTimeout)
	}
}

// TestNewServerCountsNewConnections는 공개 서버의 ConnState 훅이 지표로 이어지는지 본다(plan 0015).
func TestNewServerCountsNewConnections(t *testing.T) {
	m := metrics.New()
	srv := newServer(config.Config{Port: "9999"}, http.NotFoundHandler(), m.ConnState)
	if srv.ConnState == nil {
		t.Fatal("ConnState 훅이 없다")
	}
	srv.ConnState(nil, http.StateNew)
	srv.ConnState(nil, http.StateActive)
	if got := gatherValue(t, m, "linkpulse_http_connections_opened_total"); got != 1 {
		t.Errorf("새 연결 수 = %v, want 1(StateNew만 센다)", got)
	}
}

// TestMetricsServer는 내부 포트 서버가 지표 핸들러를 쓰고, 쓰기 상한이 30초 프로파일보다 긴지 본다.
func TestMetricsServer(t *testing.T) {
	m := metrics.New()
	srv := newMetricsServer(":9090", m.Handler())
	if srv.Addr != ":9090" {
		t.Errorf("Addr = %q, want :9090", srv.Addr)
	}
	if srv.WriteTimeout <= 30*time.Second {
		t.Errorf("WriteTimeout %v — pprof 30초 프로파일이 거부된다", srv.WriteTimeout)
	}
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "linkpulse_http_connections_opened_total") {
		t.Errorf("내부 포트 /metrics = %d, 지표 없음", rec.Code)
	}
}

// gatherValue는 레이블 없는 지표 하나의 값을 /metrics 본문에서 읽는다.
func gatherValue(t *testing.T, m *metrics.Metrics, name string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if v, ok := strings.CutPrefix(line, name+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("%s 값 해석 실패: %q", name, v)
			}
			return f
		}
	}
	t.Fatalf("%s가 /metrics에 없다", name)
	return 0
}

// fakeShutdowner는 정해 둔 오류를 돌려주는 가짜 서버다.
type fakeShutdowner struct{ err error }

func (f fakeShutdowner) Shutdown(context.Context) error { return f.err }

// TestShutdownDrainsEvenOnError는 서버 종료가 실패해도 클릭 drain을 부르는지 본다(plan 0013 r1 #5).
// 예전에는 종료 오류에서 바로 os.Exit해 버퍼의 클릭을 버렸다.
func TestShutdownDrainsEvenOnError(t *testing.T) {
	for _, want := range []error{nil, errors.New("종료 시간 초과")} {
		drained := false
		err := shutdown(fakeShutdowner{err: want}, func() { drained = true })
		if !errors.Is(err, want) {
			t.Errorf("shutdown err = %v, want %v", err, want)
		}
		if !drained {
			t.Errorf("서버 종료 결과 %v에서 drain을 부르지 않았다", want)
		}
	}
}

// fakeFlusher는 Flush 호출 순서와 상한(ctx 마감 시각) 유무를 기록한다.
type fakeFlusher struct {
	calls       *[]string
	hadDeadline bool
}

func (f *fakeFlusher) Flush(ctx context.Context) error {
	_, f.hadDeadline = ctx.Deadline()
	*f.calls = append(*f.calls, "flush")
	return nil
}

func (f *fakeFlusher) PendingCodes() int { return 0 }

// TestDrainClicks는 정상 종료 때 클릭을 잃지 않는 순서를 본다(plan 0013 코드 검토 r1 #1).
// 멈춤(stop) → 상한 있는 마지막 flush 한 번 → 워커·통계 고루틴이 끝날 때까지 기다림.
func TestDrainClicks(t *testing.T) {
	var calls []string
	f := &fakeFlusher{calls: &calls}
	workerDone, statsDone := make(chan struct{}), make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		drainClicks(func() { calls = append(calls, "stop") }, f, time.Second, workerDone, statsDone)
	}()

	close(workerDone)
	select {
	case <-returned:
		t.Fatal("통계 고루틴이 끝나기 전에 돌아왔다")
	case <-time.After(50 * time.Millisecond):
	}
	close(statsDone)
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("모든 고루틴이 끝났는데 돌아오지 않는다")
	}

	if want := []string{"stop", "flush"}; len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
		t.Errorf("호출 순서 = %v, want %v", calls, want)
	}
	if !f.hadDeadline {
		t.Error("마지막 flush에 상한(마감 시각)이 없다")
	}
}

// newPasswordFetch는 출처마다 맞는 조회 함수를 고른다(plan 0014).
// file은 그 경로를 읽고, aws는 네트워크 없이 클라이언트만 만든다. 모르는 출처는 오류다.
func TestNewPasswordFetch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db-password")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fetch, err := newPasswordFetch(context.Background(), config.Config{
		DBPasswordSource: config.PasswordSourceFile, DBPasswordFile: path,
	})
	if err != nil {
		t.Fatalf("file 출처 실패: %v", err)
	}
	if got, err := fetch(context.Background()); err != nil || got != "from-file" {
		t.Errorf("file 출처가 %q, %v를 읽었다 — 파일 내용이어야 한다", got, err)
	}

	fetch, err = newPasswordFetch(context.Background(), config.Config{
		DBPasswordSource: config.PasswordSourceAWS,
		DBSecretARN:      "arn:aws:secretsmanager:ap-northeast-2:123456789012:secret:rds!db-x-AbCdEf",
	})
	if err != nil || fetch == nil {
		t.Errorf("aws 출처 실패: %v", err)
	}

	if _, err := newPasswordFetch(context.Background(), config.Config{DBPasswordSource: "vault"}); err == nil {
		t.Error("모르는 출처인데 오류가 아니다")
	}
}
