package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// readinessTimeout은 레디니스 점검(예: DB 핑)에 허용하는 최대 시간이다.
// Route53 헬스체크가 이 엔드포인트를 프로빙하므로(ADR 0004 §1-a) 2초가 아니라 1초로 잡는다.
// Route53은 연결 후 2초 안에 2xx/3xx를 받아야 healthy로 판정하는데, 상한도 2초면 앱이 503을
// 확정하는 순간과 Route53이 포기하는 순간이 겹친다 — Route53에는 타임아웃으로만 보인다.
// 아래 slog 경고 자체는 남지만, 실제 DB 에러 대신 취소·deadline 계열 오류(context canceled 등)로
// 대체돼 원인을 잃을 수 있다(ctx가 r.Context() 파생 — ALB가 끊김을 타깃까지 전파하는지는 미실증).
// 1초면 앱이 먼저 끊어 503이 Route53에 도달하고 로그에도 앱의 판단 사유가 남는다.
// 오탐을 막는 장치가 아니다: 정상 DB 핑은 수 ms라 상한이 1초든 2초든 결과가 같다.
// 이 값의 내용은 "DB 핑이 1초를 넘으면 준비되지 않은 것으로 본다"는 판단이다.
const readinessTimeout = 1 * time.Second

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
