//go:build integration

// 2-4 통합 테스트 — Step 2의 배포 게이트 (plan 0009).
//
// 단위 fake로는 database/sql의 오류 전달·재시도, pgx 오류 래핑 판별, 기존 풀과 신규 연결이
// 섞일 때의 회복을 입증하지 못한다. 그래서 **실제 Postgres**에 전용 role을 만들고
// `ALTER ROLE ... PASSWORD`로 **진짜 비밀번호 변경**을 일으켜 프로세스 재시작 없이
// 회복하는지 본다. 회전과 같은 성질이다: 기존 커넥션은 살아 있고 신규 연결만 28P01로 죽는다.
//
// 실행 (DB는 사용자가 띄운다 — 가드레일):
//
//	docker compose up -d db
//	LINKPULSE_IT_DSN='postgres://linkpulse:linkpulse@localhost:5432/linkpulse?sslmode=disable' \
//	  go test -tags=integration -v -timeout 5m ./internal/db/
//
// 기본 `go test ./...`에서는 build tag로 제외된다 — CI에 Postgres가 없고, 이 테스트는
// **사람이 돌리는 배포 게이트**이지 PR 검사가 아니다(plan 2-4: "[사람] 통합 테스트").
//
// ⚠️ 시나리오마다 시계와 합격값이 다르다. 숫자를 서로 비교하지 않는다(plan 2-4 표):
//
//	(a) 새 값 즉시 가시화   T_set 기준     ≤ 15초
//	(b) 구값 창 D=20초      T_avail 기준   ≤ 30초   ← T_avail을 실제로 잴 수 있는 유일한 자리
//	(c) SDK K=3회 실패      마지막 실패 기준 ≤ 15초
package db_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Naerrow/linkpulse/app/internal/db"
	"github.com/Naerrow/linkpulse/app/internal/dbcreds"
	"github.com/Naerrow/linkpulse/app/internal/httpapi"
)

// fixture 상수. plan 2-4가 고정값으로 박으라고 요구한다 — 바꿀 때는 합격값도 함께 다시 계산한다.
const (
	staleWindowD = 20 * time.Second // (b) provider가 옛 값을 계속 보여주는 시간
	sdkFailuresK = 3                // (c) 연속 조회 실패 횟수

	deadlineFromTSet    = 15 * time.Second // (a) Connect 최악 13초 + 여유
	deadlineFromTAvail  = 30 * time.Second // (b) 관측 목표 24초 + 여유
	deadlineFromLastErr = 15 * time.Second // (c)

	oldPassword = "rot_pw_old_1"
	newPassword = "rot_pw_new_2"
)

// ---------- (a) 새 값이 즉시 보이면 수 초 안에 회복한다 ----------

func TestRotationRecoversWhenSecretIsImmediatelyVisible(t *testing.T) {
	env := newRotationEnv(t)
	secret := &secretFixture{value: oldPassword}
	pool := env.openPool(t, secret)

	env.warmPool(t, pool)

	tSet := time.Now()
	env.rotate(t, newPassword)
	secret.setValue(newPassword) // 전파 지연 없음 = T_avail == T_set
	forceNewConnections(pool)

	elapsed := waitForQuery(t, pool, deadlineFromTSet, tSet)
	t.Logf("(a) T_set → 쿼리 성공: %v (합격 ≤ %v)", elapsed, deadlineFromTSet)

	// 프로세스 재시작 없이, 같은 풀에서 회복해야 한다.
	if got := env.logs.count("credential_recovered"); got != 1 {
		t.Errorf("credential_recovered %d회 — 세대 전이 1회여야 한다", got)
	}
	if got := secret.callCount(); got == 0 {
		t.Error("조회가 한 번도 일어나지 않았다 — 28P01을 관찰하지 못했다는 뜻이다")
	}
}

// ---------- (b) 회전 중 창: 옛 값이 D초 동안 보여도 포기하지 않는다 ----------

func TestRotationSurvivesStaleSecretWindow(t *testing.T) {
	env := newRotationEnv(t)
	secret := &secretFixture{value: oldPassword}
	pool := env.openPool(t, secret)

	env.warmPool(t, pool)
	goroutinesBefore := runtime.NumGoroutine()

	env.rotate(t, newPassword)
	// AWSCURRENT가 아직 옛 값인 구간을 그대로 재현한다. 이 시각이 T_avail이다.
	tAvail := time.Now().Add(staleWindowD)
	secret.revealAt(newPassword, tAvail)
	forceNewConnections(pool)

	// 구값 창 동안 계속 실패하되, 재조회가 폭주하지 않아야 한다(backoff 계약).
	deadline := tAvail.Add(deadlineFromTAvail)
	elapsed := waitForQueryUntil(t, pool, deadline, tAvail)
	t.Logf("(b) T_avail → 쿼리 성공: %v (합격 ≤ %v, 구값 창 %v)", elapsed, deadlineFromTAvail, staleWindowD)

	// 구값 창은 최소 200ms backoff로 제한된다 — 20초 동안 폭주하면 수천 번이 된다.
	// 상한은 넉넉히 잡는다. 여기서 보는 것은 "폭주가 아니다"이지 정확한 횟수가 아니다.
	if calls := secret.callCount(); calls > 60 {
		t.Errorf("구값 창에서 조회 %d회 — backoff가 듣지 않는다", calls)
	} else {
		t.Logf("(b) 구값 창 조회 횟수: %d", calls)
	}
	if got := env.logs.count("secret_refresh_failed"); got != 0 {
		t.Errorf("secret_refresh_failed %d회 — 조회 자체는 성공했으므로 0이어야 한다", got)
	}
	assertNoGoroutineLeak(t, goroutinesBefore)
}

// ---------- (c) SDK가 K회 실패해도 포기하지 않는다 ----------

func TestRotationRecoversAfterRepeatedFetchFailures(t *testing.T) {
	env := newRotationEnv(t)
	secret := &secretFixture{value: oldPassword}
	pool := env.openPool(t, secret)

	env.warmPool(t, pool)

	env.rotate(t, newPassword)
	secret.failNext(sdkFailuresK, newPassword)
	forceNewConnections(pool)

	// 시계는 "마지막 주입 실패가 끝난 시각"부터다 — 그 전 구간은 이 plan이 통제하지 않는다.
	secret.waitForFailures(t, sdkFailuresK)
	tLastErr := secret.lastFailureAt()

	elapsed := waitForQuery(t, pool, deadlineFromLastErr, tLastErr)
	t.Logf("(c) 마지막 실패 → 쿼리 성공: %v (합격 ≤ %v)", elapsed, deadlineFromLastErr)

	if got := env.logs.count("secret_refresh_failed"); got < sdkFailuresK {
		t.Errorf("secret_refresh_failed %d회 — 주입한 실패 %d회가 전부 로그에 남아야 한다", got, sdkFailuresK)
	}
	if got := env.logs.count("credential_recovered"); got != 1 {
		t.Errorf("credential_recovered %d회 — 1회여야 한다", got)
	}
}

// ---------- /readyz 전용 경로: 무트래픽 회복의 유일한 통로 ----------

// 회전 후 /readyz만 반복 호출되는 상황에서 회복하는지 본다.
// 핸들러의 1초 context가 refresh 대기(3초)보다 짧으므로, 회복은 "그 호출 안에서"가 아니라
// **detached refresh가 끝난 뒤 다음 호출에서** 일어나야 한다. 이것이 성립하지 않으면
// 카나리만 도는 밤 시간대에는 영영 회복하지 못한다.
func TestReadyzRecoversWithoutOtherTraffic(t *testing.T) {
	env := newRotationEnv(t)
	secret := &secretFixture{value: oldPassword}
	pool := env.openPool(t, secret)

	srv := httptest.NewServer(httpapi.NewRouter(httpapi.RouterDeps{
		BaseURL:   "http://localhost",
		Readiness: func(ctx context.Context) error { return pool.PingContext(ctx) },
	}))
	defer srv.Close()

	if code := probeReadyz(t, srv.URL); code != http.StatusOK {
		t.Fatalf("회전 전 /readyz = %d — 200이어야 한다", code)
	}

	tSet := time.Now()
	env.rotate(t, newPassword)
	secret.setValue(newPassword)
	forceNewConnections(pool)

	// 회복까지 503이 몇 번 났는지 센다. 이 숫자가 런북에 적을 "회복 창" 실측값이다
	// (추정이 아니라 실측 — plan 2-4가 이 값을 재는 유일한 자리로 지정했다).
	var unavailable int
	deadline := tSet.Add(deadlineFromTSet)
	for time.Now().Before(deadline) {
		if probeReadyz(t, srv.URL) == http.StatusOK {
			t.Logf("/readyz 회복: %v, 그동안 503 %d회 (프로브 간격 200ms)",
				time.Since(tSet).Round(time.Millisecond), unavailable)
			if unavailable == 0 {
				t.Error("503이 한 번도 없었다 — 신규 연결이 강제되지 않아 경로를 시험하지 못했다")
			}
			return
		}
		unavailable++
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("/readyz가 %v 안에 200으로 돌아오지 않았다 (503 %d회)", deadlineFromTSet, unavailable)
}

// ---------- 동시 요청은 조회 1회로 합쳐진다 ----------

func TestConcurrentConnectionsShareSingleFetch(t *testing.T) {
	env := newRotationEnv(t)
	secret := &secretFixture{value: oldPassword}
	pool := env.openPool(t, secret)

	env.warmPool(t, pool)
	env.rotate(t, newPassword)
	secret.setValue(newPassword)
	forceNewConnections(pool)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), deadlineFromTSet)
			defer cancel()
			var one int
			_ = pool.QueryRowContext(ctx, "SELECT 1").Scan(&one)
		}()
	}
	wg.Wait()

	// 세대 전이는 1회여야 한다. 조회가 여러 번이어도 값이 같으면 세대는 오르지 않지만,
	// 여기서는 16개 동시 실패가 한 비행으로 합쳐지는지를 본다.
	if calls := secret.callCount(); calls > 2 {
		t.Errorf("조회 %d회 — singleflight면 1회(경합 시 최대 2회)여야 한다", calls)
	}
	if got := env.logs.count("credential_recovered"); got != 1 {
		t.Errorf("credential_recovered %d회 — 세대당 1회여야 한다", got)
	}
}

// ---------- 회복 로그는 실제 쿼리 성공과 같은 시각이다 ----------

// 2-6이 credential_recovered를 복구 증거로 쓰므로, 이 로그와 실제 회복이 어긋나면
// 드릴 판정이 통째로 틀어진다. 여기서 한 번 실증한다.
func TestRecoveredLogMatchesActualQuerySuccess(t *testing.T) {
	env := newRotationEnv(t)
	secret := &secretFixture{value: oldPassword}
	pool := env.openPool(t, secret)

	env.warmPool(t, pool)
	env.rotate(t, newPassword)
	secret.setValue(newPassword)
	forceNewConnections(pool)

	waitForQuery(t, pool, deadlineFromTSet, time.Now())
	querySucceededAt := time.Now()

	loggedAt, ok := env.logs.firstTime("credential_recovered")
	if !ok {
		t.Fatal("credential_recovered 로그가 없다 — 2-6이 쓸 증거가 생기지 않는다")
	}
	if gap := querySucceededAt.Sub(loggedAt); gap < 0 || gap > 2*time.Second {
		t.Errorf("로그와 실제 성공의 간격 %v — 같은 시각이어야 한다(±2초)", gap)
	}
}

// ================= 환경·도구 =================

// rotationEnv는 전용 role과 전용 데이터베이스를 쥔 테스트 환경이다.
// 운영 계정이나 compose의 공용 role을 건드리지 않는다 — 비밀번호를 실제로 바꾸기 때문이다.
type rotationEnv struct {
	admin  *sql.DB
	role   string
	dsnFor func(password string) string
	logs   *logCapture
}

func newRotationEnv(t *testing.T) *rotationEnv {
	t.Helper()
	adminDSN := os.Getenv("LINKPULSE_IT_DSN")
	if adminDSN == "" {
		t.Skip("LINKPULSE_IT_DSN 미설정 — 통합 테스트를 건너뛴다(docker compose up -d db 후 지정)")
	}

	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("관리 연결 실패: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.Ping(); err != nil {
		t.Fatalf("관리 연결 핑 실패(DB가 떠 있는가?): %v", err)
	}

	base, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		t.Fatalf("LINKPULSE_IT_DSN 파싱 실패: %v", err)
	}

	// 테스트마다 고유한 이름이라 병렬·반복 실행에서 충돌하지 않는다.
	name := fmt.Sprintf("it_rot_%d", time.Now().UnixNano())
	mustExec(t, admin, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'", name, oldPassword))
	mustExec(t, admin, fmt.Sprintf("CREATE DATABASE %s OWNER %s", name, name))
	t.Cleanup(func() {
		// FORCE가 있어야 남은 커넥션이 있어도 지워진다(PG13+).
		mustExec(t, admin, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name))
		mustExec(t, admin, fmt.Sprintf("DROP ROLE IF EXISTS %s", name))
	})

	return &rotationEnv{
		admin: admin,
		role:  name,
		dsnFor: func(password string) string {
			return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
				name, password, base.Host, base.Port, name)
		},
		logs: newLogCapture(),
	}
}

// openPool은 운영과 같은 경계로 풀을 연다 — db.Open + secret provider.
func (e *rotationEnv) openPool(t *testing.T, secret *secretFixture) *sql.DB {
	t.Helper()
	provider := dbcreds.New(oldPassword, secret.fetch, dbcreds.WithLogger(e.logs.logger()))
	t.Cleanup(provider.Close)

	pool, err := db.Open(context.Background(), db.Settings{
		DSN:      e.dsnFor(oldPassword),
		Provider: provider,
	})
	if err != nil {
		t.Fatalf("풀 생성 실패: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

// warmPool은 기존 커넥션을 미리 만들어 둔다 — 회전이 기존 세션을 끊지 않는다는 성질을 재현한다.
func (e *rotationEnv) warmPool(t *testing.T, pool *sql.DB) {
	t.Helper()
	var one int
	if err := pool.QueryRow("SELECT 1").Scan(&one); err != nil {
		t.Fatalf("예열 쿼리 실패: %v", err)
	}
}

// rotate는 실제 비밀번호를 바꾼다(= T_set). 기존 커넥션은 그대로 살아 있다.
func (e *rotationEnv) rotate(t *testing.T, password string) {
	t.Helper()
	mustExec(t, e.admin, fmt.Sprintf("ALTER ROLE %s PASSWORD '%s'", e.role, password))
}

func mustExec(t *testing.T, conn *sql.DB, stmt string) {
	t.Helper()
	if _, err := conn.Exec(stmt); err != nil {
		t.Fatalf("SQL 실패(%s): %v", stmt, err)
	}
}

// forceNewConnections는 기존 커넥션을 만료시켜 신규 연결을 유도한다.
// 운영에서 DB_CONN_MAX_LIFETIME이 하는 일과 같다(2-6이 T_fail을 설계로 만드는 손잡이).
func forceNewConnections(pool *sql.DB) {
	pool.SetConnMaxLifetime(time.Millisecond)
	pool.SetConnMaxIdleTime(time.Millisecond)
	time.Sleep(50 * time.Millisecond)
}

// waitForQuery는 쿼리가 성공할 때까지 기다리고 from 기준 경과 시간을 돌려준다.
func waitForQuery(t *testing.T, pool *sql.DB, budget time.Duration, from time.Time) time.Duration {
	t.Helper()
	return waitForQueryUntil(t, pool, from.Add(budget), from)
}

func waitForQueryUntil(t *testing.T, pool *sql.DB, deadline, from time.Time) time.Duration {
	t.Helper()
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var one int
		err := pool.QueryRowContext(ctx, "SELECT 1").Scan(&one)
		cancel()
		if err == nil {
			return time.Since(from)
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("기한 안에 회복하지 못했다(마지막 오류: %v)", lastErr)
	return 0
}

func probeReadyz(t *testing.T, baseURL string) int {
	t.Helper()
	resp, err := http.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatalf("/readyz 요청 실패: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func assertNoGoroutineLeak(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > before+4 {
		time.Sleep(50 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+4 {
		t.Errorf("goroutine %d → %d — 누수가 의심된다", before, after)
	}
}

// ================= fixture =================

// secretFixture는 Secrets Manager 자리에 들어가는 통제 가능한 조회원이다.
// 전파 지연(구값 창)과 SDK 장애를 **주입**해 시나리오별 시계를 결정적으로 만든다.
type secretFixture struct {
	mu          sync.Mutex
	value       string
	pending     string    // visibleAt 이후 돌려줄 값
	visibleAt   time.Time // 이 시각 전에는 value를 그대로 돌려준다
	failures    int       // 남은 연속 실패 횟수
	failedCount int
	lastFailure time.Time
	calls       int
}

func (f *secretFixture) fetch(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++

	if f.failures > 0 {
		f.failures--
		f.failedCount++
		f.lastFailure = time.Now()
		return "", errors.New("주입된 SDK 장애")
	}
	if !f.visibleAt.IsZero() && time.Now().After(f.visibleAt) {
		f.value, f.pending, f.visibleAt = f.pending, "", time.Time{}
	}
	return f.value, nil
}

func (f *secretFixture) setValue(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.value = v
}

// revealAt은 at 시각까지 옛 값을 보여주다가 그 뒤에 v를 노출한다(= T_avail은 at).
func (f *secretFixture) revealAt(v string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending, f.visibleAt = v, at
}

// failNext는 n회 연속 실패시킨 뒤 v를 노출한다.
func (f *secretFixture) failNext(n int, v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures, f.value = n, v
}

func (f *secretFixture) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *secretFixture) lastFailureAt() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastFailure
}

func (f *secretFixture) waitForFailures(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		done := f.failedCount >= n
		f.mu.Unlock()
		if done {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("주입한 실패 %d회가 소진되지 않았다 — 재시도가 멈춘 것이다", n)
}

// ================= 로그 수집 =================

type logCapture struct {
	mu    sync.Mutex
	lines []map[string]any
}

func newLogCapture() *logCapture { return &logCapture{} }

func (c *logCapture) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		var m map[string]any
		if err := json.Unmarshal(p, &m); err == nil {
			c.mu.Lock()
			c.lines = append(c.lines, m)
			c.mu.Unlock()
		}
		return len(p), nil
	}), nil))
}

func (c *logCapture) count(event string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.lines {
		if m["event"] == event {
			n++
		}
	}
	return n
}

// firstTime은 그 이벤트가 처음 난 시각을 돌려준다(slog JSON의 time 필드).
func (c *logCapture) firstTime(event string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.lines {
		if m["event"] != event {
			continue
		}
		raw, ok := m["time"].(string)
		if !ok {
			return time.Time{}, false
		}
		ts, err := time.Parse(time.RFC3339Nano, raw)
		return ts, err == nil
	}
	return time.Time{}, false
}

// 로그에 비밀번호가 섞이지 않는지 통합 경로에서도 확인한다.
func (c *logCapture) dump() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, m := range c.lines {
		raw, _ := json.Marshal(m)
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// 모든 시나리오가 끝난 뒤 한 번, 수집한 로그 전체에 비밀번호가 없는지 본다.
func TestIntegrationLogsNeverContainPasswords(t *testing.T) {
	env := newRotationEnv(t)
	secret := &secretFixture{value: oldPassword}
	pool := env.openPool(t, secret)

	env.warmPool(t, pool)
	env.rotate(t, newPassword)
	secret.setValue(newPassword)
	forceNewConnections(pool)
	waitForQuery(t, pool, deadlineFromTSet, time.Now())

	dump := env.logs.dump()
	for _, secretValue := range []string{oldPassword, newPassword} {
		if strings.Contains(dump, secretValue) {
			t.Errorf("로그에 비밀번호가 들어 있다: %q", secretValue)
		}
	}
}
