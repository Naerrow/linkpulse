package links

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// PostgresRepository는 Repository의 Postgres 구현이다.
// MemoryRepository와 같은 인터페이스를 만족하므로 서비스/핸들러는 그대로 재사용된다.
type PostgresRepository struct {
	db *sql.DB
}

// NewPostgresRepository는 연결 풀을 주입받아 저장소를 만든다.
func NewPostgresRepository(db *sql.DB) *PostgresRepository {
	return &PostgresRepository{db: db}
}

// Create는 코드/URL로 링크를 저장하고 저장된 행(생성시각 포함)을 반환한다.
// code가 PK라 충돌하면 UNIQUE 위반(23505)이 나고, 이를 ErrCodeExists로 바꾼다.
func (r *PostgresRepository) Create(ctx context.Context, code, destURL string) (Link, error) {
	const q = `INSERT INTO links (code, url) VALUES ($1, $2)
	           RETURNING code, url, clicks, created_at`

	var l Link
	err := r.db.QueryRowContext(ctx, q, code, destURL).Scan(&l.Code, &l.URL, &l.Clicks, &l.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return Link{}, ErrCodeExists
		}
		return Link{}, err
	}
	return l, nil
}

// linkColumns는 links를 Link로 읽을 때의 열 순서다(scanLink와 짝).
const linkColumns = `code, url, clicks, created_at, disabled_at`

// scanLink는 linkColumns 순서의 한 행을 읽는다.
func scanLink(s rowScanner) (Link, error) {
	var (
		l        Link
		disabled sql.NullTime
	)
	if err := s.Scan(&l.Code, &l.URL, &l.Clicks, &l.CreatedAt, &disabled); err != nil {
		return Link{}, err
	}
	if disabled.Valid {
		l.DisabledAt = disabled.Time
	}
	return l, nil
}

// Get은 코드로 링크를 조회한다. 없으면 ErrNotFound.
func (r *PostgresRepository) Get(ctx context.Context, code string) (Link, error) {
	l, err := scanLink(r.db.QueryRowContext(ctx, `SELECT `+linkColumns+` FROM links WHERE code = $1`, code))
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	return l, err
}

// SetDisabled는 중단 시각을 기록하거나 지운다. 이미 중단된 링크를 다시 중단해도 처음 시각을 유지한다.
func (r *PostgresRepository) SetDisabled(ctx context.Context, code string, disabled bool) (Link, error) {
	q := `UPDATE links SET disabled_at = NULL WHERE code = $1 RETURNING ` + linkColumns
	if disabled {
		q = `UPDATE links SET disabled_at = COALESCE(disabled_at, now()) WHERE code = $1 RETURNING ` + linkColumns
	}
	l, err := scanLink(r.db.QueryRowContext(ctx, q, code))
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	return l, err
}

// ListRecent는 링크를 최근 생성순으로 최대 limit건 돌려준다.
func (r *PostgresRepository) ListRecent(ctx context.Context, limit int) ([]Link, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+linkColumns+` FROM links ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Link, 0)
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ApplyClickBatch는 배치 ID 기록과 코드별 UPDATE를 한 트랜잭션으로 한다(plan 0013).
//
// 중복: 배치 ID를 click_batches에 먼저 넣는다. 이미 있으면(0행) 이전에 반영한 배치라 아무것도 바꾸지 않는다.
// 같은 배치가 두 워커에 동시에 와도 두 번째 INSERT는 첫 트랜잭션이 끝날 때까지 기다렸다가 0행이 된다.
//
// 교착: 두 워커가 겹치는 코드를 서로 다른 순서로 잠그면 교착이 난다. counts는 map이라 순서가 무작위이므로
// 코드를 정렬한 순서로 UPDATE해 모든 트랜잭션이 같은 순서로 행 잠금을 잡게 한다.
func (r *PostgresRepository) ApplyClickBatch(ctx context.Context, batchID string, counts map[string]int64) (bool, int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	// 커밋한 뒤의 Rollback은 sql.ErrTxDone을 돌려줄 뿐 아무것도 하지 않는다.
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `INSERT INTO click_batches (id) VALUES ($1) ON CONFLICT DO NOTHING`, batchID)
	if err != nil {
		return false, 0, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, 0, err
	}
	if inserted == 0 {
		return true, 0, nil
	}

	codes := make([]string, 0, len(counts))
	for code := range counts {
		codes = append(codes, code)
	}
	sort.Strings(codes)

	var unknown int64
	for _, code := range codes {
		res, err := tx.ExecContext(ctx, `UPDATE links SET clicks = clicks + $2 WHERE code = $1`, code, counts[code])
		if err != nil {
			return false, 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, 0, err
		}
		if n == 0 {
			unknown += counts[code]
		}
	}
	if err := tx.Commit(); err != nil {
		return false, 0, err
	}
	return false, unknown, nil
}

// PruneClickBatches는 olderThan보다 오래 전에 반영한 배치 ID를 지운다.
func (r *PostgresRepository) PruneClickBatches(ctx context.Context, olderThan time.Duration) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM click_batches WHERE applied_at < now() - ($1 * interval '1 second')`, int64(olderThan.Seconds()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// isUniqueViolation은 에러가 Postgres UNIQUE 제약 위반(SQLSTATE 23505)인지 확인한다.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
