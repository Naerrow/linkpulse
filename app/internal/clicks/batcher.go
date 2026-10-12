package clicks

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	// FlushInterval은 모은 클릭을 큐로 보내는 주기다. 강제 종료(SIGKILL·OOM) 때 잃을 수 있는 클릭의 상한이기도 하다.
	FlushInterval = time.Second
	// publishTimeout은 주기 flush 한 번의 상한이다(SDK 재시도 포함). SDK 기본 HTTP 클라이언트에는 응답 대기
	// 타임아웃이 없어서, 이것이 없으면 응답 없는 SendMessage 하나가 flushMu를 쥔 채 Batcher를 무기한 멈춘다.
	// 시간이 다 되면 그 배치는 재시도 목록에 남아 다음 tick에 같은 ID로 다시 나간다(plan 0013 코드 검토 r1 제안).
	publishTimeout = 5 * time.Second
	// maxPendingCodes는 아직 큐에 못 보낸 코드 수(버퍼 + 재시도 목록)의 상한이다.
	// 큐가 오래 죽어도 메모리가 무한히 늘지 않게, 넘는 새 코드의 클릭은 버리고 Dropped로 센다.
	maxPendingCodes = 100_000
)

// Batcher는 리다이렉트의 클릭을 메모리에 모았다가 주기마다 배치로 큐에 보낸다.
// links.ClickRecorder를 만족한다 — Record는 락 한 번과 map 갱신뿐이라 리다이렉트를 기다리게 하지 않는다.
type Batcher struct {
	queue   Queue
	stats   *Stats
	newID   func() string // 테스트가 ID를 고정할 수 있게 주입한다
	limit   int           // maxPendingCodes(테스트가 낮춘다)
	timeout time.Duration // publishTimeout(테스트가 낮춘다)

	mu          sync.Mutex
	pending     map[string]int64 // 아직 배치로 묶지 않은 클릭
	outbox      []Batch          // 묶었지만 아직 못 보낸 배치(발행 실패분은 ID 그대로 남는다)
	outboxCodes int              // outbox에 든 코드 수 합

	flushMu sync.Mutex // Flush를 한 번에 하나만 돌린다(주기 flush와 종료 flush가 겹치지 않게)
}

// NewBatcher는 큐와 공용 통계로 Batcher를 만든다.
func NewBatcher(q Queue, stats *Stats) *Batcher {
	return &Batcher{
		queue:   q,
		stats:   stats,
		newID:   newBatchID,
		limit:   maxPendingCodes,
		timeout: publishTimeout,
		pending: make(map[string]int64),
	}
}

// Record는 클릭 하나를 모은다. 대기 코드가 상한이면 이미 모으고 있는 코드만 받고, 새 코드는 버린다.
func (b *Batcher) Record(code string) {
	b.stats.Recorded.Add(1)
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.pending[code]; !ok && len(b.pending)+b.outboxCodes >= b.limit {
		b.stats.Dropped.Add(1)
		return
	}
	b.pending[code]++
}

// PendingCodes는 아직 큐에 못 보낸 코드 수(버퍼 + 재시도 목록)다. 통계 로그가 쓴다.
func (b *Batcher) PendingCodes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending) + b.outboxCodes
}

// Flush는 모은 클릭을 배치(코드 최대 maxCodesPerBatch개)로 묶어 재시도 목록 뒤에 붙이고, 목록을 앞에서부터 보낸다.
//
// 발행에 실패하면 그 배치부터는 ID와 개수를 그대로 남겨 다음 Flush에서 같은 ID로 다시 보낸다.
// 새 ID로 다시 묶으면 안 된다 — SDK가 오류를 돌려줬어도 실제로는 들어갔을 수 있고(응답 타임아웃),
// 그러면 같은 클릭이 서로 다른 ID 두 개로 반영된다. 같은 ID면 저장소가 두 번째를 중복으로 거른다.
// 빈 버퍼는 보내지 않는다.
func (b *Batcher) Flush(ctx context.Context) error {
	b.flushMu.Lock()
	defer b.flushMu.Unlock()

	b.mu.Lock()
	if len(b.pending) > 0 {
		for _, batch := range splitBatches(b.pending, b.newID) {
			b.outbox = append(b.outbox, batch)
			b.outboxCodes += len(batch.Counts)
		}
		b.pending = make(map[string]int64)
	}
	todo := append([]Batch(nil), b.outbox...)
	b.mu.Unlock()

	sent, sentCodes := 0, 0
	var err error
	for _, batch := range todo {
		if err = b.queue.Publish(ctx, batch); err != nil {
			break
		}
		sent++
		sentCodes += len(batch.Counts)
		b.stats.PublishedBatches.Add(1)
		b.stats.PublishedClicks.Add(sumCounts(batch.Counts))
	}

	// outbox 앞쪽 sent개가 이번에 보낸 것이다. 그 사이 Record는 pending만 바꾸고 outbox는 Flush만 바꾼다.
	b.mu.Lock()
	b.outbox = b.outbox[sent:]
	b.outboxCodes -= sentCodes
	left := len(b.outbox)
	b.mu.Unlock()

	if err != nil {
		b.stats.PublishErrors.Add(1)
		slog.Error("클릭 발행 실패", "unsent_batches", left, "error", err)
		return err
	}
	return nil
}

// Run은 ctx가 끝날 때까지 tick마다 Flush한다. 마지막 flush는 호출자가 종료 순서에 맞춰 따로 부른다.
func (b *Batcher) Run(ctx context.Context, tick <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			// 실패(시간 초과 포함)는 Flush가 로그로 남기고 다음 tick에 같은 ID로 다시 보낸다.
			fctx, cancel := context.WithTimeout(ctx, b.timeout)
			_ = b.Flush(fctx)
			cancel()
		}
	}
}

// splitBatches는 모은 클릭을 코드 maxCodesPerBatch개 단위의 배치로 나눈다. 배치마다 새 ID를 받는다.
func splitBatches(counts map[string]int64, newID func() string) []Batch {
	var out []Batch
	cur := Batch{ID: newID(), Counts: make(map[string]int64)}
	for code, n := range counts {
		if len(cur.Counts) == maxCodesPerBatch {
			out = append(out, cur)
			cur = Batch{ID: newID(), Counts: make(map[string]int64)}
		}
		cur.Counts[code] = n
	}
	return append(out, cur)
}

func sumCounts(counts map[string]int64) int64 {
	var n int64
	for _, c := range counts {
		n += c
	}
	return n
}
