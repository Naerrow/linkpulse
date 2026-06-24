package links

import (
	"strings"
	"testing"
)

// TestRandomCodeLengthAndCharset은 코드가 요청한 길이이고 base62 문자만 쓰는지 확인한다.
func TestRandomCodeLengthAndCharset(t *testing.T) {
	for _, n := range []int{4, 7, 16} {
		code, err := randomCode(n)
		if err != nil {
			t.Fatalf("randomCode(%d) 실패: %v", n, err)
		}
		if len(code) != n {
			t.Errorf("len = %d, want %d (code=%q)", len(code), n, code)
		}
		for _, c := range code {
			if !strings.ContainsRune(base62Alphabet, c) {
				t.Errorf("base62 밖 문자 %q (code=%q)", c, code)
			}
		}
	}
}

// TestRandomCodeUniqueEnough는 다량 발급 시 충돌이 없는지(사실상) 확인한다.
// 1000회 내 충돌이 나면 무작위성이 깨진 것이다.
func TestRandomCodeUniqueEnough(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		code, err := randomCode(7)
		if err != nil {
			t.Fatalf("randomCode 실패: %v", err)
		}
		if seen[code] {
			t.Fatalf("1000회 내 충돌 발생: %q (무작위성 결함 의심)", code)
		}
		seen[code] = true
	}
}
