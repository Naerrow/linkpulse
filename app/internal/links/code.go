package links

import "crypto/rand"

// base62Alphabet은 0-9, a-z, A-Z 순서의 62진 문자표다.
// 사람이 입력·복사하기 쉬운 영숫자만 쓴다.
const base62Alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// randomCode는 crypto/rand로 길이 n의 base62 랜덤 코드를 만든다.
// 순차 코드와 달리 열거가 불가능해(62^7 ≈ 3.5조) 무인증 공개 API에서도
// 남의 링크가 추측·수집되지 않는다.
//
// 모듈러 편향을 피하려 거부 표본추출(rejection sampling)을 쓴다:
// 256은 62의 배수가 아니라 단순 b%62는 앞쪽 문자를 더 자주 뽑는다.
// 62의 최대 배수(248) 이상 바이트는 버려 균등 분포를 보장한다.
func randomCode(n int) (string, error) {
	const maxUnbiased = 256 - (256 % 62) // = 248

	out := make([]byte, n)
	buf := make([]byte, n) // 한 번에 n바이트씩 뽑고, 버림이 많으면 추가로 더 뽑는다.
	filled := 0
	for filled < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err // 엔트로피 소스 고장 → 코드를 만들지 않고 실패 전파
		}
		for _, b := range buf {
			if b >= maxUnbiased {
				continue // 편향 유발 구간 → 버림
			}
			out[filled] = base62Alphabet[b%62]
			filled++
			if filled == n {
				break
			}
		}
	}
	return string(out), nil
}

// reservedCodes는 단축 코드로 발급하면 안 되는 예약 경로다.
// 라우터에서 더 구체적인 패턴이 우선하긴 하지만, 같은 단일 세그먼트 경로가
// 리다이렉트(GET /{code})에 가려지는 일을 원천 차단하기 위해 발급 단계에서 거른다.
// (짧은 코드 길이를 설정하면 "readyz"(6자) 등과 우연히 겹칠 수 있다.)
var reservedCodes = map[string]bool{
	"healthz": true,
	"readyz":  true,
	"api":     true,
}

// isReserved는 코드가 예약어인지 확인한다.
func isReserved(code string) bool {
	return reservedCodes[code]
}
