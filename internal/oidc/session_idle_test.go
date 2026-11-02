package oidc

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const idleTestRawToken = "idle-test-session-token"

// newSessionIdleTestService 构造不依赖 OIDC Provider 的会话服务：
// Authenticate 与 TouchActivity 只读写本地会话表，因此用独立 SQLite 文件库覆盖双期限语义。
func newSessionIdleTestService(t *testing.T, idle, absolute time.Duration) (*Service, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "session.sqlite")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&Session{}); err != nil {
		t.Fatalf("auto migrate session: %v", err)
	}
	codec, err := newSecretCodec(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("new codec: %v", err)
	}
	return &Service{db: db, codec: codec, checkEvery: 24 * time.Hour, sessionIdleTTL: idle, sessionAbsoluteTTL: absolute}, db
}

// seedIdleTestSession 写入一条已认证会话；AuthorizationCheckedAt 取当前时间，
// 避免 Authenticate 触发平台授权复核（测试不连接外部系统）。
func seedIdleTestSession(t *testing.T, db *gorm.DB, mutate func(*Session)) Session {
	t.Helper()
	now := time.Now().UTC()
	session := Session{
		SessionIDHash:          tokenHash(idleTestRawToken),
		TenantID:               "tenant-1",
		PlatformUserID:         "user-1",
		Username:               "user.one",
		DisplayName:            "User One",
		RolesJSON:              "[\"sales\"]",
		PermissionsJSON:        "[\"dashboard.overview.view\"]",
		DataScopesJSON:         "[{\"role_code\":\"sales\",\"scope_type\":\"TENANT\",\"scope_id\":\"tenant-1\",\"environment_code\":\"dev\"}]",
		AccessTokenCipher:      []byte("cipher"),
		ExpiresAt:              now.Add(8 * time.Hour),
		AuthorizationCheckedAt: now,
		CreatedAt:              now,
		LastSeenAt:             now,
	}
	if mutate != nil {
		mutate(&session)
	}
	if err := db.Create(&session).Error; err != nil {
		t.Fatalf("create session: %v", err)
	}
	return session
}

func loadIdleTestSession(t *testing.T, db *gorm.DB) Session {
	t.Helper()
	var session Session
	if err := db.Where("session_id_hash = ?", tokenHash(idleTestRawToken)).First(&session).Error; err != nil {
		t.Fatalf("load session: %v", err)
	}
	return session
}

// TestValidateSessionTTLs 验证双期限配置的失败关闭边界：空闲 1m~24h、绝对必须大于空闲。
func TestValidateSessionTTLs(t *testing.T) {
	t.Parallel()
	valid := []struct{ idle, absolute time.Duration }{
		{time.Minute, 8 * time.Hour},
		{30 * time.Minute, 8 * time.Hour},
		{24 * time.Hour, 25 * time.Hour},
	}
	for _, value := range valid {
		if err := ValidateSessionTTLs(value.idle, value.absolute); err != nil {
			t.Errorf("ValidateSessionTTLs(%v, %v) = %v, want nil", value.idle, value.absolute, err)
		}
	}
	invalid := []struct{ idle, absolute time.Duration }{
		{0, 8 * time.Hour},
		{59 * time.Second, 8 * time.Hour},
		{25 * time.Hour, 26 * time.Hour},
		{30 * time.Minute, 30 * time.Minute},
		{30 * time.Minute, 20 * time.Minute},
	}
	for _, value := range invalid {
		if err := ValidateSessionTTLs(value.idle, value.absolute); err == nil {
			t.Errorf("ValidateSessionTTLs(%v, %v) = nil, want rejection", value.idle, value.absolute)
		}
	}
}

// TestNewSessionLifetimesUseAbsoluteTTL 验证新会话绝对到期 = now + absolute，
// 空闲计时从登录时刻开始，二者不得混用同一个硬编码 15m。
func TestNewSessionLifetimesUseAbsoluteTTL(t *testing.T) {
	t.Parallel()
	service, _ := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	now := time.Now().UTC()
	lifetimes := service.newSessionLifetimes(now)
	if !lifetimes.ExpiresAt.Equal(now.Add(8 * time.Hour)) {
		t.Fatalf("ExpiresAt = %v, want now+8h", lifetimes.ExpiresAt)
	}
	if !lifetimes.LastInteractiveAt.Equal(now) {
		t.Fatalf("LastInteractiveAt = %v, want login time", lifetimes.LastInteractiveAt)
	}
	if lifetimes.ExpiresAt.Sub(lifetimes.LastInteractiveAt) != 8*time.Hour {
		t.Fatalf("absolute lifetime = %v, want 8h", lifetimes.ExpiresAt.Sub(lifetimes.LastInteractiveAt))
	}
}

// TestSessionValidAtCoversIdleAbsoluteRevokedAndLegacyRows 覆盖有效条件：
// 未撤销、now < absolute、now < last_interactive + idle；旧行 NULL 按 created_at 兜底。
func TestSessionValidAtCoversIdleAbsoluteRevokedAndLegacyRows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	current := now
	almostIdle := now.Add(-30*time.Minute + time.Second)
	idleDeadline := now.Add(-30 * time.Minute)
	idleExpired := now.Add(-30*time.Minute - time.Second)
	revoked := now.Add(-time.Minute)
	tests := []struct {
		name    string
		session Session
		want    bool
	}{
		{name: "fresh", session: Session{ExpiresAt: now.Add(8 * time.Hour), CreatedAt: now, LastInteractiveAt: &current}, want: true},
		{name: "idle still inside window", session: Session{ExpiresAt: now.Add(8 * time.Hour), CreatedAt: now, LastInteractiveAt: &almostIdle}, want: true},
		{name: "idle exact deadline", session: Session{ExpiresAt: now.Add(8 * time.Hour), CreatedAt: now, LastInteractiveAt: &idleDeadline}, want: false},
		{name: "idle expired", session: Session{ExpiresAt: now.Add(8 * time.Hour), CreatedAt: now, LastInteractiveAt: &idleExpired}, want: false},
		{name: "absolute expired even active", session: Session{ExpiresAt: now.Add(-time.Second), CreatedAt: now, LastInteractiveAt: &current}, want: false},
		{name: "revoked", session: Session{ExpiresAt: now.Add(8 * time.Hour), CreatedAt: now, LastInteractiveAt: &current, RevokedAt: &revoked}, want: false},
		{name: "legacy row null interactive uses created_at", session: Session{ExpiresAt: now.Add(8 * time.Hour), CreatedAt: now.Add(-20 * time.Minute)}, want: true},
		{name: "legacy row null interactive created long ago", session: Session{ExpiresAt: now.Add(8 * time.Hour), CreatedAt: now.Add(-31 * time.Minute)}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.session.ValidAt(now, 30*time.Minute); got != test.want {
				t.Fatalf("ValidAt() = %v, want %v", got, test.want)
			}
		})
	}
}

// TestSessionIdleRemainingCappedByAbsoluteExpiry 验证剩余时长取 min(空闲截止, 绝对截止)。
func TestSessionIdleRemainingCappedByAbsoluteExpiry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	fresh := now
	session := Session{ExpiresAt: now.Add(5 * time.Minute), CreatedAt: now, LastInteractiveAt: &fresh}
	if got := session.IdleRemaining(now, 30*time.Minute); got != 5*time.Minute {
		t.Fatalf("IdleRemaining() = %v, want absolute cap 5m", got)
	}
	expired := Session{ExpiresAt: now.Add(-time.Minute), CreatedAt: now, LastInteractiveAt: &fresh}
	if got := expired.IdleRemaining(now, 30*time.Minute); got != 0 {
		t.Fatalf("IdleRemaining() = %v, want 0", got)
	}
}

// TestAuthenticateRejectsIdleExpiredSessionWithoutReviving 验证空闲到期后普通请求 401，
// 且不刷新 last_interactive_at/last_seen_at，不得复活会话。
func TestAuthenticateRejectsIdleExpiredSessionWithoutReviving(t *testing.T) {
	t.Parallel()
	service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	idleExpiredAt := time.Now().UTC().Add(-31 * time.Minute)
	seedIdleTestSession(t, db, func(session *Session) {
		session.LastInteractiveAt = &idleExpiredAt
		session.CreatedAt = idleExpiredAt
	})
	before := loadIdleTestSession(t, db)
	if _, err := service.Authenticate(context.Background(), idleTestRawToken); err == nil {
		t.Fatal("Authenticate() = nil error, want idle expiry rejection")
	}
	after := loadIdleTestSession(t, db)
	if after.LastInteractiveAt == nil || !after.LastInteractiveAt.Equal(idleExpiredAt) {
		t.Fatalf("idle-expired session was revived: LastInteractiveAt = %v", after.LastInteractiveAt)
	}
	if !after.LastSeenAt.Equal(before.LastSeenAt) {
		t.Fatalf("idle-expired request refreshed last_seen_at: %v -> %v", before.LastSeenAt, after.LastSeenAt)
	}
}

// TestAuthenticateRejectsAbsoluteExpiredSessionEvenWhenActive 验证绝对到期不可被活动续期绕过。
func TestAuthenticateRejectsAbsoluteExpiredSessionEvenWhenActive(t *testing.T) {
	t.Parallel()
	service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	seedIdleTestSession(t, db, func(session *Session) {
		session.ExpiresAt = time.Now().UTC().Add(-time.Second)
	})
	if _, err := service.Authenticate(context.Background(), idleTestRawToken); err == nil {
		t.Fatal("Authenticate() = nil error, want absolute expiry rejection")
	}
}

// TestAuthenticateOnNormalRequestDoesNotExtendIdle 验证普通业务请求只刷新 last_seen_at，
// 不推进 last_interactive_at 与绝对到期。
func TestAuthenticateOnNormalRequestDoesNotExtendIdle(t *testing.T) {
	t.Parallel()
	service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	now := time.Now().UTC()
	lastInteractive := now.Add(-20 * time.Minute)
	absoluteExpiry := now.Add(4 * time.Hour)
	seedIdleTestSession(t, db, func(session *Session) {
		session.LastInteractiveAt = &lastInteractive
		session.CreatedAt = lastInteractive
		session.ExpiresAt = absoluteExpiry
	})
	principal, err := service.Authenticate(context.Background(), idleTestRawToken)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if principal.IdleTimeoutSeconds != 1800 {
		t.Fatalf("IdleTimeoutSeconds = %d, want 1800", principal.IdleTimeoutSeconds)
	}
	// 空闲截止 = last_interactive + 30m，剩余约 10m。
	if principal.IdleRemainingMS < int64((10*time.Minute-5*time.Second)/time.Millisecond) || principal.IdleRemainingMS > int64(10*time.Minute/time.Millisecond) {
		t.Fatalf("IdleRemainingMS = %d, want about 10m", principal.IdleRemainingMS)
	}
	after := loadIdleTestSession(t, db)
	if after.LastInteractiveAt == nil || !after.LastInteractiveAt.Equal(lastInteractive) {
		t.Fatalf("normal request extended idle: LastInteractiveAt = %v, want %v", after.LastInteractiveAt, lastInteractive)
	}
	if !after.ExpiresAt.Equal(absoluteExpiry) {
		t.Fatalf("normal request extended absolute expiry: %v, want %v", after.ExpiresAt, absoluteExpiry)
	}
	if after.LastSeenAt.Before(now) {
		t.Fatalf("last_seen_at not refreshed: %v", after.LastSeenAt)
	}
}

// TestTouchActivityRenewsIdleButNotAbsoluteExpiry 验证显式活动续期空闲计时且不延长绝对期限。
func TestTouchActivityRenewsIdleButNotAbsoluteExpiry(t *testing.T) {
	t.Parallel()
	service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	now := time.Now().UTC()
	lastInteractive := now.Add(-29 * time.Minute)
	absoluteExpiry := now.Add(6 * time.Hour)
	seedIdleTestSession(t, db, func(session *Session) {
		session.LastInteractiveAt = &lastInteractive
		session.CreatedAt = lastInteractive
		session.ExpiresAt = absoluteExpiry
	})
	result, err := service.TouchActivity(context.Background(), idleTestRawToken)
	if err != nil {
		t.Fatalf("TouchActivity() error = %v", err)
	}
	if result.IdleTimeout != 30*time.Minute {
		t.Fatalf("IdleTimeout = %v, want 30m", result.IdleTimeout)
	}
	if result.IdleRemaining < 29*time.Minute || result.IdleRemaining > 30*time.Minute {
		t.Fatalf("IdleRemaining = %v, want about 30m", result.IdleRemaining)
	}
	after := loadIdleTestSession(t, db)
	if after.LastInteractiveAt == nil || !after.LastInteractiveAt.After(lastInteractive) {
		t.Fatalf("TouchActivity() did not advance last_interactive_at: %v", after.LastInteractiveAt)
	}
	if !after.ExpiresAt.Equal(absoluteExpiry) {
		t.Fatalf("TouchActivity() extended absolute expiry: %v, want %v", after.ExpiresAt, absoluteExpiry)
	}
	if _, err := service.Authenticate(context.Background(), idleTestRawToken); err != nil {
		t.Fatalf("Authenticate() after activity error = %v", err)
	}
}

// TestTouchActivityCapsRemainingAtAbsoluteExpiry 验证活动续期后剩余不超过绝对到期。
func TestTouchActivityCapsRemainingAtAbsoluteExpiry(t *testing.T) {
	t.Parallel()
	service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	now := time.Now().UTC()
	lastInteractive := now.Add(-time.Minute)
	seedIdleTestSession(t, db, func(session *Session) {
		session.LastInteractiveAt = &lastInteractive
		session.CreatedAt = lastInteractive
		session.ExpiresAt = now.Add(5 * time.Minute)
	})
	result, err := service.TouchActivity(context.Background(), idleTestRawToken)
	if err != nil {
		t.Fatalf("TouchActivity() error = %v", err)
	}
	if result.IdleRemaining > 5*time.Minute || result.IdleRemaining < 4*time.Minute {
		t.Fatalf("IdleRemaining = %v, want capped near 5m", result.IdleRemaining)
	}
}

// TestTouchActivityRejectsInactiveSessionsWithoutReviving 覆盖空闲到期、绝对到期、已撤销会话。
func TestTouchActivityRejectsInactiveSessionsWithoutReviving(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	idleExpiredAt := now.Add(-31 * time.Minute)
	revokedAt := now.Add(-time.Minute)
	cases := map[string]func(*Session){
		"idle expired": func(session *Session) {
			session.LastInteractiveAt = &idleExpiredAt
			session.CreatedAt = idleExpiredAt
		},
		"absolute expired": func(session *Session) {
			session.ExpiresAt = now.Add(-time.Second)
		},
		"revoked": func(session *Session) {
			session.RevokedAt = &revokedAt
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
			seedIdleTestSession(t, db, mutate)
			before := loadIdleTestSession(t, db)
			if _, err := service.TouchActivity(context.Background(), idleTestRawToken); !errors.Is(err, ErrSessionInactive) {
				t.Fatalf("TouchActivity() error = %v, want ErrSessionInactive", err)
			}
			after := loadIdleTestSession(t, db)
			if !after.LastSeenAt.Equal(before.LastSeenAt) {
				t.Fatalf("inactive session was touched: last_seen_at %v -> %v", before.LastSeenAt, after.LastSeenAt)
			}
			if before.LastInteractiveAt != nil && (after.LastInteractiveAt == nil || !after.LastInteractiveAt.Equal(*before.LastInteractiveAt)) {
				t.Fatalf("inactive session was revived: %v -> %v", before.LastInteractiveAt, after.LastInteractiveAt)
			}
		})
	}
}

// TestNullLastInteractiveCompatibility 验证升级前的 NULL 行按 created_at 计算空闲，
// 不因升级立刻锁死在线用户，也不会把旧会话放大到超出空闲窗口。
func TestNullLastInteractiveCompatibility(t *testing.T) {
	t.Parallel()
	service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	createdAt := time.Now().UTC().Add(-20 * time.Minute)
	seedIdleTestSession(t, db, func(session *Session) {
		session.LastInteractiveAt = nil
		session.CreatedAt = createdAt
	})
	principal, err := service.Authenticate(context.Background(), idleTestRawToken)
	if err != nil {
		t.Fatalf("Authenticate() on legacy row error = %v", err)
	}
	if principal.IdleRemainingMS < int64((10*time.Minute-5*time.Second)/time.Millisecond) || principal.IdleRemainingMS > int64(10*time.Minute/time.Millisecond) {
		t.Fatalf("legacy row IdleRemainingMS = %d, want about 10m", principal.IdleRemainingMS)
	}

	staleService, staleDB := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	staleCreatedAt := time.Now().UTC().Add(-31 * time.Minute)
	seedIdleTestSession(t, staleDB, func(session *Session) {
		session.LastInteractiveAt = nil
		session.CreatedAt = staleCreatedAt
	})
	if _, err := staleService.Authenticate(context.Background(), idleTestRawToken); err == nil {
		t.Fatal("Authenticate() on stale legacy row = nil error, want idle rejection")
	}
}
