# Upstream Rewrite Plugin

This example registers `upstream.connect/v1` in dialer mode. When `match_host`
matches either the Minecraft hostname or the resolved upstream string, it dials
the configured `upstream` and returns that connection. Non-matching connections
return `api.ErrPass`.

Build and package:

```sh
./build.sh
```

The binary package is written to `dist/upstream-rewrite.mcgp`; the source
package is written to `dist/upstream-rewrite-source.mcgp`.

Build the source package through the gateway builder:

```sh
go run ../../../cmd/gateway plugin source-build dist/upstream-rewrite-source.mcgp dist/upstream-rewrite-built.mcgp
```

Example config JSON:

```json
{
  "match_host": "play.example",
  "upstream": "127.0.0.1:25566"
}
```
