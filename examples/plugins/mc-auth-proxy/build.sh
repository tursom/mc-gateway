#!/usr/bin/env sh
set -eu

repo_root=$(cd ../../.. && pwd)
cd "$repo_root"
go run ./cmd/gateway plugin build examples/plugins/mc-auth-proxy --type both --skip-tests
