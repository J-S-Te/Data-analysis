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
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/oidc"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

type Options struct {
	MetabaseInternalURL string // 如 http://metabase:3000
	MetabaseBasePath    string // Metabase 服务路径前缀，如 / 或 /mb
	EmbeddingSecret     string // Metabase Admin 设置中的嵌入密钥
	TokenTTL            time.Duration
	DashboardIDs        map[string]string // dashboard_code -> Metabase dashboard ID
	PathPrefix          string
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
	ttl := options.TokenTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Bridge{options: options, db: db, tokenTTL: ttl, dashboardIDs: options.DashboardIDs}, nil
}

// Issue 校验权限后签发一次性嵌入令牌（设计方案 §9：GET /api/v1/embed/{dashboard_code}）。
func (b *Bridge) Issue(c *gin.Context, dashboardCode string) {
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
	token, err := randomToken()
	if err != nil {
		response.Error(c, apperror.New(http.StatusInternalServerError, "EMBED_TOKEN_FAILED", "failed to create embed token"))
		return
	}
	// 行级范围：骨架阶段注入 TENANT（全量）；ORG_SUBTREE/TEAM 参数注入待 OQ-D6 落地（设计方案 §4.3 注记）。
	scope := map[string]interface{}{"tenant_id": principal.TenantID, "scope_mode": "TENANT"}
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
	})
}

// Proxy 单次消费令牌并代理到内网 Metabase（设计方案 §9：GET /api/v1/embed-proxy/{token}）。
func (b *Bridge) Proxy(c *gin.Context, token string) {
	record, ok := b.embedRecord(c, token, true)
	if !ok {
		response.Error(c, apperror.ErrForbidden)
		return
	}
	now := time.Now().UTC()
	dashboardID, ok := b.dashboardIDs[record.DashboardCode]
	if !ok {
		response.Error(c, apperror.New(http.StatusNotFound, "EMBED_DASHBOARD_UNKNOWN", "unknown dashboard"))
		return
	}
	// 解析范围参数（骨架：TENANT 全量；后续按 scope_json 注入 sales_org/team_ids）
	var scope map[string]interface{}
	_ = json.Unmarshal([]byte(record.ScopeJSON), &scope)
	signedURL, err := b.signedEmbedURL(dashboardID, nil, now.Add(b.tokenTTL))
	if err != nil {
		response.Error(c, apperror.New(http.StatusInternalServerError, "EMBED_SIGN_FAILED", "failed to sign embed url"))
		return
	}
	// Put the signed Metabase route into the iframe URL. Metabase's SPA uses
	// the browser pathname to decide which dashboard to render; serving the
	// document at only /embed-proxy/{token} would always open the home page.
	c.Redirect(http.StatusFound, b.resourcePrefix(token)+"embed/dashboard/"+signedURL.token+"?bordered=false&titled=false")
}

// ProxyResource proxies every follow-up request issued by the Metabase iframe.
// The token remains in the browser-visible path, so resources are only exposed
// while the original short-lived embed grant is still valid.
func (b *Bridge) ProxyResource(c *gin.Context, token, resource string) {
	if _, ok := b.embedRecord(c, token, false); !ok {
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
	isDocument := strings.HasPrefix(strings.TrimLeft(resource, "/"), "embed/dashboard/")
	if isDocument {
		proxy.ModifyResponse = b.modifyEmbedResponse(token)
	}
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		// Never ask the upstream for compressed bodies: ModifyResponse rewrites
		// the embed document and needs plain text. Browsers send
		// Accept-Encoding: gzip, which would otherwise break the rewrite.
		req.Header.Set("Accept-Encoding", "identity")
		// Metabase serves static assets and API endpoints from its internal root;
		// the configured base path only applies to the initial embed document.
		resourcePath := "/" + strings.TrimLeft(resource, "/")
		if isDocument {
			resourcePath = strings.TrimRight(b.options.MetabaseBasePath, "/") + resourcePath
		}
		req.URL.Path = resourcePath
		req.URL.RawPath = ""
		req.Host = metabaseTarget.Host
	}
	proxy.ServeHTTP(c.Writer, c.Request)
}

func (b *Bridge) modifyEmbedResponse(token string) func(*http.Response) error {
	return func(resp *http.Response) error {
		// The response is already behind the permission-checked, short-lived
		// embed proxy, so remove only the headers that reject iframe embedding.
		resp.Header.Del("X-Frame-Options")
		if csp := resp.Header.Get("Content-Security-Policy"); csp != "" {
			resp.Header.Set("Content-Security-Policy", withoutFrameAncestors(csp))
		}
		if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
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
		if err := b.db.Model(&oidc.EmbedToken{}).Where("token_hash = ? AND consumed_at IS NULL", record.TokenHash).
			Update("consumed_at", time.Now().UTC()).Error; err != nil {
			return oidc.EmbedToken{}, false
		}
	}
	return record, true
}

func (b *Bridge) resourcePrefix(token string) string {
	return strings.TrimRight(b.options.PathPrefix, "/") + "/api/v1/embed-proxy/" + token + "/"
}

func rewriteEmbedDocument(body []byte, prefix, metabaseBasePath string) []byte {
	text := string(body)
	text = replaceAttributeValue(text, `<base href="`, prefix)
	text = replaceMetaContent(text, `base-href`, prefix)
	// The iframe lives at {prefix}/embed/dashboard/{signed} (browser follows
	// the Proxy 302). Metabase emits uri={basePath}/embed/dashboard/{signed};
	// strip only the internal service basename so the basename detector
	// (uri vs window.location.pathname) yields {prefix} as actualRoot.
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
func (b *Bridge) signedEmbedURL(dashboardID string, params map[string]interface{}, expires time.Time) (signedEmbed, error) {
	payload := map[string]interface{}{
		"resource": map[string]string{"dashboard": dashboardID},
		"params":   params,
		"exp":      expires.Unix(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return signedEmbed{}, err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(b.options.EmbeddingSecret))
	mac.Write([]byte(encoded))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signedEmbed{token: encoded + "." + signature}, nil
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
