package links

import (
	"context"
	"errors"
)

// 도메인 에러. 상위 레이어(서비스/핸들러)는 errors.Is로 분기해 HTTP 상태코드를 정한다.
var (
	// ErrNotFound는 주어진 코드의 링크가 없을 때 반환된다.
	ErrNotFound = errors.New("링크를 찾을 수 없음")
	// ErrInvalidURL은 단축 대상 URL이 유효하지 않을 때 반환된다.
	ErrInvalidURL = errors.New("유효하지 않은 URL")
	// ErrCodeExists는 저장소에 같은 코드가 이미 있을 때 Create가 반환한다.
	// 서비스는 이 에러를 보고 새 코드로 재시도한다(인메모리=맵 충돌, Postgres=UNIQUE 위반).
	ErrCodeExists = errors.New("이미 존재하는 코드")
	// ErrCodeExhausted는 재시도 한도 안에 빈 코드를 찾지 못했을 때 반환된다(사실상 키공간 포화).
	ErrCodeExhausted = errors.New("사용 가능한 코드를 발급하지 못함")
)

// Repository는 링크 저장소 추상화다.
// 인메모리 구현으로 먼저 완결하고, 같은 인터페이스 뒤에 Postgres 구현을 끼워 넣는다.
//
// 단축 코드는 서비스가 생성해 넘긴다. 저장소는 유일성만 판단한다 →
// 같은 코드가 있으면 ErrCodeExists. 이 분담 덕에 Postgres에서도 UNIQUE 제약으로
// 동일한 충돌 처리를 그대로 구현할 수 있다.
type Repository interface {
	// Create는 코드와 URL로 링크를 저장하고, 저장된 Link(생성시각 포함)를 반환한다.
	// 코드가 이미 있으면 ErrCodeExists.
	Create(ctx context.Context, code, destURL string) (Link, error)
	// Get은 코드로 링크를 조회한다. 없으면 ErrNotFound.
	Get(ctx context.Context, code string) (Link, error)
	// IncrementClicks는 코드의 클릭 수를 1 늘린다. 없으면 ErrNotFound.
	IncrementClicks(ctx context.Context, code string) error
}
