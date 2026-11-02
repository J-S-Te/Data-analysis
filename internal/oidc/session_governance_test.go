package oidc

import (
	"context"
	"errors"
	"testing"

	central "github.com/unified-identity-auth-platform/data-analysis/internal/sessiongovernance"
)

type governanceTestClient struct {
	central.Client
	err        error
	activities int
}

// State 提供失效或故障状态，验证数析不会把二者混淆。
// 参数：请求与服务器绑定秘密。
// 返回值：空窗口和指定错误。
func (c *governanceTestClient) State(context.Context, string) (central.Context, error) {
	return central.Context{}, c.err
}

// Activity 统计显式活动，普通状态读取不可调用本方法。
// 参数：请求与服务器绑定秘密。
// 返回值：空窗口和指定错误。
func (c *governanceTestClient) Activity(context.Context, string) (central.Context, error) {
	c.activities++
	return central.Context{}, c.err
}

// TestAnalysisCentralBindingAndOutage 验证旧记录要求重登，中央故障保留依赖语义。
// 参数：t为测试上下文。
// 返回值：无；失效、网络错误分类或活动来源错误时失败。
func TestAnalysisCentralBindingAndOutage(t *testing.T) {
	codec, err := newSecretCodec(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := codec.encrypt("test-binding")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		binding        []byte
		upstream, want error
	}{
		{nil, nil, ErrUnauthenticated}, {cipher, central.ErrInactive, ErrUnauthenticated}, {cipher, central.ErrUnavailable, ErrSessionUnavailable},
	} {
		client := &governanceTestClient{err: test.upstream}
		service := &Service{codec: codec, centralClient: client}
		_, err := service.readCentral(context.Background(), Session{CentralBindingCipher: test.binding}, false)
		if !errors.Is(err, test.want) || client.activities != 0 {
			t.Fatalf("state classification %v, activities%d", err, client.activities)
		}
	}
}
