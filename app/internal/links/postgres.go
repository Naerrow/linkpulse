package links

import (
	"context"
	"database/sql"
	"errors"

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

// Get은 코드로 링크를 조회한다. 없으면 ErrNotFound.
func (r *PostgresRepository) Get(ctx context.Context, code string) (Link, error) {
	const q = `SELECT code, url, clicks, created_at FROM links WHERE code = $1`

	var l Link
	err := r.db.QueryRowContext(ctx, q, code).Scan(&l.Code, &l.URL, &l.Clicks, &l.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, err
	}
	return l, nil
}

// IncrementClicks는 클릭 수를 1 늘린다. 해당 코드가 없으면 ErrNotFound.
func (r *PostgresRepository) IncrementClicks(ctx context.Context, code string) error {
	const q = `UPDATE links SET clicks = clicks + 1 WHERE code = $1`

	res, err := r.db.ExecContext(ctx, q, code)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// isUniqueViolation은 에러가 Postgres UNIQUE 제약 위반(SQLSTATE 23505)인지 확인한다.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
