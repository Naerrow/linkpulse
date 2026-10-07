package links

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// 아래 테스트는 TEST_DATABASE_URL이 있을 때만 돈다(newTestDB가 없으면 건너뛴다).

// TestPostgresRequestCreateGetReject는 접수·조회·거절과 재결정 거부를 확인한다.
func TestPostgresRequestCreateGetReject(t *testing.T) {
	repo := NewPostgresRepository(newTestDB(t))
	ctx := context.Background()

	created, err := repo.CreateRequest(ctx, "req-1", "https://example.com", "메모", 10)
	if err != nil {
		t.Fatalf("CreateRequest 실패: %v", err)
	}
	if created.Status != StatusPending || created.CreatedAt.IsZero() || !created.DecidedAt.IsZero() {
		t.Errorf("접수 결과 = %+v", created)
	}
	got, err := repo.GetRequest(ctx, "req-1")
	if err != nil || got.Note != "메모" {
		t.Fatalf("GetRequest = %+v, %v", got, err)
	}
	if _, err := repo.GetRequest(ctx, "missing"); !errors.Is(err, ErrRequestNotFound) {
		t.Errorf("없는 요청 err = %v, want ErrRequestNotFound", err)
	}

	rejected, err := repo.Reject(ctx, "req-1")
	if err != nil || rejected.Status != StatusRejected || rejected.DecidedAt.IsZero() {
		t.Fatalf("Reject = %+v, %v", rejected, err)
	}
	if _, err := repo.Reject(ctx, "req-1"); !errors.Is(err, ErrRequestDecided) {
		t.Errorf("재거절 err = %v, want ErrRequestDecided", err)
	}
	if _, err := repo.Reject(ctx, "missing"); !errors.Is(err, ErrRequestNotFound) {
		t.Errorf("없는 요청 거절 err = %v, want ErrRequestNotFound", err)
	}
}

// TestPostgresRequestPendingCap은 대기 상한에서 삽입을 막는지 확인한다.
func TestPostgresRequestPendingCap(t *testing.T) {
	repo := NewPostgresRepository(newTestDB(t))
	ctx := context.Background()

	for _, id := range []string{"a", "b"} {
		if _, err := repo.CreateRequest(ctx, id, "https://example.com", "", 2); err != nil {
			t.Fatalf("CreateRequest(%s) 실패: %v", id, err)
		}
	}
	if _, err := repo.CreateRequest(ctx, "c", "https://example.com", "", 2); !errors.Is(err, ErrTooManyPending) {
		t.Errorf("상한 초과 err = %v, want ErrTooManyPending", err)
	}
	list, err := repo.ListPending(ctx, 10)
	if err != nil || len(list) != 2 {
		t.Errorf("ListPending = %d건, %v, want 2건", len(list), err)
	}
}

// TestPostgresApproveIsAtomic은 승인이 링크와 요청을 함께 바꾸고, 코드 충돌이면 둘 다 그대로인지 확인한다.
func TestPostgresApproveIsAtomic(t *testing.T) {
	repo := NewPostgresRepository(newTestDB(t))
	ctx := context.Background()

	if _, err := repo.Create(ctx, "taken01", "https://other.example"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateRequest(ctx, "req-a", "https://example.com/a", "", 10); err != nil {
		t.Fatal(err)
	}

	// 이미 있는 코드로 승인 → ErrCodeExists, 요청은 여전히 대기.
	if _, _, err := repo.Approve(ctx, "req-a", "taken01"); !errors.Is(err, ErrCodeExists) {
		t.Fatalf("충돌 err = %v, want ErrCodeExists", err)
	}
	if got, _ := repo.GetRequest(ctx, "req-a"); got.Status != StatusPending || got.Code != "" {
		t.Fatalf("충돌 뒤 요청 = %+v, 롤백되지 않음", got)
	}

	req, link, err := repo.Approve(ctx, "req-a", "fresh01")
	if err != nil {
		t.Fatalf("Approve 실패: %v", err)
	}
	if req.Status != StatusApproved || req.Code != "fresh01" || link.URL != "https://example.com/a" {
		t.Errorf("승인 결과 = %+v / %+v", req, link)
	}
	if _, _, err := repo.Approve(ctx, "req-a", "other01"); !errors.Is(err, ErrRequestDecided) {
		t.Errorf("재승인 err = %v, want ErrRequestDecided", err)
	}
	if _, err := repo.Get(ctx, "other01"); !errors.Is(err, ErrNotFound) {
		t.Errorf("재승인이 링크를 남김(err = %v)", err)
	}
}

// TestPostgresApproveConcurrent는 같은 요청을 동시에 승인해도 한 건만 성공하고 고아 링크가 없는지 확인한다.
func TestPostgresApproveConcurrent(t *testing.T) {
	db := newTestDB(t)
	repo := NewPostgresRepository(db)
	ctx := context.Background()
	if _, err := repo.CreateRequest(ctx, "req-race", "https://example.com/race", "", 10); err != nil {
		t.Fatal(err)
	}

	const n = 8
	codes := []string{"race001", "race002", "race003", "race004", "race005", "race006", "race007", "race008"}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(code string) {
			defer wg.Done()
			_, _, err := repo.Approve(ctx, "req-race", code)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else if !errors.Is(err, ErrRequestDecided) {
				t.Errorf("예상 밖 오류: %v", err)
			}
		}(codes[i])
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("성공 %d건, want 1", wins)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM links`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("links %d행, want 1(고아 링크 없음)", count)
	}
}
