# Upstream Rewrite Plugin

This example registers `route.resolve/v1`. When `match_host` matches the
Minecraft hostname, it returns an override decision for the configured
`upstream`. Non-matching requests return a pass decision so SQLite routing can
continue.

Build and package:

```sh
(cd ../../.. && go run ./cmd/gateway plugin test examples/plugins/upstream-rewrite --profile manifest)
(cd ../../.. && go run ./cmd/gateway plugin build examples/plugins/upstream-rewrite --type both)
```

The binary package is written to `dist/upstream-rewrite.mcgp`; the source
package is written to `dist/upstream-rewrite-source.mcgp`.
The source manifest is maintained as `manifest.yaml`; packaged `.mcgp` artifacts
still contain canonical `manifest.json`.

Build the source package through the gateway builder:

```sh
(cd ../../.. && go run ./cmd/gateway plugin build --from-source examples/plugins/upstream-rewrite/dist/upstream-rewrite-source.mcgp --out examples/plugins/upstream-rewrite/dist/upstream-rewrite-built.mcgp)
```

Example config JSON:

```json
{
  "match_host": "play.example",
  "upstream": "127.0.0.1:25566"
}
```
