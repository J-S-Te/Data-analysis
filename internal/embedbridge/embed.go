// Package embedbridge 实现嵌入桥：一次性令牌签发 + 单次消费代理 + Metabase 签名嵌入 URL。
// 设计依据：设计方案 §4.3–4.4（四层防线第 1/2 层）。浏览器不直连 Metabase。
package embedbridge

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/commercial"
	"github.com/unified-identity-auth-platform/data-analysis/internal/oidc"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

type Options struct {
	LicenseCheck        commercial.Check
	MetabaseInternalURL string // 如 http://metabase:3000
	MetabaseBasePath    string // Metabase 服务路径前缀，如 / 或 /mb
	EmbeddingSecret     string // Metabase Admin 设置中的嵌入密钥
	TokenTTL            time.Duration
	DashboardIDs        map[string]string // dashboard_code 映射到 Metabase 看板 ID。
	PathPrefix          string
	PublicOrigin        string
	EmbedPublicOrigin   string
}

type Bridge struct {
	options      Options
	db           *gorm.DB
	tokenTTL     time.Duration
	dashboardIDs map[string]string
}

func New(db *gorm.DB, options Options) (*Bridge, error) {
	if options.MetabaseInternalURL == "" || options.EmbeddingSecret == "" {
		return nil, errors.New("embed bridge requires metabase internal url and embedding secret")
	}
	if options.EmbedPublicOrigin != "" {
		origin, err := isolatedEmbedOrigin(options.PublicOrigin, options.EmbedPublicOrigin)
		if err != nil {
			return nil, err
		}
		options.EmbedPublicOrigin = origin
	}
	ttl := options.TokenTTL
	if ttl <= 0 {
		// 一次性嵌入令牌的默认有效期：缩短以压缩签名 URL 泄露后的重放窗口。
		ttl = 2 * time.Minute
	}
	return &Bridge{options: options, db: db, tokenTTL: ttl, dashboardIDs: options.DashboardIDs}, nil
}

// Issue 校验权限后签发一次性嵌入令牌（设计方案 §9：GET /api/v1/embed/{dashboard_code}）。
func (b *Bridge) Issue(c *gin.Context, dashboardCode string) {
	if commercial.Reject(c, b.options.LicenseCheck.Allow(c.Request.Context(), commercial.MutateBusiness)) {
		return
	}
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	permission, exists := b.permissionFor(dashboardCode)
	if !exists || !principal.HasPermission(permission) {
		response.Error(c, apperror.ErrForbidden)
		return
	}
	if _, err := metabaseDashboardResource(b.dashboardIDs[dashboardCode]); err != nil {
		response.Error(c, apperror.New(http.StatusServiceUnavailable, "EMBED_DASHBOARD_UNCONFIGURED", "dashboard embedding is not configured"))
		return
	}
	if b.options.EmbedPublicOrigin == "" {
		response.Error(c, apperror.New(http.StatusServiceUnavailable, "EMBED_ORIGIN_UNCONFIGURED", "isolated dashboard embedding is not configured"))
		return
	}
	token, err := randomToken()
	if err != nil {
		response.Error(c, apperror.New(http.StatusInternalServerError, "EMBED_TOKEN_FAILED", "failed to create embed token"))
		return
	}
	scope, err := embedScope(principal)
	if err != nil {
		response.Error(c, apperror.ErrForbidden)
		return
	}
	scopeJSON, _ := json.Marshal(scope)
	now := time.Now().UTC()
	err = b.db.Create(&oidc.EmbedToken{
		TokenHash:      oidc.TokenHash(token),
		TenantID:       principal.TenantID,
		PlatformUserID: principal.UserID,
		DashboardCode:  dashboardCode,
		ScopeJSON:      string(scopeJSON),
		ExpiresAt:      now.Add(b.tokenTTL),
		CreatedAt:      now,
	}).Error
	if err != nil {
		response.Error(c, apperror.New(http.StatusInternalServerError, "EMBED_TOKEN_STORE_FAILED", "failed to persist embed token"))
		return
	}
	response.OK(c, gin.H{
		"token":      token,
		"expires_at": now.Add(b.tokenTTL).Unix(),
		"embed_url":  b.options.EmbedPublicOrigin + strings.TrimRight(b.options.PathPrefix, "/") + "/api/v1/embed-proxy/" + token,
	})
}

// Proxy 单次消费令牌并代理到内网 Metabase（设计方案 §9：GET /api/v1/embed-proxy/{token}）。
func (b *Bridge) Proxy(c *gin.Context, token string) {
	if !b.requireEmbedHost(c) {
		return
	}
	if commercial.Reject(c, b.options.LicenseCheck.Allow(c.Request.Context(), commercial.MutateBusiness)) {
		return
	}
	record, ok := b.embedRecord(c, token, true)
	if !ok {
		response.Error(c, apperror.ErrForbidden)
		return
	}
	dashboardID, ok := b.dashboardIDs[record.DashboardCode]
	if !ok {
		response.Error(c, apperror.New(http.StatusNotFound, "EMBED_DASHBOARD_UNKNOWN", "unknown dashboard"))
		return
	}
	// 范围参数必须同时绑定令牌记录的租户，防止损坏或伪造的 scope_json
	// 绕过应用层的行级数据隔离。
	var scope map[string]interface{}
	if err := json.Unmarshal([]byte(record.ScopeJSON), &scope); err != nil || strings.TrimSpace(record.TenantID) == "" || scope["tenant_id"] != record.TenantID {
		response.Error(c, apperror.New(http.StatusForbidden, "EMBED_SCOPE_INVALID", "invalid embed scope"))
		return
	}
	signedURL, err := b.signedEmbedURL(dashboardID, scope, record.ExpiresAt)
	if err != nil {
		response.Error(c, apperror.New(http.StatusInternalServerError, "EMBED_SIGN_FAILED", "failed to sign embed url"))
		return
	}
	// 将签名 Metabase 路由放入 iframe URL。Metabase SPA 根据
	// 浏览器路径名决定渲染哪个看板；若仅在
	// /embed-proxy/{token} 提供文档，会始终打开首页。
	// Location 携带签名嵌入 URL：禁止将其保留在
	// 浏览器历史、中间缓存或后续请求的 Referer 中，
	// 否则会扩大 H-2 的 TTL 有界重放窗口。
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Redirect(http.StatusFound, b.resourcePrefix(token)+"embed/dashboard/"+signedURL.token+"?bordered=false&titled=false")
}

// ProxyResource 代理 Metabase iframe 发起的全部后续请求。
// 令牌保留在浏览器可见路径中，因此只有原短期嵌入
// 授权仍有效时才暴露资源。
func (b *Bridge) ProxyResource(c *gin.Context, token, resource string) {
	if !b.requireEmbedHost(c) {
		return
	}
	if commercial.Reject(c, b.options.LicenseCheck.Allow(c.Request.Context(), commercial.MutateBusiness)) {
		return
	}
	if record, ok := b.embedRecord(c, token, false); !ok || record.ConsumedAt == nil {
		response.Error(c, apperror.ErrForbidden)
		return
	}
	resourcePath, ok := allowedMetabaseResource(resource)
	if !ok {
		response.Error(c, apperror.ErrForbidden)
		return
	}
	metabaseTarget, err := url.Parse(b.options.MetabaseInternalURL)
	if err != nil {
		response.Error(c, apperror.New(http.StatusInternalServerError, "EMBED_METABASE_INVALID", "invalid metabase url"))
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(metabaseTarget)
	originalDirector := proxy.Director
	isDocument := strings.HasPrefix(resourcePath, "/embed/dashboard/")
	proxy.ModifyResponse = b.modifyProxyResponse(token, isDocument)
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		stripSensitiveProxyHeaders(req.Header)
		// 禁止向上游请求压缩响应体：ModifyResponse 重写
		// 嵌入文档，需要明文。浏览器会发送
		// Accept-Encoding: gzip，否则将破坏重写。
		req.Header.Set("Accept-Encoding", "identity")
		// Metabase 从内部根路径提供静态资源及 API 端点；
		// 配置的基础路径仅应用于初始嵌入文档。
		if isDocument {
			resourcePath = strings.TrimRight(b.options.MetabaseBasePath, "/") + resourcePath
		}
		req.URL.Path = resourcePath
		req.URL.RawPath = ""
		req.Host = metabaseTarget.Host
	}
	proxy.ServeHTTP(c.Writer, c.Request)
}

func (b *Bridge) modifyProxyResponse(token string, rewriteDocument bool) func(*http.Response) error {
	return func(resp *http.Response) error {
		// 令牌存在于 iframe URL 中，禁止 Referer 外带并避免代理响应被共享缓存。
		resp.Header.Set("Referrer-Policy", "no-referrer")
		resp.Header.Set("Cache-Control", "no-store")
		// 网关绝不向平台或隔离主机传递上游 Cookie。
		// 嵌入 SQL 使用签名且锁定租户的能力。
		resp.Header.Del("Set-Cookie")
		// 响应已经位于经过权限检查的短期嵌入
		// 代理之后，因此仅移除拒绝 iframe 嵌入的响应头。
		resp.Header.Del("X-Frame-Options")
		if csp := resp.Header.Get("Content-Security-Policy"); csp != "" {
			resp.Header.Set("Content-Security-Policy", withoutFrameAncestors(csp))
		}
		if b.options.PublicOrigin != "" {
			resp.Header.Add("Content-Security-Policy", "frame-ancestors "+b.options.PublicOrigin)
		}
		if !rewriteDocument || !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
			return nil
		}
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return readErr
		}
		_ = resp.Body.Close()
		body = rewriteEmbedDocument(body, b.resourcePrefix(token), b.options.MetabaseBasePath)
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return nil
	}
}

func (b *Bridge) requireEmbedHost(c *gin.Context) bool {
	u, err := url.Parse(b.options.EmbedPublicOrigin)
	if err != nil || u.Host == "" || !strings.EqualFold(c.Request.Host, u.Host) {
		response.Error(c, apperror.ErrForbidden)
		return false
	}
	return true
}

func (b *Bridge) embedRecord(c *gin.Context, token string, consume bool) (oidc.EmbedToken, bool) {
	if token == "" {
		return oidc.EmbedToken{}, false
	}
	var record oidc.EmbedToken
	if err := b.db.Where("token_hash = ?", oidc.TokenHash(token)).First(&record).Error; err != nil {
		return oidc.EmbedToken{}, false
	}
	if time.Now().UTC().After(record.ExpiresAt) {
		return oidc.EmbedToken{}, false
	}
	if consume {
		if record.ConsumedAt != nil {
			return oidc.EmbedToken{}, false
		}
		now := time.Now().UTC()
		if result := b.db.Model(&oidc.EmbedToken{}).
			Where("token_hash = ? AND consumed_at IS NULL AND expires_at > ?", record.TokenHash, now).
			Update("consumed_at", now); result.Error != nil || result.RowsAffected != 1 {
			return oidc.EmbedToken{}, false
		}
	}
	return record, true
}

// allowedMetabaseResource 校验嵌入页面的后续资源请求。
// 只允许签名看板文档、嵌入查询 API 和 Metabase 前端静态资源；包含路径
// 穿越或非嵌入管理 API 的请求一律拒绝。
func allowedMetabaseResource(resource string) (string, bool) {
	trimmed := strings.TrimLeft(resource, "/")
	decoded, err := url.PathUnescape(trimmed)
	if err != nil || decoded == "" || strings.ContainsAny(decoded, "\\\x00") {
		return "", false
	}
	cleaned := strings.TrimPrefix(path.Clean("/"+decoded), "/")
	if cleaned != decoded {
		return "", false
	}
	allowed := strings.HasPrefix(cleaned, "embed/dashboard/") ||
		strings.HasPrefix(cleaned, "api/embed/dashboard/") ||
		strings.HasPrefix(cleaned, "api/embed/card/") ||
		strings.HasPrefix(cleaned, "app/") ||
		cleaned == "favicon.ico" || strings.HasPrefix(cleaned, "favicon-") || cleaned == "manifest.json"
	if !allowed {
		return "", false
	}
	return "/" + cleaned, true
}

// stripSensitiveProxyHeaders 删除平台会话、用户令牌和可伪造的身份头，
// 避免 Metabase 收到与嵌入请求无关的上游凭据。
func stripSensitiveProxyHeaders(header http.Header) {
	for _, name := range []string{
		"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key", "X-Auth-Token",
		"X-Tenant-Id", "X-Da-Tenant-Id", "X-User-Id", "X-Identity-Id",
		"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
		"X-Forwarded-User", "X-Remote-User",
	} {
		header.Del(name)
	}
}

func (b *Bridge) resourcePrefix(token string) string {
	return strings.TrimRight(b.options.PathPrefix, "/") + "/api/v1/embed-proxy/" + token + "/"
}

func rewriteEmbedDocument(body []byte, prefix, metabaseBasePath string) []byte {
	text := string(body)
	text = replaceAttributeValue(text, `<base href="`, prefix)
	text = replaceMetaContent(text, `base-href`, prefix)
	// iframe 位于 {prefix}/embed/dashboard/{signed}，浏览器跟随
	// 代理 302。Metabase 输出 uri={basePath}/embed/dashboard/{signed}；
	// 仅移除内部服务基础名称，使基础名称检测器
	// 比较 uri 与 window.location.pathname 后，得到 {prefix} 作为 actualRoot。
	if uri, ok := metaContent(text, `uri`); ok {
		uri = strings.TrimPrefix(uri, strings.TrimRight(metabaseBasePath, "/"))
		if uri == "" {
			uri = "/"
		}
		text = replaceMetaContent(text, `uri`, uri)
	}
	return []byte(text)
}

func replaceAttributeValue(document, marker, value string) string {
	start := strings.Index(document, marker)
	if start < 0 {
		return document
	}
	contentStart := start + len(marker)
	contentEndRelative := strings.IndexByte(document[contentStart:], '"')
	if contentEndRelative < 0 {
		return document
	}
	contentEnd := contentStart + contentEndRelative
	return document[:contentStart] + value + document[contentEnd:]
}

func replaceMetaContent(document, name, value string) string {
	marker := `<meta name="` + name + `" content="`
	start := strings.Index(document, marker)
	if start < 0 {
		return document
	}
	contentStart := start + len(marker)
	contentEndRelative := strings.IndexByte(document[contentStart:], '"')
	if contentEndRelative < 0 {
		return document
	}
	contentEnd := contentStart + contentEndRelative
	return document[:contentStart] + value + document[contentEnd:]
}

func metaContent(document, name string) (string, bool) {
	marker := `<meta name="` + name + `" content="`
	start := strings.Index(document, marker)
	if start < 0 {
		return "", false
	}
	contentStart := start + len(marker)
	contentEndRelative := strings.IndexByte(document[contentStart:], '"')
	if contentEndRelative < 0 {
		return "", false
	}
	contentEnd := contentStart + contentEndRelative
	return document[contentStart:contentEnd], true
}

func withoutFrameAncestors(csp string) string {
	directives := strings.Split(csp, ";")
	kept := directives[:0]
	for _, directive := range directives {
		trimmed := strings.TrimSpace(directive)
		if strings.EqualFold(strings.SplitN(trimmed, " ", 2)[0], "frame-ancestors") {
			continue
		}
		if trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "; ") + ";"
}

// signedEmbedURL 生成 Metabase 签名嵌入 URL（HMAC-SHA256）。
func embedScope(principal auth.Principal) (map[string]interface{}, error) {
	mode := principal.ScopeMode
	switch mode {
	case "TENANT":
		mode = "TENANT"
	case "ORG":
		mode = "ORG_SUBTREE"
	case "TEAM":
		mode = "TEAM"
	default:
		return nil, apperror.ErrForbidden
	}
	scope := map[string]interface{}{"tenant_id": principal.TenantID, "scope_mode": mode}
	if mode == "ORG_SUBTREE" {
		if len(principal.OrganizationIDs) == 0 {
			return nil, apperror.ErrForbidden
		}
		scope["org_unit_ids"] = append([]string(nil), principal.OrganizationIDs...)
		if len(principal.TeamIDs) > 0 {
			scope["team_ids"] = append([]string(nil), principal.TeamIDs...)
		}
	}
	if mode == "TEAM" {
		ids := principal.TeamIDs
		if len(ids) == 0 {
			ids = principal.OrganizationIDs
		}
		if len(ids) == 0 {
			return nil, apperror.ErrForbidden
		}
		scope["team_ids"] = append([]string(nil), ids...)
	}
	return scope, nil
}

func (b *Bridge) signedEmbedURL(dashboardID string, params map[string]interface{}, expires time.Time) (signedEmbed, error) {
	resource, err := metabaseDashboardResource(dashboardID)
	if err != nil {
		return signedEmbed{}, err
	}
	payload := map[string]interface{}{
		"resource": map[string]interface{}{"dashboard": resource},
		"params":   params,
		"exp":      expires.Unix(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return signedEmbed{}, err
	}
	// Metabase 接受标准紧凑 HS256 JWT，而非 payload.signature。
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	encoded := header + "." + base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(b.options.EmbeddingSecret))
	mac.Write([]byte(encoded))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signedEmbed{token: encoded + "." + signature}, nil
}

func metabaseDashboardResource(value string) (interface{}, error) {
	if value == "" || value != strings.TrimSpace(value) {
		return nil, errors.New("missing or invalid dashboard ID")
	}
	if id, err := strconv.ParseInt(value, 10, 64); err == nil && id > 0 {
		return id, nil
	}
	// Metabase 也支持其稳定的 21 字符 URL 安全实体 ID。
	if len(value) == 21 && strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) == -1 {
		return value, nil
	}
	return nil, errors.New("invalid dashboard ID")
}

type signedEmbed struct{ token string }

func (b *Bridge) permissionFor(dashboardCode string) (string, bool) {
	switch dashboardCode {
	case "overview":
		return "dashboard.overview.view", true
	case "contract":
		return "dashboard.contract.view", true
	case "project":
		return "dashboard.project.view", true
	case "report":
		return "dashboard.report.view", true
	case "finance":
		return "dashboard.finance.view", true
	default:
		return "", false
	}
}

func randomToken() (string, error) { return oidc.RandomToken(32) }

var _ = strings.TrimSpace
