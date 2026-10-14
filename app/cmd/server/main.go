// linkpulse 애플리케이션 진입점.
//
// 설정을 읽고 → 구조화 로거를 세우고 → HTTP 서버를 띄운 뒤,
// 종료 시그널을 받으면 진행 중 요청을 마무리하고 우아하게 종료한다.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Naerrow/linkpulse/app/internal/clicks"
	"github.com/Naerrow/linkpulse/app/internal/clicks/sqsqueue"
	"github.com/Naerrow/linkpulse/app/internal/config"
	"github.com/Naerrow/linkpulse/app/internal/db"
	"github.com/Naerrow/linkpulse/app/internal/dbcreds"
	"github.com/Naerrow/linkpulse/app/internal/httpapi"
	"github.com/Naerrow/linkpulse/app/internal/links"
	"github.com/Naerrow/linkpulse/app/internal/metrics"
)

// shutdownTimeout은 종료 시 진행 중 요청을 기다려 주는 최대 시간이다.
const shutdownTimeout = 10 * time.Second

// poolStatsInterval은 DB 연결 풀 통계 로그 주기다. 태스크당 분당 6줄이라 비용은 무시할 만하다.
const poolStatsInterval = 10 * time.Second

// plan 0013 클릭 파이프라인.
const (
	// clickStatsInterval은 "click queue" 통계 로그 주기다(db pool 로그와 같다).
	clickStatsInterval = 10 * time.Second
	// finalFlushTimeout은 종료 때 마지막 클릭 발행의 상한이다(SDK 재시도 포함).
	// HTTP 종료(최대 shutdownTimeout 10초) + 이 값 + 워커 중지가 SIGKILL 전 유예 안에 든다
	// (차트 terminationGracePeriodSeconds 45초에서 preStop 10초를 뺀 35초).
	finalFlushTimeout = 5 * time.Second
)

// HTTP 서버 타임아웃. 느린/유휴 연결이 커넥션·메모리를 붙잡아 리소스를 고갈시키는 것을 막는다.
// 모든 핸들러가 1초 미만(리다이렉트·stats·create)이라 상수로 고정한다(자주 튜닝할 값 아님).
const (
	// 헤더 수신 상한(slowloris 최소 방어).
	readHeaderTimeout = 5 * time.Second
	// 본문까지 포함한 요청 전체 수신 상한. 본문은 8KiB로 이미 제한돼 있어 넉넉하다.
	readTimeout = 10 * time.Second
	// 응답 쓰기 상한. 레디니스 DB 핑 상한(1s, health.go)과 여유를 둔 값.
	writeTimeout = 15 * time.Second
	// keep-alive 유휴 연결 상한. ALB 유휴 타임아웃(60초, aws_lb 기본값)보다 길어야 한다(plan 0015).
	// 같거나 짧으면 서버가 막 닫는 연결에 ALB가 요청을 실어 502가 날 수 있다 — AWS는 대상의 keep-alive를
	// 로드 밸런서 유휴 타임아웃보다 길게 두라고 권한다(ELB 사용자 가이드 "Connection idle timeout").
	idleTimeout = 75 * time.Second
	// albIdleTimeout은 위 관계를 테스트로 고정하려고 둔 ALB 쪽 값이다. aws_lb idle_timeout을 바꾸면 같이 바꾼다.
	albIdleTimeout = 60 * time.Second
	// 내부 포트(/metrics·pprof)의 응답 쓰기 상한. pprof는 seconds가 WriteTimeout보다 긴 프로파일 요청을 거부하므로
	// 30초 프로파일(plan 0015 실험 E2)보다 길게 둔다.
	metricsWriteTimeout = 60 * time.Second
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

	// 지표(plan 0015). 내부 포트에서만 낸다 — 아래 newMetricsServer.
	m := metrics.New()

	// 저장소 → 서비스 → 라우터 순으로 의존성을 조립한다.
	// DATABASE_URL이 있으면 Postgres를, 없으면 인메모리(재시작 시 데이터 소실)를 쓴다.
	var repo links.Store
	var readiness func(context.Context) error
	if cfg.DatabaseURL != "" {
		// DB_PASSWORD_SOURCE가 있으면 회전 대응 provider를 붙인다. 없으면 정적 모드다(로컬).
		var provider *dbcreds.Provider
		if cfg.DBPasswordSource != "" {
			fetch, err := newPasswordFetch(context.Background(), cfg)
			if err != nil {
				slog.Error("DB 비밀번호 출처 설정 실패", "error", err)
				os.Exit(1)
			}
			// seed는 기동 때 출처에서 직접 읽는다(plan 0014). ECS처럼 넣어 주는 주체가 없다.
			provider, err = dbcreds.Start(context.Background(), fetch)
			if err != nil {
				slog.Error("DB 비밀번호 조회 실패", "error", err)
				os.Exit(1)
			}
			// 풀보다 뒤에 닫히도록 여기서 defer한다(LIFO) — 진행 중 refresh를 취소한다.
			defer provider.Close()
		}
		logProviderMode(provider, cfg)

		pool, err := db.Open(context.Background(), db.Settings{
			DSN:             cfg.DatabaseURL,
			ConnMaxLifetime: cfg.ConnMaxLifetime,
			MaxOpenConns:    cfg.DBMaxOpenConns,
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
		m.RegisterDB(pool)
		repo = links.NewPostgresRepository(pool)
		// readyz가 실제 DB 연결 상태를 반영하도록 핑 함수를 주입한다.
		readiness = func(ctx context.Context) error { return pool.PingContext(ctx) }
	} else {
		slog.Warn("DATABASE_URL 미설정 — 인메모리 저장소 사용(재시작 시 데이터 소실)")
		repo = links.NewMemoryRepository()
	}
	// plan 0013: 리다이렉트는 클릭을 메모리에 모으기만 하고, 1초마다 큐로 보낸다. 워커가 큐에서 받아 묶어서 반영한다.
	clickQueue := newClickQueue(cfg)
	clickStats := &clicks.Stats{}
	batcher := clicks.NewBatcher(clickQueue, clickStats)
	worker := clicks.NewWorker(clickQueue, repo, clickStats)
	m.RegisterClicks(clickStats, batcher.PendingCodes)
	linkSvc := links.NewService(repo, cfg.ShortCodeLength, batcher)
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
		Observer:  m,
	})
	srv := newServer(cfg, handler, m.ConnState)
	metricsSrv := newMetricsServer(cfg.MetricsAddr, m.Handler())

	// 클릭 파이프라인. 종료는 drainClicks가 HTTP 종료 뒤에 맡는다.
	clickCtx, stopClicks := context.WithCancel(context.Background())
	defer stopClicks()
	flushTicker := time.NewTicker(clicks.FlushInterval)
	defer flushTicker.Stop()
	go batcher.Run(clickCtx, flushTicker.C)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		worker.Run(clickCtx)
	}()
	pruneTicker := time.NewTicker(clicks.PruneInterval)
	defer pruneTicker.Stop()
	go worker.RunPrune(clickCtx, pruneTicker.C)
	statsTicker := time.NewTicker(clickStatsInterval)
	defer statsTicker.Stop()
	statsDone := make(chan struct{})
	go func() {
		defer close(statsDone)
		clicks.LogStats(clickCtx, clickStats, batcher.PendingCodes, statsTicker.C, slog.Default())
	}()
	drain := func() { drainClicks(stopClicks, batcher, finalFlushTimeout, workerDone, statsDone) }

	// 서버를 고루틴에서 띄우고, 리슨 실패는 채널로 전달한다. 지표 서버가 못 떠도 기동 실패다 —
	// 지표 없이 조용히 뜨면 알림 규칙이 그 파드를 보지 못한다.
	// 지표 서버는 따로 닫지 않는다. 종료 중에도 긁기를 받고, 프로세스가 끝날 때 함께 닫힌다.
	serverErr := make(chan error, 2)
	go func() {
		slog.Info("linkpulse 시작", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()
	go func() {
		slog.Info("지표 서버 시작", "addr", metricsSrv.Addr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

	if err := shutdown(srv, drain); err != nil {
		slog.Error("우아한 종료 실패", "error", err)
		os.Exit(1)
	}
	slog.Info("정상 종료 완료")
}

// newClickQueue는 CLICK_QUEUE_URL이 있으면 SQS, 없으면 메모리 큐를 만든다(config가 운영에서는 URL을 강제한다).
// 기동 로그에는 모드만 남긴다 — 큐 URL에는 계정 ID가 들어 있다.
func newClickQueue(cfg config.Config) clicks.Queue {
	if cfg.ClickQueueURL == "" {
		slog.Warn("CLICK_QUEUE_URL 미설정 — 메모리 큐 사용(프로세스 안에서만 돌고 재전달이 없다)", "click_queue", "memory")
		return clicks.NewMemoryQueue()
	}
	q, err := sqsqueue.New(context.Background(), cfg.ClickQueueURL, cfg.AWSRegion)
	if err != nil {
		slog.Error("클릭 큐(SQS) 설정 실패", "error", err)
		os.Exit(1)
	}
	slog.Info("클릭 큐", "click_queue", "sqs")
	return q
}

// flusher는 drainClicks가 쓰는 Batcher의 부분이다. 테스트가 가짜로 바꿔 끼운다.
type flusher interface {
	Flush(ctx context.Context) error
	PendingCodes() int
}

// drainClicks는 진행 중 요청이 끝난 뒤 클릭 파이프라인을 닫는다(plan 0013 "태스크 정상 종료: 유실 없음"의 경로).
//
//  1. stop — 주기 flush·워커·통계를 멈춘다. 통계 고루틴은 이때 마지막 증가분을 찍는다(HTTP가 닫혀 더는 Record가 없다).
//  2. 마지막 flush — 상한 timeout. SIGKILL 전에 끝나도록 잡았다(위 finalFlushTimeout 주석).
//  3. done이 모두 닫힐 때까지 기다린다 — 워커가 반영 중이던 배치와 통계 마지막 줄이 프로세스 종료로 잘리지 않게.
//
// 메모리 큐(로컬)는 워커가 먼저 멈추므로 마지막 배치는 반영되지 않는다 — SQS면 큐에 남아 다른 태스크가 반영한다.
func drainClicks(stop func(), f flusher, timeout time.Duration, done ...<-chan struct{}) {
	stop()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := f.Flush(ctx); err != nil {
		slog.Error("마지막 클릭 발행 실패", "pending_codes", f.PendingCodes(), "error", err)
	}
	for _, d := range done {
		<-d
	}
}

// shutdown은 HTTP 서버를 닫고(진행 중 요청 마무리), 그 결과와 상관없이 drain을 부른다.
// 서버 종료가 실패해도 버퍼에 남은 클릭은 내보내야 해서다(plan 0013 r1 #5). 서버 종료 오류를 돌려준다.
func shutdown(srv interface{ Shutdown(context.Context) error }, drain func()) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := srv.Shutdown(ctx)
	drain()
	return err
}

// newServer는 설정과 핸들러로 타임아웃이 세팅된 HTTP 서버를 조립한다.
// 타임아웃 단언을 단위 테스트할 수 있게 main에서 분리했다. connState는 새 연결 수 지표다(plan 0015).
func newServer(cfg config.Config, handler http.Handler, connState func(net.Conn, http.ConnState)) *http.Server {
	return &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ConnState:         connState,
	}
}

// newMetricsServer는 내부 포트(/metrics·pprof) 서버다(plan 0015). 공개 핸들러와 섞지 않는다 —
// ALB·TargetGroupBinding은 8080만 보므로 이 포트는 밖에서 닿지 않는다.
func newMetricsServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      metricsWriteTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// newPasswordFetch는 비밀번호 출처에 맞는 조회 함수를 만든다(plan 0014).
// 두 출처는 이 함수만 다르다 — 회전 뒤 다시 읽는 경로(provider·connector)는 같다.
func newPasswordFetch(ctx context.Context, cfg config.Config) (dbcreds.FetchFunc, error) {
	switch cfg.DBPasswordSource {
	case config.PasswordSourceAWS:
		return dbcreds.NewSecretsManagerFetch(ctx, cfg.DBSecretARN)
	case config.PasswordSourceFile:
		return dbcreds.NewFileFetch(cfg.DBPasswordFile), nil
	default:
		return nil, fmt.Errorf("알 수 없는 비밀번호 출처: %q", cfg.DBPasswordSource)
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
	if cfg.DBPasswordSource != "" {
		attrs = append(attrs, "password_source", cfg.DBPasswordSource)
	}
	if cfg.DBPasswordSource == config.PasswordSourceAWS {
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
