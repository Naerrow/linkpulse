package dbcreds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// 기동 seed 조회 예산 (plan 0014).
//
// ECS는 실행 역할이 기동 때 비밀번호를 env로 넣어 줬다(seed). EKS·k3s에는 그 주입이 없어 앱이 직접 읽는다.
// 파드가 막 뜬 노드에서는 Pod Identity 에이전트가 늦게 준비될 수 있어 몇 번 다시 시도한다.
// 끝내 실패하면 기동을 멈춘다 — 쿠버네티스가 재시작하고 CrashLoopBackOff로 드러난다.
// 최악은 시도 5번 × RefreshTimeout 10초 + 대기 4번 × 2초 = 58초다(차트의 startupProbe가 이보다 길다).
const (
	seedAttempts = 5
	seedBackoff  = 2 * time.Second
)

// Start는 출처에서 기동 seed를 읽어 provider를 만든다.
//
// seed와 회전 뒤 재조회가 같은 fetch를 쓴다. 그래서 "기동 때 읽은 값"과 "다시 읽은 값"이 출처만 같으면
// 같은 규칙(값이 같으면 세대 유지)으로 비교된다.
func Start(ctx context.Context, fetch FetchFunc, opts ...Option) (*Provider, error) {
	seed, err := fetchSeed(ctx, fetch, seedAttempts, seedBackoff)
	if err != nil {
		return nil, err
	}
	return New(seed, fetch, opts...), nil
}

// fetchSeed는 seed를 읽는다. 시도마다 RefreshTimeout을 상한으로 둔다.
// 실패 사유는 refresh 실패와 같은 정책으로 남긴다(SDK 메시지는 싣고, 비밀번호는 어느 경로로도 들어가지 않는다).
func fetchSeed(ctx context.Context, fetch FetchFunc, attempts int, backoff time.Duration) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, RefreshTimeout)
		password, err := fetch(attemptCtx)
		cancel()
		if err == nil {
			return password, nil
		}
		lastErr = err
		slog.Warn("기동 비밀번호 조회 실패, 재시도",
			"event", "secret_seed_failed", "attempt", attempt, "max", attempts, "error", err.Error())
		if attempt < attempts {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(backoff):
			}
		}
	}
	return "", fmt.Errorf("기동 비밀번호 조회 실패(%d회 시도): %w", attempts, lastErr)
}

// NewFileFetch는 파일에서 비밀번호를 읽는 FetchFunc를 만든다(DB_PASSWORD_SOURCE=file, P6 k3s용).
//
// 쿠버네티스 Secret을 볼륨으로 마운트하면 회전 때 kubelet이 파일을 바꾼다. 매 호출이 파일을 새로 읽으므로
// 인증 실패 뒤 refresh가 새 값을 집는다. subPath 마운트는 kubelet이 갱신하지 않으니 쓰지 않는다.
//
// 끝의 줄바꿈은 지운다 — 파일로 만든 Secret에 흔히 붙는다. 오류에 파일 내용을 넣지 않는다(그 내용이 비밀번호다).
func NewFileFetch(path string) FetchFunc {
	return func(context.Context) (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("비밀번호 파일 읽기 실패: %w", err)
		}
		password := strings.TrimRight(string(b), "\r\n")
		if password == "" {
			return "", errors.New("비밀번호 파일이 비어 있습니다")
		}
		return password, nil
	}
}
