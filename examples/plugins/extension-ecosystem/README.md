# Extension Ecosystem Example

This example demonstrates phase 7 extension points:

- `route.resolve/v1` returns `override`, `reject`, or `pass`.
- `status.ping/v1` returns host-specific MOTD text.
- `event.subscriber/v1` receives asynchronous plugin events.
- `admin.auth.provider/v1` registers an unavailable external provider while preserving local admin fallback.

It is intended as a conformance fixture and source example for plugin authors.
The source manifest is maintained as `manifest.yaml`; packaged `.mcgp` artifacts
still contain canonical `manifest.json`.
