// sqsqueue 패키지는 clicks.Queue의 SQS 구현이다(plan 0013).
// 앱에서 AWS SDK는 이 패키지(와 dbcreds)에만 들어간다 — 큐는 인터페이스 뒤에 둔다(AGENTS.md §5 자유 이동 원칙).
package sqsqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/Naerrow/linkpulse/app/internal/clicks"
)

const (
	// waitSeconds는 롱폴링 대기 시간이다(SQS 상한). 메시지가 없을 때 요청 수를 줄인다.
	waitSeconds = 20
	// maxMessages는 한 번에 받는 메시지 수다(SQS 상한).
	maxMessages = 10
)

// api는 이 패키지가 쓰는 SQS 호출만 모은 것이다. 테스트가 가짜로 바꿔 끼운다.
type api interface {
	SendMessage(ctx context.Context, in *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

// Queue는 SQS 표준 큐 하나다. 배치는 JSON 본문으로 오간다.
type Queue struct {
	client api
	url    string
}

var _ clicks.Queue = (*Queue)(nil)

// New는 큐 URL과 리전으로 SQS 큐를 만든다. 자격증명은 기본 체인(ECS 태스크 역할)에서 온다.
func New(ctx context.Context, queueURL, region string) (*Queue, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("AWS 설정 로드 실패: %w", err)
	}
	return &Queue{client: sqs.NewFromConfig(cfg), url: queueURL}, nil
}

// Publish는 배치를 JSON 메시지 하나로 보낸다.
func (q *Queue) Publish(ctx context.Context, b clicks.Batch) error {
	body, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_, err = q.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(q.url),
		MessageBody: aws.String(string(body)),
	})
	return err
}

// Receive는 롱폴링으로 최대 maxMessages개를 받는다. ctx가 끝나면 진행 중인 요청을 끊고 돌아온다.
// 본문을 해석하지 못한 메시지는 Delivery.Err에 담아 넘긴다 — 워커가 Ack하지 않아 재전달 끝에 DLQ로 간다.
func (q *Queue) Receive(ctx context.Context) ([]clicks.Delivery, error) {
	out, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(q.url),
		MaxNumberOfMessages: maxMessages,
		WaitTimeSeconds:     waitSeconds,
	})
	if err != nil {
		return nil, err
	}
	ds := make([]clicks.Delivery, 0, len(out.Messages))
	for _, m := range out.Messages {
		d := clicks.Delivery{Handle: aws.ToString(m.ReceiptHandle)}
		d.Batch, d.Err = decode(aws.ToString(m.Body))
		ds = append(ds, d)
	}
	return ds, nil
}

// Ack는 반영을 마친 메시지를 지운다.
func (q *Queue) Ack(ctx context.Context, d clicks.Delivery) error {
	_, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(q.url),
		ReceiptHandle: aws.String(d.Handle),
	})
	return err
}

// decode는 메시지 본문을 배치로 해석한다. ID가 없거나 개수가 0 이하인 배치는 깨진 것으로 본다.
func decode(body string) (clicks.Batch, error) {
	var b clicks.Batch
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		return clicks.Batch{}, fmt.Errorf("본문 해석 실패: %w", err)
	}
	if b.ID == "" || len(b.Counts) == 0 {
		return clicks.Batch{}, errors.New("배치 ID나 클릭이 비었다")
	}
	for code, n := range b.Counts {
		if code == "" || n <= 0 {
			return clicks.Batch{}, fmt.Errorf("잘못된 클릭 항목(코드 %q, 개수 %d)", code, n)
		}
	}
	return b, nil
}
