# Repository Guidelines

## Project Structure & Module Organization

- `cmd/simd/` contains the daemon entry point and diagnostic CLI.
- `internal/` separates UART transport (`serial`), AT sessions (`at`), ML307R adaptation (`modem`), PDU encoding (`sms`), persistence (`store`), presentation (`notification`), forwarding (`bark`), orchestration (`service`), and HTTP handlers (`httpapi`). Types live in `model`; doubles live in `testutil`.
- Tests sit beside implementation files as `*_test.go`. Versioned SQLite migrations live in `internal/store/schema*.sql`.
- `configs/` contains example YAML; `deploy/` contains systemd and environment templates. `docs/` contains requirements, OpenAPI, and validation evidence.
- `ML307R/` is an optional, ignored local vendor reference pack. `bin/` and `dist/` contain generated outputs. `scripts/` contains packaging and smoke checks.

## Build, Test, and Development Commands

Use Go 1.24 or newer. Linux amd64/arm64 are deployment targets; macOS supports development and probing.

- `make build`: build `bin/simd` for the current host.
- `make test`: run all tests with the race detector.
- `make vet`: run Go static checks.
- `make release`: build both Linux architectures without CGO; `make dist VERSION=v0.1.0` packages a clean commit.
- `go test ./internal/sms -fuzz=FuzzDecodeNeverPanics -fuzztime=10s`: fuzz malformed PDUs.
- After configuring YAML and environment variables, run `./bin/simd -config configs/simd.yaml -check-config`, then omit `-check-config` to start.

## Coding Style & Naming Conventions

Format Go with `gofmt`; use tabs, lowercase package names, exported `PascalCase` identifiers, and descriptive test names. Keep modem-specific AT commands inside the adapter. Use parameterized SQL, explicit errors, context cancellation, and bounded I/O timeouts. Update `docs/openapi.yaml` alongside API changes.

## Testing Guidelines

Use Go's standard `testing` package, `Test...` functions, and `Fuzz...` targets. Prefer deterministic protocol doubles and temporary SQLite databases. Add regression tests for transaction failures, restart recovery, SIM changes, idempotency, and independent forwarding targets. Native Linux amd64/ARM64 CI runs vet, race tests, coverage collection, release builds, and smoke checks; no numeric coverage threshold is configured. Record actual hardware results separately in `docs/validation.md`.

## Commit & Pull Request Guidelines

Use short imperative subjects, such as `Fix SIM identity check before deletion`. PR descriptions should explain behavior, relevant issues, validation commands, and hardware limitations. Include migration and compatibility notes when applicable.

## Security & Reliability

Keep tokens and Bark keys in environment variables. Never log credentials, SMS bodies, or full phone numbers. Preserve atomic SMS/task persistence before modem deletion; never automatically resend an unknown submission. Full daemon startup archives and deletes existing modem messages after persistence. Use `-probe-port /dev/ttyS0 -probe-sms` for diagnostics without reading, deleting, or sending SMS.
