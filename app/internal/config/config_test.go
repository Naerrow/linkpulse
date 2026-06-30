package config

import (
	"net/url"
	"strings"
	"testing"
)

// DATABASE_URL이 있으면 DB_*보다 우선해야 한다(로컬 docker-compose 하위호환).
func TestResolveDatabaseURL_PrefersDatabaseURL(t *testing.T) {
	const want = "postgres://u:p@h:5432/db?sslmode=disable"
	t.Setenv("DATABASE_URL", want)
	t.Setenv("DB_HOST", "ignored") // 우선순위 확인용 — 무시되어야 한다

	got, err := resolveDatabaseURL()
	if err != nil {
		t.Fatalf("예상치 못한 에러: %v", err)
	}
	if got != want {
		t.Errorf("DATABASE_URL이 우선되어야 한다: got %q, want %q", got, want)
	}
}

// DB_* 개별 변수로 DSN을 조립하고, 포트·sslmode 기본값과 비밀번호 인코딩을 검증한다.
func TestResolveDatabaseURL_AssemblesFromParts(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DB_HOST", "db.example.com")
	t.Setenv("DB_USER", "linkpulse")
	t.Setenv("DB_PASSWORD", "p@ss/w0rd ") // 특수문자·공백 포함
	t.Setenv("DB_NAME", "linkpulse")
	t.Setenv("DB_PORT", "")    // 미설정 → 기본 5432
	t.Setenv("DB_SSLMODE", "") // 미설정 → 기본 require

	got, err := resolveDatabaseURL()
	if err != nil {
		t.Fatalf("예상치 못한 에러: %v", err)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("조립된 DSN 파싱 실패: %v (%q)", err, got)
	}
	if u.Scheme != "postgres" {
		t.Errorf("scheme는 postgres여야 한다: %q", u.Scheme)
	}
	if u.Hostname() != "db.example.com" || u.Port() != "5432" {
		t.Errorf("host:port 불일치: %q", u.Host)
	}
	if u.Path != "/linkpulse" {
		t.Errorf("dbname 경로 불일치: %q", u.Path)
	}
	// 특수문자 비밀번호가 인코딩 왕복 후 원래 값으로 복원되어야 한다.
	if pw, _ := u.User.Password(); pw != "p@ss/w0rd " {
		t.Errorf("비밀번호 인코딩 왕복 실패: %q", pw)
	}
	if got := u.Query().Get("sslmode"); got != "require" {
		t.Errorf("sslmode 기본값은 require여야 한다: %q", got)
	}
}

// 핵심 4개가 모두 비면 빈 문자열 → 인메모리 폴백.
func TestResolveDatabaseURL_EmptyWhenUnset(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DB_HOST", "")
	t.Setenv("DB_USER", "")
	t.Setenv("DB_PASSWORD", "")
	t.Setenv("DB_NAME", "")

	got, err := resolveDatabaseURL()
	if err != nil {
		t.Fatalf("예상치 못한 에러: %v", err)
	}
	if got != "" {
		t.Errorf("아무 설정도 없으면 빈 문자열이어야 한다: %q", got)
	}
}

// 핵심 4개 중 일부만(여기선 DB_HOST) 있으면 나머지 누락으로 fail-fast.
func TestResolveDatabaseURL_PartialIsError(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DB_HOST", "db.example.com")
	t.Setenv("DB_USER", "")
	t.Setenv("DB_PASSWORD", "")
	t.Setenv("DB_NAME", "")

	if _, err := resolveDatabaseURL(); err == nil {
		t.Fatal("DB_HOST만 있고 나머지가 비면 에러여야 한다")
	}
}

// 보조값이 아닌 핵심값(DB_PASSWORD)만 설정되고 DB_HOST가 없으면, 누락 키를 명시해 에러여야 한다.
func TestResolveDatabaseURL_PasswordOnlyIsError(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DB_HOST", "")
	t.Setenv("DB_USER", "")
	t.Setenv("DB_PASSWORD", "secret")
	t.Setenv("DB_NAME", "")

	_, err := resolveDatabaseURL()
	if err == nil {
		t.Fatal("DB_PASSWORD만 설정되고 나머지가 비면 에러여야 한다")
	}
	if !strings.Contains(err.Error(), "DB_HOST") {
		t.Errorf("누락된 키(DB_HOST)를 에러에 명시해야 한다: %v", err)
	}
}

// Load 전체 경로에서 DB_*가 cfg.DatabaseURL로 조립되는지 확인한다.
func TestLoad_AssemblesDSNFromParts(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DB_HOST", "h")
	t.Setenv("DB_USER", "u")
	t.Setenv("DB_PASSWORD", "pw")
	t.Setenv("DB_NAME", "n")
	t.Setenv("DB_PORT", "")
	t.Setenv("DB_SSLMODE", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 실패: %v", err)
	}
	if cfg.DatabaseURL == "" {
		t.Fatal("DB_*로 DSN이 조립되어야 한다")
	}
}
