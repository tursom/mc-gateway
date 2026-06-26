#!/usr/bin/env sh
set -eu

mkdir -p dist
go build -buildmode=plugin -o dist/plugin.so .
go run ./cmd/render-manifest > dist/manifest.json
cp README.md dist/README.md
(
  cd dist
  rm -f upstream-rewrite.mcgp
  zip -q upstream-rewrite.mcgp manifest.json plugin.so README.md
)
