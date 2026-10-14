package httpapi

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// withMiddleware는 공통 미들웨어로 핸들러를 감싼다.
// 체인: requestLogger(recoverer(rateLimit(next))).
//   - requestLogger를 최외곽에 둬 429를 포함한 모든 응답의 상태코드를 기록한다.
//   - recoverer가 rateLimit을 감싸 리미터 자체 버그의 panic도 500으로 복구한다.
//   - rateLimit이 최내곽이라 한도 초과 시 실제 핸들러 실행 전에 거부한다.
func withMiddleware(obs RequestObserver, rl *rateLimit, next http.Handler) http.Handler {
	return requestLogger(obs, recoverer(rl.middleware(next)))
}

// RequestObserver는 요청 하나가 끝날 때 경로 패턴·메서드·상태코드·소요시간을 받는다(plan 0015 지표).
// httpapi가 지표 라이브러리를 모르게 인터페이스로 받는다.
type RequestObserver interface {
	ObserveRequest(route, method string, status int, d time.Duration)
}

// requestLogger는 각 요청의 메서드·경로·상태코드·소요시간을 구조화 로깅하고, obs가 있으면 지표로도 넘긴다.
func requestLogger(obs RequestObserver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		d := time.Since(start)
		if obs != nil {
			obs.ObserveRequest(routeLabel(r), r.Method, sw.status, d)
		}
		slog.Info("http request",
			"method", r.Method,
			"path", logPath(r.URL.Path),
			"status", sw.status,
			"duration_ms", d.Milliseconds(),
		)
	})
}

// routeLabel은 지표의 route 라벨이다. 원시 경로가 아니라 ServeMux 패턴(예: "GET /{code}")을 쓴다 —
// 경로를 쓰면 코드마다 시계열이 생기고 요청 ID가 지표에 남는다(logPath와 같은 이유).
//
// 전제: ServeMux는 next 안에서 같은 *Request의 Pattern을 채운다. 그래서 바깥 미들웨어가 next 뒤에 읽을 수 있다.
// 체인 중간에서 요청을 복제하면(r.WithContext 등) 바깥에서는 빈 값만 보여 모두 "none"이 된다 —
// TestRouteLabelThroughMiddleware가 지킨다. '/'로 끝나는 하위 트리 패턴을 더하면 mux의 슬래시 리디렉트가
// 실제 경로를 Pattern에 넣으므로 그때 다시 본다(지금은 그런 패턴이 없다).
// 리미터(429)처럼 mux 앞에서 끝났거나 mux가 맞추지 못한 요청(404·405)은 패턴이 비어 "none"이다.
func routeLabel(r *http.Request) string {
	if r.Pattern == "" {
		return "none"
	}
	return r.Pattern
}

// recoverer는 하위 핸들러의 panic을 잡아 500으로 응답하고 로깅한다.
// 한 요청의 오류가 프로세스 전체를 내리지 않게 한다(에러 핸들링 필수).
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered", "error", rec, "path", logPath(r.URL.Path))
				writeError(w, http.StatusInternalServerError, "internal_error", "내부 서버 오류")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusWriter는 응답 상태코드를 가로채 로깅에 활용하기 위한 ResponseWriter 래퍼다.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// WriteHeader는 첫 호출의 상태코드만 기록하고 중복 호출을 무시한다.
func (w *statusWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

// logPath는 로그에 남길 경로를 만든다. 요청 ID는 요청 확인 링크의 열쇠라 로그에서 가린다(plan 0011).
//
//	/api/requests/<id>                 → /api/requests/{id}
//	/api/admin/requests/<id>/approve   → /api/admin/requests/{id}/approve
func logPath(p string) string {
	for _, prefix := range []string{"/api/requests/", "/api/admin/requests/"} {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok || rest == "" {
			continue
		}
		if _, action, found := strings.Cut(rest, "/"); found {
			return prefix + "{id}/" + action
		}
		return prefix + "{id}"
	}
	return p
}
