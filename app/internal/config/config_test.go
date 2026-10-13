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

	got, err := resolveDatabaseURL("")
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

	got, err := resolveDatabaseURL("")
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

	got, err := resolveDatabaseURL("")
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

	if _, err := resolveDatabaseURL(""); err == nil {
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

	_, err := resolveDatabaseURL("")
	if err == nil {
		t.Fatal("DB_PASSWORD만 설정되고 나머지가 비면 에러여야 한다")
	}
	if !strings.Contains(err.Error(), "DB_HOST") {
		t.Errorf("누락된 키(DB_HOST)를 에러에 명시해야 한다: %v", err)
	}
}

// 비밀번호 출처가 있으면 DSN에 비밀번호를 넣지 않고, DB_PASSWORD 없이도 조립된다(plan 0014).
// 출처와 DB_PASSWORD를 함께 두면 무엇이 쓰이는지 모호하므로 막는다.
func TestResolveDatabaseURL_WithPasswordSource(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DB_HOST", "db.example.com")
	t.Setenv("DB_USER", "linkpulse")
	t.Setenv("DB_PASSWORD", "")
	t.Setenv("DB_NAME", "linkpulse")
	t.Setenv("DB_PORT", "")
	t.Setenv("DB_SSLMODE", "")

	got, err := resolveDatabaseURL(PasswordSourceAWS)
	if err != nil {
		t.Fatalf("출처가 있으면 DB_PASSWORD 없이 조립돼야 한다: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("조립된 DSN 파싱 실패: %v", err)
	}
	if _, has := u.User.Password(); has {
		t.Error("출처가 있으면 DSN에 비밀번호가 없어야 한다")
	}
	if u.User.Username() != "linkpulse" {
		t.Errorf("사용자 %q", u.User.Username())
	}

	t.Setenv("DB_PASSWORD", "pw")
	if _, err := resolveDatabaseURL(PasswordSourceFile); err == nil || !strings.Contains(err.Error(), "DB_PASSWORD") {
		t.Errorf("출처와 DB_PASSWORD를 함께 두면 오류여야 한다: %v", err)
	}

	// 출처만 있고 접속 키가 없으면 인메모리로 떨어지지 않고 누락을 알린다.
	t.Setenv("DB_PASSWORD", "")
	t.Setenv("DB_HOST", "")
	t.Setenv("DB_USER", "")
	t.Setenv("DB_NAME", "")
	if _, err := resolveDatabaseURL(PasswordSourceAWS); err == nil || !strings.Contains(err.Error(), "DB_HOST") {
		t.Errorf("출처만 있으면 누락 키 오류여야 한다: %v", err)
	}
}

// APP_ENV 가드: 운영 모드에서 DB 미설정을 fail-fast하고, 미인식 값도 거부하며,
// 개발 기본(빈 값)은 인메모리 폴백을 유지하는지 검증한다.
func TestLoad_AppEnvGuard(t *testing.T) {
	// dbKeys는 각 케이스 시작 시 초기화할 DB 관련 env(프로세스 환경 오염 차단).
	dbKeys := []string{"DATABASE_URL", "DB_HOST", "DB_USER", "DB_PASSWORD", "DB_NAME", "DB_PORT", "DB_SSLMODE", "APP_ENV",
		"DB_SECRET_ARN", "AWS_REGION", "DB_CONN_MAX_LIFETIME", "CLICK_QUEUE_URL",
		"DB_PASSWORD_SOURCE", "DB_PASSWORD_FILE", "DB_MAX_OPEN_CONNS"}

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
			name: "production + DB_* + 출처 aws + 클릭 큐 → 정상(EKS 실경로)", // (b)
			env: map[string]string{
				"APP_ENV": "production",
				"DB_HOST": "h", "DB_USER": "u", "DB_NAME": "n",
				"DB_PASSWORD_SOURCE": "aws", "DB_SECRET_ARN": testSecretARN, "AWS_REGION": "ap-northeast-2",
				"CLICK_QUEUE_URL": testClickQueueURL, "DB_MAX_OPEN_CONNS": "11",
			},
			check: func(t *testing.T, cfg Config) {
				if cfg.AppEnv != "production" {
					t.Errorf("AppEnv = %q, want production", cfg.AppEnv)
				}
				if cfg.DatabaseURL == "" {
					t.Error("DB_*로 DatabaseURL이 조립되어야 한다")
				}
				if cfg.ClickQueueURL != testClickQueueURL {
					t.Errorf("ClickQueueURL = %q", cfg.ClickQueueURL)
				}
				if cfg.DBPasswordSource != PasswordSourceAWS || cfg.DBSecretARN != testSecretARN {
					t.Errorf("출처 %q, ARN %q", cfg.DBPasswordSource, cfg.DBSecretARN)
				}
				if cfg.DBMaxOpenConns != 11 {
					t.Errorf("DBMaxOpenConns = %d, want 11", cfg.DBMaxOpenConns)
				}
			},
		},
		{
			name: "production + DB_* + 출처 file → 정상(P6 k3s 경로)", // (b-2)
			env: map[string]string{
				"APP_ENV": "production",
				"DB_HOST": "h", "DB_USER": "u", "DB_NAME": "n",
				"DB_PASSWORD_SOURCE": "file", "DB_PASSWORD_FILE": "/var/run/secrets/linkpulse/db-password",
				"CLICK_QUEUE_URL": testClickQueueURL, "AWS_REGION": "ap-northeast-2",
			},
			check: func(t *testing.T, cfg Config) {
				if cfg.DBPasswordSource != PasswordSourceFile || cfg.DBPasswordFile == "" {
					t.Errorf("출처 %q, 파일 %q", cfg.DBPasswordSource, cfg.DBPasswordFile)
				}
			},
		},
		{
			// 큐 설정이 빠진 배포로 새 이미지가 뜨면 메모리 큐로 조용히 돌지 않고 기동에서 멈춘다(plan 0013).
			name: "production + 클릭 큐 누락 → 에러", // (b-1)
			env: map[string]string{
				"APP_ENV": "production",
				"DB_HOST": "h", "DB_USER": "u", "DB_NAME": "n",
				"DB_PASSWORD_SOURCE": "aws", "DB_SECRET_ARN": testSecretARN, "AWS_REGION": "ap-northeast-2",
			},
			wantErr: true,
		},
		{
			// DATABASE_URL 경로는 정적 모드 전용이다. 출처와 함께 두면 무엇이 쓰이는지 모호하다.
			// 개발 모드로 둬 다른 운영 검사(클릭 큐 등)가 대신 실패하지 않게 한다 — 이 조합만이 실패 이유다.
			name: "DATABASE_URL + 출처 → 에러", // (c)
			env: map[string]string{
				"APP_ENV": "", "DATABASE_URL": "postgres://u:p@h:5432/db?sslmode=disable",
				"DB_PASSWORD_SOURCE": "aws", "DB_SECRET_ARN": testSecretARN,
			},
			wantErr: true,
		},
		{
			// 출처 없이 ARN만 있으면(옛 ECS 조합) 막는다 — 회전 대응이 켜졌다고 착각하게 된다.
			name: "DATABASE_URL + DB_SECRET_ARN(출처 없음) → 에러", // (c-3)
			env: map[string]string{
				"APP_ENV": "", "DATABASE_URL": "postgres://u:p@h:5432/db?sslmode=disable",
				"DB_SECRET_ARN": testSecretARN,
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
			// 출처가 없으면(정적 DB_PASSWORD) 새 앱이 지금 비밀번호로 정상 기동해 smoke까지 통과한다 —
			// 회전 대응이 꺼진 채 배포된 것처럼 보이는 경로를 막는다(2-2, plan 0014).
			name: "production + 정적 DB_PASSWORD(출처 없음) → 에러", // (c-2)
			env: map[string]string{
				"APP_ENV": "production",
				"DB_HOST": "h", "DB_USER": "u", "DB_PASSWORD": "pw", "DB_NAME": "n",
				"CLICK_QUEUE_URL": testClickQueueURL, "AWS_REGION": "ap-northeast-2",
			},
			wantErr: true,
		},
		{
			name: "DB_MAX_OPEN_CONNS 0 → 에러", // (f)
			env: map[string]string{
				"DB_HOST": "h", "DB_USER": "u", "DB_PASSWORD": "pw", "DB_NAME": "n", "DB_MAX_OPEN_CONNS": "0",
			},
			wantErr: true,
		},
		{
			name: "DB_MAX_OPEN_CONNS 정수 아님 → 에러", // (f-1)
			env: map[string]string{
				"DB_HOST": "h", "DB_USER": "u", "DB_PASSWORD": "pw", "DB_NAME": "n", "DB_MAX_OPEN_CONNS": "eleven",
			},
			wantErr: true,
		},
		{
			// 미설정이면 지금 동작(25)이 그대로다 — 로컬·기존 배포 불변.
			name: "DB_MAX_OPEN_CONNS 미설정 → 기본 25", // (f-2)
			env: map[string]string{
				"DB_HOST": "h", "DB_USER": "u", "DB_PASSWORD": "pw", "DB_NAME": "n",
			},
			check: func(t *testing.T, cfg Config) {
				if cfg.DBMaxOpenConns != 25 {
					t.Errorf("DBMaxOpenConns = %d, want 25", cfg.DBMaxOpenConns)
				}
			},
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

// 비밀번호 출처 계약(plan 0014). production은 출처를 요구하고, 출처와 맞지 않는 키는 막는다.
// 출처가 없으면 새 앱이 지금 비밀번호로 정상 기동하고 smoke까지 통과해,
// 회전 대응이 꺼진 채 배포된 것처럼 보인다.
func TestValidatePasswordSource(t *testing.T) {
	const arn = testSecretARN
	const file = "/var/run/secrets/linkpulse/db-password"

	tests := []struct {
		name, appEnv, source, secretARN, file, region string
		wantErr                                       string
	}{
		{name: "production aws 정상", appEnv: "production", source: "aws", secretARN: arn, region: "ap-northeast-2"},
		{name: "production aws ARN 누락", appEnv: "production", source: "aws", region: "ap-northeast-2", wantErr: "DB_SECRET_ARN"},
		{name: "production aws 리전 누락", appEnv: "production", source: "aws", secretARN: arn, wantErr: "AWS_REGION"},
		{name: "production aws 리전 불일치", appEnv: "production", source: "aws", secretARN: arn, region: "us-east-1", wantErr: "다릅니다"},
		{name: "production aws ARN 형식 오류", appEnv: "production", source: "aws", secretARN: "not-an-arn", region: "ap-northeast-2", wantErr: "올바르지 않습니다"},
		{name: "aws에 파일 경로 섞임", appEnv: "production", source: "aws", secretARN: arn, file: file, region: "ap-northeast-2", wantErr: "DB_PASSWORD_FILE"},
		// 개발에서도 aws면 ARN은 필요하다(SDK 리전이 ARN에서 온다). 리전 교차검사만 생략한다.
		{name: "development aws ARN 누락", appEnv: "development", source: "aws", wantErr: "DB_SECRET_ARN"},
		{name: "development aws 리전 없이 통과", appEnv: "development", source: "aws", secretARN: arn},
		{name: "production file 정상", appEnv: "production", source: "file", file: file},
		{name: "file 경로 누락", appEnv: "production", source: "file", wantErr: "DB_PASSWORD_FILE"},
		{name: "file에 ARN 섞임", appEnv: "production", source: "file", file: file, secretARN: arn, wantErr: "DB_SECRET_ARN"},
		{name: "production 출처 없음", appEnv: "production", wantErr: "DB_PASSWORD_SOURCE"},
		{name: "출처 없이 ARN만", appEnv: "", secretARN: arn, wantErr: "DB_PASSWORD_SOURCE"},
		{name: "출처 없이 파일만", appEnv: "", file: file, wantErr: "DB_PASSWORD_SOURCE"},
		{name: "모르는 출처", appEnv: "", source: "vault", wantErr: "올바르지 않습니다"},
		// 로컬·개발은 정적 모드라 SDK를 쓰지 않는다 → 검사 생략.
		{name: "development 전부 비어도 통과", appEnv: "development"},
		{name: "빈 APP_ENV도 통과", appEnv: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePasswordSource(tt.appEnv, tt.source, tt.secretARN, tt.file, tt.region)
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

// TestParseAdminTokenHash는 관리자 해시 형식 검사를 본다(plan 0011).
// 빈 값은 "꺼짐"이고, 값이 있는데 틀리면 기동을 막는다 — 토큰 원문을 잘못 넣는 실수가 가장 흔하다.
func TestParseAdminTokenHash(t *testing.T) {
	const valid = "6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b" // sha256("1")

	if b, err := parseAdminTokenHash("  "); err != nil || b != nil {
		t.Errorf("빈 값 = %v, %v, want nil, nil", b, err)
	}
	for _, in := range []string{valid, strings.ToUpper(valid), " " + valid + "\n"} {
		if b, err := parseAdminTokenHash(in); err != nil || len(b) != 32 {
			t.Errorf("parseAdminTokenHash(%q) = %d바이트, %v", in, len(b), err)
		}
	}
	for _, in := range []string{"my-secret-token", valid[:63], valid + "00", "zz" + valid[2:]} {
		if _, err := parseAdminTokenHash(in); err == nil {
			t.Errorf("parseAdminTokenHash(%q)가 통과됨", in)
		}
	}
}

// TestParseExemptIPs는 레이트리밋 예외 IP 해석을 본다(plan 0012).
// 빈 값은 "예외 없음"이고, 결과는 clientIP와 같은 정규화 형태여야 정확히 일치 비교가 된다.
func TestParseExemptIPs(t *testing.T) {
	for _, in := range []string{"", "  "} {
		if ips, err := parseExemptIPs(in); err != nil || ips != nil {
			t.Errorf("parseExemptIPs(%q) = %v, %v, want nil, nil", in, ips, err)
		}
	}
	ips, err := parseExemptIPs(" 198.51.100.7 ,2001:DB8::1")
	if err != nil {
		t.Fatalf("예상치 못한 에러: %v", err)
	}
	if want := []string{"198.51.100.7", "2001:db8::1"}; strings.Join(ips, "|") != strings.Join(want, "|") {
		t.Errorf("정규화 결과 %v, want %v", ips, want)
	}
	for _, in := range []string{"not-an-ip", "198.51.100.7,", "198.51.100.7/32", "198.51.100.7:8080"} {
		if _, err := parseExemptIPs(in); err == nil {
			t.Errorf("parseExemptIPs(%q)가 통과됨", in)
		}
	}
}

// TestLoadRejectsMalformedExemptIPs는 잘못된 예외 IP로 기동이 막히고, 오류 문구에 원문이 없는지 본다.
func TestLoadRejectsMalformedExemptIPs(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("APP_ENV", "")
	t.Setenv("ADMIN_TOKEN_SHA256", "")
	t.Setenv("RATE_LIMIT_EXEMPT_IPS", "198.51.100.7,secret-typo")
	_, err := Load()
	if err == nil {
		t.Fatal("잘못된 RATE_LIMIT_EXEMPT_IPS인데 Load가 성공함")
	}
	if strings.Contains(err.Error(), "198.51.100.7") || strings.Contains(err.Error(), "secret-typo") {
		t.Errorf("오류 문구에 원문이 들어갔다: %v", err)
	}
}

// TestLoadRejectsMalformedAdminHash는 형식이 틀린 해시로 기동이 막히는지 본다.
func TestLoadRejectsMalformedAdminHash(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("APP_ENV", "")
	t.Setenv("ADMIN_TOKEN_SHA256", "not-a-hash")
	if _, err := Load(); err == nil {
		t.Fatal("잘못된 ADMIN_TOKEN_SHA256인데 Load가 성공함")
	}
}

// testClickQueueURL은 테스트용 SQS 큐 URL이다(계정 ID는 0으로 채운 가짜).
const testClickQueueURL = "https://sqs.ap-northeast-2.amazonaws.com/000000000000/linkpulse-prod-clicks"

// TestValidateClickQueueURL은 클릭 큐 설정 규칙을 본다(plan 0013).
// 운영에서 비면 막고(메모리 큐 폴백 금지), 값이 있으면 https URL과 리전이 있어야 한다.
func TestValidateClickQueueURL(t *testing.T) {
	const good = testClickQueueURL
	cases := []struct {
		name, env, raw, region string
		wantErr                bool
	}{
		{"로컬 빈 값은 메모리 큐", "", "", "", false},
		{"운영 빈 값은 오류", envProduction, "", "ap-northeast-2", true},
		{"운영 정상", envProduction, good, "ap-northeast-2", false},
		{"http는 오류", "", "http://sqs.local/q", "ap-northeast-2", true},
		{"URL 아님", "", "linkpulse-prod-clicks", "ap-northeast-2", true},
		{"리전 없음", "", good, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateClickQueueURL(c.env, c.raw, c.region); (err != nil) != c.wantErr {
				t.Errorf("err = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}
