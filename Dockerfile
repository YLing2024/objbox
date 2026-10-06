# syntax=docker/dockerfile:1

# ---- 阶段一：构建前端（React + Vite + TS）----
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---- 阶段二：编译 Go 单二进制（先把 dist 放到 embed 路径再编译）----
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 前端产物的 embed 路径是 web/dist，必须先就位，保证前端进二进制。
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/objbox ./cmd/objbox

# ---- 阶段三：极简运行时（非 root、仅二进制 + 证书包）----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 10001 objbox \
    && mkdir -p /data \
    && chown -R objbox:objbox /data
COPY --from=build /out/objbox /usr/local/bin/objbox
USER 10001
VOLUME ["/data"]
EXPOSE 18930
ENTRYPOINT ["/usr/local/bin/objbox", "serve", "-addr", "0.0.0.0:18930", "-data", "/data"]
