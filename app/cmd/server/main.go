// linkpulse 애플리케이션 진입점.
//
// 설정을 읽고 → 구조화 로거를 세우고 → HTTP 서버를 띄운 뒤,
// 종료 시그널을 받으면 진행 중 요청을 마무리하고 우아하게 종료한다.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Naerrow/linkpulse/app/internal/config"
	"github.com/Naerrow/linkpulse/app/internal/db"
	"github.com/Naerrow/linkpulse/app/internal/httpapi"
	"github.com/Naerrow/linkpulse/app/internal/links"
)

// shutdownTimeout은 종료 시 진행 중 요청을 기다려 주는 최대 시간이다.
const shutdownTimeout = 10 * time.Second

// HTTP 서버 타임아웃. 느린/유휴 연결이 커넥션·메모리를 붙잡아 리소스를 고갈시키는 것을 막는다.
// 모든 핸들러가 1초 미만(리다이렉트·stats·create)이라 상수로 고정한다(자주 튜닝할 값 아님).
const (
	// 헤더 수신 상한(slowloris 최소 방어).
	readHeaderTimeout = 5 * time.Second
	// 본문까지 포함한 요청 전체 수신 상한. 본문은 8KiB로 이미 제한돼 있어 넉넉하다.
	readTimeout = 10 * time.Second
	// 응답 쓰기 상한. 레디니스 DB 핑 상한(2s, health.go)과 여유를 둔 값.
	writeTimeout = 15 * time.Second
	// keep-alive 유휴 연결 상한.
	idleTimeout = 60 * time.Second
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		// 로거 설정 전 단계의 치명적 오류 → 기본 로거로 남기고 종료.
		slog.Error("설정 로드 실패", "error", err)
		os.Exit(1)
	}

	setupLogger(cfg.LogLevel)

	// 저장소 → 서비스 → 라우터 순으로 의존성을 조립한다.
	// DATABASE_URL이 있으면 Postgres를, 없으면 인메모리(재시작 시 데이터 소실)를 쓴다.
	var repo links.Repository
	var readiness func(context.Context) error
	if cfg.DatabaseURL != "" {
		pool, err := db.Open(context.Background(), cfg.DatabaseURL)
		if err != nil {
			slog.Error("DB 초기화 실패", "error", err)
			os.Exit(1)
		}
		defer pool.Close()
		repo = links.NewPostgresRepository(pool)
		// readyz가 실제 DB 연결 상태를 반영하도록 핑 함수를 주입한다.
		readiness = func(ctx context.Context) error { return pool.PingContext(ctx) }
	} else {
		slog.Warn("DATABASE_URL 미설정 — 인메모리 저장소 사용(재시작 시 데이터 소실)")
		repo = links.NewMemoryRepository()
	}
	linkSvc := links.NewService(repo, cfg.ShortCodeLength)

	handler := httpapi.NewRouter(httpapi.RouterDeps{
		Links:     linkSvc,
		BaseURL:   cfg.PublicBaseURL,
		Readiness: readiness,
		// RateLimit 미지정 = zero-value → 운영 기본값 적용(httpapi/ratelimit.go의 default* 상수).
	})
	srv := newServer(cfg, handler)

	// 서버를 고루틴에서 띄우고, 리슨 실패는 채널로 전달한다.
	serverErr := make(chan error, 1)
	go func() {
		slog.Info("linkpulse 시작", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// SIGINT/SIGTERM을 받으면 ctx가 취소된다.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serverErr:
		slog.Error("서버 실행 실패", "error", err)
		os.Exit(1)
	case <-ctx.Done():
		slog.Info("종료 신호 수신, 우아한 종료 시작")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("우아한 종료 실패", "error", err)
		os.Exit(1)
	}
	slog.Info("정상 종료 완료")
}

// newServer는 설정과 핸들러로 타임아웃이 세팅된 HTTP 서버를 조립한다.
// 타임아웃 단언을 단위 테스트할 수 있게 main에서 분리했다.
func newServer(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// setupLogger는 LOG_LEVEL에 맞춰 JSON 구조화 로거를 전역 기본 로거로 설정한다.
func setupLogger(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(handler))
}
