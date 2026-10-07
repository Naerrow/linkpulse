package links

import (
	"context"
	"database/sql"
	"errors"
)

// requestColumns는 link_requests를 LinkRequest로 읽을 때의 열 순서다(scanRequest와 짝).
const requestColumns = `id, url, note, status, COALESCE(code, ''), created_at, decided_at`

// rowScanner는 *sql.Row와 *sql.Rows의 공통 부분이다.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRequest는 requestColumns 순서의 한 행을 읽는다.
func scanRequest(s rowScanner) (LinkRequest, error) {
	var (
		req     LinkRequest
		status  string
		decided sql.NullTime
	)
	if err := s.Scan(&req.ID, &req.URL, &req.Note, &status, &req.Code, &req.CreatedAt, &decided); err != nil {
		return LinkRequest{}, err
	}
	req.Status = RequestStatus(status)
	if decided.Valid {
		req.DecidedAt = decided.Time
	}
	return req, nil
}

// CreateRequest는 대기 중 요청이 maxPending 미만일 때만 한 문장으로 삽입한다.
// 동시에 들어온 요청 몇 건이 상한을 살짝 넘길 수 있다(READ COMMITTED). 스팸 상한이 목적이라 그 정도는 허용한다.
func (r *PostgresRepository) CreateRequest(ctx context.Context, id, destURL, note string, maxPending int) (LinkRequest, error) {
	const q = `INSERT INTO link_requests (id, url, note)
	           SELECT $1, $2, $3
	           WHERE (SELECT count(*) FROM link_requests WHERE status = 'pending') < $4
	           RETURNING ` + requestColumns

	req, err := scanRequest(r.db.QueryRowContext(ctx, q, id, destURL, note, maxPending))
	if errors.Is(err, sql.ErrNoRows) {
		return LinkRequest{}, ErrTooManyPending
	}
	return req, err
}

// GetRequest는 ID로 요청을 조회한다.
func (r *PostgresRepository) GetRequest(ctx context.Context, id string) (LinkRequest, error) {
	q := `SELECT ` + requestColumns + ` FROM link_requests WHERE id = $1`

	req, err := scanRequest(r.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return LinkRequest{}, ErrRequestNotFound
	}
	return req, err
}

// ListPending은 대기 중 요청을 최근순으로 돌려준다(link_requests_status_created 인덱스).
func (r *PostgresRepository) ListPending(ctx context.Context, limit int) ([]LinkRequest, error) {
	q := `SELECT ` + requestColumns + ` FROM link_requests
	      WHERE status = 'pending' ORDER BY created_at DESC LIMIT $1`

	rows, err := r.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]LinkRequest, 0)
	for rows.Next() {
		req, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	return out, rows.Err()
}

// Approve는 한 트랜잭션에서 요청 행을 잠그고, 링크를 만들고, 요청을 승인 상태로 바꾼다.
// FOR UPDATE로 같은 요청의 동시 승인을 줄 세운다 — 뒤에 온 쪽은 앞쪽 커밋 뒤의 상태(approved)를 보고 ErrRequestDecided를 받는다.
// 어느 단계에서 실패하든 롤백되므로 고아 링크가 남지 않는다.
func (r *PostgresRepository) Approve(ctx context.Context, id, code string) (LinkRequest, Link, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return LinkRequest{}, Link{}, err
	}
	// 커밋한 뒤의 Rollback은 sql.ErrTxDone을 돌려줄 뿐 아무것도 하지 않는다.
	defer func() { _ = tx.Rollback() }()

	var destURL, status string
	err = tx.QueryRowContext(ctx, `SELECT url, status FROM link_requests WHERE id = $1 FOR UPDATE`, id).Scan(&destURL, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return LinkRequest{}, Link{}, ErrRequestNotFound
	}
	if err != nil {
		return LinkRequest{}, Link{}, err
	}
	if RequestStatus(status) != StatusPending {
		return LinkRequest{}, Link{}, ErrRequestDecided
	}

	var link Link
	err = tx.QueryRowContext(ctx,
		`INSERT INTO links (code, url) VALUES ($1, $2) RETURNING code, url, clicks, created_at`,
		code, destURL).Scan(&link.Code, &link.URL, &link.Clicks, &link.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return LinkRequest{}, Link{}, ErrCodeExists
		}
		return LinkRequest{}, Link{}, err
	}

	req, err := scanRequest(tx.QueryRowContext(ctx,
		`UPDATE link_requests SET status = 'approved', code = $2, decided_at = now()
		 WHERE id = $1 RETURNING `+requestColumns, id, code))
	if err != nil {
		return LinkRequest{}, Link{}, err
	}
	if err := tx.Commit(); err != nil {
		return LinkRequest{}, Link{}, err
	}
	return req, link, nil
}

// Reject는 대기 중일 때만 거절 상태로 바꾼다. 0행이면 없는지 이미 결정됐는지 다시 본다.
func (r *PostgresRepository) Reject(ctx context.Context, id string) (LinkRequest, error) {
	req, err := scanRequest(r.db.QueryRowContext(ctx,
		`UPDATE link_requests SET status = 'rejected', decided_at = now()
		 WHERE id = $1 AND status = 'pending' RETURNING `+requestColumns, id))
	if err == nil {
		return req, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return LinkRequest{}, err
	}
	if _, getErr := r.GetRequest(ctx, id); getErr != nil {
		return LinkRequest{}, getErr // ErrRequestNotFound 또는 조회 오류
	}
	return LinkRequest{}, ErrRequestDecided
}
