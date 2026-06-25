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
