-- linkpulse P0 스키마.
-- 단축 링크 한 건 = 한 행.
-- code를 PRIMARY KEY로 둬 UNIQUE 제약을 얻는다 → 애플리케이션이 랜덤 코드 충돌을
-- INSERT 실패(SQLSTATE 23505)로 감지해 새 코드로 재시도한다(저장소가 유일성만 판단).
-- created_at은 DB가 now()로 채워 단일 진실 소스로 삼는다(앱/DB 시계 차이 방지).
CREATE TABLE IF NOT EXISTS links (
    code       TEXT PRIMARY KEY,
    url        TEXT NOT NULL,
    clicks     BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- plan 0011: 운영자가 링크를 중단한 시각. NULL이면 사용 중이다. 지우지 않고 끄는 이유는 클릭 기록을 남기기 위해서다.
-- 기존 테이블에도 붙도록 ADD COLUMN IF NOT EXISTS로 둔다(nullable·기본값 없음이라 즉시 끝난다).
ALTER TABLE links ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;
-- 운영자 화면의 최근 링크 목록(ORDER BY created_at DESC LIMIT 50)용.
CREATE INDEX IF NOT EXISTS links_created_at ON links (created_at DESC);

-- plan 0011: 방문자의 단축 링크 요청. 생성은 관리자만 하므로 방문자는 요청을 남기고 결과를 ID로 확인한다.
-- id는 16바이트 난수(base64url)라 추측할 수 없고, 그 자체가 요청 확인 링크의 열쇠다.
-- 개인정보(이메일 등)는 받지 않는다. code는 승인 시 발급된 링크를 가리킨다.
CREATE TABLE IF NOT EXISTS link_requests (
    id         TEXT PRIMARY KEY,
    url        TEXT NOT NULL,
    note       TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected')),
    code       TEXT REFERENCES links (code),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at TIMESTAMPTZ
);
-- 관리자 대기 목록(status = 'pending' ORDER BY created_at DESC)과 대기 상한 count용.
CREATE INDEX IF NOT EXISTS link_requests_status_created ON link_requests (status, created_at DESC);

-- plan 0013: 반영한 클릭 배치의 ID. 큐(SQS)는 최소 한 번 전달이라 같은 배치가 다시 올 수 있다.
-- 반영과 같은 트랜잭션에서 여기에 ID를 넣어 두 번 세지 않는다. 21일이 지나면 워커가 지운다 —
-- DLQ 보존 14일 + 재처리 뒤 본 큐 4일 동안 다시 들어올 수 있어서다(plan 0013 고정값 표).
-- PK가 있어 논리 복제 조건(P6)을 지킨다.
CREATE TABLE IF NOT EXISTS click_batches (
    id         TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS click_batches_applied_at ON click_batches (applied_at);
