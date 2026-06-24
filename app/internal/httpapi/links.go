package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Naerrow/linkpulse/app/internal/links"
)

// maxBodyBytes는 단축 요청 본문의 상한이다. URL 한 건을 받기에 충분하면서
// 과도하게 큰 본문으로 메모리를 소모시키는 공격을 막는다.
const maxBodyBytes = 8 << 10 // 8KiB

// linkHandler는 링크 관련 HTTP 핸들러 묶음으로, 서비스를 주입받는다(컨트롤러 레이어).
type linkHandler struct {
	svc     *links.Service
	baseURL string // 단축 URL을 구성할 외부 기준 주소 (예: https://lnk.example.com)
}

// createLinkRequest는 단축 생성 요청 본문(DTO)이다.
type createLinkRequest struct {
	URL string `json:"url"`
}

// linkResponse는 링크 응답 본문(DTO)이다. 도메인 모델을 그대로 노출하지 않는다.
type linkResponse struct {
	Code      string    `json:"code"`
	ShortURL  string    `json:"short_url"`
	URL       string    `json:"url"`
	Clicks    int64     `json:"clicks"`
	CreatedAt time.Time `json:"created_at"`
}

// create는 POST /api/links — 원본 URL을 받아 단축 링크를 만든다.
func (h *linkHandler) create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var req createLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "요청 본문을 JSON으로 해석할 수 없습니다")
		return
	}

	link, err := h.svc.Shorten(r.Context(), req.URL)
	if err != nil {
		if errors.Is(err, links.ErrInvalidURL) {
			writeError(w, http.StatusBadRequest, "invalid_url", "http 또는 https URL을 입력해 주세요")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "내부 서버 오류")
		return
	}
	writeJSON(w, http.StatusCreated, h.toResponse(link))
}

// redirect는 GET /{code} — 코드를 원본 URL로 리다이렉트하고 클릭을 집계한다.
func (h *linkHandler) redirect(w http.ResponseWriter, r *http.Request) {
	link, err := h.svc.Resolve(r.Context(), r.PathValue("code"))
	if err != nil {
		if errors.Is(err, links.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "존재하지 않는 단축 링크입니다")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "내부 서버 오류")
		return
	}
	// 302(임시)를 쓴다: 301은 브라우저가 영구 캐싱해 이후 요청이 서버에 도달하지 않고,
	// 그러면 클릭 집계가 누락된다. 분석이 서비스의 목적이므로 매 방문을 받는 302가 맞다.
	http.Redirect(w, r, link.URL, http.StatusFound)
}

// stats는 GET /api/links/{code} — 클릭 수를 포함한 링크 현황을 조회한다(집계 없음).
func (h *linkHandler) stats(w http.ResponseWriter, r *http.Request) {
	link, err := h.svc.Stats(r.Context(), r.PathValue("code"))
	if err != nil {
		if errors.Is(err, links.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "존재하지 않는 단축 링크입니다")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "내부 서버 오류")
		return
	}
	writeJSON(w, http.StatusOK, h.toResponse(link))
}

// toResponse는 도메인 Link를 응답 DTO로 변환한다.
func (h *linkHandler) toResponse(l links.Link) linkResponse {
	return linkResponse{
		Code:      l.Code,
		ShortURL:  h.baseURL + "/" + l.Code,
		URL:       l.URL,
		Clicks:    l.Clicks,
		CreatedAt: l.CreatedAt,
	}
}
