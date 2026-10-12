package clicks

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeQueue는 발행을 기록하고, 정한 횟수만큼 실패하거나 ctx가 끝날 때까지 막는 가짜 큐다.
type fakeQueue struct {
	mu        sync.Mutex
	published []Batch
	failNext  int  // 다음 Publish를 이 횟수만큼 실패시킨다
	block     bool // true면 Publish가 ctx가 끝날 때까지 돌아오지 않는다
}

func (q *fakeQueue) Publish(ctx context.Context, b Batch) error {
	if q.block {
		<-ctx.Done()
		return ctx.Err()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failNext > 0 {
		q.failNext--
		return errors.New("발행 실패(가짜)")
	}
	q.published = append(q.published, b)
	return nil
}

func (q *fakeQueue) Receive(ctx context.Context) ([]Delivery, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (q *fakeQueue) Ack(context.Context, Delivery) error { return nil }

func (q *fakeQueue) batches() []Batch {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]Batch(nil), q.published...)
}

// seqIDs는 "b1", "b2", … 순서로 ID를 내주는 함수를 만든다.
func seqIDs() func() string {
	var mu sync.Mutex
	n := 0
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return fmt.Sprintf("b%d", n)
	}
}

func newTestBatcher(q Queue) *Batcher {
	b := NewBatcher(q, &Stats{})
	b.newID = seqIDs()
	return b
}

func TestBatcherFlushAggregates(t *testing.T) {
	q := &fakeQueue{}
	b := newTestBatcher(q)
	for _, c := range []string{"a", "a", "b", "a"} {
		b.Record(c)
	}
	if err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := q.batches()
	if len(got) != 1 || got[0].Counts["a"] != 3 || got[0].Counts["b"] != 1 || len(got[0].Counts) != 2 {
		t.Fatalf("발행 = %+v, want 배치 1개 {a:3, b:1}", got)
	}
	if n := b.stats.PublishedClicks.Load(); n != 4 {
		t.Errorf("PublishedClicks = %d, want 4", n)
	}
}

// 빈 버퍼는 보내지 않는다 — 매초 빈 배치를 보내면 태스크 2개에 월 약 520만 요청이다.
func TestBatcherEmptyFlushPublishesNothing(t *testing.T) {
	q := &fakeQueue{}
	b := newTestBatcher(q)
	for i := 0; i < 3; i++ {
		if err := b.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := q.batches(); len(got) != 0 {
		t.Fatalf("빈 버퍼인데 발행했다: %+v", got)
	}
}

func TestBatcherSplitsLargeBuffer(t *testing.T) {
	q := &fakeQueue{}
	b := newTestBatcher(q)
	for i := 0; i < 2500; i++ {
		b.Record(fmt.Sprintf("c%04d", i))
	}
	if err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := q.batches()
	if len(got) != 3 {
		t.Fatalf("배치 %d개, want 3", len(got))
	}
	ids := map[string]bool{}
	total := 0
	for _, batch := range got {
		if len(batch.Counts) > maxCodesPerBatch {
			t.Errorf("배치 %s 코드 %d개 > %d", batch.ID, len(batch.Counts), maxCodesPerBatch)
		}
		ids[batch.ID] = true
		total += len(batch.Counts)
	}
	if len(ids) != 3 || total != 2500 {
		t.Errorf("ID %d개·코드 합 %d, want 3·2500", len(ids), total)
	}
}

// 발행이 실패한 배치는 ID와 개수를 그대로 둔 채 다음 Flush에서 같은 ID로 다시 나간다(plan 0013 r1 #1).
// 새 클릭은 새 배치로 나간다.
func TestBatcherRetriesWithSameID(t *testing.T) {
	q := &fakeQueue{failNext: 1}
	b := newTestBatcher(q)
	b.Record("a")
	b.Record("a")
	if err := b.Flush(context.Background()); err == nil {
		t.Fatal("첫 Flush가 실패를 돌려주지 않았다")
	}
	if b.PendingCodes() != 1 {
		t.Fatalf("실패한 배치가 남지 않았다: pending %d", b.PendingCodes())
	}
	b.Record("a")
	b.Record("b")
	if err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := q.batches()
	if len(got) != 2 {
		t.Fatalf("발행 %d개, want 2: %+v", len(got), got)
	}
	if got[0].ID != "b1" || len(got[0].Counts) != 1 || got[0].Counts["a"] != 2 {
		t.Errorf("재발행 = %+v, want {b1, a:2}(처음 ID·개수 그대로)", got[0])
	}
	if got[1].ID == "b1" || got[1].Counts["a"] != 1 || got[1].Counts["b"] != 1 {
		t.Errorf("새 배치 = %+v, want 새 ID {a:1, b:1}", got[1])
	}
	if b.PendingCodes() != 0 || b.stats.PublishErrors.Load() != 1 {
		t.Errorf("pending %d·발행 오류 %d, want 0·1", b.PendingCodes(), b.stats.PublishErrors.Load())
	}
}

// 대기 코드(버퍼 + 재시도 목록)가 상한이면 새 코드는 버리고 세고, 이미 모으는 코드는 계속 받는다.
// Recorded − Dropped = 발행 + 대기 중인 클릭이 늘 성립한다.
func TestBatcherDropsBeyondLimit(t *testing.T) {
	q := &fakeQueue{failNext: 1}
	b := newTestBatcher(q)
	b.limit = 2
	for _, c := range []string{"a", "b", "c", "a"} {
		b.Record(c)
	}
	if d := b.stats.Dropped.Load(); d != 1 {
		t.Fatalf("Dropped = %d, want 1(c)", d)
	}
	_ = b.Flush(context.Background()) // 실패 → a·b가 재시도 목록에 남는다
	b.Record("d")                     // 재시도 목록 2개가 상한을 채웠으므로 버린다
	if d := b.stats.Dropped.Load(); d != 2 {
		t.Fatalf("재시도 목록이 상한에 들어가지 않았다: Dropped = %d, want 2", d)
	}
	if err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := b.stats
	if s.Recorded.Load()-s.Dropped.Load() != s.PublishedClicks.Load() {
		t.Errorf("Recorded %d − Dropped %d ≠ Published %d", s.Recorded.Load(), s.Dropped.Load(), s.PublishedClicks.Load())
	}
}

// 마지막 flush는 상한 시간이 있다. 큐가 응답하지 않아도 ctx가 끝나면 돌아오고, 배치는 남는다.
func TestBatcherFlushRespectsContext(t *testing.T) {
	b := newTestBatcher(&fakeQueue{block: true})
	b.Record("a")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := b.Flush(ctx); err == nil {
		t.Fatal("막힌 큐인데 Flush가 성공했다")
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("Flush가 %v 걸렸다(ctx 50ms)", el)
	}
	if b.PendingCodes() != 1 {
		t.Errorf("실패한 배치가 남지 않았다")
	}
}

// 주기 flush도 상한이 있다. 큐가 응답하지 않아도 다음 tick을 받는다(Batcher가 무기한 멈추지 않는다).
func TestBatcherRunFlushHasTimeout(t *testing.T) {
	b := newTestBatcher(&fakeQueue{block: true})
	b.timeout = 20 * time.Millisecond
	tick := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx, tick)

	b.Record("a")
	tick <- time.Time{}
	select {
	case tick <- time.Time{}: // 첫 flush가 상한에 걸려 돌아와야 받아들여진다
	case <-time.After(2 * time.Second):
		t.Fatal("응답 없는 큐에서 주기 flush가 돌아오지 않는다")
	}
	if b.PendingCodes() != 1 {
		t.Errorf("시간 초과된 배치가 재시도 목록에 남지 않았다")
	}
}

func TestBatcherRunFlushesOnTickAndStops(t *testing.T) {
	q := &fakeQueue{}
	b := newTestBatcher(q)
	tick := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Run(ctx, tick)
	}()

	b.Record("a")
	tick <- time.Time{}
	tick <- time.Time{} // 두 번째 tick이 받아들여지면 첫 Flush는 끝났다
	if got := q.batches(); len(got) != 1 {
		t.Fatalf("tick 뒤 발행 %d개, want 1", len(got))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx가 끝났는데 Run이 돌아오지 않는다")
	}
}

// 여러 고루틴이 기록하는 동안 Flush가 돌아도 클릭이 사라지거나 두 번 나가지 않는다(-race로 돌린다).
func TestBatcherConcurrentRecordAndFlush(t *testing.T) {
	q := &fakeQueue{}
	b := NewBatcher(q, &Stats{})
	const workers, each = 8, 1000

	stop := make(chan struct{})
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		for {
			select {
			case <-stop:
				return
			default:
				_ = b.Flush(context.Background())
			}
		}
	}()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				b.Record(fmt.Sprintf("c%d", (w+i)%10))
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	<-flushed
	if err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, batch := range q.batches() {
		total += sumCounts(batch.Counts)
	}
	if total != workers*each {
		t.Errorf("발행한 클릭 %d, want %d", total, workers*each)
	}
}
