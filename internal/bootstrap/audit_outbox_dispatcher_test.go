package bootstrap

import (
	"context"
	"testing"

	platformaudit "github.com/unified-identity-auth-platform/data-analysis/internal/platformaudit"
)

type readyzAuditStub struct{}

func (readyzAuditStub) Report(context.Context, platformaudit.Event) error { return nil }

// B4：/readyz 的 audit_outbox_dispatcher 必须与"投递器是否随进程启动"同源。
// 投递器只在审计发布器非空时启动，因此空发布器必须报 disabled、非空报 running。
func TestAuditOutboxDispatcherState(t *testing.T) {
	if got := auditOutboxDispatcherState(nil); got != "disabled" {
		t.Fatalf("dispatcher state = %q, want disabled（发布器缺失时投递器不启动）", got)
	}
	if got := auditOutboxDispatcherState(readyzAuditStub{}); got != "running" {
		t.Fatalf("dispatcher state = %q, want running", got)
	}
}
