package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Naerrow/linkpulse/app/internal/config"
)

// TestNewServerTimeouts는 newServer가 4개 타임아웃과 주소를 기대값으로 세팅하는지 단언한다.
// 느린/유휴 연결 방어가 회귀로 사라지지 않게 고정한다.
func TestNewServerTimeouts(t *testing.T) {
	cfg := config.Config{Port: "9999"}
	srv := newServer(cfg, http.NotFoundHandler())

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
		{"IdleTimeout", srv.IdleTimeout, 60 * time.Second},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
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
