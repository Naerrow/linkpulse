package links

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestMemoryCreateRejectsDuplicate는 같은 코드 재삽입이 ErrCodeExists인지 확인한다.
func TestMemoryCreateRejectsDuplicate(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()

	if _, err := repo.Create(ctx, "abc1234", "https://example.com/a"); err != nil {
		t.Fatalf("첫 Create 실패: %v", err)
	}
	if _, err := repo.Create(ctx, "abc1234", "https://example.com/b"); !errors.Is(err, ErrCodeExists) {
		t.Errorf("중복 코드 err = %v, want ErrCodeExists", err)
	}
	// 다른 코드는 정상 저장되어야 한다.
	if _, err := repo.Create(ctx, "xyz9876", "https://example.com/c"); err != nil {
		t.Errorf("다른 코드 Create 실패: %v", err)
	}
}

// TestMemoryGetNotFound는 없는 코드 조회 시 ErrNotFound를 반환하는지 확인한다.
func TestMemoryGetNotFound(t *testing.T) {
	repo := NewMemoryRepository()
	if _, err := repo.Get(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestMemoryApplyClickBatch는 배치 반영, 같은 배치 ID의 중복 거르기, 없는 코드 건너뛰기를 확인한다(plan 0013).
func TestMemoryApplyClickBatch(t *testing.T) {
	testApplyClickBatch(t, NewMemoryRepository())
}

// TestMemoryPruneClickBatches는 보존 기간이 지난 배치 ID만 지워지는지 본다.
func TestMemoryPruneClickBatches(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	if _, _, err := repo.ApplyClickBatch(ctx, "b-old", nil); err != nil {
		t.Fatal(err)
	}
	repo.clickBatches["b-old"] = time.Now().Add(-48 * time.Hour)
	if _, _, err := repo.ApplyClickBatch(ctx, "b-new", nil); err != nil {
		t.Fatal(err)
	}
	if n, err := repo.PruneClickBatches(ctx, 24*time.Hour); err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v, want 1", n, err)
	}
	if dup, _, _ := repo.ApplyClickBatch(ctx, "b-new", nil); !dup {
		t.Error("보존 기간 안의 배치가 지워졌다")
	}
	if dup, _, _ := repo.ApplyClickBatch(ctx, "b-old", nil); dup {
		t.Error("보존 기간이 지난 배치가 남았다")
	}
}

// testApplyClickBatch는 두 저장소 구현이 같은 의미를 갖는지 확인하는 공용 본문이다.
func testApplyClickBatch(t *testing.T, repo interface {
	Repository
	ClickBatchRepository
}) {
	t.Helper()
	ctx := context.Background()
	for _, code := range []string{"clk0001", "clk0002"} {
		if _, err := repo.Create(ctx, code, "https://example.com"); err != nil {
			t.Fatalf("Create 실패: %v", err)
		}
	}

	dup, unknown, err := repo.ApplyClickBatch(ctx, "batch-1", map[string]int64{"clk0001": 3, "clk0002": 5, "nope": 7})
	if err != nil || dup || unknown != 7 {
		t.Fatalf("첫 반영 = dup %v, unknown %d, err %v; want false, 7, nil", dup, unknown, err)
	}
	// 같은 배치가 다시 오면(재전달) 아무것도 바꾸지 않는다.
	dup, unknown, err = repo.ApplyClickBatch(ctx, "batch-1", map[string]int64{"clk0001": 3, "clk0002": 5})
	if err != nil || !dup || unknown != 0 {
		t.Fatalf("중복 반영 = dup %v, unknown %d, err %v; want true, 0, nil", dup, unknown, err)
	}
	if _, _, err := repo.ApplyClickBatch(ctx, "batch-2", map[string]int64{"clk0001": 1}); err != nil {
		t.Fatal(err)
	}
	for code, want := range map[string]int64{"clk0001": 4, "clk0002": 5} {
		got, err := repo.Get(ctx, code)
		if err != nil {
			t.Fatalf("Get(%s) 실패: %v", code, err)
		}
		if got.Clicks != want {
			t.Errorf("%s Clicks = %d, want %d", code, got.Clicks, want)
		}
	}
}
