# Changelog

## v0.1.0 — 2026-10-25

Initial public release of SIMD.

- Linux daemon managing multiple ML307R modems through independent UART/AT sessions.
- GSM 7-bit/UCS2 SMS reception, single-part sending, SQLite history and durable Bark forwarding.
- Minimal notification approved in preview: unified Apple Messages icon, `New Message`, sender and timestamp, original SMS body. No device labels, Markdown, OTP banners or extra actions.
- English status titles for incomplete/unreadable messages and lossless long-message notification segmentation.
- Idempotent send requests; interrupted submissions become `unknown` and are never automatically resent.
- Bearer-authenticated HTTP API, cached health probes, systemd service and backup instructions.
- Static Linux ARM64 (ARMv8.0 / Orange Pi Zero 3) and amd64 builds, version metadata and SHA-256 release checksums.
- Native Linux amd64/ARM64 CI with race detection and daemon smoke tests.

### Validation limits

The connected ML307R was identified through read-only capability queries. Real operator SMS send/receive, long-message firmware behavior, two physical modems, Orange Pi board UART wiring and a 24-hour hardware soak remain separate acceptance tasks. Simulated tests and ARM64 CI do not replace those checks.
