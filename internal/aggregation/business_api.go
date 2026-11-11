package aggregation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
)

// readBusinessSnapshot 通过独立机器身份读取报告或财务聚合，不把上游错误当零值。
// 参数：ctx 为许可/取消上下文，endpoint/credential 为受控来源，section 限定指标契约。
// 返回值：完整同租户快照；鉴权、网络、内容或来源时间异常均返回不含原始响应的错误。
func (r *APISyncRunner) readBusinessSnapshot(
	ctx context.Context, endpoint string, credential MachineCredential, section string,
) (domain.BusinessSnapshot, error) {
	if err := r.checkLicense(ctx); err != nil {
		return domain.BusinessSnapshot{}, err
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.Fragment != "" || strings.TrimSpace(r.options.TenantID) == "" {
		return domain.BusinessSnapshot{}, errors.New("invalid business statistics endpoint or tenant")
	}
	token, err := r.machineToken(ctx, credential)
	if err != nil {
		return domain.BusinessSnapshot{}, fmt.Errorf("business statistics machine credential: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return domain.BusinessSnapshot{}, errors.New("invalid business statistics request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-DA-Tenant-ID", r.options.TenantID)
	client := &http.Client{Timeout: r.options.HTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return domain.BusinessSnapshot{}, errors.New("business statistics source request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return domain.BusinessSnapshot{}, fmt.Errorf("business statistics source returned HTTP %d", response.StatusCode)
	}
	return decodeBusinessSnapshot(response.Body, section, r.options.TenantID, time.Now().UTC())
}

// decodeBusinessSnapshot 有界解析数字和时间，保留整数精度并拒绝尾随内容。
// 参数：body 为HTTP响应，section/tenant 为预期契约，now 为当前可信时间。
// 返回值：通过领域校验的快照；缺字段或异常值不会发布到聚合库。
func decodeBusinessSnapshot(body io.Reader, section, tenant string, now time.Time) (domain.BusinessSnapshot, error) {
	raw, err := io.ReadAll(io.LimitReader(body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return domain.BusinessSnapshot{}, errors.New("business statistics response exceeds limit or cannot be read")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value domain.BusinessSnapshot
	if err := decoder.Decode(&value); err != nil {
		return value, errors.New("invalid business statistics JSON")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return value, errors.New("business statistics response has trailing content")
	}
	if err := validateBusinessSnapshot(value, section, tenant, now); err != nil {
		return value, err
	}
	value.SourceSnapshotAt = value.SnapshotAt
	if section == "report" {
		value.SourceCode = "project_management"
	} else {
		value.SourceCode = "settlement"
	}
	return value, nil
}
