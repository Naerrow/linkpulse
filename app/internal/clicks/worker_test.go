package clicks

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeApplier는 배치 ID별로 정한 결과를 돌려주고, 호출과 정리 요청을 기록한다.
type fakeApplier struct {
	mu       sync.Mutex
	applied  []string
	failIDs  map[string]bool
	dupIDs   map[string]bool
	unknown  int64
	pruned   []time.Duration
	pruneHit chan struct{}
}

func (a *fakeApplier) ApplyClickBatch(_ context.Context, id string, _ map[string]int64) (bool, int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applied = append(a.applied, id)
	if a.failIDs[id] {
		return false, 0, errors.New("반영 실패(가짜)")
	}
	if a.dupIDs[id] {
		return true, 0, nil
	}
	return false, a.unknown, nil
}

func (a *fakeApplier) PruneClickBatches(_ context.Context, olderThan time.Duration) (int64, error) {
	a.mu.Lock()
	a.pruned = append(a.pruned, olderThan)
	a.mu.Unlock()
	if a.pruneHit != nil {
		a.pruneHit <- struct{}{}
	}
	return 0, nil
}

// ackQueue는 Ack된 배치 ID만 기록하는 가짜 큐다.
type ackQueue struct {
	fakeQueue
	mu    sync.Mutex
	acked []string
}

func (q *ackQueue) Ack(_ context.Context, d Delivery) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.acked = append(q.acked, d.Batch.ID)
	return nil
}

// 반영에 성공한 메시지(중복 포함)만 Ack한다. 반영 실패·해석 실패는 Ack하지 않아 재전달 끝에 DLQ로 간다.
func TestWorkerAcksOnlyAfterApply(t *testing.T) {
	q := &ackQueue{}
	store := &fakeApplier{failIDs: map[string]bool{"bad": true}, dupIDs: map[string]bool{"dup": true}, unknown: 1}
	w := NewWorker(q, store, &Stats{})

	for _, d := range []Delivery{
		{Batch: Batch{ID: "ok", Counts: map[string]int64{"a": 2, "b": 3}}},
		{Batch: Batch{ID: "bad", Counts: map[string]int64{"a": 1}}},
		{Err: errors.New("깨진 JSON")},
		{Batch: Batch{ID: "dup", Counts: map[string]int64{"a": 9}}},
	} {
		w.handle(context.Background(), d)
	}

	if want := []string{"ok", "dup"}; !slices.Equal(q.acked, want) {
		t.Errorf("Ack = %v, want %v", q.acked, want)
	}
	if want := []string{"ok", "bad", "dup"}; !slices.Equal(store.applied, want) {
		t.Errorf("반영 호출 = %v, want %v(해석 실패는 저장소를 부르지 않는다)", store.applied, want)
	}
	s := w.stats
	if s.AppliedBatches.Load() != 1 || s.AppliedClicks.Load() != 4 || s.UnknownClicks.Load() != 1 ||
		s.DuplicateBatches.Load() != 1 || s.ApplyErrors.Load() != 2 {
		t.Errorf("통계 = applied %d/%d, unknown %d, dup %d, errors %d; want 1/4, 1, 1, 2",
			s.AppliedBatches.Load(), s.AppliedClicks.Load(), s.UnknownClicks.Load(), s.DuplicateBatches.Load(), s.ApplyErrors.Load())
	}
}

// 롱폴링으로 기다리는 중에 ctx가 끝나면 Run은 바로 돌아온다(종료 시간 상한, plan 0013 r1 #5).
func TestWorkerRunStopsOnCancel(t *testing.T) {
	w := NewWorker(&fakeQueue{}, &fakeApplier{}, &Stats{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx가 끝났는데 Run이 돌아오지 않는다")
	}
}

func TestWorkerRunPrune(t *testing.T) {
	store := &fakeApplier{pruneHit: make(chan struct{}, 1)}
	w := NewWorker(&fakeQueue{}, store, &Stats{})
	tick := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.RunPrune(ctx, tick)

	tick <- time.Time{}
	<-store.pruneHit
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.pruned) != 1 || store.pruned[0] != BatchRetention {
		t.Errorf("정리 호출 = %v, want [%v]", store.pruned, BatchRetention)
	}
}

// 기록 → 메모리 큐 → 워커 → 저장소까지 이어서, 서버 쪽 불변식 Recorded − Dropped = Applied + Unknown을 본다.
func TestPipelineInvariant(t *testing.T) {
	q := NewMemoryQueue()
	stats := &Stats{}
	b := NewBatcher(q, stats)
	store := &fakeApplier{}
	w := NewWorker(q, store, stats)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	for i := 0; i < 3000; i++ {
		b.Record([]string{"a", "b", "c"}[i%3])
		if i%700 == 0 {
			_ = b.Flush(ctx)
		}
	}
	_ = b.Flush(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for stats.AppliedClicks.Load()+stats.UnknownClicks.Load() < 3000 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if got := stats.AppliedClicks.Load() + stats.UnknownClicks.Load(); got != stats.Recorded.Load()-stats.Dropped.Load() {
		t.Errorf("Applied+Unknown %d ≠ Recorded %d − Dropped %d", got, stats.Recorded.Load(), stats.Dropped.Load())
	}
}

func TestMemoryQueueReceive(t *testing.T) {
	q := NewMemoryQueue()
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		_ = q.Publish(ctx, Batch{ID: string(rune('a' + i))})
	}
	first, _ := q.Receive(ctx)
	second, _ := q.Receive(ctx)
	if len(first) != maxReceive || len(second) != 2 || first[0].Batch.ID != "a" || second[1].Batch.ID != "l" {
		t.Fatalf("받은 수 %d·%d, want %d·2(순서 유지)", len(first), len(second), maxReceive)
	}

	// 비어 있으면 기다리다 Publish가 오면 돌아온다.
	got := make(chan []Delivery)
	go func() {
		ds, _ := q.Receive(ctx)
		got <- ds
	}()
	time.Sleep(20 * time.Millisecond)
	_ = q.Publish(ctx, Batch{ID: "late"})
	select {
	case ds := <-got:
		if len(ds) != 1 || ds[0].Batch.ID != "late" {
			t.Errorf("기다린 뒤 받은 것 = %+v", ds)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Publish 뒤에도 Receive가 돌아오지 않는다")
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := q.Receive(cctx); !errors.Is(err, context.Canceled) {
		t.Errorf("끝난 ctx로 Receive err = %v, want context.Canceled", err)
	}
}

// lineWriter는 로그 한 줄이 써질 때마다 그 줄을 채널로 넘긴다. 테스트가 줄 단위로 발을 맞춘다.
type lineWriter struct{ lines chan string }

func (w lineWriter) Write(p []byte) (int, error) {
	w.lines <- string(p)
	return len(p), nil
}

// LogStats는 tick마다 직전 줄 이후의 증가분을 찍는다(누적값이 아니다).
// 한 줄이 다 써진 것을 받은 뒤에야 통계를 바꿔, 고루틴 타이밍과 무관하게 결정적이다.
func TestLogStats(t *testing.T) {
	stats := &Stats{}
	out := lineWriter{lines: make(chan string, 1)}
	log := slog.New(slog.NewJSONHandler(out, nil))
	tick := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		LogStats(ctx, stats, func() int { return 7 }, tick, log)
	}()

	next := func() map[string]any {
		tick <- time.Time{}
		var m map[string]any
		if err := json.Unmarshal([]byte(<-out.lines), &m); err != nil {
			t.Fatalf("로그 줄 해석 실패: %v", err)
		}
		return m
	}
	first := next()
	stats.Recorded.Add(5)
	second := next()
	stats.Recorded.Add(2)
	stats.AppliedClicks.Add(3)
	third := next()
	// 종료할 때도 마지막 증가분을 한 줄 찍는다(코드 검토 r1 #2) — 그래야 종료하는 태스크의 기록이 합계에서 빠지지 않는다.
	stats.Recorded.Add(4)
	cancel()
	var last map[string]any
	if err := json.Unmarshal([]byte(<-out.lines), &last); err != nil {
		t.Fatalf("종료 줄 해석 실패: %v", err)
	}
	<-done

	for i, c := range []struct {
		line              map[string]any
		recorded, applied float64
	}{{first, 0, 0}, {second, 5, 0}, {third, 2, 3}, {last, 4, 0}} {
		if c.line["msg"] != "click queue" || c.line["recorded_clicks"] != c.recorded ||
			c.line["applied_clicks"] != c.applied || c.line["pending_codes"] != float64(7) {
			t.Errorf("%d번째 줄 = %v", i+1, c.line)
		}
	}
}
