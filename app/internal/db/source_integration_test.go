//go:build integration

// plan 0014 통합 테스트 — EKS의 출처 모드 경로.
//
// ECS에서는 DSN에 seed 비밀번호가 들어 있었다. EKS에서는 DB_PASSWORD_SOURCE가 있으면 DSN에 비밀번호가
// 없고, 매 연결마다 provider가 넣는다. 그 경로가 실제 Postgres에서 붙고 회전 뒤 회복하는지,
// DB_MAX_OPEN_CONNS가 실제 풀에 적용되는지 본다. 실행 방법은 rotation_integration_test.go 머리 주석과 같다.
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/Naerrow/linkpulse/app/internal/db"
	"github.com/Naerrow/linkpulse/app/internal/dbcreds"
)

func TestPasswordlessDSNWithSourceAndPoolSize(t *testing.T) {
	env := newRotationEnv(t)
	secret := &secretFixture{value: oldPassword}

	// 운영과 같은 순서: 기동 seed를 출처에서 읽고(Start), 비밀번호 없는 DSN으로 풀을 연다.
	provider, err := dbcreds.Start(context.Background(), secret.fetch, dbcreds.WithLogger(env.logs.logger()))
	if err != nil {
		t.Fatalf("Start 실패: %v", err)
	}
	t.Cleanup(provider.Close)

	pool, err := db.Open(context.Background(), db.Settings{
		DSN:          env.dsnFor(""),
		Provider:     provider,
		MaxOpenConns: 3,
	})
	if err != nil {
		t.Fatalf("비밀번호 없는 DSN + provider로 풀을 열어야 한다: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	if got := pool.Stats().MaxOpenConnections; got != 3 {
		t.Errorf("MaxOpenConnections %d — DB_MAX_OPEN_CONNS(3)가 적용돼야 한다", got)
	}
	env.warmPool(t, pool)

	// 회전 뒤에도 같은 경로로 회복한다(DSN에 기댈 비밀번호가 없다).
	tSet := time.Now()
	env.rotate(t, newPassword)
	secret.setValue(newPassword)
	forceNewConnections(pool)
	elapsed := waitForQuery(t, pool, deadlineFromTSet, tSet)
	t.Logf("T_set → 쿼리 성공: %v (합격 ≤ %v)", elapsed, deadlineFromTSet)
}
