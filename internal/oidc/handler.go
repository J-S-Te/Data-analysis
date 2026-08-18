package oidc

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

// HTTPOptions 控制 cookie 与同源约束。
type HTTPOptions struct {
	CookieName   string
	PublicOrigin string
	SecureCookie bool
}

type Handler struct {
	service *Service
	options HTTPOptions
}

func NewHandler(service *Service, options HTTPOptions) *Handler {
	return &Handler{service: service, options: options}
}

// Login 生成 state/nonce/PKCE 并跳转 Keycloak（接入手册 §3.1）。
func (h *Handler) Login(c *gin.Context) {
	state, err := GenerateState()
	if err != nil {
		response.Error(c, apperror.Wrap(err, http.StatusInternalServerError, "AUTH_STATE_FAILED", "failed to create login state"))
		return
	}
	nonce, err := GenerateState()
	if err != nil {
		response.Error(c, apperror.Wrap(err, http.StatusInternalServerError, "AUTH_NONCE_FAILED", "failed to create nonce"))
		return
	}
	verifier := oauth2Verifier()
	returnPath := c.Query("return_to")
	if returnPath == "" {
		returnPath = h.service.PathPrefix() + "/"
	}
	if err := h.service.SaveLoginTransaction(state, nonce, verifier, returnPath); err != nil {
		response.Error(c, apperror.Wrap(err, http.StatusInternalServerError, "AUTH_STATE_STORE_FAILED", "failed to persist login state"))
		return
	}
	c.Redirect(http.StatusFound, h.service.AuthorizationURL(state, nonce, verifier))
}

// Callback 校验 code/state/nonce，创建会话并重定向回原页面。
func (h *Handler) Callback(c *gin.Context) {
	state := c.Query("state")
	code := c.Query("code")
	if state == "" || code == "" {
		response.Error(c, apperror.New(http.StatusBadRequest, "AUTH_CALLBACK_INVALID", "missing state or code"))
		return
	}
	transaction, err := h.service.ConsumeLoginTransaction(c.Request.Context(), state)
	if err != nil {
		response.Error(c, apperror.New(http.StatusBadRequest, "AUTH_STATE_REPLAY", "invalid or replayed state"))
		return
	}
	nonce, err := h.service.DecryptNonce(transaction.NonceCipher)
	if err != nil {
		response.Error(c, apperror.New(http.StatusInternalServerError, "AUTH_NONCE_DECRYPT", "failed to decrypt nonce"))
		return
	}
	verifier, err := h.service.DecryptVerifier(transaction.CodeVerifierCipher)
	if err != nil {
		response.Error(c, apperror.New(http.StatusInternalServerError, "AUTH_VERIFIER_DECRYPT", "failed to decrypt verifier"))
		return
	}
	sessionToken, _, err := h.service.ExchangeAndCreateSession(c.Request.Context(), code, verifier, nonce, transaction)
	if err != nil {
		response.Error(c, apperror.Wrap(err, http.StatusUnauthorized, "AUTH_TOKEN_EXCHANGE_FAILED", "OIDC token exchange failed"))
		return
	}
	h.setCookie(c, sessionToken, time.Now().UTC().Add(h.service.sessionTTL))
	redirect := transaction.ReturnPath
	if !isSafeReturnPath(redirect, h.service.PathPrefix()) {
		redirect = h.service.PathPrefix() + "/"
	}
	c.Redirect(http.StatusFound, redirect)
}

// Logout 撤销本地会话，尝试平台登出端点（骨架：回退本地登出）。
func (h *Handler) Logout(c *gin.Context) {
	cookie, err := c.Request.Cookie(h.options.CookieName)
	if err == nil && cookie.Value != "" {
		_ = h.service.RevokeSession(cookie.Value)
	}
	h.setCookie(c, "", time.Unix(1, 0))
	// TODO(二期)：发现 end_session_endpoint 后拼接 id_token_hint 完成平台登出；一期仅本地登出。
	c.Redirect(http.StatusFound, h.service.PathPrefix()+"/")
}

// AuthMe 返回当前会话身份（接入手册 §3.1 /api/v1/auth/me）。
func (h *Handler) AuthMe(c *gin.Context) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	response.OK(c, gin.H{
		"user_id":          principal.UserID,
		"tenant_id":        principal.TenantID,
		"display_name":     principal.DisplayName,
		"roles":            principal.Roles,
		"permissions":      sortedKeys(principal.Permissions),
		"role_config_hash": principal.RoleConfigHash,
		"authz_revision":   principal.AuthzRevision,
		"expires_at":       principal.ExpiresAt,
	})
}

// RequireSameOrigin 写请求同源 + CSRF 头校验（对齐 CRM crmauth）。
func (h *Handler) RequireSameOrigin(c *gin.Context) {
	switch c.Request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		c.Next()
		return
	}
	origin := c.GetHeader("Origin")
	if h.options.PublicOrigin == "" || origin != h.options.PublicOrigin || c.GetHeader("X-CSRF-Token") != "1" {
		response.Error(c, apperror.ErrForbidden)
		c.Abort()
		return
	}
	c.Next()
}

func (h *Handler) setCookie(c *gin.Context, value string, expires time.Time) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     h.options.CookieName,
		Value:    value,
		Path:     h.service.PathPrefix(),
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   h.options.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
}

func isSafeReturnPath(path string, pathPrefix string) bool {
	if path == "" {
		return false
	}
	if strings.HasPrefix(path, "//") || strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return false
	}
	return strings.HasPrefix(path, pathPrefix)
}

func sortedKeys(set map[string]struct{}) []string {
	result := make([]string, 0, len(set))
	for key := range set {
		result = append(result, key)
	}
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j] < result[i] {
				result[i], result[j] = result[j], result[i]
			}
		}
	}
	return result
}
