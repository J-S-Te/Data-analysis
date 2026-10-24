# Commercial runtime enforcement

## 当前登记协议与兼容边界（2026-10-22）

受控交付使用目录 V1 / 组件登记协议 V2：分别下发
`COMMERCIAL_LICENSE_REGISTRATION_PROTOCOL=2` 和
`COMMERCIAL_LICENSE_CATALOG_VERSION=1`，组件身份由编译目录限定。登记协议 V2
与签名快照协议 1、镜像标签 `com.basic-platform.license.protocol=1` 是不同版本概念，
不能据镜像标签判断登记仍是 V1。平台仍通过独立发布完整性/批准证据核验实际镜像；
组件更新不以复制旧 coverage/image 绑定或普通业务配置绕过受控发布。

共享 consumer 仍能读取未指定登记版本的 V1 绑定，用于已有组件兼容；受控新交付
不能据此回退。`COMMERCIAL_LICENSE_ENABLED=false` 只允许从未登记且没有其他绑定配置、
固定持久状态目录不存在或为空的组件；已有状态或绑定残留时会拒绝启动，不会因环境
文件丢失而静默放开。不可通过删状态卷或修改开关解除既有执行保护。

目录权威为 `license-core/catalog/catalog.go`（工作区路径）；本仓库构建使用审核过的
`third_party/license-core/catalog/catalog.go` 镜像。组件目录/发布批准与原业务授权均须满足。
平台操作见[运行时协调与激活](../../sharedDocs/09_商业授权/03_运行时协调与激活说明.md)。


API, aggregation-worker and alert-worker each run an independent signed snapshot
consumer. `COMMERCIAL_LICENSE_ENABLED=false` is allowed only for components never
enrolled, with no other binding fields and no persisted runtime state. Controlled provisioning must set it to `true`; missing credentials,
binding or trust rejects startup, and missing/unverifiable local snapshots deny
business operations rather than authorizing them. The managed runtime setting
must not be user-editable through business APIs.

Each component needs its own `COMMERCIAL_LICENSE_SERVICE_ID`, OAuth Client ID /
Secret with `license.runtime` scope, and persistent state file. Do not use its
OIDC login client, audit client or the credentials of another component.

Common managed variables (`COMMERCIAL_LICENSE_` prefix): INSTANCE_ID, ENVIRONMENT,
SERVICE_ID, STATE_PATH, PLATFORM_PUBLIC_KEY_PATH, PLATFORM_BASE_URL, CLIENT_ID,
CLIENT_SECRET, REGISTRATION_PROTOCOL, CATALOG_VERSION, ALLOW_HTTP. Legacy V1
also requires COVERAGE_DIGEST and IMAGE_DIGEST; V2 permits them to be absent.
If provided, digests use `sha256:`.
PLATFORM_PUBLIC_KEY_PATH is read-only Ed25519 SPKI PEM. Vendor trust is compiled
into the reviewed consumer, not supplied by the customer. ENVIRONMENT binds the
commercial installation (for example `production`), not OAuth's `prod` code.
ALLOW_HTTP defaults false; controlled HTTP installations must explicitly set it
true. State parent directories must be protected and persisted per component.

Expiry rejects BI grant issuance, old grant consumption and every iframe resource
proxy request. Ordinary materialized historical reads remain available. There is
no separate export endpoint in this service; permitted historical export must not
use the Metabase dynamic-query proxy. Dictionary/rule mutations, manual sync
triggers, cross-system synchronization, new aggregation and alert calculation are
business writes. Health, login/logout and authorization recovery stay operational.

Workers check before a pass, queue claim, machine-token request and batch/snapshot
write. Long-running loops remain alive and recheck after renewal. Queued sync
requests interrupted by expiry return to QUEUED while preserving their active key;
already committed idempotent batches can be replayed. No whole-worker shutdown is
used to enforce expiry. `--once` reports denied work as an error.

Independent builds consume reviewed source from `third_party/license-core`.
Run `sh scripts/license-core-sync.sh --sync ../license-core` only when intentionally
updating shared source, then review source and manifest. CI and Docker both verify
the exact file allowlist and SHA-256 manifest; they do not rely on a sibling module
existing on a production builder.

This code is not evidence of deployment/enrollment. Platform provisioning still
has to deliver all three component bindings and verify ready/snapshot/ACK before
declaring the application ENFORCED. Metabase itself must remain private; public
direct access would bypass the embedding bridge and must fail topology acceptance.

## 操作步骤与验收

部署管理员批准真实BI三消费者和私有Metabase，平台许可管理员在商业授权页选数据看板与统计分析并刷新/确认激活。成功须部署READY、ENFORCED、snapshot_current=true及data-analysis-api / data-analysis-aggregation-worker / data-analysis-alert-worker三个当前成员当期ACK；Metabase按目录 data-analysis-metabase 作为基础设施核验镜像、副本、无公开端口和网关共享真实网络，不伪装消费者ACK。

DashboardShellView选择看板 → api/dataAnalysis.js getEmbedToken → embedbridge.Issue商业MUTATE_BUSINESS、原看板权限及独立嵌入origin → 存储grant → Proxy消费 → ProxyResource对每个资源重新检查许可/Host/grant/路径 → 私有Metabase。到期连已签token和资源请求也拒绝，无额外嵌入宽限；历史物化查询不等于可用Metabase动态代理。成功应看真实iframe资源加载、对应准入和数据权限；token返回或ACK完成不证明图表可用。

聚合Pass各通道与alert扫描/持久写复核授权，普通历史读仍须授权项存在且已生效和可信状态。失败查商业限制、原权限、真实dashboard映射、BI_EMBED_PUBLIC_ORIGIN独立cookie域、网关网络及消费者卷，不公开Metabase绕过代理。Workers不应整进程停掉，恢复后重新检查。

依据：frontend data_analysis/views/DashboardShellView.vue及api → internal/bootstrap装配 → internal/commercial、embedbridge/embed.go → grant存储/Metabase代理；cmd/aggregation-worker、cmd/alert-worker → aggregation/worker.go、alertworker/worker.go及持久化。本次未浏览器验收、发grant、计算或部署。
