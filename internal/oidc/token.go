package oidc

// TokenHash 导出哈希函数（embedbridge 复用）。
func TokenHash(value string) string { return tokenHash(value) }

// RandomToken 导出随机令牌（embedbridge 复用）。
func RandomToken(size int) (string, error) { return randomToken(size) }
