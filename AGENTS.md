# AGENTS.md — pprof-lab (Go)

`pprof-lab` is a small gRPC document-processing service used to teach
pprof-based profiling. The baseline is intentionally imperfect.

## Branches

| Branch | Purpose |
| --- | --- |
| `main` | Baseline with deliberate CPU hotspots and unbounded memory growth. |
| `optimized_cpu` | Fixes CPU hotspots found with CPU profiles. |
| `optimized_memory` | Fixes allocation/retention problems found with heap profiles. |

Do not "fix" the intentional baseline issues on `main`. Keep each optimization
branch limited to its named axis. Do not create additional branches.

## Project Setup

- **Module path**: `github.com/hrmeetsingh/pprof-lab`
- **Go version**: use the version in `go.mod` (currently Go 1.26)
- **gRPC**: `:50051`
- **Admin/pprof HTTP**: `:9090`

## Commands

| Task | Command |
| --- | --- |
| Build | `go build ./...` |
| Build binaries | `make build` |
| Test | `go test ./...` |
| Race tests | `go test -race ./...` |
| Benchmarks | `make bench` |
| Vet | `go vet ./...` |
| Format | `gofmt -w .` |
| Generate protos | `make proto` (requires `protoc`) |
| Run server | `make run` |
| Generate load | `make load` |
| Capture profiles | `make demo` |
| Docker | `docker compose up --build -d` |

## Conventions

- **Package layout**: `cmd/`, `internal/`, `gen/`, `proto/`.
- **Testing**: `_test.go` files alongside code; table-driven tests preferred.
- **Errors**: wrap with `fmt.Errorf("...: %w", err)`; check with
  `errors.Is`/`errors.As`.
- **Generated code**: `gen/docproc/v1/` is committed; regenerate it with
  `make proto` after changing `proto/docproc/v1/docproc.proto`.
- **Profiles**: `profiles/` is gitignored; never commit profile artifacts.

## Notes for Agents

- Read `README.md` before changing behavior; it is the lab manual.
- Keep the API in `proto/docproc/v1/docproc.proto` stable unless the task
  explicitly requires a change.
- Run `go mod tidy` after import changes.
- Verify changes with `go test ./...`, `go vet ./...` and `gofmt`.
- Preserve the intentional teaching differences between branches.
