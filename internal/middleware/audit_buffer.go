package middleware

import (
	"bufio"
	"bytes"
	"log/slog"
	"net"
	"net/http"

	"github.com/gin-gonic/gin"
)

// maxAuditBufferedResponseBytes 是强制审计模式下单请求响应缓冲上限（对齐 platform
// maxPlatformAuditBufferedResponseBytes 的 64KiB 口径）：超过即降级为旧行为
// （write-through）并告警，避免异常大的响应长期占用内存。
const maxAuditBufferedResponseBytes = 64 << 10

// auditBuffer 缓冲强制审计模式下“将被记录审计事件”请求（不安全方法写请求与敏感读，
// AUD-2026-006）的响应。
//
// 安全理由（SEC-D4b / AUD-2026-006）：gin 的审计中间件在业务 handler 执行完毕后才能
// 拿到 Report 的结果，而此时业务响应往往已经写给客户端、无法撤回。敏感读同样如此——
// 例如 GET /embed/:dashboard 的一次性嵌入令牌签发，若不缓冲，Report 失败时的 503 会
// 追加在已提交响应之后，令牌早已发给客户端。只有先把响应缓冲在内存里，才能在审计事件
// 写入失败时丢弃业务响应并返回 503，保证“审计写不进去 ⇒ 客户端拿不到成功”。
// 仅在 PLATFORM_AUDIT_REQUIRED/prod 强制模式下启用，非强制模式行为不变。
type auditBuffer struct {
	orig    gin.ResponseWriter
	headers http.Header
	body    bytes.Buffer
	status  int
	size    int
	// limit 是响应体缓冲上限；超过即降级为旧行为（write-through）并输出告警，
	// 防止异常大的响应长期占用内存。降级后审计失败无法再撤回响应，属于内存保护
	// 与拒绝语义之间的显式取舍：宁可对超大响应退回 fail-open，也不给无上限缓冲
	// 留下 DoS 面，可见性由告警日志兜底。
	limit      int
	overflowed bool
	logger     *slog.Logger
}

func newAuditBuffer(orig gin.ResponseWriter, limit int, logger *slog.Logger) *auditBuffer {
	if logger == nil {
		logger = slog.Default()
	}
	return &auditBuffer{orig: orig, headers: make(http.Header), status: http.StatusOK, size: -1, limit: limit, logger: logger}
}

// flushTo 在审计上报成功后把缓冲的响应提交到真实连接：
// 先搬响应头，再写状态码与响应体，语义与 gin 自带 responseWriter 一致。
func (b *auditBuffer) flushTo(dst gin.ResponseWriter) {
	b.WriteHeaderNow()
	for key, values := range b.headers {
		dst.Header().Del(key)
		for _, value := range values {
			dst.Header().Add(key, value)
		}
	}
	dst.WriteHeader(b.status)
	if b.body.Len() > 0 {
		_, _ = dst.Write(b.body.Bytes())
		return
	}
	dst.WriteHeaderNow()
}

// degradeToWriteThrough 在缓冲超限时把已缓冲内容一次性提交到真实连接并切换为直写，
// 降级为旧行为（响应无法再被审计失败撤回），同时输出告警日志。
func (b *auditBuffer) degradeToWriteThrough() {
	b.overflowed = true
	b.flushTo(b.orig)
	b.logger.Warn("data_analysis audit buffered response exceeded limit; degrading to write-through",
		"limit_bytes", b.limit, "status", b.status)
}

// —— 以下实现 gin.ResponseWriter 接口，行为对齐 gin 的 responseWriter。 ——

func (b *auditBuffer) Header() http.Header { return b.headers }

func (b *auditBuffer) WriteHeader(code int) {
	if code > 0 && b.status != code && !b.Written() {
		b.status = code
	}
}

func (b *auditBuffer) WriteHeaderNow() {
	if !b.Written() {
		b.size = 0
	}
}

func (b *auditBuffer) Write(p []byte) (int, error) {
	if !b.overflowed && b.body.Len()+len(p) > b.limit {
		b.degradeToWriteThrough()
	}
	if b.overflowed {
		return b.orig.Write(p)
	}
	b.WriteHeaderNow()
	n, err := b.body.Write(p)
	b.size += n
	return n, err
}

func (b *auditBuffer) WriteString(s string) (int, error) {
	return b.Write([]byte(s))
}

func (b *auditBuffer) Status() int   { return b.status }
func (b *auditBuffer) Size() int     { return b.size }
func (b *auditBuffer) Written() bool { return b.size != -1 }

// Flush 只标记“已写”，不能把半成品响应提前提交到真实连接，
// 否则审计失败时将无法撤回，拒绝语义会被破坏；超限直写模式下透传给真实连接。
func (b *auditBuffer) Flush() {
	if b.overflowed {
		b.orig.Flush()
		return
	}
	b.WriteHeaderNow()
}

func (b *auditBuffer) Pusher() http.Pusher { return b.orig.Pusher() }

func (b *auditBuffer) Hijack() (net.Conn, *bufio.ReadWriter, error) { return b.orig.Hijack() }

func (b *auditBuffer) CloseNotify() <-chan bool { return b.orig.CloseNotify() }

func (b *auditBuffer) Unwrap() http.ResponseWriter { return b.orig }
