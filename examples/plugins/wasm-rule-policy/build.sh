#!/usr/bin/env sh
# examples/plugins/wasm-rule-policy/build.sh builds the sample WASM module without external toolchains.

set -eu

out="${1:-${MC_GATEWAY_WASM_OUT:-plugin.wasm}}"
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

cd "$script_dir"
go run ./source/genwasm.go -out "$out"
