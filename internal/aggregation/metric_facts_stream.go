package aggregation

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/metricfacts"
)

// stageMetricSnapshot 优先接收有界流，只有旧服务明确无此接口才回退旧 JSON。
// 参数：ctx 为许可/取消上下文，base/credential/source 为固定来源，write 为围栏事务。
// 返回值：完整暂存代次或旧协议快照；损坏、鉴权与服务错误绝不降级掩盖。
func (r *APISyncRunner) stageMetricSnapshot(ctx context.Context, base string,
	credential MachineCredential, source string, write metricfacts.WriteTransaction) (*metricfacts.Staged, *metricfacts.Bundle, error) {
	response, err := r.openMetricStream(ctx, base, credential)
	if err != nil {
		return nil, nil, err
	}
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed {
		_ = response.Body.Close()
		bundle, err := r.readMetricSnapshot(ctx, base, credential, source)
		if err != nil {
			return nil, nil, err
		}
		return nil, &bundle, nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("metric facts stream returned HTTP %d", response.StatusCode)
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "application/x-ndjson" {
		return nil, nil, errors.New("metric facts stream content type is invalid")
	}
	staged, err := metricfacts.StageStream(response.Body, r.options.TenantID, source, time.Now().UTC(), write)
	return staged, nil, err
}

// openMetricStream 使用原有独立机器身份打开同源固定流接口，禁止重定向。
// 参数：ctx 为请求生命周期，base 为已配置看板 URL，credential 为既有只读凭据。
// 返回值：响应或脱敏错误；调用者负责关闭响应体。
func (r *APISyncRunner) openMetricStream(ctx context.Context, base string, credential MachineCredential) (*http.Response, error) {
	if err := r.checkLicense(ctx); err != nil {
		return nil, err
	}
	endpoint, err := metricEndpoint(base)
	if err != nil {
		return nil, err
	}
	token, err := r.machineToken(ctx, credential)
	if err != nil {
		return nil, errors.New("metric facts machine credential unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/stream", nil)
	if err != nil {
		return nil, errors.New("invalid metric facts stream request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-DA-Tenant-ID", r.options.TenantID)
	request.Header.Set("Accept", "application/x-ndjson")
	client := &http.Client{Timeout: r.options.HTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("metric facts stream request failed")
	}
	return response, nil
}
