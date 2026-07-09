// config 패키지는 환경변수에서 애플리케이션 설정을 읽어들인다.
// 비밀값을 포함한 모든 설정은 코드가 아니라 환경변수로만 주입한다(가드레일 #2).
package config

import (
	"errors"
	"fmt"
	"net"
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
	// 실행 환경 (APP_ENV): ""(기본=development)|development|production.
	// production이면 DB 미설정 시 인메모리 폴백을 금지하고 기동을 중단한다(Load 참고).
	AppEnv string
	// Postgres 접속 문자열. DATABASE_URL이 있으면 그 값을, 없으면 DB_* 개별 변수로
	// 조립한 값을 담는다. 비어 있으면 인메모리 저장소를 사용한다. (resolveDatabaseURL 참고)
	DatabaseURL string
}

// 인식되는 APP_ENV 값. 그 외(예: "prod" 오타)는 fail-fast로 막아
// 가드가 조용히 비활성화되는 footgun을 방지한다.
const (
	envDevelopment = "development"
	envProduction  = "production"
)

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

	// DB 접속 문자열을 해석한다(DATABASE_URL 우선, 없으면 DB_* 조립).
	dsn, err := resolveDatabaseURL()
	if err != nil {
		return Config{}, err
	}

	// 실행 환경을 확인한다. 빈 값은 development(로컬 편의)로 본다.
	appEnv := os.Getenv("APP_ENV")
	switch appEnv {
	case "", envDevelopment, envProduction:
		// 인식되는 값.
	default:
		return Config{}, fmt.Errorf("APP_ENV가 올바르지 않습니다(허용: %s|%s, 빈 값=개발): %q",
			envDevelopment, envProduction, appEnv)
	}
	// 운영 모드에서 DB가 없으면 조용히 인메모리로 떠 데이터가 증발하는 사고를 막는다(fail-fast).
	if appEnv == envProduction && dsn == "" {
		return Config{}, errors.New("APP_ENV=production인데 DATABASE_URL/DB_*가 미설정입니다 — 인메모리 폴백 금지")
	}

	return Config{
		Port:            getEnv("APP_PORT", "8080"),
		LogLevel:        getEnv("LOG_LEVEL", "info"),
		PublicBaseURL:   baseURL,
		ShortCodeLength: codeLen,
		AppEnv:          appEnv,
		// 비어 있으면 main에서 인메모리 저장소로 폴백한다(로컬 개발 편의).
		DatabaseURL: dsn,
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

// resolveDatabaseURL은 DB 접속 문자열(DSN)을 결정한다.
//
// 우선순위:
//  1. DATABASE_URL이 설정돼 있으면 그대로 사용한다(로컬 docker-compose 하위호환).
//  2. 아니면 DB_HOST/DB_USER/DB_PASSWORD/DB_NAME으로 URL을 조립한다. 운영(ECS)에서는
//     비밀번호를 Secrets Manager에서 DB_PASSWORD로만 분리 주입하기 위함이다(가드레일 #2).
//  3. 핵심 4개가 모두 비어 있으면 빈 문자열을 돌려준다 → 호출부가 인메모리로 폴백한다.
//
// 핵심 4개(DB_HOST/DB_USER/DB_PASSWORD/DB_NAME) 중 하나라도 설정되면 4개 전부를 필수
// 검사하고, 누락된 키를 명시해 에러를 반환한다 — 운영에서 일부만 주입돼 조용히 인메모리로
// 떠 데이터가 증발하는 사고를 막는다(fail-fast). DB_PORT/DB_SSLMODE는 기본값(5432/require)이
// 있는 보조값이라 이 트리거에서 제외한다.
func resolveDatabaseURL() (string, error) {
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		return dsn, nil
	}

	host := os.Getenv("DB_HOST")
	user := os.Getenv("DB_USER")
	password := os.Getenv("DB_PASSWORD")
	name := os.Getenv("DB_NAME")

	// 핵심 4개가 전부 비면 DB 미설정으로 보고 인메모리로 폴백한다(로컬 개발 편의).
	if host == "" && user == "" && password == "" && name == "" {
		return "", nil
	}

	// 하나라도 설정됐으면 전부 필수 — 어떤 키가 빠졌는지 알려 준다(fail-fast).
	var missing []string
	if host == "" {
		missing = append(missing, "DB_HOST")
	}
	if user == "" {
		missing = append(missing, "DB_USER")
	}
	if password == "" {
		missing = append(missing, "DB_PASSWORD")
	}
	if name == "" {
		missing = append(missing, "DB_NAME")
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("DB 접속 설정이 일부만 지정됐습니다. 누락: %s", strings.Join(missing, ", "))
	}

	port := getEnv("DB_PORT", "5432")
	sslmode := getEnv("DB_SSLMODE", "require")

	// url.URL로 조립해 비밀번호의 특수문자를 안전하게 퍼센트 인코딩한다
	// (RDS가 생성한 비밀번호에 @ / 등이 섞여도 DSN이 깨지지 않게).
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     net.JoinHostPort(host, port),
		Path:     "/" + name,
		RawQuery: url.Values{"sslmode": {sslmode}}.Encode(),
	}
	return u.String(), nil
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
