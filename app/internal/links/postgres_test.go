package links

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// newTestDB는 TEST_DATABASE_URL이 있으면 연결을 돌려주고, 없으면 테스트를 건너뛴다.
// 매 테스트가 깨끗한 상태에서 시작하도록 links 테이블을 준비·비운다.
// 덕분에 DB 없이 `go test ./...`를 돌려도 통합 테스트는 skip되어 통과한다.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 미설정 — Postgres 통합 테스트 건너뜀")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open 실패: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	const schema = `CREATE TABLE IF NOT EXISTS links (
		code TEXT PRIMARY KEY,
		url TEXT NOT NULL,
		clicks BIGINT NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now())`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		t.Fatalf("스키마 준비 실패: %v", err)
	}
	if _, err := db.ExecContext(ctx, `TRUNCATE links`); err != nil {
		t.Fatalf("TRUNCATE 실패: %v", err)
	}
	return db
}

// TestPostgresCreateAndGet은 저장·조회와 DB가 채운 생성시각을 확인한다.
func TestPostgresCreateAndGet(t *testing.T) {
	repo := NewPostgresRepository(newTestDB(t))
	ctx := context.Background()

	created, err := repo.Create(ctx, "abc1234", "https://example.com")
	if err != nil {
		t.Fatalf("Create 실패: %v", err)
	}
	if created.CreatedAt.IsZero() {
		t.Error("created_at이 비어 있음(DB 기본값 미적용)")
	}

	got, err := repo.Get(ctx, "abc1234")
	if err != nil {
		t.Fatalf("Get 실패: %v", err)
	}
	if got.URL != "https://example.com" {
		t.Errorf("URL = %q, want %q", got.URL, "https://example.com")
	}
}

// TestPostgresCreateDuplicate은 같은 코드 INSERT가 ErrCodeExists로 매핑되는지 확인한다.
func TestPostgresCreateDuplicate(t *testing.T) {
	repo := NewPostgresRepository(newTestDB(t))
	ctx := context.Background()

	if _, err := repo.Create(ctx, "dup1234", "https://a.com"); err != nil {
		t.Fatalf("첫 Create 실패: %v", err)
	}
	if _, err := repo.Create(ctx, "dup1234", "https://b.com"); !errors.Is(err, ErrCodeExists) {
		t.Errorf("중복 코드 err = %v, want ErrCodeExists", err)
	}
}

// TestPostgresGetNotFound는 없는 코드 조회가 ErrNotFound인지 확인한다.
func TestPostgresGetNotFound(t *testing.T) {
	repo := NewPostgresRepository(newTestDB(t))
	if _, err := repo.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestPostgresIncrementClicks는 클릭 증가와 없는 코드 처리를 확인한다.
func TestPostgresIncrementClicks(t *testing.T) {
	repo := NewPostgresRepository(newTestDB(t))
	ctx := context.Background()

	if _, err := repo.Create(ctx, "clk1234", "https://example.com"); err != nil {
		t.Fatalf("Create 실패: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := repo.IncrementClicks(ctx, "clk1234"); err != nil {
			t.Fatalf("IncrementClicks 실패: %v", err)
		}
	}
	got, err := repo.Get(ctx, "clk1234")
	if err != nil {
		t.Fatalf("Get 실패: %v", err)
	}
	if got.Clicks != 2 {
		t.Errorf("Clicks = %d, want 2", got.Clicks)
	}
	if err := repo.IncrementClicks(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("없는 코드 IncrementClicks err = %v, want ErrNotFound", err)
	}
}
