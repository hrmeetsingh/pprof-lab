# AGENTS.md — test_opencode (Go)

This is a new/empty Go repository. Establish conventions early.

## Project Setup
- **Module path**: Set in `go.mod` (run `go mod init <module-path>`)
- **Go version**: Use the version in `go.mod` (or `.tool-versions`/`.go-version` if present)

## Commands
| Task | Command |
|------|---------|
| Build | `go build ./...` |
| Test (all) | `go test ./...` |
| Test (single package) | `go test ./pkg/...` |
| Test (verbose) | `go test -v ./...` |
| Lint | `golangci-lint run` (install if needed) |
| Format | `gofmt -w .` / `goimports -w .` |
| Vet | `go vet ./...` |
| Generate | `go generate ./...` |

## Conventions
- **Package layout**: Standard Go layout (`cmd/`, `internal/`, `pkg/`)
- **Dependencies**: Use `go mod tidy` after adding imports
- **Testing**: `_test.go` files alongside code; table-driven tests preferred
- **Errors**: Wrap with `fmt.Errorf("...: %w", err)`; check with `errors.Is/As`

## CI / Pre-commit
- Add `golangci-lint`, `go vet`, `go test` to CI pipeline
- Consider `pre-commit` hooks for fmt/vet/lint

## Notes for Agents
- No existing code — verify module path before generating files
- Prefer `internal/` for non-exported code to enforce encapsulation
- Run `go mod tidy` after any import changes