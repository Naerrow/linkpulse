package config

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// DATABASE_URL이 있으면 DB_*보다 우선해야 한다(로컬 docker-compose 하위호환).
func TestResolveDatabaseURL_PrefersDatabaseURL(t *testing.T) {
	const want = "postgres://u:p@h:5432/db?sslmode=disable"
	t.Setenv("DATABASE_URL", want)
	t.Setenv("DB_HOST", "ignored") // 우선순위 확인용 — 무시되어야 한다

	got, _, err := resolveDatabaseURL()
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

	got, seed, err := resolveDatabaseURL()
	if seed != "p@ss/w0rd " {
		t.Errorf("seed 비밀번호 %q — provider seed로 그대로 나와야 한다", seed)
	}
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

	got, _, err := resolveDatabaseURL()
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

	if _, _, err := resolveDatabaseURL(); err == nil {
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

	_, _, err := resolveDatabaseURL()
	if err == nil {
		t.Fatal("DB_PASSWORD만 설정되고 나머지가 비면 에러여야 한다")
	}
	if !strings.Contains(err.Error(), "DB_HOST") {
		t.Errorf("누락된 키(DB_HOST)를 에러에 명시해야 한다: %v", err)
	}
}

// APP_ENV 가드: 운영 모드에서 DB 미설정을 fail-fast하고, 미인식 값도 거부하며,
// 개발 기본(빈 값)은 인메모리 폴백을 유지하는지 검증한다.
func TestLoad_AppEnvGuard(t *testing.T) {
	// dbKeys는 각 케이스 시작 시 초기화할 DB 관련 env(프로세스 환경 오염 차단).
	dbKeys := []string{"DATABASE_URL", "DB_HOST", "DB_USER", "DB_PASSWORD", "DB_NAME", "DB_PORT", "DB_SSLMODE", "APP_ENV",
		"DB_SECRET_ARN", "AWS_REGION", "DB_CONN_MAX_LIFETIME"}

	cases := []struct {
		name    string
		env     map[string]string
		wantErr bool
		check   func(t *testing.T, cfg Config) // 정상 케이스의 세부 단언(선택)
	}{
		{
			name:    "production + DB 미설정 → 에러", // (a)
			env:     map[string]string{"APP_ENV": "production"},
			wantErr: true,
		},
		{
			name: "production + 완전한 DB_* + 회전 env → 정상(운영 실경로)", // (b)
			env: map[string]string{
				"APP_ENV": "production",
				"DB_HOST": "h", "DB_USER": "u", "DB_PASSWORD": "pw", "DB_NAME": "n",
				"DB_SECRET_ARN": testSecretARN, "AWS_REGION": "ap-northeast-2",
			},
			check: func(t *testing.T, cfg Config) {
				if cfg.AppEnv != "production" {
					t.Errorf("AppEnv = %q, want production", cfg.AppEnv)
				}
				if cfg.DatabaseURL == "" {
					t.Error("DB_*로 DatabaseURL이 조립되어야 한다")
				}
			},
		},
		{
			// DATABASE_URL 경로는 비밀번호를 DSN 안에만 두고 cfg.DBPassword를 비워 둔다.
			// 그 상태로 provider를 만들면 seed가 빈 문자열이 되어 2-3 (a)의 폴백이 깨진다.
			name: "production + DATABASE_URL + DB_SECRET_ARN → 에러", // (c)
			env: map[string]string{
				"APP_ENV": "production", "DATABASE_URL": "postgres://u:p@h:5432/db?sslmode=disable",
				"DB_SECRET_ARN": testSecretARN, "AWS_REGION": "ap-northeast-2",
			},
			wantErr: true,
		},
		{
			// 반대로 ARN이 없으면 정적 모드라 문제가 없다(로컬 docker-compose 경로).
			name: "DATABASE_URL 단독 → 정상(정적 모드)", // (c-1)
			env:  map[string]string{"APP_ENV": "", "DATABASE_URL": "postgres://u:p@h:5432/db?sslmode=disable"},
			check: func(t *testing.T, cfg Config) {
				if cfg.DBSecretARN != "" {
					t.Error("정적 모드에서는 ARN이 비어 있어야 한다")
				}
			},
		},
		{
			// 회전 env가 없으면 새 앱이 구 비밀번호로 정상 기동해 smoke까지 통과한다 —
			// Step 2가 배포되지 않았는데 배포된 것처럼 보이는 경로를 막는다(2-2).
			name: "production + DB 설정 + DB_SECRET_ARN 누락 → 에러", // (c-2)
			env: map[string]string{
				"APP_ENV": "production",
				"DB_HOST": "h", "DB_USER": "u", "DB_PASSWORD": "pw", "DB_NAME": "n",
			},
			wantErr: true,
		},
		{
			name: "빈 APP_ENV + DB 미설정 → 정상(인메모리)", // (d)
			env:  map[string]string{"APP_ENV": ""},
			check: func(t *testing.T, cfg Config) {
				if cfg.DatabaseURL != "" {
					t.Errorf("DB 미설정이면 DatabaseURL이 비어야 한다(인메모리): %q", cfg.DatabaseURL)
				}
			},
		},
		{
			name:    "미인식 값(prod) → 에러", // (e)
			env:     map[string]string{"APP_ENV": "prod"},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, k := range dbKeys {
				t.Setenv(k, "")
			}
			for k, v := range c.env {
				t.Setenv(k, v)
			}

			cfg, err := Load()
			if c.wantErr {
				if err == nil {
					t.Fatal("에러를 기대했으나 nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("정상을 기대했으나 에러: %v", err)
			}
			if c.check != nil {
				c.check(t, cfg)
			}
		})
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

// ---- 회전 대응 설정 (plan 0009 2-2) ----

// testSecretARN은 RDS 관리 시크릿 형식을 재현한 자리표시자다.
const testSecretARN = "arn:aws:secretsmanager:ap-northeast-2:123456789012:secret:rds!db-x-AbCdEf"

// production에서 DB_SECRET_ARN이 비면 기동을 막는다.
// 이 검사가 없으면 새 앱이 구 비밀번호로 정상 기동하고 smoke까지 통과해,
// Step 2가 배포되지 않았는데 배포된 것처럼 보인다.
func TestValidateRotationEnv(t *testing.T) {
	const arn = testSecretARN

	tests := []struct {
		name, appEnv, secretARN, region string
		wantErr                         string
	}{
		{name: "production 정상", appEnv: "production", secretARN: arn, region: "ap-northeast-2"},
		{name: "production ARN 누락", appEnv: "production", secretARN: "", region: "ap-northeast-2", wantErr: "DB_SECRET_ARN"},
		{name: "production 리전 누락", appEnv: "production", secretARN: arn, region: "", wantErr: "AWS_REGION"},
		{name: "production 리전 불일치", appEnv: "production", secretARN: arn, region: "us-east-1", wantErr: "다릅니다"},
		{name: "production ARN 형식 오류", appEnv: "production", secretARN: "not-an-arn", region: "ap-northeast-2", wantErr: "올바르지 않습니다"},
		// 로컬·개발은 정적 모드라 SDK를 쓰지 않는다 → 검사 생략.
		{name: "development 전부 비어도 통과", appEnv: "development"},
		{name: "빈 APP_ENV도 통과", appEnv: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRotationEnv(tt.appEnv, tt.secretARN, tt.region)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("통과해야 하는데 오류: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("오류를 기대했다(%s)", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("오류 메시지에 %q가 있어야 한다: %v", tt.wantErr, err)
			}
		})
	}
}

// DB_CONN_MAX_LIFETIME은 검증 편의용 손잡이라 잘못된 값에도 기동을 막지 않는다.
func TestResolveConnMaxLifetime(t *testing.T) {
	tests := []struct {
		name, value  string
		want         time.Duration
		wantRejected string // 기동 로그에 남아야 할 원문. 빈 값이면 정상.
	}{
		{name: "미설정 → 기본 5분", value: "", want: 5 * time.Minute},
		{name: "30s → 30s", value: "30s", want: 30 * time.Second},
		{name: "파싱 실패 → 기본값", value: "abc", want: 5 * time.Minute, wantRejected: "abc"},
		{name: "0 → 기본값", value: "0s", want: 5 * time.Minute, wantRejected: "0s"},
		{name: "음수 → 기본값", value: "-1s", want: 5 * time.Minute, wantRejected: "-1s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DB_CONN_MAX_LIFETIME", tt.value)
			got, rejected := resolveConnMaxLifetime()
			if got != tt.want {
				t.Errorf("%v — %v여야 한다", got, tt.want)
			}
			if rejected != tt.wantRejected {
				t.Errorf("거부 원문 %q — %q여야 한다", rejected, tt.wantRejected)
			}
		})
	}
}
