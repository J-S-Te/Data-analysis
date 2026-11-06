package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/auditoutbox"
)

// textContentType 是 Prometheus text exposition 的响应类型。
const textContentType = "text/plain; version=0.0.4; charset=utf-8"

// OutboxCounter 读取事务内审计 outbox 的三态计数（由 auditoutbox.GORMStore 实现）。
type OutboxCounter interface {
	Counts(ctx context.Context) (auditoutbox.OutboxCounts, error)
}

// Handler 渲染 dashboard-api 的只读文本指标。
type Handler struct {
	// Outbox 读取审计 outbox 计数；nil 时只输出进程指标并把计数错误置 1
	// （不能因为没接上 store 就让抓取端以为“没有积压”）。
	Outbox OutboxCounter
	// StartedAt 是进程启动时间，用于 progress/重启可观测。
	StartedAt time.Time
	// Logger 记录计数查询失败；nil 时用 slog.Default。
	Logger *slog.Logger
}

// Handle 是 gin 处理函数：渲染 exposition 文本。
// 参数：c 为抓取请求，其上下文作为 outbox 查询的生命周期。
// 返回值：无；始终以 200 + text/plain 返回，查询失败通过
// data_analysis_audit_outbox_counts_error=1 暴露（抓取成功≠数据可信）。
func (h *Handler) Handle(c *gin.Context) {
	var builder strings.Builder
	counts, countsErr := h.counts(c.Request.Context())
	errorValue := 0
	if countsErr != nil {
		errorValue = 1
		h.logger().ErrorContext(c.Request.Context(), "metrics: read audit outbox counts", "error", countsErr)
	}
	writeGauge(&builder, "data_analysis_audit_outbox_pending",
		"Audit outbox events still in the delivery chain (pending, retry or processing).", counts.Pending)
	writeGauge(&builder, "data_analysis_audit_outbox_delivered",
		"Audit outbox events delivered to the platform audit API.", counts.Delivered)
	writeGauge(&builder, "data_analysis_audit_outbox_dead",
		"Audit outbox events parked as failed after exhausting retries.", counts.Dead)
	writeGauge(&builder, "data_analysis_audit_outbox_counts_error",
		"1 when this scrape could not read audit outbox counts.", int64(errorValue))

	builder.WriteString("# HELP data_analysis_build_info Build target of this process.\n")
	builder.WriteString("# TYPE data_analysis_build_info gauge\n")
	builder.WriteString(fmt.Sprintf("data_analysis_build_info{go_version=%q,goos=%q,goarch=%q} 1\n",
		runtime.Version(), runtime.GOOS, runtime.GOARCH))
	startedAt := h.StartedAt
	if !startedAt.IsZero() {
		writeGauge(&builder, "data_analysis_process_start_time_seconds",
			"Start time of the process since unix epoch in seconds.", startedAt.Unix())
	}
	writeGauge(&builder, "data_analysis_process_goroutines",
		"Number of goroutines that currently exist.", int64(runtime.NumGoroutine()))

	c.Data(http.StatusOK, textContentType, []byte(builder.String()))
}

// counts 读取 outbox 计数；store 未注入时返回显式错误。
// 参数：ctx 为抓取请求上下文。
// 返回值：三态计数与错误。
func (h *Handler) counts(ctx context.Context) (auditoutbox.OutboxCounts, error) {
	if h.Outbox == nil {
		return auditoutbox.OutboxCounts{}, fmt.Errorf("audit outbox counter is not configured")
	}
	return h.Outbox.Counts(ctx)
}

// logger 返回结构化日志器。
// 参数：无。
// 返回值：Handler.Logger 或默认日志器。
func (h *Handler) logger() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// writeGauge 输出一个无标签 gauge 的 HELP/TYPE/样本三行。
// 参数：builder 为输出缓冲；name/help 为指标名与说明；value 为样本值。
// 返回值：无。
func writeGauge(builder *strings.Builder, name, help string, value int64) {
	builder.WriteString("# HELP " + name + " " + help + "\n")
	builder.WriteString("# TYPE " + name + " gauge\n")
	builder.WriteString(name + " " + strconv.FormatInt(value, 10) + "\n")
}
