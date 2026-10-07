package links

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryRepository는 Repository의 인메모리 구현이다.
// Postgres 도입 전까지 로컬에서 전체 플로우를 완결하고 단위 테스트를 돌리는 데 쓴다.
// 프로세스 재시작 시 데이터가 사라진다(영속성 없음).
type MemoryRepository struct {
	mu       sync.Mutex              // 두 map을 함께 보호한다(승인이 둘을 한 번에 바꾸므로 락도 하나다).
	byCode   map[string]*Link        // 코드 → 링크
	requests map[string]*LinkRequest // 요청 ID → 링크 요청
}

// NewMemoryRepository는 빈 인메모리 저장소를 만든다.
func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{byCode: make(map[string]*Link), requests: make(map[string]*LinkRequest)}
}

// Create는 코드/URL로 링크를 저장한다. 같은 코드가 있으면 ErrCodeExists.
// 검사와 삽입을 같은 락 구간에서 처리해 원자적이다(Postgres의 UNIQUE 제약과 같은 의미).
func (r *MemoryRepository) Create(_ context.Context, code, destURL string) (Link, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.byCode[code]; exists {
		return Link{}, ErrCodeExists
	}
	link := &Link{
		Code:      code,
		URL:       destURL,
		Clicks:    0,
		CreatedAt: time.Now().UTC(),
	}
	r.byCode[code] = link
	return *link, nil // 내부 포인터가 새 나가지 않도록 복사본을 반환한다.
}

// Get은 코드로 링크 복사본을 조회한다.
func (r *MemoryRepository) Get(_ context.Context, code string) (Link, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	link, ok := r.byCode[code]
	if !ok {
		return Link{}, ErrNotFound
	}
	return *link, nil
}

// IncrementClicks는 클릭 수를 원자적으로 1 늘린다.
func (r *MemoryRepository) IncrementClicks(_ context.Context, code string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	link, ok := r.byCode[code]
	if !ok {
		return ErrNotFound
	}
	link.Clicks++
	return nil
}

// SetDisabled는 링크의 중단 시각을 바꾼다.
func (r *MemoryRepository) SetDisabled(_ context.Context, code string, disabled bool) (Link, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	link, ok := r.byCode[code]
	if !ok {
		return Link{}, ErrNotFound
	}
	switch {
	case disabled && link.DisabledAt.IsZero():
		link.DisabledAt = time.Now().UTC()
	case !disabled:
		link.DisabledAt = time.Time{}
	}
	return *link, nil
}

// ListRecent는 링크를 최근 생성순으로 최대 limit건 돌려준다.
func (r *MemoryRepository) ListRecent(_ context.Context, limit int) ([]Link, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Link, 0, len(r.byCode))
	for _, link := range r.byCode {
		out = append(out, *link)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
