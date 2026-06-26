#!/usr/bin/env sh
set -eu

mkdir -p dist
go build -buildmode=plugin -o dist/plugin.so .
go run ./cmd/render-manifest > dist/manifest.json
cp README.md dist/README.md
(
  cd dist
  rm -f mc-auth-proxy.mcgp
  zip -q mc-auth-proxy.mcgp manifest.json plugin.so README.md
)
