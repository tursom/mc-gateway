# Dockerfile 构建网关二进制、编译管理前端，并打包带 SQLite 友好默认值的运行镜像。

# syntax=docker/dockerfile:1

ARG NODE_IMAGE=node:24.11.1-alpine
ARG GO_IMAGE=golang:1.25.0-alpine
ARG RUNTIME_IMAGE=alpine:3.22

FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS admin-frontend

WORKDIR /src

COPY package.json package-lock.json tsconfig.admin.json ./
RUN --mount=type=cache,id=mc-gateway-npm,target=/root/.npm,sharing=locked \
    npm ci

COPY cmd/gateway/admin_frontend ./cmd/gateway/admin_frontend
COPY cmd/gateway/admin_static/index.html cmd/gateway/admin_static/app.css ./cmd/gateway/admin_static/
RUN npm run build:admin

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build

ARG TARGETOS
ARG TARGETARCH
ARG GOPROXY=https://proxy.golang.org,direct

ENV GOPROXY=${GOPROXY}

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,id=mc-gateway-go-mod,target=/go/pkg/mod,sharing=locked \
    go mod download

COPY . .

RUN --mount=type=cache,id=mc-gateway-go-mod,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=mc-gateway-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    mkdir -p /out \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /out/mc-gateway ./cmd/gateway

FROM ${RUNTIME_IMAGE}

WORKDIR /data

COPY --from=build /out/mc-gateway /usr/local/bin/mc-gateway
COPY --from=admin-frontend /src/cmd/gateway/admin_static /usr/share/mc-gateway/admin_static

ENV MC_GATEWAY_DB=/data/mc-gateway.sqlite3
ENV MC_GATEWAY_ADMIN_STATIC_DIR=/usr/share/mc-gateway/admin_static

EXPOSE 25565/tcp

CMD ["/usr/local/bin/mc-gateway"]
