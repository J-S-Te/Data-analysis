package auditoutbox

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"gorm.io/gorm"

	platformaudit "github.com/unified-identity-auth-platform/data-analysis/internal/platformaudit"
)

// Entry 既是事务内写入的审计事件，也是投递器读取的待投递事件。
// 写入时 ID/EventID 由 Append 生成；读取时由 Store 填充。
//
// Payload 是本地审计证据（迁移 000028 的 payload JSON 列）：
// 只放可判定的标识性信息（如精确版本三元组），不放令牌、凭据或查询串原文；
// 投递器不会把它转发给平台（platformaudit.Event 没有负载字段）。
type Entry struct {
	ID, TenantID, EventID, ActorID, Action, ResourceType, ResourceID, RequestID string
	LoginIP, ClientIP, IPSource, Result                                         string
	Payload                                                                     string
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
	columns := map[string]any{
		"id": newID(), "tenant_id": entry.TenantID, "event_id": newID(),
		"actor_id": nullable(entry.ActorID), "action": entry.Action,
		"resource_type": entry.ResourceType, "resource_id": entry.ResourceID,
		"login_ip": ipBytes(entry.LoginIP), "client_ip": ipBytes(entry.ClientIP), "ip_source": ipSource,
		"result": result, "delivery_status": "pending", "attempts": 0,
		"next_attempt_at": now, "created_at": now,
	}
	// request_id/payload 来自迁移 000028。为空时整列省略（等价于写入 NULL），
	// 这样在"新二进制先于 000028 生效"的滚动窗口里，缺列不会把整条业务事务打回；
	// 有值时按真实列写入，绝不把空串或非法 JSON 塞进 MySQL JSON 列。
	if requestID := nullable(entry.RequestID); requestID != nil {
		columns["request_id"] = requestID
	}
	if payload := payloadJSON(entry.Payload); payload != nil {
		columns["payload"] = payload
	}
	return tx.Table("bi_audit_outbox").Create(columns).Error
}

// payloadJSON 只返回能通过 json.Valid 的非空负载；空串与非法 JSON 一律落 NULL。
//
// 不能把空串或非法 JSON 写进 MySQL JSON 列：那会让整条业务事务（业务变更 + 审计）
// 一起失败，把"审计证据缺失"放大成"业务不可用"。
func payloadJSON(value string) any {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return nil
	}
	return trimmed
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
