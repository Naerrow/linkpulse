package httpapi

import "net/http"

// statusResponse는 헬스/레디 체크의 응답 본문이다.
type statusResponse struct {
	Status string `json:"status"`
}

// handleHealthz는 라이브니스 체크다.
// 프로세스가 살아 요청을 처리할 수 있으면 200을 돌려준다(외부 의존성은 보지 않는다).
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// handleReadyz는 레디니스 체크다. 트래픽을 받을 준비가 되면 200을 돌려준다.
// 현재는 외부 의존성이 없어 항상 준비됨. DB 연결 확인은 DB 도입 시점에 연결한다.
func handleReadyz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, statusResponse{Status: "ready"})
}
