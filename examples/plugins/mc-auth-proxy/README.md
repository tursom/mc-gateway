# MC Auth Proxy Plugin

This example registers `upstream.connect/v2`. It takes over the untouched client
stream, reads the handshake and login start packets, then returns a login
disconnect response unless `fixture_accept` is enabled. When the configured host
does not match, it calls `Next` with a replay wrapper so downstream handlers and
core still receive every byte.

The example is intentionally small: gateway core does not parse authentication
results, identity mapping, forwarding, or play packets. Those responsibilities
belong inside a connection takeover plugin.

The fixture emits `auth.success` / `auth.failure` events and an `auth.attempts`
counter with low-cardinality `result` and `mode` labels.

Build and package:

```sh
(cd ../../.. && go run ./cmd/gateway plugin test examples/plugins/mc-auth-proxy --profile manifest)
(cd ../../.. && go run ./cmd/gateway plugin build examples/plugins/mc-auth-proxy --type both)
```

The binary package is written to `dist/mc-auth-proxy.mcgp`; the source package
is written to `dist/mc-auth-proxy-source.mcgp`.
The source manifest is maintained as `manifest.yaml`; packaged `.mcgp` artifacts
still contain canonical `manifest.json`.

Build the source package through the gateway builder:

```sh
(cd ../../.. && go run ./cmd/gateway plugin build --from-source examples/plugins/mc-auth-proxy/dist/mc-auth-proxy-source.mcgp --out examples/plugins/mc-auth-proxy/dist/mc-auth-proxy-built.mcgp)
```

Example config JSON:

```json
{
  "match_host": "play.example",
  "fixture_accept": false,
  "disconnect_message": "Authentication fixture rejected the login"
}
```
