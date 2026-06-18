package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// writeJSON은 v를 JSON으로 직렬화해 주어진 status 코드와 함께 응답한다.
// 먼저 버퍼로 직렬화한 뒤 헤더·본문을 쓴다(marshal-then-write). 이렇게 하면
// 직렬화가 실패해도 상태코드를 정확히 500으로 보낼 수 있고, 깨진 부분 응답을 흘리지 않는다.
// 모든 핸들러가 이 헬퍼를 사용해 응답 형식을 일관되게 유지한다.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if v == nil {
		w.WriteHeader(status)
		return
	}

	data, err := json.Marshal(v)
	if err != nil {
		slog.Error("응답 직렬화 실패", "error", err)
		// 마지막 보루 경로라 json.Marshal에 다시 기대지 않고 고정 리터럴로 응답한다.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"내부 서버 오류"}}`))
		return
	}

	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// errorResponse는 일관된 에러 응답 형식이다: {"error":{"code","message"}}.
type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`    // 기계가 분기할 수 있는 짧은 식별자
	Message string `json:"message"` // 사람이 읽는 설명
}

// writeError는 일관된 에러 응답을 보낸다.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: errorDetail{Code: code, Message: message}})
}
