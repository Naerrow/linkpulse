package links

import (
	"context"
	"sort"
	"time"
)

// CreateRequest는 대기 중 요청 수를 확인하고 저장한다. 확인과 삽입이 같은 락 구간이라 상한이 정확하다.
func (r *MemoryRepository) CreateRequest(_ context.Context, id, destURL, note string, maxPending int) (LinkRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	pending := 0
	for _, req := range r.requests {
		if req.Status == StatusPending {
			pending++
		}
	}
	if pending >= maxPending {
		return LinkRequest{}, ErrTooManyPending
	}
	req := &LinkRequest{
		ID:        id,
		URL:       destURL,
		Note:      note,
		Status:    StatusPending,
		CreatedAt: time.Now().UTC(),
	}
	r.requests[id] = req
	return *req, nil
}

// GetRequest는 ID로 요청 복사본을 조회한다.
func (r *MemoryRepository) GetRequest(_ context.Context, id string) (LinkRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	req, ok := r.requests[id]
	if !ok {
		return LinkRequest{}, ErrRequestNotFound
	}
	return *req, nil
}

// ListPending은 대기 중 요청을 최근순으로 최대 limit건 돌려준다.
func (r *MemoryRepository) ListPending(_ context.Context, limit int) ([]LinkRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]LinkRequest, 0)
	for _, req := range r.requests {
		if req.Status == StatusPending {
			out = append(out, *req)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Approve는 같은 락 안에서 링크를 만들고 요청을 승인 상태로 바꾼다.
// 검사를 모두 끝낸 뒤에만 쓰므로, 실패하면 아무것도 바뀌지 않는다(Postgres 트랜잭션과 같은 의미).
func (r *MemoryRepository) Approve(_ context.Context, id, code string) (LinkRequest, Link, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	req, ok := r.requests[id]
	if !ok {
		return LinkRequest{}, Link{}, ErrRequestNotFound
	}
	if req.Status != StatusPending {
		return LinkRequest{}, Link{}, ErrRequestDecided
	}
	if _, exists := r.byCode[code]; exists {
		return LinkRequest{}, Link{}, ErrCodeExists
	}
	now := time.Now().UTC()
	link := &Link{Code: code, URL: req.URL, CreatedAt: now}
	r.byCode[code] = link
	req.Status = StatusApproved
	req.Code = code
	req.DecidedAt = now
	return *req, *link, nil
}

// Reject는 대기 중 요청을 거절 상태로 바꾼다.
func (r *MemoryRepository) Reject(_ context.Context, id string) (LinkRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	req, ok := r.requests[id]
	if !ok {
		return LinkRequest{}, ErrRequestNotFound
	}
	if req.Status != StatusPending {
		return LinkRequest{}, ErrRequestDecided
	}
	req.Status = StatusRejected
	req.DecidedAt = time.Now().UTC()
	return *req, nil
}
