package links

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// maxCodeAttempts는 코드 충돌 시 재발급을 시도하는 최대 횟수다.
// 키공간이 충분히 크면(예: 7자 base62 ≈ 3.5조) 충돌은 사실상 없지만,
// 루프는 반드시 유한해야 하므로 한도를 둔다.
const maxCodeAttempts = 5

// ClickRecorder는 리다이렉트 한 번을 클릭으로 기록한다(plan 0013).
// 기록은 메모리에서 끝나야 한다 — 리다이렉트가 DB·네트워크를 기다리지 않게 하기 위해서다.
// DB 반영은 큐 뒤의 워커가 묶어서 한다(clicks 패키지).
type ClickRecorder interface {
	Record(code string)
}

// Service는 링크 도메인의 비즈니스 로직을 담는다(URL 검증, 코드 발급, 클릭 기록 정책).
// 저장소·코드 길이·클릭 기록기는 생성자 주입으로 받는다(의존성 주입).
type Service struct {
	repo    Repository
	codeLen int           // 발급할 단축 코드 길이(base62 문자 수)
	clicks  ClickRecorder // 리다이렉트의 클릭 기록
}

// NewService는 저장소·코드 길이·클릭 기록기를 주입받아 서비스를 만든다.
func NewService(repo Repository, codeLen int, clicks ClickRecorder) *Service {
	return &Service{repo: repo, codeLen: codeLen, clicks: clicks}
}

// Shorten은 원본 URL을 검증·정규화한 뒤 랜덤 코드를 발급해 단축 링크를 만든다.
// 코드가 충돌하면 새 코드로 재시도하고, 한도를 넘으면 ErrCodeExhausted를 반환한다.
func (s *Service) Shorten(ctx context.Context, rawURL string) (Link, error) {
	normalized, err := normalizeURL(rawURL)
	if err != nil {
		return Link{}, err
	}

	for attempt := 0; attempt < maxCodeAttempts; attempt++ {
		code, err := randomCode(s.codeLen)
		if err != nil {
			return Link{}, err // RNG 고장 → 즉시 실패
		}
		if isReserved(code) {
			continue // 예약 경로와 겹치면 다시 뽑는다.
		}
		link, err := s.repo.Create(ctx, code, normalized)
		if err == nil {
			return link, nil
		}
		if errors.Is(err, ErrCodeExists) {
			continue // 충돌 → 새 코드로 재시도
		}
		return Link{}, err // 그 밖의 오류는 전파
	}
	return Link{}, ErrCodeExhausted
}

// Resolve는 코드를 리다이렉트 대상 링크로 바꾼다.
// 부수효과로 클릭을 기록한다(리다이렉트가 곧 한 번의 방문이므로).
// 기록은 메모리에서 끝나고 요청 context와 무관하다 — 클라이언트가 끊겨도 클릭은 남는다.
func (s *Service) Resolve(ctx context.Context, code string) (Link, error) {
	link, err := s.repo.Get(ctx, code)
	if err != nil {
		return Link{}, err
	}
	// 중단된 링크는 이동시키지 않고 클릭으로 세지도 않는다.
	if link.Disabled() {
		return Link{}, ErrLinkDisabled
	}
	// 같은 행을 리다이렉트마다 UPDATE하면 인기 링크에서 행 잠금 경합이 첫 병목이었다(0012 기준선).
	s.clicks.Record(code)
	return link, nil
}

// Stats는 클릭 수를 포함한 링크 현황을 조회한다(집계하지 않는 순수 조회).
func (s *Service) Stats(ctx context.Context, code string) (Link, error) {
	return s.repo.Get(ctx, code)
}

// recentListLimit은 운영자 화면의 링크 목록 건수다.
const recentListLimit = 50

// SetDisabled는 운영자가 링크를 중단하거나 다시 켠다. 지우지 않으므로 클릭 기록이 남는다.
func (s *Service) SetDisabled(ctx context.Context, code string, disabled bool) (Link, error) {
	return s.repo.SetDisabled(ctx, code, disabled)
}

// ListRecent는 운영자 화면에 보여 줄 최근 링크 목록이다.
func (s *Service) ListRecent(ctx context.Context) ([]Link, error) {
	return s.repo.ListRecent(ctx, recentListLimit)
}

// normalizeURL은 입력 URL을 검증하고 정규화한다.
// http/https 스킴만 허용한다 → javascript:, data:, file: 등으로 인한
// 오픈 리다이렉트/XSS 악용을 차단한다(보안).
func normalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrInvalidURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", ErrInvalidURL
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", ErrInvalidURL
	}
	if u.Host == "" {
		return "", ErrInvalidURL
	}
	// userinfo(https://www.paypal.com@evil.example/)는 사람이 읽으면 앞쪽 도메인으로 보인다.
	// 요청은 사람이 URL을 보고 승인하므로(plan 0011) 판독을 속이는 이 형식은 서버가 먼저 거른다.
	if u.User != nil {
		return "", ErrInvalidURL
	}
	return u.String(), nil
}
