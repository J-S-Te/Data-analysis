package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

const hexDigits = "0123456789abcdef"

func assertValidHexID(t *testing.T, value string, wantLength int) {
	t.Helper()
	if len(value) != wantLength {
		t.Fatalf("id length = %d, want %d", len(value), wantLength)
	}
	for i, r := range value {
		if !strings.ContainsRune(hexDigits, r) {
			t.Fatalf("id %q contains non-hex rune %q at %d", value, r, i)
		}
	}
}

// AUD-2026-032：randomHex 必须产出定长合法 hex（签名与输出格式保持不变）。
func TestRandomHexProducesFixedLengthHexOutput(t *testing.T) {
	assertValidHexID(t, randomHex(16), 32)
	assertValidHexID(t, randomHex(8), 16)
}

// AUD-2026-032：连续调用不得重复（原时间种子 LCG 可预测且高频碰撞，这里抽样验证熵源）。
func TestRandomHexValuesDoNotRepeatAcrossCalls(t *testing.T) {
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

// AUD-2026-032：RequestID 注入行为保持不变——客户端未提供 X-Request-ID 时生成
// 32 位 hex 追踪号；提供了则原样回显（auth.go 回显逻辑未改动）。
func TestRequestIDGeneratesHexAndEchoesClientHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestID())
	router.GET("/ping", func(c *gin.Context) { c.Status(http.StatusOK) })

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ping", nil))
	assertValidHexID(t, recorder.Header().Get("X-Request-ID"), 32)

	const clientID = "client-provided-trace-id"
	recorder = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/ping", nil)
	request.Header.Set("X-Request-ID", clientID)
	router.ServeHTTP(recorder, request)
	if got := recorder.Header().Get("X-Request-ID"); got != clientID {
		t.Fatalf("X-Request-ID = %q, want client-supplied %q echoed", got, clientID)
	}
}
