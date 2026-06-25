// links 패키지는 URL 단축의 핵심 도메인이다.
// 단축 코드 발급·리다이렉트 대상 조회·클릭 집계를 담당하며,
// 저장소(Repository)는 인터페이스로 추상화해 인메모리/Postgres 등 구현을 교체할 수 있게 한다.
package links

import "time"

// Link은 단축 링크 한 건을 나타내는 도메인 모델이다.
type Link struct {
	Code      string    // 단축 코드 (예: "1", "a", "Zk") — 경로 /{code}로 노출된다.
	URL       string    // 리다이렉트 대상 원본 URL
	Clicks    int64     // 누적 클릭 수
	CreatedAt time.Time // 생성 시각 (UTC)
}
