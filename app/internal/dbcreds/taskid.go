package dbcreds

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// task_id 조회 예산. 기동 경로라 짧게 끊고 실패하면 unknown으로 간다 —
// 이 값 때문에 기동이 늦어지거나 막히면 안 된다.
const (
	metadataTimeout  = 2 * time.Second
	metadataMaxBytes = 64 << 10
)

// TaskIDUnknown은 메타데이터를 못 읽었을 때 쓰는 값이다.
// 이 값이 상시 발생하면 2-6이 두 task_id를 대조할 수 없어 판정 불가가 된다.
const TaskIDUnknown = "unknown"

// TaskID는 ECS 컨테이너 메타데이터에서 태스크 ID를 읽어 모든 구조화 로그에 붙일 값을 만든다.
//
// TaskARN은 컨테이너 기본 응답이 아니라 /task 응답의 계약이므로 반드시 /task를 부른다.
// 로컬·비ECS 환경에서는 환경변수가 없어 곧장 unknown이 된다.
func TaskID(ctx context.Context) string {
	base := os.Getenv("ECS_CONTAINER_METADATA_URI_V4")
	if base == "" {
		return TaskIDUnknown
	}
	return taskIDFrom(ctx, strings.TrimRight(base, "/")+"/task")
}

func taskIDFrom(ctx context.Context, url string) string {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return TaskIDUnknown
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return TaskIDUnknown
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return TaskIDUnknown
	}

	var payload struct {
		TaskARN string `json:"TaskARN"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, metadataMaxBytes)).Decode(&payload); err != nil {
		return TaskIDUnknown
	}
	if idx := strings.LastIndex(payload.TaskARN, "/"); idx >= 0 && idx+1 < len(payload.TaskARN) {
		return payload.TaskARN[idx+1:]
	}
	return TaskIDUnknown
}
