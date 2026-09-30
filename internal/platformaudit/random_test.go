package platformaudit

import "testing"

// AUD-2026-032：randomHex（request_id/event_id 熵源）必须产出定长合法 hex。
func TestRandomHexProducesFixedLengthHexOutput(t *testing.T) {
	t.Parallel()
	for _, size := range []int{16, 8} {
		value := randomHex(size)
		if len(value) != size*2 {
			t.Fatalf("randomHex(%d) length = %d, want %d", size, len(value), size*2)
		}
		for i, r := range value {
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
			if !isHex {
				t.Fatalf("randomHex(%d) = %q contains non-hex rune %q at %d", size, value, r, i)
			}
		}
	}
}

// AUD-2026-032：连续调用不得重复（原实现时间戳回退会产生可预测、可碰撞的 id）。
func TestRandomHexValuesDoNotRepeatAcrossCalls(t *testing.T) {
	t.Parallel()
	const samples = 500
	seen := make(map[string]bool, samples)
	for i := 0; i < samples; i++ {
		value := randomHex(16)
		if seen[value] {
			t.Fatalf("duplicate randomHex value after %d samples: %s", i+1, value)
		}
		seen[value] = true
	}
}
