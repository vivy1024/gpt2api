# =============================================================================
# gpt2api Zeabur 多阶段构建 Dockerfile
# Stage 1: 前端 (npm) → Stage 2: 后端 (go) → Stage 3: 运行时
# =============================================================================

ARG NODE_IMAGE=node:24-alpine
ARG GOLANG_IMAGE=golang:1.26-alpine
ARG ALPINE_IMAGE=alpine:3.21

# Stage 1: 前端构建
FROM ${NODE_IMAGE} AS frontend-builder
WORKDIR /app/web
COPY web/ ./
RUN npm install && npm run build

# Stage 2: 后端构建
FROM ${GOLANG_IMAGE} AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
COPY --from=frontend-builder /app/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /app/gpt2api ./cmd/server
RUN go install github.com/pressly/goose/v3/cmd/goose@latest && cp $(go env GOPATH)/bin/goose /app/goose

# Stage 3: 运行时
FROM ${ALPINE_IMAGE}
RUN apk add --no-cache ca-certificates tzdata curl bash mariadb-client \
    && ln -sf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone

WORKDIR /app
COPY --from=builder /app/gpt2api /app/gpt2api
COPY --from=builder /app/goose /usr/local/bin/goose
COPY --from=builder /app/web/dist /app/web/dist
COPY sql /app/sql
COPY configs /app/configs
COPY deploy/entrypoint.sh /app/entrypoint.sh

RUN sed -i 's/\r$//' /app/entrypoint.sh \
    && chmod +x /app/entrypoint.sh /app/gpt2api \
    && mkdir -p /app/data/backups /app/logs

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -fsS http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["/app/gpt2api", "-c", "/app/configs/config.yaml"]
