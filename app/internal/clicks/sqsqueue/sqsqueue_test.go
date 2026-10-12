package sqsqueue

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/Naerrow/linkpulse/app/internal/clicks"
)

const testURL = "https://sqs.ap-northeast-2.amazonaws.com/000000000000/linkpulse-test-clicks"

// fakeSQS는 호출 인자를 기록하고 정해 둔 메시지를 돌려주는 가짜 SQS다.
type fakeSQS struct {
	sent     []*sqs.SendMessageInput
	received []*sqs.ReceiveMessageInput
	deleted  []*sqs.DeleteMessageInput
	messages []types.Message
	block    bool // true면 ReceiveMessage가 ctx가 끝날 때까지 돌아오지 않는다(롱폴링 흉내)
}

func (f *fakeSQS) SendMessage(_ context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	f.sent = append(f.sent, in)
	return &sqs.SendMessageOutput{}, nil
}

func (f *fakeSQS) ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.received = append(f.received, in)
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &sqs.ReceiveMessageOutput{Messages: f.messages}, nil
}

func (f *fakeSQS) DeleteMessage(_ context.Context, in *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.deleted = append(f.deleted, in)
	return &sqs.DeleteMessageOutput{}, nil
}

func TestPublishRoundTrip(t *testing.T) {
	f := &fakeSQS{}
	q := &Queue{client: f, url: testURL}
	want := clicks.Batch{ID: "b1", Counts: map[string]int64{"abc1234": 3, "xyz9876": 1}}
	if err := q.Publish(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || aws.ToString(f.sent[0].QueueUrl) != testURL {
		t.Fatalf("SendMessage 호출 = %+v", f.sent)
	}
	got, err := decode(aws.ToString(f.sent[0].MessageBody))
	if err != nil || got.ID != want.ID || len(got.Counts) != 2 || got.Counts["abc1234"] != 3 {
		t.Errorf("왕복 = %+v, %v; want %+v", got, err, want)
	}
}

func TestReceiveDecodesAndKeepsBroken(t *testing.T) {
	good, _ := json.Marshal(clicks.Batch{ID: "b1", Counts: map[string]int64{"abc1234": 2}})
	f := &fakeSQS{messages: []types.Message{
		{Body: aws.String(string(good)), ReceiptHandle: aws.String("h-good")},
		{Body: aws.String("{깨진"), ReceiptHandle: aws.String("h-bad")},
	}}
	q := &Queue{client: f, url: testURL}

	ds, err := q.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	in := f.received[0]
	if aws.ToString(in.QueueUrl) != testURL || in.MaxNumberOfMessages != maxMessages || in.WaitTimeSeconds != waitSeconds {
		t.Errorf("ReceiveMessage 인자 = url %q, max %d, wait %d", aws.ToString(in.QueueUrl), in.MaxNumberOfMessages, in.WaitTimeSeconds)
	}
	if len(ds) != 2 {
		t.Fatalf("받은 수 %d, want 2(깨진 메시지도 넘겨 워커가 Ack하지 않게)", len(ds))
	}
	if ds[0].Err != nil || ds[0].Batch.ID != "b1" || ds[0].Handle != "h-good" {
		t.Errorf("정상 메시지 = %+v", ds[0])
	}
	if ds[1].Err == nil || ds[1].Handle != "h-bad" {
		t.Errorf("깨진 메시지 = %+v, want Err 있음", ds[1])
	}
}

// 롱폴링 중에 ctx가 끝나면 Receive는 바로 돌아온다(종료 시간 상한).
func TestReceiveStopsOnCancel(t *testing.T) {
	q := &Queue{client: &fakeSQS{block: true}, url: testURL}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := q.Receive(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("Receive가 %v 걸렸다", el)
	}
}

func TestAckDeletesByHandle(t *testing.T) {
	f := &fakeSQS{}
	q := &Queue{client: f, url: testURL}
	if err := q.Ack(context.Background(), clicks.Delivery{Handle: "h-1"}); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 1 || aws.ToString(f.deleted[0].ReceiptHandle) != "h-1" || aws.ToString(f.deleted[0].QueueUrl) != testURL {
		t.Errorf("DeleteMessage 호출 = %+v", f.deleted)
	}
}

func TestDecodeRejects(t *testing.T) {
	for _, body := range []string{
		``,
		`{}`,
		`{"id":"","counts":{"a":1}}`,
		`{"id":"b1","counts":{}}`,
		`{"id":"b1","counts":{"a":0}}`,
		`{"id":"b1","counts":{"a":-2}}`,
		`{"id":"b1","counts":{"":3}}`,
	} {
		if b, err := decode(body); err == nil {
			t.Errorf("decode(%q) = %+v, want 오류", body, b)
		}
	}
}
