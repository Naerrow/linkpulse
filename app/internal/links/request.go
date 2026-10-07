package links

import (
	"context"
	"errors"
	"time"
)

// LinkRequest는 방문자가 낸 단축 링크 요청 한 건이다.
// 생성은 관리자만 하므로, 방문자는 요청을 남기고 관리자가 URL을 직접 보고 승인·거절한다.
// 개인정보(이메일 등)는 받지 않는다 — 방문자는 추측 불가능한 ID로 결과를 확인한다.
type LinkRequest struct {
	ID        string        // 16바이트 crypto/rand의 base64url. 요청 확인 링크의 열쇠다
	URL       string        // 요청된 원본 URL(접수 시 정규화됨)
	Note      string        // 방문자 메모(선택)
	Status    RequestStatus // pending → approved | rejected
	Code      string        // 승인 시 발급된 단축 코드. 그 전에는 빈 문자열
	CreatedAt time.Time
	DecidedAt time.Time // 결정 전에는 zero
}

// RequestStatus는 요청의 상태다. DB의 CHECK 제약과 같은 값을 쓴다.
type RequestStatus string

const (
	StatusPending  RequestStatus = "pending"
	StatusApproved RequestStatus = "approved"
	StatusRejected RequestStatus = "rejected"
)

// 요청 도메인 에러. 핸들러는 errors.Is로 분기해 HTTP 상태코드를 정한다.
var (
	// ErrRequestNotFound는 주어진 ID의 요청이 없을 때 반환된다.
	ErrRequestNotFound = errors.New("요청을 찾을 수 없음")
	// ErrRequestDecided는 이미 승인·거절된 요청을 다시 결정하려 할 때 반환된다.
	ErrRequestDecided = errors.New("이미 결정된 요청")
	// ErrTooManyPending은 대기 중인 요청이 상한에 닿아 새 요청을 받지 않을 때 반환된다.
	ErrTooManyPending = errors.New("대기 중인 요청이 너무 많음")
	// ErrNoteTooLong은 메모가 길이 상한을 넘을 때 반환된다.
	ErrNoteTooLong = errors.New("메모가 너무 김")
)

// RequestRepository는 링크 요청 저장소 추상화다.
// 승인은 "링크 생성 + 요청 상태 변경"을 하나로 묶어야 하므로(둘 중 하나만 반영되면 안 된다)
// 링크 저장소와 같은 구현체가 이 인터페이스도 만족한다 — 같은 락·같은 트랜잭션을 쓰기 위해서다.
type RequestRepository interface {
	// CreateRequest는 대기 중 요청이 maxPending 미만일 때만 저장한다. 아니면 ErrTooManyPending.
	CreateRequest(ctx context.Context, id, destURL, note string, maxPending int) (LinkRequest, error)
	// GetRequest는 ID로 요청을 조회한다. 없으면 ErrRequestNotFound.
	GetRequest(ctx context.Context, id string) (LinkRequest, error)
	// ListPending은 대기 중 요청을 최근순으로 최대 limit건 돌려준다.
	ListPending(ctx context.Context, limit int) ([]LinkRequest, error)
	// Approve는 요청의 URL로 code 링크를 만들고 요청을 approved로 바꾼다. 둘은 원자적이다.
	// 코드가 이미 있으면 ErrCodeExists(아무것도 바뀌지 않는다), 이미 결정됐으면 ErrRequestDecided.
	Approve(ctx context.Context, id, code string) (LinkRequest, Link, error)
	// Reject는 대기 중 요청을 rejected로 바꾼다. 이미 결정됐으면 ErrRequestDecided.
	Reject(ctx context.Context, id string) (LinkRequest, error)
}

// Store는 링크와 요청을 함께 다루는 저장소다. 두 구현 모두 만족한다(컴파일 시점에 확인).
type Store interface {
	Repository
	RequestRepository
}

var (
	_ Store = (*MemoryRepository)(nil)
	_ Store = (*PostgresRepository)(nil)
)
