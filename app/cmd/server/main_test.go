package main

import (
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
