# syntax=docker/dockerfile:1

FROM golang:1.24.4-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,id=mc-gateway-go-mod,target=/go/pkg/mod,sharing=locked \
    go mod download

COPY . .
RUN --mount=type=cache,id=mc-gateway-go-mod,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=mc-gateway-go-build,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/mc-gateway ./cmd/gateway

FROM alpine:3.22

RUN apk add --no-cache su-exec \
    && addgroup -S mc-gateway \
    && adduser -S -G mc-gateway mc-gateway

WORKDIR /data
RUN chown mc-gateway:mc-gateway /data

COPY --from=build /out/mc-gateway /usr/local/bin/mc-gateway
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

ENV MC_GATEWAY_DB=/data/mc-gateway.sqlite3

EXPOSE 25565/tcp

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["/usr/local/bin/mc-gateway"]
