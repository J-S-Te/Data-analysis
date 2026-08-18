# 单 Dockerfile 多二进制（对齐 customer_and_opportunity/Dockerfile）
FROM golang:1.26.4-alpine AS builder

ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src

COPY go.mod go.sum* ./
RUN go mod download

COPY . ./

RUN set -eu; \
    for command in \
      dashboard-api aggregation-worker alert-worker authz-catalog \
      local-migrate production-migrate; do \
      CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o "/out/${command}" "./cmd/${command}"; \
    done

FROM alpine:3.21 AS runtime-base

RUN apk add --no-cache ca-certificates tzdata wget

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
ENTRYPOINT ["/app/production-migrate"]
