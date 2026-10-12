package clicks

import (
	"context"
	"log/slog"
	"time"
)

const (
	// BatchRetention은 반영한 배치 ID를 남겨 두는 기간이다. 이보다 늦게 다시 온 배치는 중복으로 걸러지지 않는다.
	// 반영은 됐는데 삭제 응답을 잃고 이어진 수신이 모두 실패하면 이미 반영된 메시지가 DLQ로 간다.
	// 표준 큐는 DLQ에서 처음 넣은 시각 기준으로 만료되고(14일), DLQ 재처리는 새 메시지로 다시 넣는다(본 큐 4일).
	// 21일은 그 합에 여유를 둔 값이다(plan 0013 고정값 표).
	BatchRetention = 21 * 24 * time.Hour
	// PruneInterval은 오래된 배치 ID를 지우는 주기다.
	PruneInterval = time.Hour
	// receiveRetryDelay는 수신이 실패했을 때 다시 시도하기 전 쉬는 시간이다(오류 로그가 쏟아지지 않게).
	receiveRetryDelay = time.Second
)

// Applier는 클릭 배치를 DB에 반영하는 저장소다(links.ClickBatchRepository가 만족한다).
type Applier interface {
	ApplyClickBatch(ctx context.Context, batchID string, counts map[string]int64) (duplicate bool, unknownClicks int64, err error)
	PruneClickBatches(ctx context.Context, olderThan time.Duration) (int64, error)
}

// Worker는 큐에서 배치를 받아 저장소에 반영하고, 반영이 끝난 메시지만 Ack한다.
type Worker struct {
	queue Queue
	store Applier
	stats *Stats
}

// NewWorker는 큐·저장소·공용 통계로 Worker를 만든다.
func NewWorker(q Queue, store Applier, stats *Stats) *Worker {
	return &Worker{queue: q, store: store, stats: stats}
}

// Run은 ctx가 끝날 때까지 받고 반영하기를 되풀이한다. 수신 대기 중에 ctx가 끝나면 바로 돌아온다.
//
// 반영 중에 ctx가 끝나면 그 배치는 실패로 남고 Ack하지 않는다. 큐가 가시성 타임아웃 뒤 다시 주고,
// 이미 커밋됐다면 저장소가 중복으로 거른다.
func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		ds, err := w.queue.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("클릭 수신 실패", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(receiveRetryDelay):
			}
			continue
		}
		for _, d := range ds {
			w.handle(ctx, d)
		}
	}
}

// handle은 메시지 하나를 반영하고, 성공했을 때만 Ack한다.
func (w *Worker) handle(ctx context.Context, d Delivery) {
	if d.Err != nil {
		// 깨진 메시지는 Ack하지 않는다 → 재전달 끝에 DLQ로 가고 DLQ 알람이 울린다.
		w.stats.ApplyErrors.Add(1)
		slog.Error("클릭 반영 실패", "reason", "decode", "error", d.Err)
		return
	}
	dup, unknown, err := w.store.ApplyClickBatch(ctx, d.Batch.ID, d.Batch.Counts)
	if err != nil {
		w.stats.ApplyErrors.Add(1)
		slog.Error("클릭 반영 실패", "batch_id", d.Batch.ID, "error", err)
		return
	}
	if dup {
		w.stats.DuplicateBatches.Add(1)
	} else {
		w.stats.AppliedBatches.Add(1)
		w.stats.AppliedClicks.Add(sumCounts(d.Batch.Counts) - unknown)
		w.stats.UnknownClicks.Add(unknown)
	}
	if err := w.queue.Ack(ctx, d); err != nil {
		// 반영은 끝났다. 다시 와도 저장소가 중복으로 거르므로 로그만 남긴다.
		slog.Warn("클릭 Ack 실패", "batch_id", d.Batch.ID, "error", err)
	}
}

// RunPrune은 ctx가 끝날 때까지 tick마다 BatchRetention보다 오래된 배치 ID를 지운다.
func (w *Worker) RunPrune(ctx context.Context, tick <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			n, err := w.store.PruneClickBatches(ctx, BatchRetention)
			if err != nil {
				slog.Error("클릭 배치 ID 정리 실패", "error", err)
				continue
			}
			slog.Info("클릭 배치 ID 정리", "deleted", n)
		}
	}
}
