package auditoutbox

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"net"
	"strings"
	"time"

	"gorm.io/gorm"

	platformaudit "github.com/unified-identity-auth-platform/data-analysis/internal/platformaudit"
)

// Entry 既是事务内写入的审计事件，也是投递器读取的待投递事件。
// 写入时 ID/EventID 由 Append 生成；读取时由 Store 填充。
type Entry struct {
	ID, TenantID, EventID, ActorID, Action, ResourceType, ResourceID, RequestID string
	LoginIP, ClientIP, IPSource, Result                                         string
	Attempts                                                                    uint
}

// Append 把审计事件写入 outbox；必须由已开启的业务事务调用，
// 使业务变更与审计同成同败。
func Append(tx *gorm.DB, entry Entry) error {
	now := time.Now().UTC()
	ipSource := entry.IPSource
	if ipSource == "" {
		ipSource = "UNKNOWN"
	}
	result := entry.Result
	if result == "" {
		result = "SUCCESS"
	}
	return tx.Table("bi_audit_outbox").Create(map[string]any{
		"id": newID(), "tenant_id": entry.TenantID, "event_id": newID(),
		"actor_id": nullable(entry.ActorID), "action": entry.Action,
		"resource_type": entry.ResourceType, "resource_id": entry.ResourceID,
		"login_ip": ipBytes(entry.LoginIP), "client_ip": ipBytes(entry.ClientIP), "ip_source": ipSource,
		"result": result, "delivery_status": "pending", "attempts": 0,
		"next_attempt_at": now, "created_at": now,
	}).Error
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func ipBytes(value string) []byte {
	parsed := net.ParseIP(strings.TrimSpace(value))
	if parsed == nil {
		return nil
	}
	if ipv4 := parsed.To4(); ipv4 != nil {
		return append([]byte(nil), ipv4...)
	}
	return append([]byte(nil), parsed.To16()...)
}

var _ = platformaudit.Event{}

// newID 生成 26 字符以内的稳定标识（base32，无填充）。
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
}
