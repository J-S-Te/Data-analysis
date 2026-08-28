package oidc

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const backchannelEvent = "http://schemas.openid.net/event/backchannel-logout"

type logoutClaims struct {
	Subject  string                     `json:"sub"`
	JTI      string                     `json:"jti"`
	Issued   int64                      `json:"iat"`
	Expires  int64                      `json:"exp"`
	Nonce    string                     `json:"nonce"`
	Audience interface{}                `json:"aud"`
	Events   map[string]json.RawMessage `json:"events"`
}

// BackchannelLogout 接收标准 OIDC logout_token，并在事务中防重放和撤销会话。
func (s *Service) BackchannelLogout(c *gin.Context) {
	if c.Request.Method != http.MethodPost {
		c.Status(http.StatusMethodNotAllowed)
		return
	}
	if err := c.Request.ParseForm(); err != nil {
		c.Status(http.StatusBadRequest)
		return
	}
	raw := strings.TrimSpace(c.Request.Form.Get("logout_token"))
	if raw == "" || len(raw) > 64*1024 || !validLogoutType(raw) {
		c.Status(http.StatusBadRequest)
		return
	}
	token, err := s.verifier.Verify(c.Request.Context(), raw)
	if err != nil {
		c.Status(http.StatusUnauthorized)
		return
	}
	var claims logoutClaims
	if token.Claims(&claims) != nil || !validLogoutClaims(claims, s.clientID, time.Now().UTC()) {
		c.Status(http.StatusUnauthorized)
		return
	}
	now := time.Now().UTC()
	tx := s.db.WithContext(c.Request.Context()).Begin()
	if tx.Error != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	defer tx.Rollback()
	if err := tx.Exec("INSERT INTO da_oidc_backchannel_logout_replay (jti_hash,expires_at,created_at) VALUES (?,?,?)", tokenHash(claims.JTI), now.Add(5*time.Minute), now).Error; err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			c.Status(http.StatusOK)
			return
		}
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if err := tx.Model(&Session{}).Where("tenant_id=? AND revoked_at IS NULL AND platform_user_id=?", s.tenantID, claims.Subject).Updates(map[string]any{"revoked_at": now}).Error; err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if err := tx.Commit().Error; err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	c.Status(http.StatusOK)
}

func validLogoutClaims(c logoutClaims, clientID string, now time.Time) bool {
	if c.Subject == "" || c.JTI == "" || c.Nonce != "" || c.Issued <= 0 || c.Expires <= now.Unix() || c.Expires-c.Issued > 300 {
		return false
	}
	e, ok := c.Events[backchannelEvent]
	if !ok {
		return false
	}
	var props map[string]json.RawMessage
	if json.Unmarshal(e, &props) != nil || len(props) != 0 {
		return false
	}
	switch a := c.Audience.(type) {
	case string:
		return a == clientID
	case []interface{}:
		for _, v := range a {
			if value, ok := v.(string); ok && value == clientID {
				return true
			}
		}
	}
	return false
}

func validLogoutType(raw string) bool {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return false
	}
	h, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var v struct {
		Typ string `json:"typ"`
	}
	return json.Unmarshal(h, &v) == nil && v.Typ == "logout+jwt"
}
