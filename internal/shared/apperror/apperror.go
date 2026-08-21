// Package apperror 定义统一业务错误（对齐 CRM internal/shared/apperror）。
package apperror

import (
	"errors"
	"fmt"
	"net/http"
)

// AppError 是统一业务错误结构体，包含 HTTP 状态码、业务错误码与展示消息。
type AppError struct {
	Status  int
	Code    string
	Message string
}

// Error 实现 error 接口，返回面向日志与响应的错误文本。
func (e *AppError) Error() string { return e.Message }

// New 构造标准化的 AppError。
// status 对应 HTTP 状态码、code 对应业务错误码、message 对应返回文案，返回值可直接透传给网关响应层。
func New(status int, code, message string) *AppError {
	return &AppError{Status: status, Code: code, Message: message}
}

// Wrap 在已有错误上追加业务上下文并返回 AppError。
// 常用于保留底层错误细节而不丢失统一错误码语义。
func Wrap(err error, status int, code, message string) *AppError {
	return &AppError{Status: status, Code: code, Message: fmt.Sprintf("%s: %v", message, err)}
}

var (
	ErrUnauthenticated = New(http.StatusUnauthorized, "UNAUTHENTICATED", "未登录或会话已失效")
	ErrForbidden       = New(http.StatusForbidden, "FORBIDDEN", "无权限执行该操作")
	ErrUnavailable     = New(http.StatusServiceUnavailable, "COMMON_DEPENDENCY_UNAVAILABLE", "依赖服务暂时不可用")
)

// AsAppError 尝试将任意错误转换为 *AppError，返回 (nil, false) 表示不匹配。
func AsAppError(err error) (*AppError, bool) {
	var target *AppError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}
