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
		t.Setenv("POD_NAME", "")
		t.Setenv("HOSTNAME", "")
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

// 쿠버네티스(plan 0014): ECS 메타데이터가 없으면 POD_NAME, 그다음 HOSTNAME을 쓴다.
// ECS 메타데이터가 있으면 그것이 우선이다(파드 이름 env가 함께 있어도).
func TestTaskIDFallbackOrder(t *testing.T) {
	t.Run("POD_NAME이 HOSTNAME보다 먼저", func(t *testing.T) {
		t.Setenv("ECS_CONTAINER_METADATA_URI_V4", "")
		t.Setenv("POD_NAME", "linkpulse-7d9f-abcde")
		t.Setenv("HOSTNAME", "host-x")
		if got := TaskID(context.Background()); got != "linkpulse-7d9f-abcde" {
			t.Errorf("task_id %q — POD_NAME이어야 한다", got)
		}
	})

	t.Run("POD_NAME이 없으면 HOSTNAME", func(t *testing.T) {
		t.Setenv("ECS_CONTAINER_METADATA_URI_V4", "")
		t.Setenv("POD_NAME", "")
		t.Setenv("HOSTNAME", "linkpulse-7d9f-fghij")
		if got := TaskID(context.Background()); got != "linkpulse-7d9f-fghij" {
			t.Errorf("task_id %q — HOSTNAME이어야 한다", got)
		}
	})

	t.Run("ECS 메타데이터가 있으면 그것이 먼저", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"TaskARN":"arn:aws:ecs:ap-northeast-2:123456789012:task/c/ecs123"}`))
		}))
		defer srv.Close()
		t.Setenv("ECS_CONTAINER_METADATA_URI_V4", srv.URL)
		t.Setenv("POD_NAME", "pod-x")
		t.Setenv("HOSTNAME", "host-x")
		if got := TaskID(context.Background()); got != "ecs123" {
			t.Errorf("task_id %q — ECS 태스크 ID여야 한다", got)
		}
	})
}
