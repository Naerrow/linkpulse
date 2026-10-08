package db

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

// LogPoolStats는 ctx가 끝날 때까지 interval마다 연결 풀 통계를 한 줄씩 남긴다(plan 0012).
//
// 풀이 찼다는 사실(대기 발생)을 보여 주는 임시 관측 수단이다. 이유(쿼리가 느려져서인지,
// 풀이 작아서인지)는 보여 주지 않는다. Prometheus(plan 0015) 전까지 Logs Insights로 집계한다.
func LogPoolStats(ctx context.Context, pool *sql.DB, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	runPoolStats(ctx, pool.Stats, ticker.C, slog.Default())
}

// runPoolStats는 tick마다 직전 기록 이후의 대기 증가분을 로그로 남긴다.
// 통계 원천과 tick을 주입받아 테스트가 시간을 직접 진행한다.
func runPoolStats(ctx context.Context, stats func() sql.DBStats, tick <-chan time.Time, log *slog.Logger) {
	prev := stats()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			cur := stats()
			waitCount, waitDur := poolWaitDelta(prev, cur)
			prev = cur
			// wait_count·wait_ms는 누적값이 아니라 직전 줄 이후 증가분이다 — Logs Insights에서 sum으로 합친다.
			log.Info("db pool",
				"in_use", cur.InUse,
				"idle", cur.Idle,
				"max_open", cur.MaxOpenConnections,
				"wait_count", waitCount,
				"wait_ms", waitDur.Milliseconds(),
			)
		}
	}
}

// poolWaitDelta는 두 시점 사이의 풀 대기 건수·대기 시간 증가분이다.
// sql.DBStats의 WaitCount·WaitDuration은 풀이 열린 뒤의 누적값이라 그대로 찍으면 구간 비교가 안 된다.
func poolWaitDelta(prev, cur sql.DBStats) (int64, time.Duration) {
	return cur.WaitCount - prev.WaitCount, cur.WaitDuration - prev.WaitDuration
}
