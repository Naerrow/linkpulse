// db 패키지는 Postgres 연결 풀 생성과 스키마 적용을 담당한다(인프라 어댑터).
package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib" // database/sql용 "pgx" 드라이버

	"github.com/Naerrow/linkpulse/app/internal/dbcreds"
)

//go:embed schema.sql
var schemaSQL string

// Settings는 연결 풀 생성에 필요한 구조화된 설정이다.
// DSN 문자열 하나를 받던 경계를 바꾼 이유는 회전 대응 provider와 검증용 손잡이를
// 명시적으로 주입하기 위해서다(plan 0009 2-2).
type Settings struct {
	// DSN은 접속 정보다. 여기 담긴 비밀번호는 secret 모드에서 provider의 seed로만 쓰이고,
	// 실제 dial 비밀번호는 매 시도마다 provider가 준다.
	DSN string
	// ConnMaxLifetime은 커넥션 최대 수명이다. 0이면 기본값(5분)을 쓴다.
	ConnMaxLifetime time.Duration
	// Provider가 nil이 아니면 인증 실패를 관찰해 비밀번호를 갱신하는 connector로 연다.
	// nil이면 기존 정적 동작 그대로다(로컬 docker-compose).
	Provider *dbcreds.Provider
}

// defaultConnMaxLifetime은 Settings.ConnMaxLifetime이 0일 때 쓰는 값이다.
const defaultConnMaxLifetime = 5 * time.Minute

// Open은 설정으로 연결 풀을 만들고, DB가 준비될 때까지 핑을 재시도한 뒤 스키마를 멱등 적용한다.
// 어느 단계든 실패하면 풀을 닫고 에러를 반환한다(기동 중단, fail-fast).
func Open(ctx context.Context, s Settings) (*sql.DB, error) {
	connConfig, err := pgx.ParseConfig(s.DSN)
	if err != nil {
		return nil, fmt.Errorf("DSN 파싱 실패: %w", err)
	}

	// secret 모드에서는 매 Connect가 provider에서 현재 비밀번호를 읽어 ConnConfig **사본**에
	// 주입한다. 원본은 절대 고치지 않는다 — 동시 연결이 서로 덮어쓴다.
	var connector driver.Connector = stdlib.GetConnector(*connConfig)
	if s.Provider != nil {
		connector = dbcreds.NewConnector(s.Provider, dialWith(*connConfig), stdlib.GetDefaultDriver())
	}
	pool := sql.OpenDB(connector)

	lifetime := s.ConnMaxLifetime
	if lifetime <= 0 {
		lifetime = defaultConnMaxLifetime
	}

	// 작은 서비스에 맞춘 보수적 풀 설정. 부하 테스트(P4) 결과에 따라 조정한다.
	pool.SetMaxOpenConns(25)
	pool.SetMaxIdleConns(25)
	pool.SetConnMaxLifetime(lifetime)
	// SetConnMaxIdleTime은 그대로 둔다 — 신규 연결을 유도하는 것은 lifetime 쪽이다.
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

	slog.Info("Postgres 연결·스키마 적용 완료", "conn_max_lifetime", lifetime.String())
	return pool, nil
}

// dialWith는 주어진 비밀번호로 실제 연결을 시도하는 함수를 만든다.
// base를 값으로 받아 매 호출이 자기 사본을 쓰게 한다.
func dialWith(base pgx.ConnConfig) dbcreds.DialFunc {
	return func(ctx context.Context, password string) (driver.Conn, error) {
		cfg := base // 사본
		cfg.Password = password
		return stdlib.GetConnector(cfg).Connect(ctx)
	}
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
