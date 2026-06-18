// config 패키지는 환경변수에서 애플리케이션 설정을 읽어들인다.
// 비밀값을 포함한 모든 설정은 코드가 아니라 환경변수로만 주입한다(가드레일 #2).
package config

import "os"

// Config는 실행에 필요한 설정값 모음이다.
type Config struct {
	Port     string // 리슨 포트 (APP_PORT, 기본 8080)
	LogLevel string // 로그 레벨 (LOG_LEVEL): debug|info|warn|error
}

// Load는 환경변수를 읽어 Config를 만든다.
// 필수값이 없으면 에러를 반환해 기동을 중단시킨다(fail-fast).
// 현재 단계에는 필수값이 없으며, DB 접속 문자열 등은 도입 시점에 검증을 추가한다.
func Load() (Config, error) {
	cfg := Config{
		Port:     getEnv("APP_PORT", "8080"),
		LogLevel: getEnv("LOG_LEVEL", "info"),
	}
	return cfg, nil
}

// getEnv는 환경변수를 읽되, 비어 있으면 기본값을 돌려준다.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
