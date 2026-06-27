#!/usr/bin/env sh
# examples/plugins/mc-auth-proxy/build.sh 是示例插件代码，用于演示托管插件接入方式。

set -eu

repo_root=$(cd ../../.. && pwd)
cd "$repo_root"
go run ./cmd/gateway plugin build examples/plugins/mc-auth-proxy --type both --skip-tests
