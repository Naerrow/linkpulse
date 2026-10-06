package dbcreds

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	oldPassword = "old-secret-value"
	newPassword = "new-secret-value"
)

// authErr는 pgx가 **실제로** 돌려주는 형태다 — 맨 *pgconn.PgError가 아니라
// ConnectError → errors.Join(다중 호스트) → "server error: %w" 의 다단 래핑이다
// (pgconn/errors.go:62-95, pgconn/pgconn.go:163,325,535).
// 맨 오류만 쓰면 분류기가 아니라 비교문 한 줄을 시험하게 되고, pgx 업그레이드 한 번에
// Step 2의 진입 조건이 조용히 깨진다(round-1 claude-ide#3).
func authErr() error {
	return fmt.Errorf("failed to connect to `host=db user=app database=linkpulse`: %w",
		errors.Join(
			errors.New("dial tcp 10.0.0.9:5432: connect: connection refused"),
			fmt.Errorf("server error: %w", bareAuthErr()),
		))
}

// bareAuthErr는 래핑 없는 28P01이다. 분류기가 두 형태 모두를 받는지 가른다.
func bareAuthErr() *pgconn.PgError {
	return &pgconn.PgError{Code: authFailedSQLState, Message: "password authentication failed"}
}

type fakeConn struct{ driver.Conn }

// fakeDial은 정해진 비밀번호에만 성공하고 나머지는 28P01을 돌려준다.
type fakeDial struct {
	mu       sync.Mutex
	accepts  string
	attempts []string // 각 dial이 받은 비밀번호 (검증 ⑯)
	delay    time.Duration
	failAll  bool // refresh는 성공했는데 새 세대 dial이 실패하는 경로 (검증 ⑫)
	netErr   bool
	blockOn  string        // 이 비밀번호로 들어온 dial을 block이 닫힐 때까지 붙잡는다
	block    chan struct{} // 늦게 끝나는 옛 세대 dial을 결정적으로 만들기 위한 손잡이
}

func (f *fakeDial) fn(ctx context.Context, password string) (driver.Conn, error) {
	f.mu.Lock()
	f.attempts = append(f.attempts, password)
	delay, accepts, failAll, netErr := f.delay, f.accepts, f.failAll, f.netErr
	blockOn, block := f.blockOn, f.block
	f.mu.Unlock()

	if block != nil && password == blockOn {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if netErr {
		return nil, errors.New("dial tcp: connection refused")
	}
	if failAll || password != accepts {
		return nil, authErr()
	}
	return &fakeConn{}, nil
}

func (f *fakeDial) passwords() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.attempts...)
}

func (f *fakeDial) setAccepts(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accepts = p
}

// blockDial은 특정 비밀번호의 dial을 release가 닫힐 때까지 붙잡는다.
func (f *fakeDial) blockDial(password string, release chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockOn, f.block = password, release
}

// waitForAttempt는 그 비밀번호로 dial이 시작될 때까지 기다린다(순서 제어용).
func (f *fakeDial) waitForAttempt(t *testing.T, password string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, got := range f.passwords() {
			if got == password {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%q로 dial이 시도되지 않았다", password)
}

// fakeFetch는 호출 횟수를 세고 지정된 값을 돌려준다.
type fakeFetch struct {
	mu     sync.Mutex
	value  string
	err    error
	calls  int32
	delay  time.Duration
	notify chan struct{}
}

func (f *fakeFetch) fn(ctx context.Context) (string, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	value, err, delay, notify := f.value, f.err, f.delay, f.notify
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if notify != nil {
		select {
		case notify <- struct{}{}:
		default:
		}
	}
	return value, err
}

func (f *fakeFetch) count() int { return int(atomic.LoadInt32(&f.calls)) }

func (f *fakeFetch) set(value string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.value, f.err = value, err
}

// fakeClock은 backoff 계약을 실시간 없이 검증한다.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }
func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// logCapture는 구조화 로그를 JSON 줄로 모은다.
type logCapture struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func newLogCapture() (*logCapture, *slog.Logger) {
	c := &logCapture{buf: &bytes.Buffer{}, mu: &sync.Mutex{}}
	return c, slog.New(slog.NewJSONHandler(&syncWriter{c.buf, c.mu}, nil))
}

type syncWriter struct {
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (c *logCapture) events() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func (c *logCapture) countEvent(name string) int {
	n := 0
	for _, e := range c.events() {
		if e["event"] == name {
			n++
		}
	}
	return n
}

func (c *logCapture) raw() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// newTestSetup은 provider + connector + fake들을 한 번에 만든다.
// 기본은 "seed가 옛 값이고 DB는 새 값만 받는다" = 회전 직후 상태다.
func newTestSetup(t *testing.T, fetch FetchFunc, opts ...Option) (*Provider, *Connector, *fakeDial, *logCapture) {
	t.Helper()
	cap, logger := newLogCapture()
	p := New(oldPassword, fetch, append(opts, WithLogger(logger))...)
	t.Cleanup(p.Close)
	d := &fakeDial{accepts: newPassword}
	return p, NewConnector(p, d.fn, nil), d, cap
}

// ① 정상 경로: dial이 성공하면 SDK를 부르지 않는다.
func TestConnectSuccessDoesNotFetch(t *testing.T) {
	f := &fakeFetch{value: newPassword}
	p, c, d, _ := newTestSetup(t, f.fn)
	d.setAccepts(oldPassword) // seed가 이미 맞는 값이다

	if _, err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect 실패: %v", err)
	}
	if f.count() != 0 {
		t.Errorf("SDK 호출 %d회 — 캐시 히트면 0회여야 한다", f.count())
	}
	if got := p.Current().Generation; got != 1 {
		t.Errorf("세대 %d — 변하면 안 된다", got)
	}
}

// ② 28P01 후 재조회는 1회, 그리고 ⑯ 두 dial이 서로 다른 비밀번호를 받는다.
func TestAuthFailureRefreshesOnceAndRedials(t *testing.T) {
	f := &fakeFetch{value: newPassword}
	p, c, d, _ := newTestSetup(t, f.fn)

	if _, err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect가 새 값으로 회복해야 한다: %v", err)
	}
	if f.count() != 1 {
		t.Errorf("재조회 %d회 — 1회여야 한다", f.count())
	}
	got := d.passwords()
	if len(got) != 2 || got[0] != oldPassword || got[1] != newPassword {
		t.Errorf("dial 비밀번호 순서가 %v — [old new]여야 한다(⑯)", got)
	}
	if p.Current().Generation != 2 {
		t.Errorf("세대 %d — 값이 바뀌었으니 2여야 한다", p.Current().Generation)
	}
}

// ③ 동시 실패 다수여도 재조회는 1회다(singleflight).
func TestConcurrentFailuresFetchOnce(t *testing.T) {
	f := &fakeFetch{value: newPassword, delay: 30 * time.Millisecond}
	_, c, _, _ := newTestSetup(t, f.fn)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Connect(context.Background())
		}()
	}
	wg.Wait()
	if f.count() != 1 {
		t.Errorf("재조회 %d회 — singleflight면 1회여야 한다", f.count())
	}
}

// ④ 네트워크 오류는 비밀번호 문제가 아니므로 재조회하지 않는다.
func TestNetworkErrorDoesNotRefresh(t *testing.T) {
	f := &fakeFetch{value: newPassword}
	_, c, d, cap := newTestSetup(t, f.fn)
	d.mu.Lock()
	d.netErr = true
	d.mu.Unlock()

	if _, err := c.Connect(context.Background()); err == nil {
		t.Fatal("네트워크 오류는 그대로 반환돼야 한다")
	}
	if f.count() != 0 {
		t.Errorf("재조회 %d회 — 0회여야 한다", f.count())
	}
	if cap.countEvent("auth_failed_observed") != 0 {
		t.Error("네트워크 오류에 auth_failed_observed가 나면 안 된다")
	}
}

// ⑤ 늦게 도착한 옛 세대의 28P01은 재조회를 아예 시작하지 않는다.
//
// 세대가 이미 전진했다면 조회로 얻을 것은 없고 위험만 있다 — 비단조적 응답이 옛 값을
// 새 세대로 올려 복구를 되돌리거나, 값 동일 판정이 게이트를 닫아 바로 뒤의 정상 refresh를
// 늦춘다(round-1 codex-cli#1 / codex-ide#1).
func TestStaleGenerationFailureDoesNotRefetch(t *testing.T) {
	f := &fakeFetch{value: newPassword}
	p, c, _, _ := newTestSetup(t, f.fn)

	if _, err := c.Connect(context.Background()); err != nil {
		t.Fatalf("첫 회복 실패: %v", err)
	}
	if got := p.Current().Generation; got != 2 {
		t.Fatalf("세대 %d — 회복했으면 2여야 한다", got)
	}

	// 세대 1을 들고 뒤늦게 실패한 호출을 그대로 재현한다.
	if ch := p.RequestRefresh(1); ch != nil {
		t.Error("낡은 세대의 실패가 재조회를 시작했다")
	}
	if f.count() != 1 {
		t.Errorf("조회 %d회 — 1회여야 한다", f.count())
	}
}

// ⑤-b 늦게 끝난 옛 세대 dial이 실패하면, 조회 없이 **현재 값**으로 재시도해 성공한다.
// 두 dial을 채널로 순서 제어한다 — 순차 호출로는 이 경로가 재현되지 않는다.
func TestLateFailureRedialsWithCurrentValue(t *testing.T) {
	f := &fakeFetch{value: newPassword}
	p, c, d, _ := newTestSetup(t, f.fn)

	release := make(chan struct{})
	d.blockDial(oldPassword, release)

	done := make(chan error, 1)
	go func() {
		_, err := c.Connect(context.Background())
		done <- err
	}()
	d.waitForAttempt(t, oldPassword) // 이 호출은 세대 1(옛 값)을 들고 dial에 들어가 있다

	// 그동안 다른 연결이 먼저 회복해 세대가 2로 올라간다.
	res := <-p.RequestRefresh(1)
	if res.Err != nil {
		t.Fatalf("refresh 실패: %v", res.Err)
	}
	if got := p.Current().Generation; got != 2 {
		t.Fatalf("세대 %d — 2여야 한다", got)
	}

	close(release) // 붙잡아 둔 dial이 이제 28P01로 끝난다
	if err := <-done; err != nil {
		t.Fatalf("낡은 실패는 현재 값으로 재시도해 성공해야 한다: %v", err)
	}
	if f.count() != 1 {
		t.Errorf("조회 %d회 — 낡은 실패가 새 조회를 만들면 안 된다", f.count())
	}
	if p.Current().Password != newPassword {
		t.Error("비밀번호가 옛 값으로 되돌아갔다")
	}
}

// 게이트가 닫힌 뒤 비행에 들어온 호출은 조회하지 않는다.
// RequestRefresh의 게이트 확인과 DoChan 등록 사이에 먼저 끝난 비행이 게이트를 닫는 경로이고
// (round-1 codex-cli#2), 비행 함수를 직접 불러 그 틈을 결정적으로 재현한다.
func TestClosedGateInsideFlightSkipsFetch(t *testing.T) {
	clock := newFakeClock()
	f := &fakeFetch{value: oldPassword} // 값 동일 → 게이트가 닫힌다
	p, c, _, _ := newTestSetup(t, f.fn, WithClock(clock.now), WithJitter(func(x float64) float64 { return x }))

	_, _ = c.Connect(context.Background())
	if f.count() != 1 {
		t.Fatalf("조회 %d회 — 1회여야 한다", f.count())
	}

	if _, err := p.doRefresh(p.Current().Generation); err != nil {
		t.Fatalf("예상치 못한 오류: %v", err)
	}
	if f.count() != 1 {
		t.Errorf("조회 %d회 — 닫힌 게이트에서는 비행 안에서도 조회하지 않아야 한다", f.count())
	}
}

// ⑬ 분류기는 다단 래핑에서도 28P01을 찾고, 그렇지 않은 다중 오류는 걸러낸다.
func TestIsAuthFailureAcrossErrorShapes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "맨 PgError", err: bareAuthErr(), want: true},
		{name: "production 다단 래핑", err: authErr(), want: true},
		{name: "다른 SQLSTATE", err: fmt.Errorf("server error: %w",
			&pgconn.PgError{Code: "57P03", Message: "the database system is starting up"}), want: false},
		{name: "래핑된 네트워크 오류", err: fmt.Errorf("failed to connect: %w",
			errors.Join(errors.New("i/o timeout"), errors.New("connection refused"))), want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isAuthFailure(c.err); got != c.want {
				t.Errorf("isAuthFailure=%v — %v여야 한다", got, c.want)
			}
		})
	}
}

// ⑥ 회전 중 창: 재조회 결과가 옛 값과 같으면 세대 불변 + backoff 증가.
func TestSameValueKeepsGenerationAndBacksOff(t *testing.T) {
	clock := newFakeClock()
	f := &fakeFetch{value: oldPassword} // AWSCURRENT가 아직 옛 값이다
	p, c, _, cap := newTestSetup(t, f.fn, WithClock(clock.now), WithJitter(func(f float64) float64 { return f }))

	if _, err := c.Connect(context.Background()); err == nil {
		t.Fatal("회전 중 창에서는 회복할 수 없다")
	}
	if p.Current().Generation != 1 {
		t.Errorf("세대 %d — 값이 같으면 올라가면 안 된다", p.Current().Generation)
	}
	if cap.countEvent("secret_refreshed") != 0 {
		t.Error("값이 같은 refresh는 세대 로그를 남기면 안 된다")
	}
	// 게이트가 닫혀 있으므로 곧바로 다시 부르면 재조회하지 않는다(폭주 방지).
	before := f.count()
	_, _ = c.Connect(context.Background())
	if f.count() != before {
		t.Errorf("backoff 게이트가 닫힌 동안 재조회가 %d → %d로 늘었다", before, f.count())
	}
	// provider 수준의 "포기 없음": 시간이 지나면 다시 시도한다.
	clock.advance(maxBackoff)
	_, _ = c.Connect(context.Background())
	if f.count() <= before {
		t.Error("게이트가 열린 뒤에는 다시 조회해야 한다(provider 수준 포기 없음)")
	}
}

// ⑨ backoff 계약: 옛 값 반복 → 상한 도달 → 새 값 노출 → 회복 → reset.
func TestBackoffGrowsCapsAndResets(t *testing.T) {
	clock := newFakeClock()
	f := &fakeFetch{value: oldPassword}
	p, c, _, _ := newTestSetup(t, f.fn, WithClock(clock.now), WithJitter(func(f float64) float64 { return f }))

	for i := 0; i < 8; i++ {
		_, _ = c.Connect(context.Background())
		clock.advance(maxBackoff) // 매번 게이트를 열어 준다
	}
	p.mu.Lock()
	interval := p.interval
	p.mu.Unlock()
	if interval != maxBackoff {
		t.Errorf("backoff 간격이 %v — 상한 %v에서 멈춰야 한다", interval, maxBackoff)
	}

	f.set(newPassword, nil)
	if _, err := c.Connect(context.Background()); err != nil {
		t.Fatalf("새 값이 보이면 회복해야 한다: %v", err)
	}
	p.mu.Lock()
	interval, gate := p.interval, p.gateOpen
	p.mu.Unlock()
	if interval != initialBackoff {
		t.Errorf("성공 후 backoff가 %v — 초기값 %v로 reset돼야 한다", interval, initialBackoff)
	}
	if !gate.IsZero() {
		t.Error("성공 후 게이트가 열려 있어야 한다")
	}
}

// ⑧ Connect 1회는 유한하게 끝나고, 호출자 ctx가 죽으면 즉시 반환한다.
func TestConnectReturnsPromptlyOnCallerCancel(t *testing.T) {
	f := &fakeFetch{value: newPassword, delay: 5 * time.Second} // refresh가 느리다
	_, c, _, _ := newTestSetup(t, f.fn)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := c.Connect(ctx); err == nil {
		t.Fatal("취소된 호출은 오류로 끝나야 한다")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("%v 걸렸다 — 호출자 ctx가 죽으면 즉시 반환해야 한다(opener 점유 금지)", elapsed)
	}
}

// ⑪ detached 승계: 호출자가 취소돼도 refresh는 끝나고 다음 Connect가 성공한다.
// 취소는 첫 dial이 28P01을 반환한 *뒤*에 일어나야 이 경로가 검증된다.
func TestRefreshSurvivesCallerCancellation(t *testing.T) {
	done := make(chan struct{}, 1)
	f := &fakeFetch{value: newPassword, delay: 200 * time.Millisecond, notify: done}
	p, c, _, _ := newTestSetup(t, f.fn)

	// 첫 dial(즉시 28P01) 뒤 refresh 대기 중에 죽도록 짧게 준다.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Connect(ctx); err == nil {
		t.Fatal("취소된 호출은 오류로 끝나야 한다")
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("detached refresh가 완료되지 않았다")
	}
	// 상태 반영까지 잠깐 기다린다(refresh는 fetch 반환 뒤 상태를 쓴다).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.Current().Generation < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if p.Current().Generation != 2 {
		t.Fatalf("세대 %d — detached refresh 결과가 반영돼야 한다", p.Current().Generation)
	}
	if _, err := c.Connect(context.Background()); err != nil {
		t.Errorf("다음 Connect가 승계해 성공해야 한다: %v", err)
	}
}

// ⑫ credential_recovered는 refresh 성공과 분리된다.
// refresh는 성공했는데 새 세대 dial이 실패하면 refresh 로그만 나와야 한다.
func TestRecoveredLogSeparateFromRefresh(t *testing.T) {
	f := &fakeFetch{value: newPassword}
	p, c, d, cap := newTestSetup(t, f.fn)
	d.mu.Lock()
	d.failAll = true // 어떤 비밀번호로도 dial이 실패한다
	d.mu.Unlock()

	_, _ = c.Connect(context.Background())

	if cap.countEvent("secret_refreshed") != 1 {
		t.Errorf("refresh 성공 로그 %d회 — 1회여야 한다", cap.countEvent("secret_refreshed"))
	}
	if cap.countEvent("credential_recovered") != 0 {
		t.Error("dial이 실패했는데 credential_recovered가 나면 2-6이 false positive가 된다")
	}

	// 이제 새 세대가 통하게 만들면 딱 한 번 난다.
	d.mu.Lock()
	d.failAll = false
	d.mu.Unlock()
	if _, err := c.Connect(context.Background()); err != nil {
		t.Fatalf("회복해야 한다: %v", err)
	}
	if cap.countEvent("credential_recovered") != 1 {
		t.Errorf("credential_recovered %d회 — 세대당 1회여야 한다", cap.countEvent("credential_recovered"))
	}
	// 같은 세대로 또 연결해도 다시 나지 않는다.
	_, _ = c.Connect(context.Background())
	if cap.countEvent("credential_recovered") != 1 {
		t.Error("같은 세대에서 credential_recovered가 반복됐다")
	}
	_ = p
}

// ⑮ auth_failed_observed는 세대당 1회, 세대가 바뀌면 다시 난다.
func TestAuthFailedObservedOncePerGeneration(t *testing.T) {
	f := &fakeFetch{value: oldPassword} // 값이 안 바뀌어 세대가 유지된다
	clock := newFakeClock()
	_, c, _, cap := newTestSetup(t, f.fn, WithClock(clock.now), WithJitter(func(f float64) float64 { return f }))

	for i := 0; i < 5; i++ {
		_, _ = c.Connect(context.Background())
		clock.advance(maxBackoff)
	}
	if n := cap.countEvent("auth_failed_observed"); n != 1 {
		t.Errorf("auth_failed_observed %d회 — 같은 세대에서는 1회여야 한다(폭주 방지)", n)
	}

	f.set(newPassword, nil)
	_, _ = c.Connect(context.Background()) // 세대 2로 전이
	// 세대 2에서 다시 실패시킨다.
	_, c2, d2, cap2 := newTestSetup(t, f.fn)
	_ = d2
	_, _ = c2.Connect(context.Background())
	if cap2.countEvent("auth_failed_observed") < 1 {
		t.Error("새 세대에서는 auth_failed_observed가 다시 나야 한다")
	}
}

// ⑭ Close() 뒤에는 새 refresh가 시작되지 않는다.
func TestCloseStopsRefresh(t *testing.T) {
	f := &fakeFetch{value: newPassword}
	p, c, _, _ := newTestSetup(t, f.fn)
	p.Close()

	if _, err := c.Connect(context.Background()); err == nil {
		t.Fatal("Close 뒤에는 회복할 수 없다")
	}
	if f.count() != 0 {
		t.Errorf("Close 뒤 재조회 %d회 — 0회여야 한다", f.count())
	}
	if p.RequestRefresh(p.Current().Generation) != nil {
		t.Error("Close 뒤 RequestRefresh는 nil이어야 한다")
	}
}

// ⑦ 장시간 동일 값·SDK 장애에서 goroutine 누수가 없다.
func TestNoGoroutineLeakUnderRepeatedFailure(t *testing.T) {
	f := &fakeFetch{value: "", err: errors.New("SDK 장애")}
	clock := newFakeClock()
	p, c, _, _ := newTestSetup(t, f.fn, WithClock(clock.now), WithJitter(func(f float64) float64 { return f }))

	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		_, _ = c.Connect(context.Background())
		clock.advance(maxBackoff)
	}
	p.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > before+2 {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Errorf("goroutine %d → %d — 누수가 의심된다", before, after)
	}
}

// 정적 모드(로컬)에서는 SDK를 부르지 않고 refresh도 하지 않는다.
func TestStaticModeNeverRefreshes(t *testing.T) {
	p := New(oldPassword, nil)
	t.Cleanup(p.Close)
	if p.Mode() != "static" {
		t.Errorf("Mode()=%q — static이어야 한다", p.Mode())
	}
	if p.RequestRefresh(p.Current().Generation) != nil {
		t.Error("정적 모드에서는 refresh 채널이 없어야 한다")
	}
}

// 로그에 비밀번호가 절대 들어가지 않는다.
func TestLogsNeverContainPassword(t *testing.T) {
	f := &fakeFetch{value: newPassword}
	_, c, _, cap := newTestSetup(t, f.fn)
	_, _ = c.Connect(context.Background())
	_, _ = c.Connect(context.Background())

	raw := cap.raw()
	for _, secret := range []string{oldPassword, newPassword} {
		if strings.Contains(raw, secret) {
			t.Fatalf("로그에 비밀번호가 들어갔다: %s", raw)
		}
	}
}

// ---- 2-3 폴백 계약: 두 경우는 결과가 다르다 ----

// (a) 기동 시 SDK 장애 — ECS가 주입한 seed는 그 시점의 AWSCURRENT라 유효하다.
// SDK가 죽어 있어도 정상 기동·정상 연결이어야 하고, 애초에 조회하지도 않는다.
func TestFallbackSeedWorksWhenSDKIsDown(t *testing.T) {
	f := &fakeFetch{err: errors.New("SDK 장애")}
	p, c, d, cap := newTestSetup(t, f.fn)
	d.setAccepts(oldPassword) // seed가 아직 유효하다(회전 전)

	if _, err := c.Connect(context.Background()); err != nil {
		t.Fatalf("seed로 정상 연결돼야 한다: %v", err)
	}
	if f.count() != 0 {
		t.Errorf("SDK 호출 %d회 — 28P01이 없으면 조회하지 않는다", f.count())
	}
	if cap.countEvent("auth_failed_observed") != 0 {
		t.Error("인증 실패가 없었는데 T_fail 로그가 났다")
	}
	if p.Current().Generation != 1 {
		t.Error("seed 세대가 유지돼야 한다")
	}
}

// (b) 회전 후 refresh 장애 — 실행 중 태스크의 env는 이미 옛 비밀번호다.
// 폴백으로 복구되지 않고 장애가 지속된다. 그 사실이 구조화 로그로 드러나야 한다
// (이 구간의 복구는 Step 1의 자동 재배포가 맡는다).
func TestFallbackCannotRecoverAfterRotationWhenSDKIsDown(t *testing.T) {
	clock := newFakeClock()
	f := &fakeFetch{err: errors.New("SDK 장애")}
	p, c, _, cap := newTestSetup(t, f.fn, WithClock(clock.now), WithJitter(func(x float64) float64 { return x }))

	// DB는 이미 새 비밀번호이고(fakeDial 기본값), seed는 옛 값이다 = 회전 직후.
	for i := 0; i < 3; i++ {
		if _, err := c.Connect(context.Background()); err == nil {
			t.Fatal("SDK가 죽어 있으면 회복할 수 없다 — 폴백은 복구 수단이 아니다")
		}
		clock.advance(maxBackoff)
	}
	if p.Current().Generation != 1 {
		t.Error("조회가 실패했으므로 세대는 그대로여야 한다")
	}
	// 장애가 조용히 지나가지 않는다 — 실패 사유가 조회마다 남아야 IAM 거부·타임아웃·
	// 회전 중 창을 운영자가 구분할 수 있다(round-1 codex-cli#4).
	if cap.countEvent("secret_refresh_failed") != f.count() {
		t.Errorf("secret_refresh_failed %d회 — 조회 %d회와 같아야 한다",
			cap.countEvent("secret_refresh_failed"), f.count())
	}
	if cap.countEvent("auth_failed_observed") != 1 {
		t.Errorf("auth_failed_observed %d회 — 세대당 1회 남아야 한다", cap.countEvent("auth_failed_observed"))
	}
	if cap.countEvent("credential_recovered") != 0 {
		t.Error("회복하지 않았는데 credential_recovered가 났다")
	}
	// 그리고 계속 시도한다(provider 수준 포기 없음) — Step 1이 재배포할 때까지.
	if f.count() < 3 {
		t.Errorf("SDK 조회 %d회 — 게이트가 열릴 때마다 다시 시도해야 한다", f.count())
	}
}
