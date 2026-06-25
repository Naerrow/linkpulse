package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// readinessTimeout은 레디니스 점검(예: DB 핑)에 허용하는 최대 시간이다.
const readinessTimeout = 2 * time.Second

// statusResponse는 헬스/레디 체크의 응답 본문이다.
type statusResponse struct {
	Status string `json:"status"`
}

// handleHealthz는 라이브니스 체크다.
// 프로세스가 살아 요청을 처리할 수 있으면 200을 돌려준다(외부 의존성은 보지 않는다).
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// readinessHandler는 레디니스 체크다. 외부 의존성(DB 등) 점검 함수를 주입받는다.
type readinessHandler struct {
	check func(ctx context.Context) error // nil이면 항상 준비됨(외부 의존성 없음)
}

// handle은 트래픽을 받을 준비가 됐는지 응답한다.
// 점검 함수가 있으면 짧은 타임아웃 안에서 호출하고, 실패하면 503을 돌려줘
// 로드밸런서가 이 인스턴스로 트래픽을 보내지 않게 한다.
func (h *readinessHandler) handle(w http.ResponseWriter, r *http.Request) {
	if h.check != nil {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		if err := h.check(ctx); err != nil {
			slog.Warn("레디니스 점검 실패", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, statusResponse{Status: "unavailable"})
			return
		}
	}
	writeJSON(w, http.StatusOK, statusResponse{Status: "ready"})
}
