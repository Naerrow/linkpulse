package dbcreds

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// Secrets Manager 호출 예산 (plan 0009 2-0 "시도당 예산" 표).
//
//	시도당 3초 + backoff 2초 + 재시도 3초 = 최악 8초
//	refreshCtx operation 상한 10초 (마진 2초)
//
// 시도당 상한이 없으면 MaxAttempts=2가 아무 의미도 갖지 않는다 —
// 시도 1이 operation 상한을 다 쓰고 재시도가 한 번도 일어나지 않는 것도 계약을 만족해 버린다.
const (
	perAttemptTimeout = 3 * time.Second
	maxRetryBackoff   = 2 * time.Second
	maxAttempts       = 2
)

// awscurrent는 연결에 쓰는 유일한 스테이징 라벨이다. AWSPENDING은 쓰지 않는다 —
// 회전 중 AWSPENDING은 아직 DB에 반영되지 않았을 수 있다.
const awscurrent = "AWSCURRENT"

// RegionFromSecretARN은 시크릿 ARN을 검증하고 리전을 뽑는다.
//
// 이것이 SDK 리전의 source of truth다. ECS Fargate는 Lambda와 달리 AWS_REGION을 자동 주입하지
// 않고 SDK v2에는 기본 리전이 없다. ARN은 리전을 항상 포함하고 어차피 시크릿을 지목하는
// 값이라 리전이 어긋날 수 없다.
//
// 리전만 뽑고 끝내면 `arn:aws:secretsmanager:<region>:<account>:not-a-secret`처럼 **시크릿이
// 아닌 ARN이 기동 검사를 통과**한다. 그러면 결함이 다음 회전 때까지 드러나지 않는다 —
// 2-2의 fail-fast가 막으려던 바로 그 경로다. 그래서 서비스·리소스 종류까지 본다.
//
// 형식: arn:aws:secretsmanager:<region>:<account>:secret:<name>
func RegionFromSecretARN(raw string) (string, error) {
	parsed, err := arn.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("시크릿 ARN 형식이 아닙니다(%v): %q", err, raw)
	}
	if parsed.Service != "secretsmanager" {
		return "", fmt.Errorf("secretsmanager ARN이 아닙니다: %q", raw)
	}
	if parsed.Region == "" {
		return "", fmt.Errorf("시크릿 ARN에 리전이 없습니다: %q", raw)
	}
	if parsed.AccountID == "" {
		return "", fmt.Errorf("시크릿 ARN에 계정이 없습니다: %q", raw)
	}
	// 파티션(aws/aws-cn/aws-us-gov)은 검사하지 않는다 — 이 서비스는 상용 파티션에서만 돌지만
	// 그 제약을 코드로 박을 이유가 없고, 리전 교차검사가 같은 실수를 이미 잡는다.
	if !strings.HasPrefix(parsed.Resource, "secret:") {
		return "", fmt.Errorf("시크릿 리소스 ARN이 아닙니다: %q", raw)
	}
	return parsed.Region, nil
}

// NewSecretsManagerFetch는 AWSCURRENT 비밀번호를 읽는 FetchFunc를 만든다.
//
// 반환 함수는 refreshCtx(=detached, 상한 10초) 안에서 호출된다.
func NewSecretsManagerFetch(ctx context.Context, secretARN string) (FetchFunc, error) {
	// 시도마다 새 요청이라 http.Client.Timeout이 시도당 상한으로 걸린다.
	return newSecretsManagerFetch(ctx, secretARN, &http.Client{Timeout: perAttemptTimeout})
}

// newSecretsManagerFetch는 HTTP client를 주입받는다 — 테스트가 fake transport로
// "시도당 상한이 실제로 시도를 자르고 재시도가 일어나는지"를 고정하기 위해서다.
func newSecretsManagerFetch(ctx context.Context, secretARN string, httpClient *http.Client) (FetchFunc, error) {
	region, err := RegionFromSecretARN(secretARN)
	if err != nil {
		return nil, err
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithHTTPClient(httpClient),
		// 주의: retry.StandardOptions에 MaxBackoffDelay 필드는 없다. 그 이름은 아래 래퍼 함수다.
		// 팩토리는 호출마다 새 retryer를 반환해야 한다.
		awsconfig.WithRetryer(func() aws.Retryer {
			return retry.AddWithMaxBackoffDelay(
				retry.NewStandard(func(o *retry.StandardOptions) { o.MaxAttempts = maxAttempts }),
				maxRetryBackoff,
			)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("AWS 설정 로드 실패: %w", err)
	}

	client := secretsmanager.NewFromConfig(cfg)
	return func(ctx context.Context) (string, error) {
		out, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
			SecretId:     aws.String(secretARN),
			VersionStage: aws.String(awscurrent),
		})
		if err != nil {
			// 오류 메시지에 시크릿 값이 섞이지 않는다(SDK가 값을 오류에 넣지 않는다).
			return "", fmt.Errorf("GetSecretValue 실패: %w", err)
		}
		if out.SecretString == nil {
			return "", fmt.Errorf("SecretString이 비어 있습니다")
		}
		return passwordFromSecretString(*out.SecretString)
	}, nil
}

// passwordFromSecretString은 RDS 관리 시크릿 JSON에서 password 필드를 꺼낸다.
// 파싱 실패 메시지에 원문을 넣지 않는다 — 그 문자열이 비밀번호다.
func passwordFromSecretString(s string) (string, error) {
	var payload struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal([]byte(s), &payload); err != nil {
		return "", fmt.Errorf("SecretString JSON 파싱 실패: %w", redactJSONError(err))
	}
	if payload.Password == "" {
		return "", fmt.Errorf("SecretString에 password 필드가 없습니다")
	}
	return payload.Password, nil
}

// redactJSONError는 json 오류가 원문 일부를 담는 경우(SyntaxError는 오프셋만 담지만
// UnmarshalTypeError 등은 값을 담을 수 있다) 타입만 남긴다.
func redactJSONError(err error) error {
	return fmt.Errorf("%T", err)
}
