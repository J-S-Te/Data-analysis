package middleware

import (
	"bufio"
	"bytes"
	"net"
	"net/http"

	"github.com/gin-gonic/gin"
)

// auditBuffer 缓冲强制审计模式下不安全方法（写请求）的响应。
//
// 安全理由（SEC-D4b）：gin 的审计中间件在业务 handler 执行完毕后才能拿到
// Report 的结果，而此时业务响应往往已经写给客户端、无法撤回。只有先把响应
// 缓冲在内存里，才能在审计事件写入失败时丢弃业务响应并返回 503，保证
// “审计写不进去 ⇒ 客户端拿不到成功”，杜绝强制审计模式下的静默无审计写入。
// 仅在 PLATFORM_AUDIT_REQUIRED/prod 强制模式下启用，非强制模式行为不变。
type auditBuffer struct {
	orig    gin.ResponseWriter
	headers http.Header
	body    bytes.Buffer
	status  int
	size    int
}

func newAuditBuffer(orig gin.ResponseWriter) *auditBuffer {
	return &auditBuffer{orig: orig, headers: make(http.Header), status: http.StatusOK, size: -1}
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
	b.WriteHeaderNow()
	n, err := b.body.Write(p)
	b.size += n
	return n, err
}

func (b *auditBuffer) WriteString(s string) (int, error) {
	b.WriteHeaderNow()
	n, err := b.body.WriteString(s)
	b.size += n
	return n, err
}

func (b *auditBuffer) Status() int   { return b.status }
func (b *auditBuffer) Size() int     { return b.size }
func (b *auditBuffer) Written() bool { return b.size != -1 }

// Flush 只标记“已写”，不能把半成品响应提前提交到真实连接，
// 否则审计失败时将无法撤回，拒绝语义会被破坏。
func (b *auditBuffer) Flush() { b.WriteHeaderNow() }

func (b *auditBuffer) Pusher() http.Pusher { return b.orig.Pusher() }

func (b *auditBuffer) Hijack() (net.Conn, *bufio.ReadWriter, error) { return b.orig.Hijack() }

func (b *auditBuffer) CloseNotify() <-chan bool { return b.orig.CloseNotify() }

func (b *auditBuffer) Unwrap() http.ResponseWriter { return b.orig }
