package links

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf8"
)

const (
	// MaxRequestURLLen은 요청 URL의 길이 상한이다(브라우저·프록시가 무난히 다루는 범위).
	MaxRequestURLLen = 2048
	// MaxRequestNoteLen은 메모의 글자 수 상한이다.
	MaxRequestNoteLen = 200
	// maxPendingRequests는 대기 중 요청의 상한이다. 스팸이 쌓여도 관리자 화면이 감당할 수 있는 양으로 막는다.
	maxPendingRequests = 100
	// pendingListLimit은 관리자 목록 한 번에 보여 줄 건수다.
	pendingListLimit = 50
	// requestIDBytes는 요청 ID의 난수 바이트 수다. 128비트라 추측·열거가 불가능하다.
	requestIDBytes = 16
)

// RequestService는 링크 요청의 비즈니스 로직이다(접수 검증, 승인 시 코드 발급).
type RequestService struct {
	repo    RequestRepository
	codeLen int
}

// NewRequestService는 저장소와 코드 길이를 주입받아 서비스를 만든다.
func NewRequestService(repo RequestRepository, codeLen int) *RequestService {
	return &RequestService{repo: repo, codeLen: codeLen}
}

// Submit은 방문자의 요청을 검증해 대기 상태로 저장한다.
func (s *RequestService) Submit(ctx context.Context, rawURL, note string) (LinkRequest, error) {
	if len(rawURL) > MaxRequestURLLen {
		return LinkRequest{}, ErrInvalidURL
	}
	normalized, err := normalizeURL(rawURL)
	if err != nil {
		return LinkRequest{}, err
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > MaxRequestNoteLen {
		return LinkRequest{}, ErrNoteTooLong
	}
	id, err := newRequestID()
	if err != nil {
		return LinkRequest{}, err
	}
	return s.repo.CreateRequest(ctx, id, normalized, note, maxPendingRequests)
}

// Get은 ID로 요청을 조회한다. ID 형식이 틀리면 저장소에 가지 않고 없음으로 답한다.
func (s *RequestService) Get(ctx context.Context, id string) (LinkRequest, error) {
	if !validRequestID(id) {
		return LinkRequest{}, ErrRequestNotFound
	}
	return s.repo.GetRequest(ctx, id)
}

// ListPending은 관리자가 볼 대기 목록을 돌려준다.
func (s *RequestService) ListPending(ctx context.Context) ([]LinkRequest, error) {
	return s.repo.ListPending(ctx, pendingListLimit)
}

// Approve는 요청을 승인하고 단축 링크를 발급한다. 코드 충돌 시 Shorten과 같은 규칙으로 재시도한다.
func (s *RequestService) Approve(ctx context.Context, id string) (LinkRequest, Link, error) {
	if !validRequestID(id) {
		return LinkRequest{}, Link{}, ErrRequestNotFound
	}
	for attempt := 0; attempt < maxCodeAttempts; attempt++ {
		code, err := randomCode(s.codeLen)
		if err != nil {
			return LinkRequest{}, Link{}, err
		}
		if isReserved(code) {
			continue
		}
		req, link, err := s.repo.Approve(ctx, id, code)
		if errors.Is(err, ErrCodeExists) {
			continue // 저장소가 전부 되돌렸으므로 새 코드로 다시 시도한다.
		}
		return req, link, err
	}
	return LinkRequest{}, Link{}, ErrCodeExhausted
}

// Reject는 요청을 거절한다.
func (s *RequestService) Reject(ctx context.Context, id string) (LinkRequest, error) {
	if !validRequestID(id) {
		return LinkRequest{}, ErrRequestNotFound
	}
	return s.repo.Reject(ctx, id)
}

// newRequestID는 crypto/rand 16바이트를 base64url(패딩 없음, 22자)로 만든다.
func newRequestID() (string, error) {
	b := make([]byte, requestIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// validRequestID는 ID가 newRequestID가 만드는 형식인지 확인한다.
func validRequestID(id string) bool {
	b, err := base64.RawURLEncoding.DecodeString(id)
	return err == nil && len(b) == requestIDBytes
}
