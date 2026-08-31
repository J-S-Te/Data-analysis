# Current Task

## 目标

对数据看板与统计分析进行业务、后端和前端深度测试，并修复确认的问题。

## 本轮完成

- 月度趋势在截取最近月份前合并合同事实和项目快照，保留只有项目数据的月份。
- 合同明细分页参数在应用层统一归一化，HTTP 响应元数据与实际查询一致。
- 合同摘要无快照时返回稳定的空 `discount_buckets` 对象。
- 总览部分接口失败时明确提示并保留成功数据。
- 合同摘要成功但明细失败时保留摘要，并单独提示明细错误。
- 为趋势合并和分页归一化补充回归测试。

## 已执行验证

- `GOCACHE=/tmp/data-analysis-deep-go-cache go test ./...`：通过。
- `GOCACHE=/tmp/data-analysis-deep-go-cache go vet ./...`：通过。
- `git diff --check`：通过。
- `frontend/npm test -- --runInBand`：428 项通过。
- `frontend/npm run build`：通过；仅有既有 chunk size warning。

## 外部验证边界

真实 MySQL 数据量、跨租户数据、同步 worker 和 Metabase/服务器运行时仍需在部署环境用真实凭据与备份门禁验证；本轮未伪造生产数据或凭据。
