# Extension Ecosystem Example

This example demonstrates phase 7 extension points:

- `route.resolve/v1` returns `override`, `fallback`, `reject`, or `pass`, with cache TTL, SQLite fallback, upstream rewrite, and decision explain metadata.
- `status.ping/v1` returns MOTD, favicon, online/max players, version text, and maintenance window fields.
- `rule.evaluate/v1` evaluates allow/deny policy independently from route/status handling.
- `connection.filter/v1` rejects the fixture documentation CIDR, enforces a source allow CIDR, and applies a simple per-source rate limit.
- `handshake.filter/v1` rewrites `legacy.example` to `blue.example`.
- `event.subscriber/v1` receives asynchronous plugin events and covers retry, persisted dead-letter replay/drop, cross-node at-least-once policy, and non-blocking subscriber failure.
- `admin.auth.provider/v1` registers an unavailable external provider while preserving local admin fallback.

It is intended as a conformance fixture and source example for plugin authors.
The source manifest is maintained as `manifest.yaml`; packaged `.mcgp` artifacts
still contain canonical `manifest.json`.
