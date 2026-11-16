package ownerdirectory

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDirectoryIdentityAndTokenCache 验证姓名读取使用专用scope、精确人员及缓存令牌。
// 参数：t 为测试上下文，HTTP服务与身份均为内存合成夹具。
// 返回值：无；身份、目录范围或令牌缓存不符合约定时失败。
func TestDirectoryIdentityAndTokenCache(t *testing.T) {
	tokens := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokens++
			user, secret, _ := r.BasicAuth()
			if user != "directory-reader" || secret != "synthetic" || r.FormValue("scope") != defaultScope {
				t.Error("dedicated credential or scope missing")
			}
			fmt.Fprint(w, `{"access_token":"synthetic-token","expires_in":300}`)
			return
		}
		if r.URL.Query().Get("user_id") != "person-a" || r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Error("directory request lost its exact identity or token")
		}
		fmt.Fprint(w, `{"code":"OK","data":{"items":[{"display_name":"合成人员"}]}}`)
	}))
	defer server.Close()
	client := directoryTestClient(server.URL)
	for range 2 {
		name, err := client.Lookup(context.Background(), "person-a")
		if err != nil || name != "合成人员" {
			t.Fatalf("lookup: %q %v", name, err)
		}
	}
	if tokens != 1 {
		t.Fatalf("token fetched %d times", tokens)
	}
}

// TestDirectoryRejectsRedirect 验证带凭据的来源调用不接受重定向掩盖配置错误。
// 参数：t 为本地HTTP夹具上下文。
// 返回值：无；重定向被跟随并误判成功时失败。
func TestDirectoryRejectsRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
			return
		}
		fmt.Fprint(w, `{"access_token":"synthetic-token","expires_in":300}`)
	}))
	defer server.Close()
	if _, err := directoryTestClient(server.URL).tokenValue(context.Background()); err == nil {
		t.Fatal("credential endpoint redirect accepted")
	}
}

// directoryTestClient 构造仅指向隔离HTTP服务的目录客户端。
// 参数：base 为测试HTTP服务地址。
// 返回值：使用合成专用身份的客户端。
func directoryTestClient(base string) *Client {
	values := map[string]string{
		"PLATFORM_OWNER_DIRECTORY_URL": base + "/directory", "MACHINE_TOKEN_URL": base + "/token",
		"PLATFORM_OWNER_DIRECTORY_CLIENT_ID": "directory-reader", "PLATFORM_OWNER_DIRECTORY_CLIENT_SECRET": "synthetic",
	}
	return New(func(key string) string { return values[key] })
}
