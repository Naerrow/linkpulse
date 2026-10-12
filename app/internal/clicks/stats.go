package clicks

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Stats는 Batcher와 Worker가 함께 쓰는 누적 카운터다. LogStats가 주기마다 증가분을 로그로 남긴다.
//
// 정합성 불변식(plan 0013 판정 (b)): 큐가 빈 뒤 Recorded − Dropped = AppliedClicks + UnknownClicks.
// 없는 코드는 리다이렉트가 404로 끝나 기록되지 않으므로 사실상 UnknownClicks = 0이고, 그러면 우변이 DB 합계다.
// 클라이언트가 302를 받았는지와 상관없이 서버 쪽에서 성립해야 한다.
type Stats struct {
	Recorded         atomic.Int64 // Record 호출 수
	Dropped          atomic.Int64 // 대기 상한에 걸려 버린 클릭
	PublishedBatches atomic.Int64
	PublishedClicks  atomic.Int64
	PublishErrors    atomic.Int64
	AppliedBatches   atomic.Int64
	AppliedClicks    atomic.Int64 // DB에 실제로 더한 클릭(없는 코드 제외)
	DuplicateBatches atomic.Int64 // 이미 반영한 배치가 다시 온 수
	UnknownClicks    atomic.Int64 // 없는 코드라 건너뛴 클릭
	ApplyErrors      atomic.Int64 // 반영 실패와 해석 실패
}

// statsSnapshot은 한 시점의 카운터 값이다.
type statsSnapshot struct {
	recorded, dropped, publishedBatches, publishedClicks, publishErrors         int64
	appliedBatches, appliedClicks, duplicateBatches, unknownClicks, applyErrors int64
}

func (s *Stats) snapshot() statsSnapshot {
	return statsSnapshot{
		recorded:         s.Recorded.Load(),
		dropped:          s.Dropped.Load(),
		publishedBatches: s.PublishedBatches.Load(),
		publishedClicks:  s.PublishedClicks.Load(),
		publishErrors:    s.PublishErrors.Load(),
		appliedBatches:   s.AppliedBatches.Load(),
		appliedClicks:    s.AppliedClicks.Load(),
		duplicateBatches: s.DuplicateBatches.Load(),
		unknownClicks:    s.UnknownClicks.Load(),
		applyErrors:      s.ApplyErrors.Load(),
	}
}

// LogStats는 ctx가 끝날 때까지 tick마다 직전 줄 이후의 증가분을 "click queue" 한 줄로 남긴다.
// db pool 로그와 같은 방식이다 — Logs Insights에서 sum으로 합치면 구간 합계가 된다.
// pending은 그 시점에 아직 큐에 못 보낸 코드 수(버퍼 + 재시도 목록)를 돌려준다.
//
// ctx가 끝나면 마지막 증가분을 한 줄 더 찍고 돌아온다. 그러지 않으면 종료하는 태스크의 마지막 10초 recorded_clicks가
// 빠지고, 그 태스크가 마지막 flush로 보낸 배치는 다른 태스크가 반영해 applied로 세므로 정합성 줄이 거짓으로 어긋난다
// (plan 0013 코드 검토 r1 #2). 호출자는 HTTP를 닫은 뒤(더는 Record가 없을 때) ctx를 끝낸다.
func LogStats(ctx context.Context, s *Stats, pending func() int, tick <-chan time.Time, log *slog.Logger) {
	prev := s.snapshot()
	for {
		select {
		case <-ctx.Done():
			logDelta(log, prev, s.snapshot(), pending())
			return
		case <-tick:
			cur := s.snapshot()
			logDelta(log, prev, cur, pending())
			prev = cur
		}
	}
}

// logDelta는 두 시점 사이의 증가분을 "click queue" 한 줄로 찍는다.
func logDelta(log *slog.Logger, prev, cur statsSnapshot, pendingCodes int) {
	// 필드 이름은 Logs Insights 집계 별칭과 겹치지 않게 그대로 둔다(별칭은 다른 이름으로 — ops-gotchas G-12).
	log.Info("click queue",
		"recorded_clicks", cur.recorded-prev.recorded,
		"dropped_clicks", cur.dropped-prev.dropped,
		"published_batches", cur.publishedBatches-prev.publishedBatches,
		"published_clicks", cur.publishedClicks-prev.publishedClicks,
		"publish_errors", cur.publishErrors-prev.publishErrors,
		"pending_codes", pendingCodes,
		"applied_batches", cur.appliedBatches-prev.appliedBatches,
		"applied_clicks", cur.appliedClicks-prev.appliedClicks,
		"duplicate_batches", cur.duplicateBatches-prev.duplicateBatches,
		"unknown_clicks", cur.unknownClicks-prev.unknownClicks,
		"apply_errors", cur.applyErrors-prev.applyErrors,
	)
}
