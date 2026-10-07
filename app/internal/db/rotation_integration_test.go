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
//	docker run -d --rm --name linkpulse-it-pg -e POSTGRES_PASSWORD=itpw \
//	  -p 55432:5432 postgres:16-alpine
//	LINKPULSE_IT_DSN='postgres://postgres:itpw@localhost:55432/postgres?sslmode=disable' \
//	  go test -tags=integration -race -v -timeout 5m ./internal/db/
//	docker stop linkpulse-it-pg
//
// `-race`는 선택이 아니다 — 검증 ⑯("dialWith가 원본 ConnConfig를 건드리지 않는다")의 위반은
// 낡은 세대 redial이 흡수해 **테스트가 통과해 버리고**, 레이스 검출기만 그것을 잡는다.
//
// ⚠️ **compose의 db 서비스를 쓰지 않는다.** 그 서비스에는 published port가 없어
// (`docker compose port db 5432` → `:0`) 호스트의 5432로는 닿지 않는다. 그 주소로 붙이면
// 호스트에 이미 떠 있는 **다른** Postgres에 연결되고, 이 테스트는 거기에 role과 데이터베이스를
// 만든다. 그래서 비표준 포트(55432)의 일회용 컨테이너를 쓴다 — plan 2-4의 "disposable"이 이 뜻이다.
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
	elapsed := waitForQuery(t, pool, deadlineFromTAvail, tAvail)
	t.Logf("(b) T_avail → 쿼리 성공: %v (합격 ≤ %v, 구값 창 %v)", elapsed, deadlineFromTAvail, staleWindowD)

	// ⚠️ 이 임계는 **폴링 간격(200ms)에 의존한다.** backoff가 없어도 20초 창에서 조회는 약 95회지
	// 수천 회가 아니다 — 조회를 유발하는 것은 쿼리 시도이고 그 간격이 200ms이기 때문이다.
	// 즉 60은 "backoff가 폴링보다 더 묶는가"를 보는 값이다(실측 13회). 폴링 간격을 바꾸면
	// 이 임계도 다시 계산한다. 정확한 횟수가 아니라 "폭주가 아니다"를 본다.
	if calls := secret.callCount(); calls > 60 {
		t.Errorf("구값 창에서 조회 %d회 — backoff가 듣지 않는다", calls)
	} else {
		t.Logf("(b) 구값 창 조회 횟수: %d", calls)
	}
	if got := env.logs.count("secret_refresh_failed"); got != 0 {
		t.Errorf("secret_refresh_failed %d회 — 조회 자체는 성공했으므로 0이어야 한다", got)
	}
	// 구값 창에서 조회는 여러 번이지만 **세대 전이는 정확히 1회**여야 한다.
	// 단위 ⑥("값 동일 refresh는 세대를 올리지 않는다")의 실경로판이다.
	if got := env.logs.count("secret_refreshed"); got != 1 {
		t.Errorf("secret_refreshed %d회 — 값이 바뀐 1회만 세대를 올려야 한다", got)
	}
	if got := env.logs.count("credential_recovered"); got != 1 {
		t.Errorf("credential_recovered %d회 — 1회여야 한다", got)
	}
	if got := env.logs.count("auth_failed_observed"); got == 0 {
		t.Error("auth_failed_observed가 없다 — 28P01을 한 번도 관찰하지 못했다")
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

	// ⚠️ 재시도를 **누가 끌고 가는지**가 이 시나리오의 핵심이다.
	// "포기하지 않는다"는 호출 수준이 아니라 provider 수준의 계약이다 — Connect 1회는 유한하게
	// 실패하고 backoff 상태만 남으며, 재시도는 **다음 Connect가 승계**한다. 즉 트래픽이 없으면
	// 재시도도 없다(그 구간은 Step 1의 자동 재배포가 받는다). 그러므로 이 테스트는 신규 연결을
	// 계속 만들어 줘야 한다. 그렇게 하지 않으면 조회가 0회로 끝나고, 멀쩡한 코드를
	// "재시도가 멈췄다"고 오판한다.
	stopTraffic := driveQueries(t, pool)
	defer stopTraffic()

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
	// ⚠️ 조회를 일부러 느리게 만든다(1초 readiness 예산 < 2초 조회 < 3초 refresh 대기).
	// 즉시 응답하는 fixture로는 회복이 **한 번의 /readyz 호출 안에서** 끝나 버려
	// (실측: 643ms에 200) 정작 시험하려던 "옛 값 → detached refresh → **다음 호출**에서 200"
	// 경로를 한 번도 지나지 않는다. 프로덕션의 Secrets Manager는 즉답하지 않으므로
	// 느린 쪽이 실제에 가깝고, 무트래픽 회복이 성립하는지는 이 경로에서만 드러난다.
	secret := &secretFixture{value: oldPassword, delay: 2 * time.Second}
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

	// 회복까지 503이 몇 번 났는지 센다.
	// ⚠️ 이 건수는 **주입한 조회 지연에 비례**하므로 프로덕션 값이 아니다. 여기서 고정하는 것은
	// "503이 나다가 detached refresh가 끝난 뒤 200으로 돌아온다"는 **모양**이다.
	// 프로덕션 건수는 실제 Secrets Manager 지연에 달려 있어 2-6에서만 나온다.
	var unavailable int
	deadline := tSet.Add(deadlineFromTSet)
	for time.Now().Before(deadline) {
		if probeReadyz(t, srv.URL) == http.StatusOK {
			t.Logf("/readyz 회복: %v, 그동안 503 %d회 (프로브 간격 200ms)",
				time.Since(tSet).Round(time.Millisecond), unavailable)
			if unavailable == 0 {
				t.Error("503이 한 번도 없었다 — 조회 지연보다 빨리 회복했다면 detached 승계 경로를 지나지 않았다")
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
	failures := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), deadlineFromTSet)
			defer cancel()
			var one int
			if err := pool.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("동시 쿼리 실패: %v", err)
	}

	// ⚠️ **1회만 통과**다. "경합 시 2회까지"로 느슨하게 두면 round-1 결함이 되살아나도 못 잡는다 —
	// 그 결함 코드(865b2d2)의 지문이 바로 **조회 2회**였다(round-2 리뷰 실측: 200회 중 190회가 2회,
	// 수정 후 HEAD는 200회 전부 1회). plan 2-4도 "1회로 합쳐지는지"를 요구한다.
	if calls := secret.callCount(); calls != 1 {
		t.Errorf("조회 %d회 — singleflight면 정확히 1회여야 한다", calls)
	}
	if got := env.logs.count("secret_refreshed"); got != 1 {
		t.Errorf("secret_refreshed %d회 — 세대 전이는 1회여야 한다", got)
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
		// ⚠️ compose의 db를 안내하지 않는다 — published port가 없어 호스트의 다른 Postgres에
		// 붙고, 이 테스트는 거기에 role과 DB를 만든다(ops-gotchas G-7). 머리 주석의 절차를 가리킨다.
		t.Skip("LINKPULSE_IT_DSN 미설정 — 통합 테스트를 건너뛴다. " +
			"일회용 컨테이너로 띄운다: docker run -d --rm --name linkpulse-it-pg " +
			"-e POSTGRES_PASSWORD=itpw -p 55432:5432 postgres:16-alpine (이 파일 머리 주석 참고)")
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
	// 이 테스트는 role과 데이터베이스를 **만들고 지운다.** 원격 호스트(RDS 등)를 가리키면
	// 그 서버에 흔적을 남기므로 로컬만 허용한다. 실수로 운영 DSN을 넣는 경로를 여기서 끊는다.
	if base.Host != "localhost" && base.Host != "127.0.0.1" && !strings.HasPrefix(base.Host, "/") {
		t.Fatalf("LINKPULSE_IT_DSN의 호스트가 로컬이 아니다(%s) — 이 테스트는 role·DB를 생성한다", base.Host)
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

	env := &rotationEnv{logs: newLogCapture()}
	// 비밀번호 비노출은 **시나리오마다** 검사한다. env별로 logCapture가 따로라 한 테스트에서
	// 모아 보는 것은 불가능하고, 특히 오류 문자열을 싣는 secret_refresh_failed 경로((c))는
	// 그 시나리오에서만 생긴다.
	t.Cleanup(func() {
		dump := env.logs.dump()
		for _, secretValue := range []string{oldPassword, newPassword} {
			if strings.Contains(dump, secretValue) {
				t.Errorf("로그에 비밀번호가 들어 있다: %q", secretValue)
			}
		}
	})

	env.admin = admin
	env.role = name
	env.dsnFor = func(password string) string {
		return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
			name, password, base.Host, base.Port, name)
	}
	return env
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
//
// ⚠️ 경과 시간을 **여기서 단언한다.** 기한은 "시도를 시작할 수 있는 시각"에만 걸리므로, 기한
// 직전에 시작한 시도가 성공하면 쿼리 ctx(5초)만큼 더 걸려도 루프는 빠져나온다. 반환값을
// 로그로만 쓰면 합격선이 사실상 +5초가 된다. 하한도 본다 — from 이전에 성공했다면 애초에
// 28P01이 나지 않은 것이고, 그 실행은 자기 주장을 입증하지 못한다.
func waitForQuery(t *testing.T, pool *sql.DB, budget time.Duration, from time.Time) time.Duration {
	t.Helper()
	elapsed := waitForQueryUntil(t, pool, from.Add(budget), from)
	if elapsed < 0 {
		t.Errorf("기준 시각보다 %v 먼저 성공했다 — 회복 경로를 지나지 않았다", -elapsed)
	}
	if elapsed > budget {
		t.Errorf("회복에 %v 걸렸다 — 합격선 %v를 넘었다", elapsed, budget)
	}
	return elapsed
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

// driveQueries는 신규 연결을 계속 만들어 "다음 Connect가 승계한다"를 성립시킨다.
// 운영에서는 실사용 트래픽이나 카나리가 이 역할을 한다.
func driveQueries(t *testing.T, pool *sql.DB) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(100 * time.Millisecond):
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				var one int
				_ = pool.QueryRowContext(ctx, "SELECT 1").Scan(&one)
				cancel()
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
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
	delay       time.Duration // 조회 1회가 걸리는 시간. 0이면 즉답(프로덕션은 즉답하지 않는다)
	value       string
	pending     string    // visibleAt 이후 돌려줄 값
	visibleAt   time.Time // 이 시각 전에는 value를 그대로 돌려준다
	failures    int       // 남은 연속 실패 횟수
	failedCount int
	lastFailure time.Time
	calls       int
}

func (f *secretFixture) fetch(ctx context.Context) (string, error) {
	f.mu.Lock()
	delay := f.delay
	f.calls++
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

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

// 비밀번호 비노출 검사는 newRotationEnv의 t.Cleanup이 **모든 시나리오에** 건다.
// 별도 테스트를 두면 그 시나리오 하나만 보게 되므로 두지 않는다.
