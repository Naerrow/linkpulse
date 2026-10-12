package clicks

import (
	"context"
	"sync"
)

// MemoryQueue는 한 프로세스 안에서만 도는 큐다(로컬 docker-compose·테스트용).
//
// 재전달이 없다: Receive가 꺼낸 메시지는 Ack 여부와 상관없이 사라진다. 그래서 반영에 실패한 배치나
// 종료 직전에 넣은 배치는 잃는다. 운영은 반드시 SQS를 쓴다(config가 production에서 큐 URL을 요구한다).
type MemoryQueue struct {
	mu    sync.Mutex
	items []Batch
	ready chan struct{} // 새 항목이 들어왔다는 신호(버퍼 1)
}

// NewMemoryQueue는 빈 메모리 큐를 만든다.
func NewMemoryQueue() *MemoryQueue {
	return &MemoryQueue{ready: make(chan struct{}, 1)}
}

// Publish는 배치를 큐 끝에 넣는다. 실패하지 않는다.
func (q *MemoryQueue) Publish(_ context.Context, b Batch) error {
	q.mu.Lock()
	q.items = append(q.items, b)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default: // 이미 신호가 있다.
	}
	return nil
}

// Receive는 항목이 생길 때까지 기다렸다가 최대 maxReceive개를 꺼낸다. ctx가 끝나면 바로 돌아온다.
func (q *MemoryQueue) Receive(ctx context.Context) ([]Delivery, error) {
	for {
		q.mu.Lock()
		if n := min(len(q.items), maxReceive); n > 0 {
			out := make([]Delivery, n)
			for i := range out {
				out[i] = Delivery{Batch: q.items[i]}
			}
			q.items = q.items[n:]
			q.mu.Unlock()
			return out, nil
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-q.ready:
		}
	}
}

// Ack는 할 일이 없다(Receive가 이미 꺼냈다).
func (q *MemoryQueue) Ack(context.Context, Delivery) error { return nil }
