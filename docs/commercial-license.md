# Commercial runtime enforcement

API, aggregation-worker and alert-worker each run an independent signed snapshot
consumer. Default `COMMERCIAL_LICENSE_ENABLED=false` preserves deployments not
yet enrolled. Controlled provisioning must set it to `true`; missing credentials,
binding or trust rejects startup, and missing/unverifiable local snapshots deny
business operations rather than authorizing them. The managed runtime setting
must not be user-editable through business APIs.

Each component needs its own `COMMERCIAL_LICENSE_SERVICE_ID`, OAuth Client ID /
Secret with `license.runtime` scope, and persistent state file. Do not use its
OIDC login client, audit client or the credentials of another component.

Common managed variables (`COMMERCIAL_LICENSE_` prefix): INSTANCE_ID, ENVIRONMENT,
SERVICE_ID, STATE_PATH, PLATFORM_PUBLIC_KEY_PATH, PLATFORM_BASE_URL, CLIENT_ID,
CLIENT_SECRET, COVERAGE_DIGEST, IMAGE_DIGEST, ALLOW_HTTP. Digests use `sha256:`.
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
