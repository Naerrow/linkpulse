package links

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// newRequestTestSetup은 같은 인메모리 저장소를 쓰는 링크·요청 서비스를 만든다.
func newRequestTestSetup() (*Service, *RequestService, *MemoryRepository) {
	repo := NewMemoryRepository()
	return NewService(repo, 7), NewRequestService(repo, 7), repo
}

// TestSubmitValid는 접수가 대기 상태·정규화된 URL·다듬은 메모·올바른 ID 형식을 돌려주는지 본다.
func TestSubmitValid(t *testing.T) {
	_, rs, _ := newRequestTestSetup()

	req, err := rs.Submit(context.Background(), "  https://example.com/a  ", "  블로그 링크  ")
	if err != nil {
		t.Fatalf("Submit 실패: %v", err)
	}
	if req.Status != StatusPending {
		t.Errorf("status = %q, want pending", req.Status)
	}
	if req.URL != "https://example.com/a" {
		t.Errorf("url = %q, 정규화되지 않음", req.URL)
	}
	if req.Note != "블로그 링크" {
		t.Errorf("note = %q, 앞뒤 공백이 남음", req.Note)
	}
	if !validRequestID(req.ID) || len(req.ID) != 22 {
		t.Errorf("id = %q, 16바이트 base64url(22자) 형식이 아님", req.ID)
	}
}

// TestSubmitRejectsInvalidInput은 위험한 스킴·빈 값·길이 초과를 거절하는지 본다.
func TestSubmitRejectsInvalidInput(t *testing.T) {
	_, rs, _ := newRequestTestSetup()
	ctx := context.Background()

	long := "https://example.com/" + strings.Repeat("a", MaxRequestURLLen)
	for _, u := range []string{"", "javascript:alert(1)", "data:text/html,x", "example.com", long} {
		if _, err := rs.Submit(ctx, u, ""); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("Submit(%q) err = %v, want ErrInvalidURL", u[:min(len(u), 30)], err)
		}
	}
	// 글자 수 기준이다 — 한글 200자는 통과, 201자는 거절.
	if _, err := rs.Submit(ctx, "https://example.com", strings.Repeat("가", MaxRequestNoteLen)); err != nil {
		t.Errorf("메모 %d자인데 거절됨: %v", MaxRequestNoteLen, err)
	}
	if _, err := rs.Submit(ctx, "https://example.com", strings.Repeat("가", MaxRequestNoteLen+1)); !errors.Is(err, ErrNoteTooLong) {
		t.Errorf("메모 %d자 err = %v, want ErrNoteTooLong", MaxRequestNoteLen+1, err)
	}
}

// TestSubmitPendingCap은 대기 상한에서 새 요청을 막고, 하나를 결정하면 다시 받는지 본다.
func TestSubmitPendingCap(t *testing.T) {
	_, rs, _ := newRequestTestSetup()
	ctx := context.Background()

	var first LinkRequest
	for i := 0; i < maxPendingRequests; i++ {
		req, err := rs.Submit(ctx, "https://example.com", "")
		if err != nil {
			t.Fatalf("요청 %d 실패: %v", i+1, err)
		}
		if i == 0 {
			first = req
		}
	}
	if _, err := rs.Submit(ctx, "https://example.com", ""); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("상한 초과 err = %v, want ErrTooManyPending", err)
	}
	if _, err := rs.Reject(ctx, first.ID); err != nil {
		t.Fatalf("Reject 실패: %v", err)
	}
	if _, err := rs.Submit(ctx, "https://example.com", ""); err != nil {
		t.Errorf("하나를 거절했는데도 접수 실패: %v", err)
	}
}

// TestGetRequestNotFound는 형식이 틀린 ID와 없는 ID를 모두 없음으로 답하는지 본다.
func TestGetRequestNotFound(t *testing.T) {
	_, rs, _ := newRequestTestSetup()
	ctx := context.Background()

	missing, _ := newRequestID()
	for _, id := range []string{"", "short", "../../etc", missing} {
		if _, err := rs.Get(ctx, id); !errors.Is(err, ErrRequestNotFound) {
			t.Errorf("Get(%q) err = %v, want ErrRequestNotFound", id, err)
		}
	}
}

// TestApproveCreatesLink는 승인이 요청 URL로 링크를 만들고, 그 링크가 실제로 리다이렉트되는지 본다.
func TestApproveCreatesLink(t *testing.T) {
	ls, rs, _ := newRequestTestSetup()
	ctx := context.Background()

	req, _ := rs.Submit(ctx, "https://example.com/approved", "")
	got, link, err := rs.Approve(ctx, req.ID)
	if err != nil {
		t.Fatalf("Approve 실패: %v", err)
	}
	if got.Status != StatusApproved || got.Code != link.Code || got.DecidedAt.IsZero() {
		t.Errorf("승인 결과 = %+v, 상태·코드·결정시각이 맞지 않음", got)
	}
	resolved, err := ls.Resolve(ctx, link.Code)
	if err != nil || resolved.URL != "https://example.com/approved" {
		t.Errorf("Resolve = %+v, %v — 승인된 링크가 요청 URL로 가야 한다", resolved, err)
	}
	stored, _ := rs.Get(ctx, req.ID)
	if stored.Status != StatusApproved || stored.Code != link.Code {
		t.Errorf("저장된 요청 = %+v, 승인이 반영되지 않음", stored)
	}
}

// TestDecisionIsFinal은 한 번 결정한 요청을 다시 승인·거절할 수 없는지 본다.
func TestDecisionIsFinal(t *testing.T) {
	_, rs, _ := newRequestTestSetup()
	ctx := context.Background()

	approved, _ := rs.Submit(ctx, "https://example.com/1", "")
	if _, _, err := rs.Approve(ctx, approved.ID); err != nil {
		t.Fatalf("Approve 실패: %v", err)
	}
	if _, _, err := rs.Approve(ctx, approved.ID); !errors.Is(err, ErrRequestDecided) {
		t.Errorf("재승인 err = %v, want ErrRequestDecided", err)
	}
	if _, err := rs.Reject(ctx, approved.ID); !errors.Is(err, ErrRequestDecided) {
		t.Errorf("승인 뒤 거절 err = %v, want ErrRequestDecided", err)
	}

	rejected, _ := rs.Submit(ctx, "https://example.com/2", "")
	if _, err := rs.Reject(ctx, rejected.ID); err != nil {
		t.Fatalf("Reject 실패: %v", err)
	}
	if _, _, err := rs.Approve(ctx, rejected.ID); !errors.Is(err, ErrRequestDecided) {
		t.Errorf("거절 뒤 승인 err = %v, want ErrRequestDecided", err)
	}
}

// TestApproveConcurrentOnlyOneWins는 같은 요청을 동시에 승인해도 한 번만 성공하고 링크도 하나만 생기는지 본다.
func TestApproveConcurrentOnlyOneWins(t *testing.T) {
	_, rs, repo := newRequestTestSetup()
	ctx := context.Background()
	req, _ := rs.Submit(ctx, "https://example.com/race", "")

	const n = 20
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    int
		decided int
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := rs.Approve(ctx, req.ID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrRequestDecided):
				decided++
			default:
				t.Errorf("예상 밖 오류: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || decided != n-1 {
		t.Errorf("성공 %d · 이미 결정 %d, want 1 · %d", wins, decided, n-1)
	}
	repo.mu.Lock()
	links := len(repo.byCode)
	repo.mu.Unlock()
	if links != 1 {
		t.Errorf("생성된 링크 %d개, want 1(고아 링크 없음)", links)
	}
}

// collidingRequestRepo는 처음 collisions번의 Approve를 ErrCodeExists로 실패시킨다.
type collidingRequestRepo struct {
	*MemoryRepository
	collisions int
	calls      int
}

func (r *collidingRequestRepo) Approve(ctx context.Context, id, code string) (LinkRequest, Link, error) {
	r.calls++
	if r.calls <= r.collisions {
		return LinkRequest{}, Link{}, ErrCodeExists
	}
	return r.MemoryRepository.Approve(ctx, id, code)
}

// TestApproveRetriesOnCollision은 코드 충돌 시 새 코드로 재시도하고, 한도를 넘으면 포기하는지 본다.
func TestApproveRetriesOnCollision(t *testing.T) {
	ctx := context.Background()

	repo := &collidingRequestRepo{MemoryRepository: NewMemoryRepository(), collisions: 2}
	rs := NewRequestService(repo, 7)
	req, _ := rs.Submit(ctx, "https://example.com", "")
	if _, _, err := rs.Approve(ctx, req.ID); err != nil {
		t.Fatalf("충돌 2번 뒤 성공해야 하는데 %v", err)
	}
	if repo.calls != 3 {
		t.Errorf("Approve 호출 %d번, want 3", repo.calls)
	}

	exhausted := &collidingRequestRepo{MemoryRepository: NewMemoryRepository(), collisions: maxCodeAttempts}
	rs = NewRequestService(exhausted, 7)
	req, _ = rs.Submit(ctx, "https://example.com", "")
	if _, _, err := rs.Approve(ctx, req.ID); !errors.Is(err, ErrCodeExhausted) {
		t.Errorf("err = %v, want ErrCodeExhausted", err)
	}
}

// TestListPendingExcludesDecidedAndLimits는 결정된 요청을 빼고 한 번에 pendingListLimit건까지만 주는지 본다.
func TestListPendingExcludesDecidedAndLimits(t *testing.T) {
	_, rs, _ := newRequestTestSetup()
	ctx := context.Background()

	var ids []string
	for i := 0; i < pendingListLimit+10; i++ {
		req, _ := rs.Submit(ctx, "https://example.com", "")
		ids = append(ids, req.ID)
	}
	if _, err := rs.Reject(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	list, err := rs.ListPending(ctx)
	if err != nil {
		t.Fatalf("ListPending 실패: %v", err)
	}
	if len(list) != pendingListLimit {
		t.Errorf("목록 %d건, want %d", len(list), pendingListLimit)
	}
	for _, req := range list {
		if req.Status != StatusPending {
			t.Errorf("목록에 %s 상태가 섞임", req.Status)
		}
	}
}
