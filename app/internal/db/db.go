// db 패키지는 Postgres 연결 풀 생성과 스키마 적용을 담당한다(인프라 어댑터).
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql용 "pgx" 드라이버 등록
)

//go:embed schema.sql
var schemaSQL string

// Open은 DSN으로 연결 풀을 만들고, DB가 준비될 때까지 핑을 재시도한 뒤 스키마를 멱등 적용한다.
// 어느 단계든 실패하면 풀을 닫고 에러를 반환한다(기동 중단, fail-fast).
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("DB 핸들 생성 실패: %w", err)
	}

	// 작은 서비스에 맞춘 보수적 풀 설정. 부하 테스트(P4) 결과에 따라 조정한다.
	pool.SetMaxOpenConns(25)
	pool.SetMaxIdleConns(25)
	pool.SetConnMaxLifetime(5 * time.Minute)
	pool.SetConnMaxIdleTime(5 * time.Minute)

	if err := pingWithRetry(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	// 스키마는 CREATE TABLE IF NOT EXISTS라 매 기동 호출해도 안전하다(멱등).
	if _, err := pool.ExecContext(ctx, schemaSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("스키마 적용 실패: %w", err)
	}

	slog.Info("Postgres 연결·스키마 적용 완료")
	return pool, nil
}

// pingWithRetry는 DB가 기동 직후 아직 안 떠 있을 수 있으므로(특히 docker-compose에서
// 앱이 먼저 뜨는 경우) 짧은 간격으로 핑을 재시도한다. 끝내 실패하면 에러를 반환한다.
func pingWithRetry(ctx context.Context, pool *sql.DB) error {
	const (
		maxAttempts  = 15
		retryBackoff = 1 * time.Second
		pingTimeout  = 3 * time.Second
	)

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
		err := pool.PingContext(pingCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		slog.Warn("DB 핑 실패, 재시도", "attempt", attempt, "max", maxAttempts, "error", err)

		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryBackoff):
			}
		}
	}
	return fmt.Errorf("DB 연결 실패(%d회 시도): %w", maxAttempts, lastErr)
}
