# WASM Rule Policy

This example builds a minimal `runtime.type=wasm` plugin for the `mc-gateway.wasm.host/v1` ABI.

Build the module:

```sh
./build.sh
```

Package it:

```sh
go run ./cmd/gateway plugin build examples/plugins/wasm-rule-policy --type binary --skip-tests
```

Validate and run conformance:

```sh
go run ./cmd/gateway plugin validate examples/plugins/wasm-rule-policy
go run ./cmd/gateway plugin test examples/plugins/wasm-rule-policy --profile manifest
go run ./cmd/gateway plugin conformance examples/plugins/wasm-rule-policy
```
