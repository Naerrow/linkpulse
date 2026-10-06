package dbcreds

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ⑩ task_id: 메타데이터 정상 응답에서 ARN 마지막 세그먼트를 뽑는다.
func TestTaskIDFromMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/task" {
			t.Errorf("경로가 %q — TaskARN은 /task 응답의 계약이다", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"Cluster":"linkpulse-test-cluster","TaskARN":"arn:aws:ecs:ap-northeast-2:123456789012:task/linkpulse-test-cluster/abc123def456"}`))
	}))
	defer srv.Close()

	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", srv.URL)
	if got := TaskID(context.Background()); got != "abc123def456" {
		t.Errorf("task_id %q — abc123def456여야 한다", got)
	}
}

// 실패 경로는 전부 unknown으로 떨어지고 기동을 막지 않는다.
func TestTaskIDFailuresFallBackToUnknown(t *testing.T) {
	t.Run("환경변수 없음(로컬)", func(t *testing.T) {
		t.Setenv("ECS_CONTAINER_METADATA_URI_V4", "")
		if got := TaskID(context.Background()); got != TaskIDUnknown {
			t.Errorf("task_id %q — unknown이어야 한다", got)
		}
	})

	t.Run("5xx 응답", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		t.Setenv("ECS_CONTAINER_METADATA_URI_V4", srv.URL)
		if got := TaskID(context.Background()); got != TaskIDUnknown {
			t.Errorf("task_id %q — unknown이어야 한다", got)
		}
	})

	t.Run("깨진 JSON", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"TaskARN":`))
		}))
		defer srv.Close()
		t.Setenv("ECS_CONTAINER_METADATA_URI_V4", srv.URL)
		if got := TaskID(context.Background()); got != TaskIDUnknown {
			t.Errorf("task_id %q — unknown이어야 한다", got)
		}
	})

	t.Run("TaskARN 필드 없음", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"Cluster":"x"}`))
		}))
		defer srv.Close()
		t.Setenv("ECS_CONTAINER_METADATA_URI_V4", srv.URL)
		if got := TaskID(context.Background()); got != TaskIDUnknown {
			t.Errorf("task_id %q — unknown이어야 한다", got)
		}
	})

	t.Run("도달 불가", func(t *testing.T) {
		t.Setenv("ECS_CONTAINER_METADATA_URI_V4", "http://127.0.0.1:1")
		if got := TaskID(context.Background()); got != TaskIDUnknown {
			t.Errorf("task_id %q — unknown이어야 한다", got)
		}
	})
}
