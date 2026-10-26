# 单 Dockerfile 多二进制（对齐 customer_and_opportunity/Dockerfile）
FROM golang:1.26.10-alpine@sha256:27f2d3b83cfc5803c8078f8e730af0f7c431bf067afaec8f60a36fb763187cba AS builder

ARG GOPROXY=https://goproxy.cn|https://proxy.golang.org|direct
ARG GOSUMDB=sum.golang.google.cn
ENV GOPROXY=${GOPROXY} \
    GOSUMDB=${GOSUMDB}

WORKDIR /src

COPY go.mod go.sum* ./
COPY third_party/license-core/ ./third_party/license-core/
COPY scripts/license-core-sync.sh scripts/license-core.sha256 ./scripts/
RUN sh scripts/license-core-sync.sh --check
RUN set -eu; \
    for attempt in 1 2 3 4 5; do \
      if go mod download && go mod verify; then exit 0; fi; \
      echo "go module download failed (attempt ${attempt}/5)" >&2; \
      sleep $((attempt * 2)); \
    done; \
    exit 1

COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY migrations/ ./migrations/

RUN set -eu; \
    for command in \
      dashboard-api aggregation-worker alert-worker authz-catalog \
      local-migrate production-migrate sync-job-recover-legacy; do \
      CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o "/out/${command}" "./cmd/${command}"; \
    done

FROM alpine:3.21@sha256:bbfcd601d7cd20d35bb0b8b866ad1a8a0d107ca3e1c20870c75c5ce2c1186d30 AS runtime-base
ARG APP_VERSION
LABEL org.opencontainers.image.version=${APP_VERSION} com.basic-platform.license.protocol="1"

RUN apk add --no-cache ca-certificates tzdata wget && mkdir -p /var/lib/commercial-license

WORKDIR /app

# 运行时按命令复制对应二进制（多 target）
FROM runtime-base AS dashboard-api
COPY --from=builder /out/dashboard-api /app/dashboard-api
ENTRYPOINT ["/app/dashboard-api"]

FROM runtime-base AS aggregation-worker
COPY --from=builder /out/aggregation-worker /app/aggregation-worker
ENTRYPOINT ["/app/aggregation-worker"]

FROM runtime-base AS alert-worker
COPY --from=builder /out/alert-worker /app/alert-worker
ENTRYPOINT ["/app/alert-worker"]

FROM runtime-base AS authz-catalog
COPY --from=builder /out/authz-catalog /app/authz-catalog
ENTRYPOINT ["/app/authz-catalog"]

FROM runtime-base AS local-migrate
COPY --from=builder /out/local-migrate /app/local-migrate
ENTRYPOINT ["/app/local-migrate"]

FROM runtime-base AS production-migrate
COPY --from=builder /out/production-migrate /app/production-migrate
# 发布脚本从本镜像读取内嵌授权目录哈希并原子写回运行配置；
# 携带 print 工具避免为读取哈希额外构建与分发第五个镜像。
COPY --from=builder /out/authz-catalog /app/authz-catalog
COPY --from=builder /out/sync-job-recover-legacy /app/sync-job-recover-legacy
ENTRYPOINT ["/app/production-migrate"]
