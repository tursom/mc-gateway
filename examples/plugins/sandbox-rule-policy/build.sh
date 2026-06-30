#!/bin/sh
set -eu

out="${1:-${MC_GATEWAY_SANDBOX_OUT:-bin/plugin}}"
mkdir -p "$(dirname "$out")"
CGO_ENABLED=0 go build -trimpath -buildvcs=false -o "$out" .
