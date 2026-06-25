# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.24.4-alpine AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,id=mc-gateway-go-mod,target=/go/pkg/mod,sharing=locked \
    go mod download

COPY . .
RUN --mount=type=cache,id=mc-gateway-go-mod,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=mc-gateway-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    mkdir -p /out \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /out/mc-gateway ./cmd/gateway

FROM alpine:3.22

WORKDIR /data

COPY --from=build /out/mc-gateway /usr/local/bin/mc-gateway

ENV MC_GATEWAY_DB=/data/mc-gateway.sqlite3

EXPOSE 25565/tcp

CMD ["/usr/local/bin/mc-gateway"]
