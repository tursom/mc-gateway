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
rm -rf dist/source-package
mkdir -p dist/source-package/cmd/render-manifest
ARTIFACT_TYPE=source go run ./cmd/render-manifest > dist/source-package/manifest.json
cp main.go go.mod README.md dist/source-package/
cp cmd/render-manifest/main.go dist/source-package/cmd/render-manifest/main.go
go mod vendor -o dist/source-package/vendor
(
  cd dist/source-package
  rm -f ../upstream-rewrite-source.mcgp
  zip -qr ../upstream-rewrite-source.mcgp manifest.json main.go go.mod README.md cmd/render-manifest/main.go vendor
)
