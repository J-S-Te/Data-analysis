package oidc

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// consumeCentralLogin 在行锁事务中消费state，桥接和正式回调不能并发越过检查。
// 参数：ctx为请求，state为浏览器Cookie已经核验的随机状态。
// 返回值：有效原始事务；过期、重放或存储故障返回错误。
func (s *Service) consumeCentralLogin(ctx context.Context, state string) (LoginTransaction, error) {
	var result LoginTransaction
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("state_hash = ? AND tenant_id = ? AND expires_at > ?", tokenHash(state), s.tenantID, time.Now().UTC()).Take(&result).Error; err != nil {
			return err
		}
		if len(result.CentralTicketCipher) == 0 {
			return ErrUnauthenticated
		}
		return tx.Where("state_hash = ?", result.StateHash).Delete(&LoginTransaction{}).Error
	})
	return result, err
}

// attachSessionLink 将平台票据密文写入未消费的原浏览器登录事务。
// 参数：ctx为请求，state为Cookie已验证的原始状态，ticket为一次性票据。
// 返回值：原nonce/PKCE的OIDC地址；过期、重复桥接或存储失败时返回错误。
func (s *Service) attachSessionLink(ctx context.Context, state, ticket string) (string, error) {
	if ticket == "" || len(ticket) > 128 {
		return "", ErrUnauthenticated
	}
	cipher, err := s.codec.encrypt(ticket)
	if err != nil {
		return "", ErrSessionUnavailable
	}
	var transaction LoginTransaction
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("state_hash = ? AND expires_at > ?", tokenHash(state), time.Now().UTC()).Take(&transaction).Error; err != nil {
			return err
		}
		if len(transaction.CentralTicketCipher) > 0 {
			return ErrUnauthenticated
		}
		return tx.Model(&LoginTransaction{}).Where("state_hash = ?", transaction.StateHash).Update("central_ticket_cipher", cipher).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, ErrUnauthenticated) {
		return "", ErrUnauthenticated
	}
	if err != nil {
		return "", ErrSessionUnavailable
	}
	nonce, err := s.codec.decrypt(transaction.NonceCipher)
	if err != nil {
		return "", ErrUnauthenticated
	}
	verifier, err := s.codec.decrypt(transaction.CodeVerifierCipher)
	if err != nil {
		return "", ErrUnauthenticated
	}
	return s.AuthorizationURL(state, nonce, verifier), nil
}

// stateCookie 将登录state绑定发起登录的浏览器，不以单独数据库记录代替浏览器证明。
// 参数：state为原始随机状态，maxAge为有效秒数或删除标志。
// 返回值：相同数析Cookie路径和安全属性的HttpOnly临时Cookie。
func (h *Handler) stateCookie(state string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: h.options.CookieName + "_oidc_state", Value: state, Path: h.service.PathPrefix(),
		HttpOnly: true, Secure: h.options.SecureCookie, SameSite: http.SameSiteLaxMode, MaxAge: maxAge}
}

// SessionState 仅返回当前数析Cookie绑定的非敏感中央窗口。
// 参数：c为已通过数析身份中间件的请求。
// 返回值：无；不推进活动或刷新原始绝对截止。
func (h *Handler) SessionState(c *gin.Context) { h.lifecycle(c, false) }

// lifecycle 为同源状态和活动端点执行精确Cookie关联和错误分类。
// 参数：c为请求，interact表示显式交互。
// 返回值：无；确定失效返回401，中央或存储故障503，响应不含秘密。
func (h *Handler) lifecycle(c *gin.Context, interact bool) {
	c.Header("Cache-Control", "no-store")
	if h.service.centralClient == nil {
		response.Error(c, apperror.New(503, "SESSION_NOT_ENABLED", "中央会话暂未启用"))
		return
	}
	cookie, err := c.Request.Cookie(h.options.CookieName)
	if err != nil || cookie.Value == "" {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	var session Session
	err = h.service.db.WithContext(c.Request.Context()).Where("session_id_hash = ? AND revoked_at IS NULL AND expires_at > ?", tokenHash(cookie.Value), time.Now().UTC()).Take(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	if err != nil {
		response.Error(c, apperror.New(503, "SESSION_UNAVAILABLE", "会话服务暂不可用"))
		return
	}
	state, err := h.service.readCentral(c.Request.Context(), session, interact)
	if errors.Is(err, ErrUnauthenticated) {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	if err != nil {
		response.Error(c, apperror.New(503, "SESSION_UNAVAILABLE", "会话服务暂不可用"))
		return
	}
	response.OK(c, state)
}
