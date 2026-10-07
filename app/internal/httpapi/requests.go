package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Naerrow/linkpulse/app/internal/links"
)

// requestHandler는 링크 요청 HTTP 핸들러 묶음이다(컨트롤러 레이어, plan 0011).
type requestHandler struct {
	svc     *links.RequestService
	baseURL string
}

// submitRequestBody는 요청 접수 본문(DTO)이다.
type submitRequestBody struct {
	URL  string `json:"url"`
	Note string `json:"note"`
}

// requestResponse는 요청 응답 본문(DTO)이다.
type requestResponse struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	URL       string     `json:"url"`
	Note      string     `json:"note"`
	ShortURL  string     `json:"short_url,omitempty"` // 승인된 경우에만
	StatusURL string     `json:"status_url"`          // 방문자가 결과를 확인할 링크
	CreatedAt time.Time  `json:"created_at"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
}

// approveResponse는 승인 응답이다 — 갱신된 요청과 발급된 링크를 함께 준다.
type approveResponse struct {
	Request requestResponse `json:"request"`
	Link    linkResponse    `json:"link"`
}

// pendingListResponse는 관리자 대기 목록 응답이다.
type pendingListResponse struct {
	Requests []requestResponse `json:"requests"`
}

// submit은 POST /api/requests — 방문자가 단축 링크를 요청한다.
func (h *requestHandler) submit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var body submitRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "요청 본문을 JSON으로 해석할 수 없습니다")
		return
	}
	req, err := h.svc.Submit(r.Context(), body.URL, body.Note)
	if err != nil {
		h.writeRequestError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, h.toResponse(req))
}

// get은 GET /api/requests/{id} — 요청 확인 링크로 상태와 결과를 본다.
func (h *requestHandler) get(w http.ResponseWriter, r *http.Request) {
	req, err := h.svc.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeRequestError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h.toResponse(req))
}

// listPending은 GET /api/admin/requests — 관리자가 대기 목록을 본다.
func (h *requestHandler) listPending(w http.ResponseWriter, r *http.Request) {
	reqs, err := h.svc.ListPending(r.Context())
	if err != nil {
		writeInternalError(w, r, err)
		return
	}
	out := pendingListResponse{Requests: make([]requestResponse, 0, len(reqs))}
	for _, req := range reqs {
		out.Requests = append(out.Requests, h.toResponse(req))
	}
	writeJSON(w, http.StatusOK, out)
}

// approve는 POST /api/admin/requests/{id}/approve — 승인하고 단축 링크를 발급한다.
func (h *requestHandler) approve(w http.ResponseWriter, r *http.Request) {
	req, link, err := h.svc.Approve(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeRequestError(w, r, err)
		return
	}
	lh := linkHandler{baseURL: h.baseURL}
	writeJSON(w, http.StatusOK, approveResponse{Request: h.toResponse(req), Link: lh.toResponse(link)})
}

// reject는 POST /api/admin/requests/{id}/reject — 거절한다.
func (h *requestHandler) reject(w http.ResponseWriter, r *http.Request) {
	req, err := h.svc.Reject(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeRequestError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h.toResponse(req))
}

// writeRequestError는 요청 도메인 에러를 HTTP 상태코드로 바꾼다.
func (h *requestHandler) writeRequestError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, links.ErrInvalidURL):
		writeError(w, http.StatusBadRequest, "invalid_url", "http 또는 https URL을 입력해 주세요(2048자 이하)")
	case errors.Is(err, links.ErrNoteTooLong):
		writeError(w, http.StatusBadRequest, "note_too_long", "메모는 200자 이하로 입력해 주세요")
	case errors.Is(err, links.ErrTooManyPending):
		writeError(w, http.StatusTooManyRequests, "too_many_pending", "대기 중인 요청이 많습니다. 나중에 다시 시도해 주세요")
	case errors.Is(err, links.ErrRequestNotFound):
		writeError(w, http.StatusNotFound, "not_found", "존재하지 않는 요청입니다")
	case errors.Is(err, links.ErrRequestDecided):
		writeError(w, http.StatusConflict, "already_decided", "이미 처리된 요청입니다")
	default:
		writeInternalError(w, r, err)
	}
}

// toResponse는 도메인 LinkRequest를 응답 DTO로 바꾼다.
func (h *requestHandler) toResponse(req links.LinkRequest) requestResponse {
	out := requestResponse{
		ID:        req.ID,
		Status:    string(req.Status),
		URL:       req.URL,
		Note:      req.Note,
		StatusURL: h.baseURL + "/#r=" + req.ID,
		CreatedAt: req.CreatedAt,
	}
	if req.Code != "" {
		out.ShortURL = h.baseURL + "/" + req.Code
	}
	if !req.DecidedAt.IsZero() {
		decided := req.DecidedAt
		out.DecidedAt = &decided
	}
	return out
}
