package dbcreds

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegionFromSecretARN(t *testing.T) {
	tests := []struct {
		name, arn, want string
		wantErr         bool
	}{
		{
			name: "RDS 관리 시크릿",
			arn:  "arn:aws:secretsmanager:ap-northeast-2:644076162314:secret:rds!db-af3ffe20-E9J0rG",
			want: "ap-northeast-2",
		},
		{name: "리전 비어 있음", arn: "arn:aws:secretsmanager::123:secret:x", wantErr: true},
		{name: "다른 서비스", arn: "arn:aws:ssm:ap-northeast-2:123:parameter/x", wantErr: true},
		{name: "ARN 아님", arn: "just-a-name", wantErr: true},
		{name: "빈 값", arn: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RegionFromSecretARN(tt.arn)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("오류를 기대했는데 %q를 받았다", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("예상치 못한 오류: %v", err)
			}
			if got != tt.want {
				t.Errorf("리전 %q — %q여야 한다", got, tt.want)
			}
		})
	}
}

func TestPasswordFromSecretString(t *testing.T) {
	got, err := passwordFromSecretString(`{"username":"linkpulse","password":"p@ss/word+1"}`)
	if err != nil {
		t.Fatalf("파싱 실패: %v", err)
	}
	if got != "p@ss/word+1" {
		t.Errorf("비밀번호 %q — 특수문자가 보존돼야 한다", got)
	}

	if _, err := passwordFromSecretString(`{"username":"x"}`); err == nil {
		t.Error("password 필드가 없으면 오류여야 한다")
	}
}

// 파싱 오류 메시지에 원문(=비밀번호)이 절대 섞이지 않는다.
func TestParseErrorNeverLeaksSecret(t *testing.T) {
	const secret = "super-secret-do-not-log"
	_, err := passwordFromSecretString(`{"password": ` + secret + `}`) // 깨진 JSON
	if err == nil {
		t.Fatal("깨진 JSON은 오류여야 한다")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("오류 메시지에 원문이 들어갔다: %v", err)
	}
}

// countingTransport는 시도 수를 세고, 첫 시도만 시도당 상한을 넘기도록 지연시킨다.
type countingTransport struct {
	attempts int32
	slowFor  int32 // 이 횟수까지는 느리게 응답한다
}

func (tr *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	n := atomic.AddInt32(&tr.attempts, 1)
	if n <= tr.slowFor {
		// http.Client.Timeout이 이 요청을 자를 때까지 기다린다.
		select {
		case <-time.After(perAttemptTimeout * 3):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	body := `{"SecretString":"{\"password\":\"from-aws\"}"}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/x-amz-json-1.1"}},
		Request:    req,
	}, nil
}

// ⑬ 시도당 상한이 실제로 시도를 자르고 재시도가 일어나며,
// 호출 전체가 refreshCtx 상한(10초) 안에 끝난다.
func TestPerAttemptTimeoutCutsAttemptAndRetries(t *testing.T) {
	// SDK가 자격증명·IMDS를 찾아다니지 않게 정적 값을 준다.
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	tr := &countingTransport{slowFor: 1} // 첫 시도만 느리다
	client := &http.Client{Timeout: perAttemptTimeout, Transport: tr}

	fetch, err := newSecretsManagerFetch(context.Background(),
		"arn:aws:secretsmanager:ap-northeast-2:123456789012:secret:test-AbCdEf", client)
	if err != nil {
		t.Fatalf("fetch 생성 실패: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), RefreshTimeout)
	defer cancel()

	start := time.Now()
	got, err := fetch(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("재시도로 회복해야 한다: %v", err)
	}
	if got != "from-aws" {
		t.Errorf("비밀번호 %q — from-aws여야 한다", got)
	}
	if n := atomic.LoadInt32(&tr.attempts); n != maxAttempts {
		t.Errorf("시도 %d회 — 시도당 상한이 첫 시도를 잘라 %d회여야 한다", n, maxAttempts)
	}
	if elapsed >= RefreshTimeout {
		t.Errorf("%v 걸렸다 — refreshCtx 상한 %v 안에 끝나야 한다", elapsed, RefreshTimeout)
	}
	if elapsed < perAttemptTimeout {
		t.Errorf("%v 걸렸다 — 첫 시도가 상한 %v까지는 기다려야 한다", elapsed, perAttemptTimeout)
	}
}
