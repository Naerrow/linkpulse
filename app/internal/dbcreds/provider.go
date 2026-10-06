// dbcreds 패키지는 DB 비밀번호를 런타임에 다시 읽어 회전을 견디게 한다(plan 0009 Step 2).
//
// 왜 필요한가: 앱은 기동 시 주입된 비밀번호를 DSN 문자열에 박아 프로세스 수명 내내 재사용했다.
// RDS 마스터 비밀번호가 회전하면 신규 커넥션이 전부 SQLSTATE 28P01로 죽고, 재배포 전에는
// 회복 경로가 없었다(2026년 4회 재발: 66h/24h/118h/13h).
//
// 계약 (plan 0009 2-0):
//
//   - 인증 실패 관찰은 pgx 훅이 아니라 driver.Connector 래퍼에서 한다. 훅(BeforeConnect)은 연결
//     *전에* 설정을 고칠 뿐이고 인증 실패는 그다음 줄이 곧장 반환한다.
//   - 28P01일 때만 무효화한다. 네트워크·타임아웃은 비밀번호 문제가 아니다.
//   - 재조회는 singleflight로 1회로 합치고, **호출자 context와 분리된 detached context**에서 돈다.
//     /readyz의 1초 context에 묶이면 Secrets Manager 호출이 매번 잘려 영영 회복하지 못한다.
//     호출자가 취소돼도 refresh는 완료되고, 결과를 **다음 Connect가 승계**한다.
//   - "포기하지 않는다"는 호출 수준이 아니라 provider 수준의 계약이다. Connect 1회는 유한하게
//     실패하고(opener 무한 점유 금지), backoff 상태는 provider 전역에 남아 다음 호출이 이어받는다.
//   - **값이 바뀌지 않은 refresh는 세대를 올리지 않는다.** 회전 중 창(DB는 새 비밀번호인데
//     AWSCURRENT는 아직 옛 값)에서 세대만 올리면 이어지는 28P01이 전부 "새 세대의 실패"로 집계돼
//     재조회가 폭주한다. 값이 같으면 backoff 간격만 늘린다.
package dbcreds

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// backoff·타임아웃 상수 (plan 0009 2-0 "backoff와 호출별 제한" 표).
const (
	// 전파 지연 직후의 짧은 창을 놓치지 않도록 짧게 시작한다.
	initialBackoff = 200 * time.Millisecond
	// 목표가 "수 초 회복"이다. 새 AWSCURRENT가 보인 뒤 재시도까지 최대 이만큼.
	maxBackoff = 5 * time.Second
	// detached refresh 상한. SDK operation timeout과 같은 값이고, 이 시간 뒤 goroutine은 반드시 끝난다.
	// 호출 전체 최악 8초(시도 3초 + backoff 2초 + 재시도 3초)에 마진 2초를 둔 값이다 —
	// 마진 0이면 정상적인 스로틀 재시도 한 번이 잘려 refresh가 실패한다.
	RefreshTimeout = 10 * time.Second
)

// singleflight 키. 이 provider가 다루는 시크릿은 하나뿐이라 상수로 충분하다.
const refreshKey = "db-password"

// Credential은 provider가 현재 들고 있는 비밀번호와 그 세대다.
// 세대는 "값이 실제로 바뀐 refresh"에서만 올라간다.
type Credential struct {
	Password   string
	Generation uint64
}

// FetchFunc는 현재 AWSCURRENT 비밀번호를 조회한다. 테스트는 fake를 넣는다.
type FetchFunc func(ctx context.Context) (string, error)

// Provider는 비밀번호와 세대, backoff 게이트를 들고 있는 전역 상태다.
type Provider struct {
	mu       sync.Mutex
	cred     Credential
	interval time.Duration // 현재 backoff 간격(다음 실패 시 2배)
	gateOpen time.Time     // 이 시각 전에는 refresh를 시작하지 않는다

	// 세대당 1회 로그를 위한 상태. 0은 "아직 없음"이다(세대는 1부터 시작한다).
	authFailedGen uint64
	recoveredGen  uint64

	fetch FetchFunc // nil이면 정적 모드(로컬·개발)
	group singleflight.Group

	// refreshCtx는 호출자 context와 분리돼 있다. Close()만 이것을 취소한다.
	refreshCtx context.Context
	cancel     context.CancelFunc

	now    func() time.Time // fake clock 주입점
	jitter func(float64) float64
	log    *slog.Logger
}

// Option은 테스트가 시계·지터·로거를 갈아끼우기 위한 주입점이다.
type Option func(*Provider)

// WithClock은 backoff 계약을 실시간 없이 검증하기 위해 시계를 갈아끼운다.
func WithClock(now func() time.Time) Option { return func(p *Provider) { p.now = now } }

// WithJitter는 full jitter의 난수를 고정한다(0.0~1.0을 받아 배수를 돌려준다).
func WithJitter(f func(float64) float64) Option { return func(p *Provider) { p.jitter = f } }

// WithLogger는 로그 필드를 단언하기 위해 로거를 갈아끼운다.
func WithLogger(l *slog.Logger) Option { return func(p *Provider) { p.log = l } }

// New는 seed 비밀번호로 provider를 만든다.
//
// seed는 ECS가 기동 시 주입한 값이다 — 그 시점의 AWSCURRENT이므로 유효하다(2-3 (a)).
// fetch가 nil이면 정적 모드가 되어 refresh가 값을 바꾸지 않는다(로컬 docker-compose 경로).
func New(seed string, fetch FetchFunc, opts ...Option) *Provider {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Provider{
		cred:       Credential{Password: seed, Generation: 1},
		interval:   initialBackoff,
		fetch:      fetch,
		refreshCtx: ctx,
		cancel:     cancel,
		now:        time.Now,
		jitter:     func(f float64) float64 { return rand.Float64() * f },
		log:        slog.Default(),
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Mode는 기동 로그에 남길 provider 모드다. 2-5 smoke가 이 값으로
// "Step 2 코드가 실제로 도는 이미지인지"를 확인한다.
func (p *Provider) Mode() string {
	if p.fetch == nil {
		return "static"
	}
	return "secret"
}

// Current는 지금 쓸 비밀번호와 세대를 반환한다. Connect는 매 시도마다 이것을 읽는다.
func (p *Provider) Current() Credential {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cred
}

// Close는 진행 중인 detached refresh를 취소하고 새 refresh를 막는다.
// 앱 종료 시 sql.DB.Close 직후 부른다 — 없으면 종료 뒤에도 최대 RefreshTimeout 동안
// detached 작업이 provider 상태를 갱신한다.
func (p *Provider) Close() { p.cancel() }

// NoteAuthFailure는 첫 28P01을 세대당 1회만 기록한다(= 시계의 T_fail).
//
// 세대당 1회인 이유는 폭주 방지다 — 회전 중 창에서는 실패가 계속 반복된다.
// 2-6이 "기존 세션만 재사용돼 28P01이 오지 않음"과 "Step 2 코드가 깨져서 안 남"을
// 가르는 데 이 로그가 필요하다. 둘은 이 로그 없이는 완전히 같아 보인다.
func (p *Provider) NoteAuthFailure(gen uint64) {
	p.mu.Lock()
	if p.authFailedGen == gen {
		p.mu.Unlock()
		return
	}
	p.authFailedGen = gen
	p.mu.Unlock()
	p.log.Warn("DB 인증 실패 관찰", "event", "auth_failed_observed", "generation", gen)
}

// NoteRecovered는 새 세대로 dial이 처음 성공했을 때 세대당 1회 기록한다(= 시계의 T_ready).
//
// refresh 성공 로그는 "GetSecretValue가 다른 값을 반환했다"는 증거일 뿐 그 값으로 인증이
// 됐다는 증거가 아니다. 둘을 분리하지 않으면, 비밀번호 주입이나 둘째 dial이 깨져도
// Step 1의 재배포가 /readyz를 되돌려 "Step 2가 복구했다"는 false positive가 난다.
//
// seed 세대(1)는 전이가 아니므로 기록하지 않는다.
func (p *Provider) NoteRecovered(gen uint64) {
	p.mu.Lock()
	if gen <= 1 || p.recoveredGen >= gen {
		p.mu.Unlock()
		return
	}
	p.recoveredGen = gen
	p.mu.Unlock()
	p.log.Info("새 자격증명으로 연결 성공", "event", "credential_recovered", "generation", gen)
}

// RequestRefresh는 재조회를 요청하고 결과 채널을 돌려준다.
//
// failedGen은 28P01을 관찰한 호출이 그때 쓰던 세대다. **그 세대가 이미 낡았으면 조회하지
// 않는다** — 다른 연결이 먼저 갱신했다는 뜻이고, 늦게 도착한 옛 실패로 조회를 시작하면
// (1) 비단조적 응답이 옛 값을 새 세대로 올려 복구를 되돌리거나 (2) 값 동일 판정으로 게이트를
// 닫아 바로 뒤에 올 정상 refresh를 최대 maxBackoff만큼 늦춘다.
//
// backoff 게이트가 닫혀 있거나 정적 모드여도 nil을 반환한다 — 호출자는 기다리지 않고 즉시
// 오류로 끝내고, 다음 Connect가 다시 시도한다. 게이트 대기를 Connect 안에서 하지 않는 이유는
// 그만큼 opener를 점유하기 때문이다.
//
// 반환 채널을 호출자가 버려도 refresh는 detached context에서 끝까지 돌고 결과가
// provider 상태에 반영된다. 그것이 "다음 Connect가 승계한다"의 실체다.
func (p *Provider) RequestRefresh(failedGen uint64) <-chan singleflight.Result {
	if p.fetch == nil {
		return nil
	}
	p.mu.Lock()
	skip := p.cred.Generation != failedGen || p.now().Before(p.gateOpen)
	p.mu.Unlock()
	if skip {
		return nil
	}

	if p.refreshCtx.Err() != nil { // Close() 이후
		return nil
	}
	return p.group.DoChan(refreshKey, func() (any, error) { return p.doRefresh(failedGen) })
}

// doRefresh는 singleflight 안에서 실제 조회를 수행한다.
// 호출자 context가 아니라 provider 소유의 refreshCtx에서 돈다(detached).
//
// startGen·게이트를 **비행 안에서 다시** 확인한다. RequestRefresh의 확인과 DoChan 등록
// 사이에는 틈이 있어서, 먼저 끝난 비행이 게이트를 닫거나 세대를 올린 뒤에 들어온 호출이
// 새 비행을 시작할 수 있다 — 그러면 backoff가 무력해지고 조회가 동시 호출 수만큼 늘어난다.
func (p *Provider) doRefresh(startGen uint64) (any, error) {
	p.mu.Lock()
	if p.cred.Generation != startGen || p.now().Before(p.gateOpen) {
		cred := p.cred
		p.mu.Unlock()
		return cred, nil
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(p.refreshCtx, RefreshTimeout)
	defer cancel()

	password, err := p.fetch(ctx)

	p.mu.Lock()
	defer p.mu.Unlock()

	if err != nil {
		p.advanceBackoffLocked()
		// 실패 사유를 남기지 않으면 운영자가 IAM 거부·SDK 타임아웃·회전 중 창을 구분할 수 없다.
		// 조회 실패 경로에는 비밀값이 없고(SDK는 값을 오류에 넣지 않는다, JSON 파싱 오류는
		// 타입만 남긴다) 호출은 게이트로 이미 제한돼 있어 메시지를 그대로 실어도 안전하다.
		p.log.Warn("비밀번호 refresh 실패",
			"event", "secret_refresh_failed", "generation", p.cred.Generation,
			"retry_after", p.gateOpen.Format(time.RFC3339Nano), "error", err.Error())
		return p.cred, err
	}
	if p.cred.Generation != startGen {
		// 비행 중에 세대가 전진했다면 이 조회 결과는 그보다 낡았을 수 있으므로 버린다.
		// 현재 코드에서 세대는 이 함수 안에서만, 즉 singleflight 비행 안에서만 오르므로
		// 도달하지 않는다. 그래도 남기는 이유는 **그 안전성이 singleflight의 직렬화라는
		// 비자명한 성질에 기대고 있어서다** — 세대를 올리는 경로가 하나라도 늘면 복구된
		// 값이 옛 값으로 되돌아간다. 네 줄로 그 가능성을 지역적으로 닫는다.
		return p.cred, nil
	}
	if password == p.cred.Password {
		// 회전 중 창: DB는 새 비밀번호인데 AWSCURRENT는 아직 옛 값이다.
		// 세대를 올리지 않고 backoff만 늘려 재조회 폭주를 막는다.
		p.advanceBackoffLocked()
		return p.cred, nil
	}

	from := p.cred.Generation
	p.cred = Credential{Password: password, Generation: from + 1}
	p.resetBackoffLocked()
	p.log.Info("비밀번호 refresh 성공",
		"event", "secret_refreshed", "from_generation", from, "to_generation", p.cred.Generation)
	return p.cred, nil
}

// advanceBackoffLocked는 full jitter로 다음 게이트 시각을 정하고 간격을 2배로 늘린다.
func (p *Provider) advanceBackoffLocked() {
	p.gateOpen = p.now().Add(time.Duration(p.jitter(float64(p.interval))))
	if p.interval < maxBackoff {
		p.interval *= 2
		if p.interval > maxBackoff {
			p.interval = maxBackoff
		}
	}
}

// resetBackoffLocked는 값이 실제로 바뀐 성공에서만 부른다.
// 값 동일 성공은 reset하지 않는다 — 회전 중 창에서 폭주로 되돌아간다.
func (p *Provider) resetBackoffLocked() {
	p.interval = initialBackoff
	p.gateOpen = time.Time{}
}
