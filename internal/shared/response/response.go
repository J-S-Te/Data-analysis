// Package response 统一响应信封（对齐 CRM internal/shared/response）。
package response

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
)

type envelope struct {
	Code      string      `json:"code"`
	Message   string      `json:"message"`
	RequestID string      `json:"request_id"`
	Data      interface{} `json:"data,omitempty"`
}

func OK(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, envelope{Code: "OK", Message: "success", RequestID: requestID(c), Data: data})
}

func Error(c *gin.Context, err error) {
	if appErr, ok := apperror.AsAppError(err); ok {
		c.JSON(appErr.Status, envelope{Code: appErr.Code, Message: appErr.Message, RequestID: requestID(c)})
		return
	}
	c.JSON(http.StatusInternalServerError, envelope{Code: "INTERNAL_ERROR", Message: "internal error", RequestID: requestID(c)})
}

func requestID(c *gin.Context) string {
	if value := c.GetHeader("X-Request-ID"); value != "" {
		return value
	}
	return c.GetString("request_id")
}
