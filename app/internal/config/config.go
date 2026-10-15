// config 패키지는 환경변수에서 애플리케이션 설정을 읽어들인다.
// 비밀값을 포함한 모든 설정은 코드가 아니라 환경변수로만 주입한다(가드레일 #2).
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Naerrow/linkpulse/app/internal/db"
	"github.com/Naerrow/linkpulse/app/internal/dbcreds"
)

const (
	defaultShortCodeLength = 7
	// 코드가 짧을수록 열거가 쉬워진다(보안). 기본 7을 권장하며,
	// 아래는 명백히 잘못된 설정을 막는 하한/상한이다.
	minShortCodeLength = 4
	maxShortCodeLength = 32
)

// defaultConnMaxLifetime은 db 패키지의 값을 그대로 쓴다. 풀 설정의 주인이 그쪽이고,
// 두 곳에 상수를 두면 한쪽만 바뀌었을 때 기동 로그가 서로 다른 실효값을 말한다.
const defaultConnMaxLifetime = db.DefaultConnMaxLifetime

// Config는 실행에 필요한 설정값 모음이다.
type Config struct {
	Port            string // 리슨 포트 (APP_PORT, 기본 8080)
	MetricsAddr     string // 내부 지표 포트 주소 (METRICS_ADDR, 기본 :9090, plan 0015). /metrics·pprof만 둔다
	LogLevel        string // 로그 레벨 (LOG_LEVEL): debug|info|warn|error
	PublicBaseURL   string // 단축 URL에 붙일 외부 공개 주소 (PUBLIC_BASE_URL)
	ShortCodeLength int    // 단축 코드 길이 (SHORT_CODE_LENGTH, 기본 7)
	// 실행 환경 (APP_ENV): ""(기본=development)|development|production.
	// production이면 DB 미설정 시 인메모리 폴백을 금지하고 기동을 중단한다(Load 참고).
	AppEnv string
	// Postgres 접속 문자열. DATABASE_URL이 있으면 그 값을, 없으면 DB_* 개별 변수로
	// 조립한 값을 담는다. 비어 있으면 인메모리 저장소를 사용한다. (resolveDatabaseURL 참고)
	DatabaseURL string
	// DB 비밀번호 출처 (DB_PASSWORD_SOURCE, plan 0014): ""(정적, DB_PASSWORD)|aws|file.
	// aws·file이면 앱이 기동 때 직접 읽고 인증 실패 때 다시 읽는다(회전 대응). 이때 DSN에는 비밀번호가 없다.
	DBPasswordSource string
	// 회전 대응에 쓸 시크릿 ARN (DB_SECRET_ARN). DB_PASSWORD_SOURCE=aws에서만 쓴다.
	// 비밀이 아니라 식별자다.
	DBSecretARN string
	// 비밀번호 파일 경로 (DB_PASSWORD_FILE). DB_PASSWORD_SOURCE=file에서만 쓴다(P6 k3s의 Secret 마운트).
	DBPasswordFile string
	// 연결 풀 최대 연결 수 (DB_MAX_OPEN_CONNS, 기본 25). 파드가 HPA로 늘면 파드 수 × 이 값이
	// RDS 연결 한도를 넘지 않게 차트가 낮춘다(plan 0014).
	DBMaxOpenConns int
	// 배포 시점 교차검사용 리전 (AWS_REGION). SDK 리전의 source of truth가 아니다 —
	// 그것은 DBSecretARN 파싱값이다.
	AWSRegion string
	// 커넥션 최대 수명 (DB_CONN_MAX_LIFETIME). 2-6 검증 기간에만 짧게 넣어
	// 신규 연결을 강제하고, 평시에는 미설정(기본 5분)이다.
	ConnMaxLifetime time.Duration
	// 관리자 토큰의 SHA-256 해시 (ADMIN_TOKEN_SHA256, 16진수 64자). 비어 있으면 nil이고
	// 관리자 기능(링크 생성·요청 승인)이 꺼진다(plan 0011). 토큰 원문은 서버에 두지 않는다.
	AdminTokenSHA256 []byte
	// 레이트리밋을 건너뛰는 클라이언트 IP 목록 (RATE_LIMIT_EXEMPT_IPS, 쉼표 구분). 정규화된 형태로 담는다.
	// 한계 측정(plan 0012) 동안 측정 클라이언트 한 대만 풀어 주려고 둔다. 평시에는 비어 있다(nil).
	RateLimitExemptIPs []string
	// 클릭 큐(SQS) URL (CLICK_QUEUE_URL, plan 0013). 비면 프로세스 안의 메모리 큐를 쓴다(로컬).
	// 운영에서는 필수다 — 태스크마다 따로 노는 메모리 큐로 조용히 뜨면 재전달도 없고 종료 때 클릭을 잃는다.
	ClickQueueURL string
	// 요청 로그 표본 N (ACCESS_LOG_SAMPLE, 기본 1 = 전부, plan 0016). 성공했고 빠른 요청만 1/N로 남긴다.
	// 차트 기본값은 100이고 사이트 ConfigMap이 덮어쓴다. 로컬·docker-compose는 기본 1이라 예전과 같다.
	AccessLogSample int
	// 잘못돼서 무시한 DB_CONN_MAX_LIFETIME 원문. 비어 있으면 정상이다.
	// 값이 있으면 기동 로그가 그 사실을 함께 남긴다 — 이 경고를 Load 안에서 바로 찍으면
	// 구조화 로거 설정 전이라 JSON도 task_id도 붙지 않는다.
	ConnMaxLifetimeRejected string
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
	// 출처가 있으면 비밀번호는 DSN에 넣지 않는다 — 매 연결마다 provider가 준다.
	passwordSource := os.Getenv("DB_PASSWORD_SOURCE")
	dsn, err := resolveDatabaseURL(passwordSource)
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

	// 회전 대응 설정. 같은 자리·같은 형태의 fail-fast다.
	secretARN := os.Getenv("DB_SECRET_ARN")
	passwordFile := os.Getenv("DB_PASSWORD_FILE")
	awsRegion := os.Getenv("AWS_REGION")
	// DATABASE_URL 경로는 정적 모드 전용이다(로컬 docker-compose). 출처와 함께 두면 DSN 안의
	// 비밀번호와 provider가 읽는 비밀번호 중 무엇이 쓰이는지 설정만 보고 알 수 없다. 조합 자체를 막는다.
	if os.Getenv("DATABASE_URL") != "" && passwordSource != "" {
		return Config{}, errors.New("DATABASE_URL과 DB_PASSWORD_SOURCE는 함께 쓸 수 없습니다 — 회전 대응은 DB_* 경로를 씁니다")
	}
	if err := validatePasswordSource(appEnv, passwordSource, secretARN, passwordFile, awsRegion); err != nil {
		return Config{}, err
	}

	maxOpenConns, err := getEnvInt("DB_MAX_OPEN_CONNS", db.DefaultMaxOpenConns)
	if err != nil {
		return Config{}, err
	}
	if maxOpenConns < 1 {
		return Config{}, fmt.Errorf("DB_MAX_OPEN_CONNS는 1 이상이어야 합니다(현재 %d)", maxOpenConns)
	}

	connMaxLifetime, rejectedLifetime := resolveConnMaxLifetime()

	adminHash, err := parseAdminTokenHash(os.Getenv("ADMIN_TOKEN_SHA256"))
	if err != nil {
		return Config{}, err
	}

	exemptIPs, err := parseExemptIPs(os.Getenv("RATE_LIMIT_EXEMPT_IPS"))
	if err != nil {
		return Config{}, err
	}

	clickQueueURL := os.Getenv("CLICK_QUEUE_URL")
	if err := validateClickQueueURL(appEnv, clickQueueURL, awsRegion); err != nil {
		return Config{}, err
	}

	// 0·음수는 "로그를 끈다"로 읽힐 수 있어 막는다 — 오류·느린 요청 줄까지 사라진 줄 알고 장애 때 헤매지 않게.
	accessLogSample, err := getEnvInt("ACCESS_LOG_SAMPLE", 1)
	if err != nil {
		return Config{}, err
	}
	if accessLogSample < 1 {
		return Config{}, fmt.Errorf("ACCESS_LOG_SAMPLE은 1 이상이어야 합니다(1 = 전부 남김, 현재 %d)", accessLogSample)
	}

	return Config{
		Port:            getEnv("APP_PORT", "8080"),
		MetricsAddr:     getEnv("METRICS_ADDR", ":9090"),
		LogLevel:        getEnv("LOG_LEVEL", "info"),
		PublicBaseURL:   baseURL,
		ShortCodeLength: codeLen,
		AppEnv:          appEnv,
		// 비어 있으면 main에서 인메모리 저장소로 폴백한다(로컬 개발 편의).
		DatabaseURL:             dsn,
		DBPasswordSource:        passwordSource,
		DBSecretARN:             secretARN,
		DBPasswordFile:          passwordFile,
		DBMaxOpenConns:          maxOpenConns,
		AWSRegion:               awsRegion,
		ConnMaxLifetime:         connMaxLifetime,
		ConnMaxLifetimeRejected: rejectedLifetime,
		AdminTokenSHA256:        adminHash,
		RateLimitExemptIPs:      exemptIPs,
		ClickQueueURL:           clickQueueURL,
		AccessLogSample:         accessLogSample,
	}, nil
}

// validateClickQueueURL은 클릭 큐 설정을 확인한다(plan 0013).
// production에서 비면 기동을 막는다(DB 미설정과 같은 fail-fast) — 서킷브레이커가 이전 리비전으로 되돌린다.
// 값이 있으면 https URL이어야 하고, SDK 리전으로 쓸 AWS_REGION이 있어야 한다.
func validateClickQueueURL(appEnv, raw, awsRegion string) error {
	if raw == "" {
		if appEnv == envProduction {
			return errors.New("APP_ENV=production인데 CLICK_QUEUE_URL이 미설정입니다 — 메모리 큐 폴백 금지")
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("CLICK_QUEUE_URL은 SQS 큐의 https URL이어야 합니다")
	}
	if awsRegion == "" {
		return errors.New("CLICK_QUEUE_URL을 쓰려면 AWS_REGION이 필요합니다(SQS 클라이언트 리전)")
	}
	return nil
}

// parseExemptIPs는 RATE_LIMIT_EXEMPT_IPS를 IP 목록으로 해석한다.
// 레이트리밋의 clientIP와 같은 정규화(net.ParseIP → String)를 거쳐야 정확히 일치 비교가 된다.
// 값이 있는데 IP가 아닌 항목이 있으면 기동을 막는다 — 오타가 조용히 "예외 없음"이 되면
// 측정이 429로 막힌 이유를 찾기 어렵다. 오류 문구에 원문을 넣지 않는다(측정 클라이언트의 집 IP다).
func parseExemptIPs(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	ips := make([]string, 0, len(parts))
	for i, p := range parts {
		ip := net.ParseIP(strings.TrimSpace(p))
		if ip == nil {
			return nil, fmt.Errorf("RATE_LIMIT_EXEMPT_IPS의 %d번째 항목이 IP가 아닙니다", i+1)
		}
		ips = append(ips, ip.String())
	}
	return ips, nil
}

// parseAdminTokenHash는 ADMIN_TOKEN_SHA256을 32바이트로 해석한다.
// 빈 값은 "관리자 기능 끔"으로 받아들이지만, 값이 있는데 형식이 틀리면 기동을 막는다 —
// 오타가 조용히 "관리자 기능 꺼짐"이 되면 왜 로그인이 안 되는지 찾기 어렵기 때문이다.
func parseAdminTokenHash(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	b, err := hex.DecodeString(raw)
	if err != nil || len(b) != sha256.Size {
		return nil, errors.New("ADMIN_TOKEN_SHA256은 SHA-256 해시(16진수 64자)여야 합니다 — 토큰 원문을 넣지 않았는지 확인하세요")
	}
	return b, nil
}

// DB 비밀번호 출처 (DB_PASSWORD_SOURCE, plan 0014). 빈 값은 정적 모드(DB_PASSWORD)다.
const (
	PasswordSourceAWS  = "aws"  // Secrets Manager(DB_SECRET_ARN). EKS 기본값.
	PasswordSourceFile = "file" // 파일(DB_PASSWORD_FILE). P6 k3s에서 쿠버네티스 Secret을 마운트한다.
)

// validatePasswordSource는 비밀번호 출처 설정의 계약을 강제한다.
//
// 왜 production에서 출처를 요구하는가: 정적 모드로 뜨면 새 앱이 **지금 비밀번호로 정상 기동**하고
// /readyz 200·왕복 302 smoke까지 통과한다 — 회전 대응(Step 2)이 꺼진 채 배포된 것처럼 보이고,
// 다음 회전 때까지 드러나지 않는다.
//
// 출처와 맞지 않는 키(file인데 DB_SECRET_ARN 등)도 막는다. 둘 중 무엇이 쓰이는지 설정만 보고 알 수 없게 되기 때문이다.
//
// AWS_REGION은 SDK를 동작시키는 값이 아니다(SDK 리전은 ARN 파싱에서 온다). 목적은
// "배포 설정이 의도한 리전으로 만들어졌는지"를 기동 시점에 거르는 이중 방어이고,
// 그 목적을 살리려면 production에서 빈 값은 "검사 생략"이 아니라 오류여야 한다.
func validatePasswordSource(appEnv, source, secretARN, passwordFile, awsRegion string) error {
	switch source {
	case "":
		if appEnv == envProduction {
			return errors.New("APP_ENV=production인데 DB_PASSWORD_SOURCE가 미설정입니다(aws|file) — 회전 대응 없이 기동 금지")
		}
		if secretARN != "" || passwordFile != "" {
			return errors.New("DB_SECRET_ARN·DB_PASSWORD_FILE은 DB_PASSWORD_SOURCE와 함께 씁니다")
		}
		return nil
	case PasswordSourceAWS:
		if passwordFile != "" {
			return errors.New("DB_PASSWORD_SOURCE=aws에서는 DB_PASSWORD_FILE을 쓰지 않습니다")
		}
		if secretARN == "" {
			return errors.New("DB_PASSWORD_SOURCE=aws인데 DB_SECRET_ARN이 미설정입니다")
		}
		arnRegion, err := dbcreds.RegionFromSecretARN(secretARN)
		if err != nil {
			return fmt.Errorf("DB_SECRET_ARN이 올바르지 않습니다: %w", err)
		}
		if appEnv != envProduction {
			return nil
		}
		if awsRegion == "" {
			return errors.New("APP_ENV=production인데 AWS_REGION이 미설정입니다 — 배포 시점 교차검사 불가")
		}
		if arnRegion != awsRegion {
			return fmt.Errorf("DB_SECRET_ARN의 리전(%s)과 AWS_REGION(%s)이 다릅니다", arnRegion, awsRegion)
		}
		return nil
	case PasswordSourceFile:
		if secretARN != "" {
			return errors.New("DB_PASSWORD_SOURCE=file에서는 DB_SECRET_ARN을 쓰지 않습니다")
		}
		if passwordFile == "" {
			return errors.New("DB_PASSWORD_SOURCE=file인데 DB_PASSWORD_FILE이 미설정입니다")
		}
		return nil
	default:
		return fmt.Errorf("DB_PASSWORD_SOURCE가 올바르지 않습니다(허용: %s|%s, 빈 값=정적): %q",
			PasswordSourceAWS, PasswordSourceFile, source)
	}
}

// resolveConnMaxLifetime은 DB_CONN_MAX_LIFETIME을 읽는다.
//
// 이 값은 안전이 아니라 검증 편의(2-6이 T_fail을 설계로 만드는 손잡이)라, 잘못된 값에도
// 기동을 막지 않고 기본값으로 떨어뜨린다. 실효값은 기동 로그로 확인한다 —
// 폴백이 있으므로 task definition 문자열만으로는 실효값을 증명하지 못한다.
//
// 거부한 원문은 **여기서 찍지 않고 돌려준다.** Load는 구조화 로거 설정보다 먼저 돌기 때문에
// 여기서 경고하면 그 줄만 text 핸들러로 stderr에 나가 JSON도 task_id도 붙지 않는다 —
// 하필 이 줄이 "지정한 값이 무시됐다"는 유일한 신호다.
func resolveConnMaxLifetime() (d time.Duration, rejected string) {
	raw := os.Getenv("DB_CONN_MAX_LIFETIME")
	if raw == "" {
		return defaultConnMaxLifetime, ""
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return defaultConnMaxLifetime, raw
	}
	return parsed, ""
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
//  2. 아니면 DB_HOST/DB_USER/DB_PASSWORD/DB_NAME으로 URL을 조립한다. 비밀번호를 코드가 아니라
//     env로만 받기 위함이다(가드레일 #2).
//     비밀번호 출처(source: aws|file)가 있으면 DB_PASSWORD 대신 출처가 비밀번호를 준다 — DSN에는
//     비밀번호를 넣지 않고, DB_PASSWORD가 함께 있으면 무엇이 쓰이는지 모호하므로 막는다.
//  3. 핵심 키와 출처가 모두 비어 있으면 빈 문자열을 돌려준다 → 호출부가 인메모리로 폴백한다.
//
// 핵심 키(DB_HOST/DB_USER/DB_NAME, 정적 모드면 DB_PASSWORD까지) 중 하나라도 설정되면 전부를 필수
// 검사하고, 누락된 키를 명시해 에러를 반환한다 — 운영에서 일부만 주입돼 조용히 인메모리로
// 떠 데이터가 증발하는 사고를 막는다(fail-fast). DB_PORT/DB_SSLMODE는 기본값(5432/require)이
// 있는 보조값이라 이 트리거에서 제외한다.
func resolveDatabaseURL(source string) (string, error) {
	if raw := os.Getenv("DATABASE_URL"); raw != "" {
		// 이 경로는 로컬 docker-compose 전용이라 회전 provider를 쓰지 않는다(정적 모드).
		return raw, nil
	}

	host := os.Getenv("DB_HOST")
	user := os.Getenv("DB_USER")
	password := os.Getenv("DB_PASSWORD")
	name := os.Getenv("DB_NAME")

	// 전부 비면 DB 미설정으로 보고 인메모리로 폴백한다(로컬 개발 편의).
	if host == "" && user == "" && password == "" && name == "" && source == "" {
		return "", nil
	}
	if source != "" && password != "" {
		return "", errors.New("DB_PASSWORD_SOURCE를 쓰면 DB_PASSWORD를 비워야 합니다 — 비밀번호는 출처에서 읽습니다")
	}

	// 하나라도 설정됐으면 전부 필수 — 어떤 키가 빠졌는지 알려 준다(fail-fast).
	var missing []string
	if host == "" {
		missing = append(missing, "DB_HOST")
	}
	if user == "" {
		missing = append(missing, "DB_USER")
	}
	if password == "" && source == "" {
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
	userInfo := url.UserPassword(user, password)
	if source != "" {
		userInfo = url.User(user)
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     userInfo,
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
