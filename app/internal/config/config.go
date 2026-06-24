// config 패키지는 환경변수에서 애플리케이션 설정을 읽어들인다.
// 비밀값을 포함한 모든 설정은 코드가 아니라 환경변수로만 주입한다(가드레일 #2).
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	defaultShortCodeLength = 7
	// 코드가 짧을수록 열거가 쉬워진다(보안). 기본 7을 권장하며,
	// 아래는 명백히 잘못된 설정을 막는 하한/상한이다.
	minShortCodeLength = 4
	maxShortCodeLength = 32
)

// Config는 실행에 필요한 설정값 모음이다.
type Config struct {
	Port            string // 리슨 포트 (APP_PORT, 기본 8080)
	LogLevel        string // 로그 레벨 (LOG_LEVEL): debug|info|warn|error
	PublicBaseURL   string // 단축 URL에 붙일 외부 공개 주소 (PUBLIC_BASE_URL)
	ShortCodeLength int    // 단축 코드 길이 (SHORT_CODE_LENGTH, 기본 7)
}

// Load는 환경변수를 읽어 Config를 만든다.
// 잘못된 값은 에러를 반환해 기동을 중단시킨다(fail-fast) — 운영 중 조용히 깨진 응답을
// 내보내는 것보다 부팅에서 실패하는 편이 낫다.
func Load() (Config, error) {
	// 코드와 합칠 때 슬래시 중복을 막기 위해 뒤쪽 "/"를 제거한 뒤 검증한다.
	baseURL := strings.TrimRight(getEnv("PUBLIC_BASE_URL", "http://localhost:8080"), "/")
	if err := validatePublicBaseURL(baseURL); err != nil {
		return Config{}, err
	}

	codeLen, err := getEnvInt("SHORT_CODE_LENGTH", defaultShortCodeLength)
	if err != nil {
		return Config{}, err
	}
	if codeLen < minShortCodeLength || codeLen > maxShortCodeLength {
		return Config{}, fmt.Errorf("SHORT_CODE_LENGTH는 %d~%d 사이여야 합니다(현재 %d)",
			minShortCodeLength, maxShortCodeLength, codeLen)
	}

	return Config{
		Port:            getEnv("APP_PORT", "8080"),
		LogLevel:        getEnv("LOG_LEVEL", "info"),
		PublicBaseURL:   baseURL,
		ShortCodeLength: codeLen,
	}, nil
}

// validatePublicBaseURL은 공개 주소가 절대 http/https URL인지 검증한다.
// 이 값은 외부 API 응답(short_url)에 그대로 노출되므로 형식이 깨지면 안 된다.
func validatePublicBaseURL(raw string) error {
	if raw == "" {
		return errors.New("PUBLIC_BASE_URL이 비어 있습니다")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("PUBLIC_BASE_URL 파싱 실패: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("PUBLIC_BASE_URL은 http 또는 https여야 합니다: %q", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("PUBLIC_BASE_URL에 호스트가 없습니다: %q", raw)
	}
	return nil
}

// getEnv는 환경변수를 읽되, 비어 있으면 기본값을 돌려준다.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getEnvInt는 환경변수를 정수로 읽되, 비어 있으면 기본값을 돌려준다.
// 정수가 아니면 에러를 반환한다(fail-fast).
func getEnvInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s는 정수여야 합니다: %q", key, v)
	}
	return n, nil
}
