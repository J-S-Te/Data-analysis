package bootstrap

import (
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/oidc"
)

// 与 config_test.go 的常量互不重复，聚焦会话双期限配置装载。
const sessionTestCodecKeyHex = "1f0e0d0c0b0a09080706050403020100f1e2d3c4b5a697887766554433221100"

func clearSessionTTLEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OIDC_SESSION_IDLE_TTL", "")
	t.Setenv("OIDC_SESSION_ABSOLUTE_TTL", "")
	t.Setenv("OIDC_SESSION_TTL", "")
}

// TestLoadConfigSessionTTLDefaults 验证未配置时采用空闲 30m + 绝对 8h。
func TestLoadConfigSessionTTLDefaults(t *testing.T) {
	t.Setenv("OIDC_CODEC_KEY", sessionTestCodecKeyHex)
	clearSessionTTLEnv(t)
	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.OIDCSessionIdleTTL != oidc.DefaultSessionIdleTTL {
		t.Fatalf("OIDCSessionIdleTTL = %v, want %v", config.OIDCSessionIdleTTL, oidc.DefaultSessionIdleTTL)
	}
	if config.OIDCSessionAbsoluteTTL != oidc.DefaultSessionAbsoluteTTL {
		t.Fatalf("OIDCSessionAbsoluteTTL = %v, want %v", config.OIDCSessionAbsoluteTTL, oidc.DefaultSessionAbsoluteTTL)
	}
}

// TestLoadConfigSessionTTLExplicit 验证显式配置被解析并采用。
func TestLoadConfigSessionTTLExplicit(t *testing.T) {
	t.Setenv("OIDC_CODEC_KEY", sessionTestCodecKeyHex)
	clearSessionTTLEnv(t)
	t.Setenv("OIDC_SESSION_IDLE_TTL", "45m")
	t.Setenv("OIDC_SESSION_ABSOLUTE_TTL", "12h")
	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.OIDCSessionIdleTTL != 45*time.Minute || config.OIDCSessionAbsoluteTTL != 12*time.Hour {
		t.Fatalf("session TTLs = %v/%v, want 45m/12h", config.OIDCSessionIdleTTL, config.OIDCSessionAbsoluteTTL)
	}
}

// TestLoadConfigSessionTTLLegacyKey 验证旧 OIDC_SESSION_TTL 在新绝对键缺省时作为绝对期限，
// 不会被静默放大成 8h 默认值。
func TestLoadConfigSessionTTLLegacyKey(t *testing.T) {
	t.Setenv("OIDC_CODEC_KEY", sessionTestCodecKeyHex)
	clearSessionTTLEnv(t)
	t.Setenv("OIDC_SESSION_TTL", "2h")
	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.OIDCSessionAbsoluteTTL != 2*time.Hour {
		t.Fatalf("OIDCSessionAbsoluteTTL = %v, want legacy 2h", config.OIDCSessionAbsoluteTTL)
	}
	if config.OIDCSessionIdleTTL != oidc.DefaultSessionIdleTTL {
		t.Fatalf("OIDCSessionIdleTTL = %v, want default 30m", config.OIDCSessionIdleTTL)
	}
}

// TestLoadConfigSessionTTLNewKeyWinsOverLegacy 验证新旧键同时存在时以新键为准。
func TestLoadConfigSessionTTLNewKeyWinsOverLegacy(t *testing.T) {
	t.Setenv("OIDC_CODEC_KEY", sessionTestCodecKeyHex)
	clearSessionTTLEnv(t)
	t.Setenv("OIDC_SESSION_TTL", "2h")
	t.Setenv("OIDC_SESSION_ABSOLUTE_TTL", "6h")
	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.OIDCSessionAbsoluteTTL != 6*time.Hour {
		t.Fatalf("OIDCSessionAbsoluteTTL = %v, want new key 6h", config.OIDCSessionAbsoluteTTL)
	}
}

// TestLoadConfigRejectsInvalidSessionTTLs 验证无效双期限配置在启动期失败关闭。
func TestLoadConfigRejectsInvalidSessionTTLs(t *testing.T) {
	tests := []struct {
		name     string
		idle     string
		absolute string
		legacy   string
	}{
		{name: "idle too short", idle: "30s", absolute: "8h"},
		{name: "idle too long", idle: "25h", absolute: "26h"},
		{name: "absolute not longer than idle", idle: "30m", absolute: "20m"},
		{name: "absolute equals idle", idle: "30m", absolute: "30m"},
		{name: "invalid idle duration", idle: "half-hour", absolute: "8h"},
		{name: "invalid absolute duration", idle: "30m", absolute: "eight-hours"},
		{name: "legacy smaller than idle window", legacy: "15m"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("OIDC_CODEC_KEY", sessionTestCodecKeyHex)
			clearSessionTTLEnv(t)
			t.Setenv("OIDC_SESSION_IDLE_TTL", test.idle)
			t.Setenv("OIDC_SESSION_ABSOLUTE_TTL", test.absolute)
			t.Setenv("OIDC_SESSION_TTL", test.legacy)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("LoadConfig() = nil error, want fail-closed rejection")
			}
		})
	}
}
