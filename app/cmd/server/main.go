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

	srv := &http.Server{
		Addr: ":" + cfg.Port,
		Handler: httpapi.NewRouter(httpapi.RouterDeps{
			Links:     linkSvc,
			BaseURL:   cfg.PublicBaseURL,
			Readiness: readiness,
		}),
		// 헤더 수신 타임아웃으로 느린 연결(slowloris)에 대한 최소 방어.
		ReadHeaderTimeout: 5 * time.Second,
	}

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
