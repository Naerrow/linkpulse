package dbcreds

import (
	"context"
	"database/sql/driver"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Connect 1회의 예산 (plan 0009 2-0).
//
// 최악 = dial 5 + refresh 대기 3 + dial 5 = 13초. 유한해야 하는 이유는 database/sql이
// 풀 보충용 Connect를 단일 connectionOpener에서 **직렬로** 부르기 때문이다 — 그 안에서
// 무한히 돌면 opener 자체를 점유해 장기 장애 때 연결 생성 경로가 막힌다.
// 이 설계가 주는 보장은 "무한 점유가 없다"까지다.
const (
	dialTimeout = 5 * time.Second
	// 초과해도 refresh는 detached로 계속되고 다음 Connect가 승계한다.
	// /readyz(1초 ctx) 경로에서는 dial #1이 끝나면 남는 시간이 수백 ms라 실질 0에 가깝다.
	refreshWait = 3 * time.Second
)

// authFailedSQLState는 Postgres의 invalid_password다. 이것만 비밀번호 문제로 본다.
const authFailedSQLState = "28P01"

// DialFunc는 주어진 비밀번호로 실제 연결을 시도한다.
// pgx를 이 패키지 밖에 두어 단위 테스트가 fake dial로 계약을 고정할 수 있게 한다.
type DialFunc func(ctx context.Context, password string) (driver.Conn, error)

// Connector는 driver.Connector를 감싸 인증 실패를 관찰하고 비밀번호를 갱신한다.
//
// pgx의 BeforeConnect 훅으로는 구현할 수 없다 — 훅은 연결 *전에* ConnConfig 사본을 고칠 뿐이고
// 인증 실패는 pgx.ConnectConfig가 곧장 반환한다(stdlib/sql.go:266-273 확인).
type Connector struct {
	provider *Provider
	dial     DialFunc
	drv      driver.Driver
}

// NewConnector는 provider와 dial 함수로 connector를 만든다.
func NewConnector(p *Provider, dial DialFunc, drv driver.Driver) *Connector {
	return &Connector{provider: p, dial: dial, drv: drv}
}

// Driver는 driver.Connector 인터페이스 요구사항이다.
func (c *Connector) Driver() driver.Driver { return c.drv }

// Connect는 현재 세대로 dial하고, 28P01이면 재조회 후 한 번 더 시도한다.
//
// 흐름 (plan 0009 2-0):
//
//  1. 현재 세대 비밀번호로 dial      timeout = min(ctx 잔여, 5초)
//  2. 성공 → 반환
//  3. 28P01 관찰 → refresh 요청(단일 비행, detached)
//  4. min(ctx 잔여, 3초)까지만 결과를 기다린다
//     값이 오면 → 새 값으로 dial 재시도 → 반환
//     안 오면   → 오류 반환 (refresh는 계속되고 다음 Connect가 승계한다)
//
// ctx가 죽으면 즉시 반환한다(driver.Connector 취소 계약). 그때도 refresh는 계속되고
// 결과는 기다리는 호출자가 하나도 없어도 provider 상태에 반영된다.
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	cred := c.provider.Current()

	conn, err := c.dialWithTimeout(ctx, cred.Password)
	if err == nil {
		c.provider.NoteRecovered(cred.Generation)
		return conn, nil
	}
	if !isAuthFailure(err) {
		// 네트워크·타임아웃은 비밀번호 문제가 아니다. 재조회하지 않는다.
		return nil, err
	}
	c.provider.NoteAuthFailure(cred.Generation)

	ch := c.provider.RequestRefresh()
	if ch == nil {
		// backoff 게이트가 닫혀 있거나 정적 모드다. 여기서 기다리면 opener를 점유한다.
		return nil, err
	}

	waitCtx, cancel := context.WithTimeout(ctx, refreshWait)
	defer cancel()

	select {
	case <-waitCtx.Done():
		return nil, err
	case res := <-ch:
		if res.Err != nil {
			return nil, err
		}
		fresh, ok := res.Val.(Credential)
		if !ok || fresh.Generation == cred.Generation {
			// 값이 그대로다 — 회전 중 창. 다음 Connect가 다시 시도한다.
			return nil, err
		}
		conn, dialErr := c.dialWithTimeout(ctx, fresh.Password)
		if dialErr != nil {
			return nil, dialErr
		}
		c.provider.NoteRecovered(fresh.Generation)
		return conn, nil
	}
}

// dialWithTimeout은 호출자 ctx와 dial 상한 중 짧은 쪽을 적용한다.
func (c *Connector) dialWithTimeout(ctx context.Context, password string) (driver.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	return c.dial(dialCtx, password)
}

// isAuthFailure는 오류가 Postgres 28P01인지 판별한다.
// 네트워크·타임아웃까지 재조회 대상으로 삼으면 회전과 무관한 장애에서 Secrets Manager를 두드린다.
func isAuthFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == authFailedSQLState
}
