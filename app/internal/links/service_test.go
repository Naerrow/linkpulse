package links

import (
	"context"
	"errors"
	"testing"
	"time"
)

const testCodeLen = 7

// TestShortenValid는 정상 URL이 설정 길이의 코드로 단축되는지 확인한다.
func TestShortenValid(t *testing.T) {
	svc := NewService(NewMemoryRepository(), testCodeLen)

	link, err := svc.Shorten(context.Background(), "https://example.com/page?q=1")
	if err != nil {
		t.Fatalf("Shorten 실패: %v", err)
	}
	if len(link.Code) != testCodeLen {
		t.Errorf("코드 길이 = %d, want %d (code=%q)", len(link.Code), testCodeLen, link.Code)
	}
	if link.URL != "https://example.com/page?q=1" {
		t.Errorf("URL = %q, 원본과 다름", link.URL)
	}
	if link.Clicks != 0 {
		t.Errorf("Clicks = %d, want 0", link.Clicks)
	}
}

// TestShortenRejectsInvalidURL는 비-http(s)·빈 값·스킴 누락을 거부하는지 확인한다.
func TestShortenRejectsInvalidURL(t *testing.T) {
	svc := NewService(NewMemoryRepository(), testCodeLen)
	bad := []string{
		"",
		"   ",
		"example.com",                          // 스킴 없음
		"ftp://example.com",                    // 허용 안 되는 스킴
		"javascript:alert(1)",                  // XSS 악용 차단
		"https://",                             // 호스트 없음
		"https://www.paypal.com@evil.example/", // userinfo로 도메인 위장
	}
	for _, in := range bad {
		if _, err := svc.Shorten(context.Background(), in); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("Shorten(%q) err = %v, want ErrInvalidURL", in, err)
		}
	}
}

// TestResolveCountsClicks는 Resolve가 대상을 돌려주면서 클릭을 집계하는지 확인한다.
func TestResolveCountsClicks(t *testing.T) {
	svc := NewService(NewMemoryRepository(), testCodeLen)
	ctx := context.Background()

	created, err := svc.Shorten(ctx, "https://example.com")
	if err != nil {
		t.Fatalf("Shorten 실패: %v", err)
	}
	for i := 0; i < 3; i++ {
		got, err := svc.Resolve(ctx, created.Code)
		if err != nil {
			t.Fatalf("Resolve 실패: %v", err)
		}
		if got.URL != "https://example.com" {
			t.Errorf("URL = %q, want %q", got.URL, "https://example.com")
		}
	}
	stats, err := svc.Stats(ctx, created.Code)
	if err != nil {
		t.Fatalf("Stats 실패: %v", err)
	}
	if stats.Clicks != 3 {
		t.Errorf("Clicks = %d, want 3", stats.Clicks)
	}
}

// TestResolveNotFound는 없는 코드 Resolve가 ErrNotFound인지 확인한다.
func TestResolveNotFound(t *testing.T) {
	svc := NewService(NewMemoryRepository(), testCodeLen)
	if _, err := svc.Resolve(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// flakyRepo는 코드 충돌 재시도 로직을 검증하기 위한 스텁이다.
// Create가 처음 failTimes번은 ErrCodeExists를 돌려주고 그 뒤 성공한다.
type flakyRepo struct {
	failTimes int
	calls     int
}

func (r *flakyRepo) Create(_ context.Context, code, destURL string) (Link, error) {
	r.calls++
	if r.calls <= r.failTimes {
		return Link{}, ErrCodeExists
	}
	return Link{Code: code, URL: destURL}, nil
}
func (r *flakyRepo) Get(context.Context, string) (Link, error)     { return Link{}, ErrNotFound }
func (r *flakyRepo) IncrementClicks(context.Context, string) error { return nil }
func (r *flakyRepo) SetDisabled(context.Context, string, bool) (Link, error) {
	return Link{}, ErrNotFound
}
func (r *flakyRepo) ListRecent(context.Context, int) ([]Link, error) { return nil, nil }

// TestShortenRetriesOnCollision은 충돌 시 새 코드로 재시도해 결국 성공하는지 확인한다.
func TestShortenRetriesOnCollision(t *testing.T) {
	repo := &flakyRepo{failTimes: 2} // 2번 충돌 후 3번째 성공
	svc := NewService(repo, testCodeLen)

	if _, err := svc.Shorten(context.Background(), "https://example.com"); err != nil {
		t.Fatalf("Shorten 실패: %v", err)
	}
	if repo.calls != 3 {
		t.Errorf("Create 호출 횟수 = %d, want 3", repo.calls)
	}
}

// TestShortenExhaustsAttempts는 계속 충돌하면 ErrCodeExhausted를 반환하는지 확인한다.
func TestShortenExhaustsAttempts(t *testing.T) {
	repo := &flakyRepo{failTimes: 1000} // 항상 충돌
	svc := NewService(repo, testCodeLen)

	if _, err := svc.Shorten(context.Background(), "https://example.com"); !errors.Is(err, ErrCodeExhausted) {
		t.Errorf("err = %v, want ErrCodeExhausted", err)
	}
	if repo.calls != maxCodeAttempts {
		t.Errorf("Create 호출 횟수 = %d, want %d", repo.calls, maxCodeAttempts)
	}
}

// TestDisabledLinkDoesNotRedirect는 중단된 링크가 이동하지 않고 클릭도 세지 않으며, 다시 켜면 돌아오는지 본다(plan 0011).
func TestDisabledLinkDoesNotRedirect(t *testing.T) {
	repo := NewMemoryRepository()
	svc := NewService(repo, 7)
	ctx := context.Background()

	link, _ := svc.Shorten(ctx, "https://example.com/later-bad")
	first, err := svc.SetDisabled(ctx, link.Code, true)
	if err != nil || !first.Disabled() {
		t.Fatalf("SetDisabled(true) = %+v, %v", first, err)
	}
	if _, err := svc.Resolve(ctx, link.Code); !errors.Is(err, ErrLinkDisabled) {
		t.Fatalf("중단 뒤 Resolve err = %v, want ErrLinkDisabled", err)
	}
	if got, _ := svc.Stats(ctx, link.Code); got.Clicks != 0 {
		t.Errorf("중단된 링크의 클릭이 %d로 셌다", got.Clicks)
	}
	// 다시 중단해도 처음 중단 시각을 유지한다. 시계가 확실히 움직이도록 잠깐 기다린다
	// (두 호출이 같은 마이크로초에 끝나면 덮어써도 같은 값이라 검사가 무의미해진다).
	time.Sleep(2 * time.Millisecond)
	again, _ := svc.SetDisabled(ctx, link.Code, true)
	if !again.DisabledAt.Equal(first.DisabledAt) {
		t.Errorf("재중단이 시각을 바꿈: %v → %v", first.DisabledAt, again.DisabledAt)
	}

	if _, err := svc.SetDisabled(ctx, link.Code, false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(ctx, link.Code); err != nil {
		t.Errorf("다시 켠 뒤 Resolve err = %v", err)
	}
	if _, err := svc.SetDisabled(ctx, "missing", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("없는 코드 err = %v, want ErrNotFound", err)
	}
}

// TestListRecentLimit은 최근 목록이 중단된 링크도 포함하고 recentListLimit건까지만 주는지 본다.
func TestListRecentLimit(t *testing.T) {
	svc := NewService(NewMemoryRepository(), 7)
	ctx := context.Background()

	var codes []string
	for i := 0; i < recentListLimit+5; i++ {
		l, _ := svc.Shorten(ctx, "https://example.com")
		codes = append(codes, l.Code)
	}
	if _, err := svc.SetDisabled(ctx, codes[len(codes)-1], true); err != nil {
		t.Fatal(err)
	}
	list, err := svc.ListRecent(ctx)
	if err != nil || len(list) != recentListLimit {
		t.Fatalf("ListRecent = %d건, %v, want %d", len(list), err, recentListLimit)
	}
}
