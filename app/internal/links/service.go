package links

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
)

// maxCodeAttempts는 코드 충돌 시 재발급을 시도하는 최대 횟수다.
// 키공간이 충분히 크면(예: 7자 base62 ≈ 3.5조) 충돌은 사실상 없지만,
// 루프는 반드시 유한해야 하므로 한도를 둔다.
const maxCodeAttempts = 5

// Service는 링크 도메인의 비즈니스 로직을 담는다(URL 검증, 코드 발급, 클릭 집계 정책).
// 저장소와 코드 길이는 생성자 주입으로 받는다(의존성 주입).
type Service struct {
	repo    Repository
	codeLen int // 발급할 단축 코드 길이(base62 문자 수)
}

// NewService는 저장소와 코드 길이를 주입받아 서비스를 만든다.
func NewService(repo Repository, codeLen int) *Service {
	return &Service{repo: repo, codeLen: codeLen}
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
// 부수효과로 클릭을 집계한다(리다이렉트가 곧 한 번의 방문이므로).
// 집계는 베스트-에포트다: 집계가 실패해도 사용자 리다이렉트는 막지 않는다.
func (s *Service) Resolve(ctx context.Context, code string) (Link, error) {
	link, err := s.repo.Get(ctx, code)
	if err != nil {
		return Link{}, err
	}
	if err := s.repo.IncrementClicks(ctx, code); err != nil {
		slog.Error("클릭 집계 실패", "code", code, "error", err)
	}
	return link, nil
}

// Stats는 클릭 수를 포함한 링크 현황을 조회한다(집계하지 않는 순수 조회).
func (s *Service) Stats(ctx context.Context, code string) (Link, error) {
	return s.repo.Get(ctx, code)
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
	return u.String(), nil
}
