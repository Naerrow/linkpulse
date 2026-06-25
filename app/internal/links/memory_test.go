package links

import (
	"context"
	"errors"
	"testing"
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

// TestMemoryIncrementClicks는 클릭 증가와 없는 코드 처리를 확인한다.
func TestMemoryIncrementClicks(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()

	link, err := repo.Create(ctx, "code123", "https://example.com")
	if err != nil {
		t.Fatalf("Create 실패: %v", err)
	}
	if err := repo.IncrementClicks(ctx, link.Code); err != nil {
		t.Fatalf("IncrementClicks 실패: %v", err)
	}
	got, err := repo.Get(ctx, link.Code)
	if err != nil {
		t.Fatalf("Get 실패: %v", err)
	}
	if got.Clicks != 1 {
		t.Errorf("Clicks = %d, want 1", got.Clicks)
	}
	if err := repo.IncrementClicks(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("없는 코드 IncrementClicks err = %v, want ErrNotFound", err)
	}
}
