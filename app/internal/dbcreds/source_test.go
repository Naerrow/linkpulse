package dbcreds

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writePasswordFile은 테스트용 비밀번호 파일을 쓴다. kubelet처럼 내용을 통째로 바꾼다.
func writePasswordFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("파일 쓰기 실패: %v", err)
	}
}

// file 출처는 매 호출이 파일을 새로 읽는다 — 회전 뒤 kubelet이 바꾼 값을 refresh가 집는 근거다.
func TestFileFetchRereadsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db-password")
	fetch := NewFileFetch(path)

	writePasswordFile(t, path, oldPassword+"\n") // 파일로 만든 Secret에 흔히 붙는 줄바꿈
	got, err := fetch(context.Background())
	if err != nil || got != oldPassword {
		t.Fatalf("첫 읽기 %q, %v — 줄바꿈을 지운 %q여야 한다", got, err, oldPassword)
	}

	writePasswordFile(t, path, newPassword)
	got, err = fetch(context.Background())
	if err != nil || got != newPassword {
		t.Fatalf("바뀐 파일 읽기 %q, %v — 새 값 %q여야 한다(캐시 금지)", got, err, newPassword)
	}
}

// 파일이 없거나 비면 오류다. 오류에 내용이 들어가지 않는다.
func TestFileFetchFailures(t *testing.T) {
	dir := t.TempDir()

	if _, err := NewFileFetch(filepath.Join(dir, "missing"))(context.Background()); err == nil {
		t.Error("없는 파일인데 오류가 아니다")
	}

	empty := filepath.Join(dir, "empty")
	writePasswordFile(t, empty, "\n")
	if _, err := NewFileFetch(empty)(context.Background()); err == nil {
		t.Error("빈 파일(줄바꿈만)인데 오류가 아니다")
	}
}

// file 출처 회전 경로 전체: 기동 seed는 파일의 옛 값, DB가 새 값만 받게 된 뒤 kubelet이 파일을 바꾸면
// 인증 실패 → 파일 재조회 → 새 값으로 다시 연결한다.
func TestFileSourceRecoversAfterRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db-password")
	writePasswordFile(t, path, oldPassword)

	cap, logger := newLogCapture()
	p, err := Start(context.Background(), NewFileFetch(path), WithLogger(logger))
	if err != nil {
		t.Fatalf("Start 실패: %v", err)
	}
	t.Cleanup(p.Close)
	if got := p.Current().Password; got != oldPassword {
		t.Fatalf("seed %q — 파일의 값이어야 한다", got)
	}

	d := &fakeDial{accepts: newPassword} // 회전 직후: DB는 새 값만 받는다
	c := NewConnector(p, d.fn, nil)
	writePasswordFile(t, path, newPassword) // kubelet이 마운트 파일을 바꿨다

	if _, err := c.Connect(context.Background()); err != nil {
		t.Fatalf("파일을 다시 읽어 회복해야 한다: %v", err)
	}
	if got := d.passwords(); len(got) != 2 || got[0] != oldPassword || got[1] != newPassword {
		t.Errorf("dial 비밀번호 순서 %v — [old new]여야 한다", got)
	}
	if cap.countEvent("credential_recovered") != 1 {
		t.Error("새 값으로 연결 성공 로그가 한 번 남아야 한다")
	}
}

// aws 출처(가짜 fetch): 기동 seed를 출처에서 직접 읽는다 — ECS처럼 넣어 주는 주체가 없다.
func TestStartFetchesSeed(t *testing.T) {
	f := &fakeFetch{value: oldPassword}
	_, logger := newLogCapture()
	p, err := Start(context.Background(), f.fn, WithLogger(logger))
	if err != nil {
		t.Fatalf("Start 실패: %v", err)
	}
	t.Cleanup(p.Close)
	if got := p.Current(); got.Password != oldPassword || got.Generation != 1 {
		t.Errorf("seed %+v — 출처에서 읽은 값의 세대 1이어야 한다", got)
	}
	if f.count() != 1 {
		t.Errorf("기동 조회 %d회 — 1회여야 한다", f.count())
	}
	if p.Mode() != "secret" {
		t.Errorf("모드 %q — 출처가 있으면 secret이다", p.Mode())
	}
}

// Start가 실제로 다시 시도한다 — 첫 조회가 실패해도 기동한다(실제 backoff 2초를 한 번 기다린다).
// fetchSeed만 시험하면 Start가 시도 1회로 부르도록 바뀌어도 모른다(code-review r1).
func TestStartRetriesSeed(t *testing.T) {
	var calls int32
	fetch := func(context.Context) (string, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return "", errors.New("자격증명 아직 없음")
		}
		return oldPassword, nil
	}
	_, logger := newLogCapture()
	p, err := Start(context.Background(), fetch, WithLogger(logger))
	if err != nil {
		t.Fatalf("두 번째 시도에서 기동해야 한다: %v", err)
	}
	t.Cleanup(p.Close)
	if got := p.Current().Password; got != oldPassword || calls != 2 {
		t.Errorf("seed %q, 시도 %d회 — 두 번째 값, 2회여야 한다", got, calls)
	}
}

// 기동 조회는 몇 번 다시 시도한다(새 노드에서 Pod Identity 에이전트가 늦게 준비되는 경우).
func TestFetchSeedRetriesThenSucceeds(t *testing.T) {
	var calls int32
	fetch := func(context.Context) (string, error) {
		if atomic.AddInt32(&calls, 1) < 3 {
			return "", errors.New("자격증명 아직 없음")
		}
		return newPassword, nil
	}
	got, err := fetchSeed(context.Background(), fetch, 5, time.Millisecond)
	if err != nil || got != newPassword {
		t.Fatalf("seed %q, %v — 세 번째 시도에서 성공해야 한다", got, err)
	}
	if calls != 3 {
		t.Errorf("시도 %d회 — 3회여야 한다", calls)
	}
}

// 끝내 실패하면 오류로 끝난다(기동 중단 → 쿠버네티스가 재시작). 시도 수는 상한을 넘지 않는다.
func TestFetchSeedGivesUp(t *testing.T) {
	var calls int32
	fetch := func(context.Context) (string, error) {
		atomic.AddInt32(&calls, 1)
		return "", errors.New("AccessDenied")
	}
	_, err := fetchSeed(context.Background(), fetch, 4, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("마지막 사유를 담은 오류여야 한다: %v", err)
	}
	if calls != 4 {
		t.Errorf("시도 %d회 — 4회여야 한다", calls)
	}
}

// 시도마다 상한이 걸린다 — 출처가 멈춰도 기동이 무한정 걸리지 않는다.
func TestFetchSeedAttemptHasDeadline(t *testing.T) {
	fetch := func(ctx context.Context) (string, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("시도 context에 상한이 없다")
		}
		return newPassword, nil
	}
	if _, err := fetchSeed(context.Background(), fetch, 1, time.Millisecond); err != nil {
		t.Fatal(err)
	}
}
