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
	"strings"
	"syscall"
	"time"

	"github.com/Naerrow/linkpulse/app/internal/config"
	"github.com/Naerrow/linkpulse/app/internal/db"
	"github.com/Naerrow/linkpulse/app/internal/dbcreds"
	"github.com/Naerrow/linkpulse/app/internal/httpapi"
	"github.com/Naerrow/linkpulse/app/internal/links"
)

// shutdownTimeout은 종료 시 진행 중 요청을 기다려 주는 최대 시간이다.
const shutdownTimeout = 10 * time.Second

// poolStatsInterval은 DB 연결 풀 통계 로그 주기다. 태스크당 분당 6줄이라 비용은 무시할 만하다.
const poolStatsInterval = 10 * time.Second

// HTTP 서버 타임아웃. 느린/유휴 연결이 커넥션·메모리를 붙잡아 리소스를 고갈시키는 것을 막는다.
// 모든 핸들러가 1초 미만(리다이렉트·stats·create)이라 상수로 고정한다(자주 튜닝할 값 아님).
const (
	// 헤더 수신 상한(slowloris 최소 방어).
	readHeaderTimeout = 5 * time.Second
	// 본문까지 포함한 요청 전체 수신 상한. 본문은 8KiB로 이미 제한돼 있어 넉넉하다.
	readTimeout = 10 * time.Second
	// 응답 쓰기 상한. 레디니스 DB 핑 상한(1s, health.go)과 여유를 둔 값.
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

	// task_id를 모든 구조화 로그에 고정 첨부한다. 2-6이 "회전 전에 기록한 task ID 집합"으로
	// 판정하므로 이 필드가 없으면 판정이 불가능하다. 실패하면 unknown으로 두고 기동은 계속한다.
	setupLogger(cfg.LogLevel, dbcreds.TaskID(context.Background()))

	// 저장소 → 서비스 → 라우터 순으로 의존성을 조립한다.
	// DATABASE_URL이 있으면 Postgres를, 없으면 인메모리(재시작 시 데이터 소실)를 쓴다.
	var repo links.Store
	var readiness func(context.Context) error
	if cfg.DatabaseURL != "" {
		// DB_SECRET_ARN이 있으면 회전 대응 provider를 붙인다. 없으면 정적 모드다(로컬).
		var provider *dbcreds.Provider
		if cfg.DBSecretARN != "" {
			fetch, err := dbcreds.NewSecretsManagerFetch(context.Background(), cfg.DBSecretARN)
			if err != nil {
				slog.Error("Secrets Manager 설정 실패", "error", err)
				os.Exit(1)
			}
			// seed는 ECS가 주입한 값 — 그 시점의 AWSCURRENT이므로 유효하다(2-3 (a)).
			provider = dbcreds.New(cfg.DBPassword, fetch)
			// 풀보다 뒤에 닫히도록 여기서 defer한다(LIFO) — 진행 중 refresh를 취소한다.
			defer provider.Close()
		}
		logProviderMode(provider, cfg)

		pool, err := db.Open(context.Background(), db.Settings{
			DSN:             cfg.DatabaseURL,
			ConnMaxLifetime: cfg.ConnMaxLifetime,
			Provider:        provider,
		})
		if err != nil {
			slog.Error("DB 초기화 실패", "error", err)
			os.Exit(1)
		}
		defer pool.Close()
		// 풀 통계 로그(plan 0012). 이 defer가 pool.Close보다 먼저 돈다(LIFO). 취소만 하고 고루틴 종료는
		// 기다리지 않아서, 종료와 겹친 tick이 닫힌 풀을 한 번 더 찍을 수 있다. Stats()는 Close 뒤에도 안전하다.
		statsCtx, stopStats := context.WithCancel(context.Background())
		defer stopStats()
		go db.LogPoolStats(statsCtx, pool, poolStatsInterval)
		repo = links.NewPostgresRepository(pool)
		// readyz가 실제 DB 연결 상태를 반영하도록 핑 함수를 주입한다.
		readiness = func(ctx context.Context) error { return pool.PingContext(ctx) }
	} else {
		slog.Warn("DATABASE_URL 미설정 — 인메모리 저장소 사용(재시작 시 데이터 소실)")
		repo = links.NewMemoryRepository()
	}
	linkSvc := links.NewService(repo, cfg.ShortCodeLength)
	// 같은 저장소가 요청도 맡는다 — 승인이 링크와 요청을 한 트랜잭션으로 바꿔야 해서다.
	reqSvc := links.NewRequestService(repo, cfg.ShortCodeLength)
	slog.Info("관리자 기능", "admin_enabled", cfg.AdminTokenSHA256 != nil)
	// 예외 IP는 개수만 남긴다 — 측정 클라이언트의 집 IP라 로그에 찍지 않는다(plan 0012).
	slog.Info("레이트리밋", "rate_limit_exempt_count", len(cfg.RateLimitExemptIPs))

	handler := httpapi.NewRouter(httpapi.RouterDeps{
		Links:            linkSvc,
		Requests:         reqSvc,
		AdminTokenSHA256: cfg.AdminTokenSHA256,
		BaseURL:          cfg.PublicBaseURL,
		Readiness:        readiness,
		// 예외 IP 외의 한도 필드는 zero-value → 운영 기본값 적용(httpapi/ratelimit.go의 default* 상수).
		RateLimit: httpapi.RateLimitConfig{ExemptIPs: cfg.RateLimitExemptIPs},
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

// logProviderMode는 기동 시 provider 모드와 커넥션 수명을 한 줄로 남긴다.
//
// 2-5 smoke가 이 줄로 "Step 2 코드가 실제로 도는 이미지인지"를 확인한다 — task definition의
// env 확인만으로는 이미지 내용을 보지 못한다. conn_max_lifetime을 같은 줄에 넣는 이유는
// 잘못된 값이 기동 실패가 아니라 폴백 + 경고라, 문자열 확인만으로는 실효값을 증명하지 못해서다.
func logProviderMode(p *dbcreds.Provider, cfg config.Config) {
	mode := "static"
	if p != nil {
		mode = p.Mode()
	}
	attrs := []any{
		"event", "startup",
		"secret_provider_mode", mode,
		"conn_max_lifetime", cfg.ConnMaxLifetime.String(),
	}
	if mode == "secret" {
		// ARN 자체가 아니라 시크릿 이름만 남긴다(계정 ID를 로그에 흘리지 않는다).
		attrs = append(attrs, "secret_name", secretNameFromARN(cfg.DBSecretARN))
	}
	if cfg.ConnMaxLifetimeRejected != "" {
		// 지정값이 무시되고 기본값이 쓰였다는 유일한 신호다. 2-6이 T_fail을 설계로 만들 때
		// 이 줄을 놓치면 신규 연결이 유도되지 않은 이유를 찾지 못한다.
		attrs = append(attrs, "conn_max_lifetime_rejected", cfg.ConnMaxLifetimeRejected)
	}
	slog.Info("DB 자격증명 provider 준비", attrs...)
}

// secretNameFromARN은 ARN의 ":secret:" 뒤 이름만 뽑는다. 형식이 아니면 빈 값이다.
func secretNameFromARN(arn string) string {
	const marker = ":secret:"
	if i := strings.Index(arn, marker); i >= 0 {
		return arn[i+len(marker):]
	}
	return ""
}

// setupLogger는 LOG_LEVEL에 맞춰 JSON 구조화 로거를 전역 기본 로거로 설정하고,
// 모든 로그에 task_id를 붙인다.
func setupLogger(level string, taskID string) {
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
	slog.SetDefault(slog.New(handler).With("task_id", taskID))
}
