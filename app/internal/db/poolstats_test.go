package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestPoolWaitDelta(t *testing.T) {
	prev := sql.DBStats{WaitCount: 3, WaitDuration: 50 * time.Millisecond}
	cur := sql.DBStats{WaitCount: 8, WaitDuration: 120 * time.Millisecond}
	if n, d := poolWaitDelta(prev, cur); n != 5 || d != 70*time.Millisecond {
		t.Errorf("poolWaitDelta = %d, %v, want 5, 70ms", n, d)
	}
	if n, d := poolWaitDelta(cur, cur); n != 0 || d != 0 {
		t.Errorf("변화 없음 = %d, %v, want 0, 0", n, d)
	}
}

// TestRunPoolStats는 tick마다 한 줄을 남기고, 증가분이 루프 시작 시점(기동 전 누적분 제외)부터
// 직전 줄까지로 계산되며, ctx가 끝나면 루프가 돌아오는지 본다.
// 통계 값은 채널에서 차례로 꺼내므로 고루틴 타이밍과 무관하게 결정적이다.
func TestRunPoolStats(t *testing.T) {
	vals := make(chan sql.DBStats, 3)
	vals <- sql.DBStats{WaitCount: 3, WaitDuration: 50 * time.Millisecond} // 루프 시작 기준점
	vals <- sql.DBStats{MaxOpenConnections: 25, InUse: 4, Idle: 1, WaitCount: 5, WaitDuration: 80 * time.Millisecond}
	vals <- sql.DBStats{MaxOpenConnections: 25, InUse: 0, Idle: 5, WaitCount: 5, WaitDuration: 80 * time.Millisecond}

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	tick := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPoolStats(ctx, func() sql.DBStats { return <-vals }, tick, log)
	}()

	tick <- time.Time{}
	tick <- time.Time{}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx가 끝났는데 루프가 돌아오지 않는다")
	}

	type line struct {
		Msg       string `json:"msg"`
		InUse     int    `json:"in_use"`
		Idle      int    `json:"idle"`
		MaxOpen   int    `json:"max_open"`
		WaitCount int64  `json:"wait_count"`
		WaitMS    int64  `json:"wait_ms"`
	}
	want := []line{
		{Msg: "db pool", InUse: 4, Idle: 1, MaxOpen: 25, WaitCount: 2, WaitMS: 30},
		{Msg: "db pool", InUse: 0, Idle: 5, MaxOpen: 25, WaitCount: 0, WaitMS: 0},
	}
	raw := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(raw) != len(want) {
		t.Fatalf("로그 %d줄, want %d줄:\n%s", len(raw), len(want), buf.String())
	}
	for i, r := range raw {
		var got line
		if err := json.Unmarshal([]byte(r), &got); err != nil {
			t.Fatalf("%d번째 줄 파싱 실패: %v", i+1, err)
		}
		if got != want[i] {
			t.Errorf("%d번째 줄 = %+v, want %+v", i+1, got, want[i])
		}
	}
}
