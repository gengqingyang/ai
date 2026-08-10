# Repository Guidelines

## Project Structure & Module Organization

This is a Go 1.24 command-line diagnostic assistant built with Eino. `cmd/chat/main.go` is the thin executable entry point. Code under `internal/` is organized by domain: `chat` owns the terminal UI, `agent` coordinates model calls, `intent` classifies requests, `tools` implements approval and remote commands, and `session` persists history. All Go tests are centralized in `test/` and use `package test`. Files such as `.env`, logs, and chat history are local artifacts.

## Build, Test, and Development Commands

- `make run`: run the interactive assistant with `go run ./cmd/chat`.
- `make build`: compile the binary to `bin/chat`.
- `make test`: run the complete suite with `go test ./...`.
- `go test -race ./test`: check tests for data races when a C toolchain is available.
- `make fmt`: format every Go package with `go fmt ./...`.
- `make vet`: run Go's static checks.
- `make tidy`: reconcile `go.mod` and `go.sum` after dependency changes.

Run `make fmt`, `make vet`, and `make test` before submitting changes.

## Coding Style & Naming Conventions

Follow standard Go formatting: `gofmt` tabs, short package names, `MixedCaps` identifiers, and doc comments for exports. Add functionality to the relevant `internal/<domain>` package rather than expanding `cmd/chat`. Wrap errors with context using `%w`. Mutating tools must pass through `Gate.Wrap` before registration.

## Testing Guidelines

Use the standard `testing` package. Place new files in `test/`, name them `<area>_test.go`, and name cases `TestBehavior` or `TestComponentBehavior`. Prefer table-driven tests for parsers, validation, and risk rules. Use `t.TempDir`, `t.Setenv`, and fakes instead of real credentials, persistent user files, model endpoints, or node commands. Do not weaken approval-gate assertions to make tests pass.

## Commit & Pull Request Guidelines

History contains only an initial commit, so no formal convention is established. Use a concise imperative subject, optionally scoped, such as `tools: reject duplicate approvals`. Keep commits focused. Pull requests should explain behavior and risk, list verification commands, link the relevant issue, and include terminal output or screenshots for UI changes. Call out configuration, dependency, persistence-format, or remote-execution changes explicitly.

## Security & Configuration

Never commit `.env`, API tokens, audit logs, diagnostic logs, or chat history. Document new environment variables in `README.md`, provide safe defaults where possible, and keep approval failures fail-closed.
