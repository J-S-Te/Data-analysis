package embedbridge

import (
	_ "embed"
	"html"
	"net/url"
	"strings"
)

//go:embed session_activity.js
var sessionActivityScript []byte

// injectSessionActivity 加入隔离来源下的外链活动桥，不引入内联脚本或认证材料。
// 参数：body为看板HTML，prefix为受控资源路径，parentOrigin为平台公开来源。
// 返回值：加入脚本的HTML；来源无效或文档无head时保持原文。
func injectSessionActivity(body []byte, prefix, parentOrigin string) []byte {
	origin, err := url.Parse(parentOrigin)
	if err != nil || origin.Host == "" || origin.User != nil ||
		(origin.Scheme != "http" && origin.Scheme != "https") ||
		origin.RawQuery != "" || origin.Fragment != "" ||
		(origin.Path != "" && origin.Path != "/") {
		return body
	}
	text := string(body)
	position := strings.Index(strings.ToLower(text), "</head>")
	if position < 0 {
		return body
	}
	script := `<script src="` + html.EscapeString(prefix+"__platform_session_activity.js") +
		`" data-platform-origin="` + html.EscapeString(strings.TrimRight(parentOrigin, "/")) + `"></script>`
	return []byte(text[:position] + script + text[position:])
}
