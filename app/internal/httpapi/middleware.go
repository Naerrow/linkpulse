package httpapi

import (
	"log/slog"
	"net/http"
	"time"
)

// withMiddleware는 공통 미들웨어로 핸들러를 감싼다.
// requestLogger가 상태코드 래퍼를 만들고, 그 안에서 recoverer가 panic을 잡아
// 같은 래퍼로 500을 기록하도록 requestLogger를 바깥에 둔다.
func withMiddleware(next http.Handler) http.Handler {
	return requestLogger(recoverer(next))
}

// requestLogger는 각 요청의 메서드·경로·상태코드·소요시간을 구조화 로깅한다.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// recoverer는 하위 핸들러의 panic을 잡아 500으로 응답하고 로깅한다.
// 한 요청의 오류가 프로세스 전체를 내리지 않게 한다(에러 핸들링 필수).
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered", "error", rec, "path", r.URL.Path)
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
