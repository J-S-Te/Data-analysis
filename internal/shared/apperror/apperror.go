// Package apperror 定义统一业务错误（对齐 CRM internal/shared/apperror）。
package apperror

import (
	"errors"
	"fmt"
	"net/http"
)

type AppError struct {
	Status  int
	Code    string
	Message string
}

func (e *AppError) Error() string { return e.Message }

func New(status int, code, message string) *AppError {
	return &AppError{Status: status, Code: code, Message: message}
}

func Wrap(err error, status int, code, message string) *AppError {
	return &AppError{Status: status, Code: code, Message: fmt.Sprintf("%s: %v", message, err)}
}

var (
	ErrUnauthenticated = New(http.StatusUnauthorized, "UNAUTHENTICATED", "未登录或会话已失效")
	ErrForbidden       = New(http.StatusForbidden, "FORBIDDEN", "无权限执行该操作")
	ErrUnavailable     = New(http.StatusServiceUnavailable, "COMMON_DEPENDENCY_UNAVAILABLE", "依赖服务暂时不可用")
)

func AsAppError(err error) (*AppError, bool) {
	var target *AppError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}
