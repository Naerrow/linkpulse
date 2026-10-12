package links

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	if _, err := db.ExecContext(ctx, `ALTER TABLE links ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ`); err != nil {
		t.Fatalf("disabled_at 열 준비 실패: %v", err)
	}
	// link_requests가 links를 참조하므로 함께 만들고 함께 비운다(참조된 테이블만 TRUNCATE하면 실패한다).
	const requestSchema = `CREATE TABLE IF NOT EXISTS link_requests (
		id TEXT PRIMARY KEY,
		url TEXT NOT NULL,
		note TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected')),
		code TEXT REFERENCES links (code),
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		decided_at TIMESTAMPTZ)`
	if _, err := db.ExecContext(ctx, requestSchema); err != nil {
		t.Fatalf("요청 스키마 준비 실패: %v", err)
	}
	const clickBatchSchema = `CREATE TABLE IF NOT EXISTS click_batches (
		id TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`
	if _, err := db.ExecContext(ctx, clickBatchSchema); err != nil {
		t.Fatalf("click_batches 스키마 준비 실패: %v", err)
	}
	if _, err := db.ExecContext(ctx, `TRUNCATE link_requests, links, click_batches`); err != nil {
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

// TestPostgresApplyClickBatch는 메모리 구현과 같은 의미(반영·중복 거르기·없는 코드)를 확인한다(plan 0013).
func TestPostgresApplyClickBatch(t *testing.T) {
	testApplyClickBatch(t, NewPostgresRepository(newTestDB(t)))
}

// TestPostgresApplyClickBatchConcurrent는 겹치는 코드를 담은 배치 두 개를 동시에 반영해도 교착이 없고 합계가 맞는지 본다.
// counts는 map이라 두 트랜잭션의 순회 순서가 매번 다르다. 코드를 50개나 겹쳐 두었으므로,
// 정렬 없이 UPDATE하면 거의 모든 회차에서 서로 반대 순서로 잠그다 교착(40P01)이 난다.
func TestPostgresApplyClickBatchConcurrent(t *testing.T) {
	repo := NewPostgresRepository(newTestDB(t))
	ctx := context.Background()

	const nCodes, rounds = 50, 200
	counts := make(map[string]int64, nCodes)
	for i := 0; i < nCodes; i++ {
		code := fmt.Sprintf("cc%05d", i)
		if _, err := repo.Create(ctx, code, "https://example.com"); err != nil {
			t.Fatalf("Create 실패: %v", err)
		}
		counts[code] = 1
	}

	var failed atomic.Int64
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		for w := 0; w < 2; w++ {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				if _, _, err := repo.ApplyClickBatch(ctx, id, counts); err != nil {
					if failed.Add(1) <= 3 {
						t.Errorf("배치 %s 반영 실패: %v", id, err)
					}
				}
			}(fmt.Sprintf("r%03d-w%d", r, w))
		}
		wg.Wait()
	}
	if n := failed.Load(); n > 0 {
		t.Fatalf("반영 실패 %d건(교착 의심)", n)
	}
	for code := range counts {
		got, err := repo.Get(ctx, code)
		if err != nil {
			t.Fatalf("Get(%s) 실패: %v", code, err)
		}
		if got.Clicks != 2*rounds {
			t.Fatalf("%s Clicks = %d, want %d", code, got.Clicks, 2*rounds)
		}
	}
}

// TestPostgresPruneClickBatches는 보존 기간이 지난 배치 ID만 지워지는지 본다.
func TestPostgresPruneClickBatches(t *testing.T) {
	db := newTestDB(t)
	repo := NewPostgresRepository(db)
	ctx := context.Background()
	for _, id := range []string{"b-old", "b-new"} {
		if _, _, err := repo.ApplyClickBatch(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE click_batches SET applied_at = now() - interval '48 hours' WHERE id = 'b-old'`); err != nil {
		t.Fatal(err)
	}
	if n, err := repo.PruneClickBatches(ctx, 24*time.Hour); err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v, want 1", n, err)
	}
	if dup, _, _ := repo.ApplyClickBatch(ctx, "b-new", nil); !dup {
		t.Error("보존 기간 안의 배치가 지워졌다")
	}
}

// TestPostgresSetDisabledAndListRecent는 중단·재개와 최근 목록을 확인한다(plan 0011).
func TestPostgresSetDisabledAndListRecent(t *testing.T) {
	repo := NewPostgresRepository(newTestDB(t))
	ctx := context.Background()

	if _, err := repo.Create(ctx, "keep001", "https://example.com/keep"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, "stop001", "https://example.com/stop"); err != nil {
		t.Fatal(err)
	}
	first, err := repo.SetDisabled(ctx, "stop001", true)
	if err != nil || !first.Disabled() {
		t.Fatalf("SetDisabled(true) = %+v, %v", first, err)
	}
	again, _ := repo.SetDisabled(ctx, "stop001", true)
	if !again.DisabledAt.Equal(first.DisabledAt) {
		t.Errorf("재중단이 시각을 바꿈: %v → %v", first.DisabledAt, again.DisabledAt)
	}
	got, err := repo.Get(ctx, "stop001")
	if err != nil || !got.Disabled() {
		t.Errorf("Get = %+v, %v — 중단 상태가 저장되지 않음", got, err)
	}
	if back, err := repo.SetDisabled(ctx, "stop001", false); err != nil || back.Disabled() {
		t.Errorf("SetDisabled(false) = %+v, %v", back, err)
	}
	if _, err := repo.SetDisabled(ctx, "missing", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("없는 코드 err = %v, want ErrNotFound", err)
	}
	list, err := repo.ListRecent(ctx, 10)
	if err != nil || len(list) != 2 {
		t.Errorf("ListRecent = %d건, %v, want 2", len(list), err)
	}
}
