// clicks 패키지는 리다이렉트의 클릭을 모아 큐로 보내고(Batcher), 큐에서 받아 DB에 묶어 반영한다(Worker).
//
// plan 0013: 리다이렉트마다 같은 행을 UPDATE하면 인기 링크에서 행 잠금 경합이 첫 병목이었다(0012 기준선).
// 태스크 안에서 1초씩 모아 보내면, 한 링크에 몰린 초당 수천 번의 UPDATE가 태스크당 초당 한 번이 된다.
//
// 큐는 Queue 인터페이스 뒤에 둔다. 운영은 SQS(clicks/sqsqueue), 로컬·테스트는 MemoryQueue다.
// 이 패키지는 AWS SDK도 links 패키지도 import하지 않는다.
package clicks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

// Batch는 한 태스크가 한 주기 동안 모은 클릭이다. ID는 중복 반영을 거르는 열쇠다 —
// 큐는 최소 한 번 전달이라 같은 배치가 다시 올 수 있고, 저장소가 이 ID로 두 번 세지 않는다.
type Batch struct {
	ID     string           `json:"id"`
	Counts map[string]int64 `json:"counts"`
}

// Delivery는 큐에서 받은 메시지 하나다.
type Delivery struct {
	Batch Batch
	// Handle은 큐 구현이 Ack에 쓰는 값이다(SQS의 ReceiptHandle).
	Handle string
	// Err가 nil이 아니면 본문을 해석하지 못한 메시지다. Ack하지 않는다 → 재전달 끝에 DLQ로 간다.
	Err error
}

// Queue는 클릭 배치를 나르는 큐다.
type Queue interface {
	// Publish는 배치 하나를 큐에 넣는다.
	Publish(ctx context.Context, b Batch) error
	// Receive는 메시지를 최대 maxReceive개 받는다. 없으면 기다리다(롱폴링) 빈 결과를 돌려준다.
	// ctx가 끝나면 바로 돌아온다.
	Receive(ctx context.Context) ([]Delivery, error)
	// Ack는 반영을 마친 메시지를 큐에서 지운다.
	Ack(ctx context.Context, d Delivery) error
}

const (
	// maxCodesPerBatch는 메시지 하나에 담는 코드 수 상한이다. SQS 본문 한도(256KB) 안쪽으로 둔다 —
	// 코드 32자 + 개수 19자리 + JSON 구분자로 잡아도 1,000개면 약 60KB다.
	maxCodesPerBatch = 1000
	// maxReceive는 한 번에 받는 메시지 수다(SQS ReceiveMessage의 상한).
	maxReceive = 10
)

// newBatchID는 배치 ID로 쓸 16바이트 난수(16진수 32자)를 만든다. 태스크가 여럿이어도 겹치지 않는다.
func newBatchID() string {
	var b [16]byte
	// crypto/rand.Read는 실패하지 않는다(Go 1.24부터 실패하면 프로세스를 멈춘다).
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
