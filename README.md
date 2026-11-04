# pprof-lab: profiling a Go microservice

A small gRPC document-processing service built to be profiled. The baseline
branch contains real CPU and memory problems; two optimization branches fix
those problems based on evidence from `pprof`.

> **Branch: `main` (baseline).** This branch intentionally contains CPU
> hotspots and unbounded memory growth. The point is to find them with
> profiles, not to ship them.

## Branches

| Branch | Purpose |
| --- | --- |
| `main` | Baseline service with CPU hot spots and memory leaks. |
| `optimized_cpu` | Starts from `main` and fixes CPU hot spots found with CPU profiles. |
| `optimized_memory` | Starts from `main` and fixes allocation/retention problems found with heap profiles. |

Each optimization branch changes only the axis named in the branch. That keeps
the lesson clear: you can see which profile drove which change. Run
`git branch --show-current` to see where you are.

## What you will learn

- How a Go service exposes `net/http/pprof` on a separate admin port.
- How to capture and read CPU, heap, allocation, goroutine, block and mutex
  profiles.
- The difference between `inuse_space`, `alloc_space` and their object-count
  equivalents.
- How to go from a flame graph or `top` table to the source line that caused
  the problem with `list`, `peek` and `focus`.
- How to compare profiles from two branches with `-diff_base`.
- How to validate a fix with benchmarks (`-benchmem`) as well as profiles.
- Why some fixes trade one resource for another.

The full command reference is in
[`docs/PPROF_CHEATSHEET.md`](docs/PPROF_CHEATSHEET.md).

## Architecture

```
                 gRPC :50051                        HTTP :9090
  loadgen  ──────────────────►  DocumentService   ──────────────►  /debug/pprof/*
  (client)                      ├── docs map                        /stats
                                ├── analysis cache                  /healthz
                                └── processor
```

- **gRPC port `50051`** serves `Ingest`, `Analyze`, `Search` and `Stats`.
- **Admin port `9090`** serves pprof, JSON stats and health checks. Keeping it
  separate means profiling does not interfere with the gRPC protocol and the
  admin listener can be firewalled independently.
- **`loadgen`** creates synthetic documents and drives a configurable mix of
  ingest, analyze and search RPCs.
- **Labels**: every unary RPC is wrapped with a `grpc_method` pprof label, so
  you can attribute samples to an endpoint.

## Requirements

- Go 1.26+ (the `go` directive in `go.mod` is 1.26; newer toolchains
  auto-download if needed).
- `curl`.
- Optional: Docker + Docker Compose.
- Optional: Graphviz (`dot`) for the pprof graph view. The flame graph, top
  table and source view work without it.
- Optional: `protoc` only if you change the `.proto` file and run
  `make proto`. Generated code is committed.

## Quick start

```bash
# One command: starts the server, generates load, captures profiles, prints top
# tables and leaves branch-prefixed profiles in ./profiles.
make demo
```

`make demo` takes roughly 30 seconds with the defaults (20s load, 6s CPU
profile). It writes:

```
profiles/main-cpu.pprof
profiles/main-heap-inuse.pprof
profiles/main-heap-alloc.pprof
profiles/main-goroutine.pprof
profiles/main-stats.json
profiles/main-server.log
profiles/main-loadgen.log
```

The branch name is taken from git, so running the same command on the other
branches produces `optimized_cpu-*.pprof` and `optimized_memory-*.pprof`.

If you prefer to run each step yourself:

```bash
# Terminal 1
make run

# Terminal 2 - generate load while you profile
make load

# Terminal 3 - capture 30s of CPU and open the UI
make profile-cpu
```

The defaults are deliberately modest so the intentionally leaky baseline does
not fill your machine's memory. For a heavier profile, override them:

```bash
LOAD_DURATION=2m CONCURRENCY=8 DOC_BYTES=4096 make load
PROFILE_SECONDS=30 make profile-cpu
```

## Project layout

```
cmd/server/          gRPC server, admin server, interceptors
cmd/loadgen/         synthetic gRPC client/workload generator
internal/processing/ document store, cache, tokenizer, analysis, search
internal/docproc/    protobuf <-> domain mapping and gRPC service
internal/admin/      /debug/pprof, /stats, /healthz
gen/docproc/v1/      generated protobuf/gRPC code (committed)
proto/docproc/v1/    service contract
docs/                pprof cheat sheet
scripts/demo.sh      end-to-end profile capture
profiles/            local profile output (ignored by git)
```

## The service

`Ingest` stores documents in memory. `Analyze` tokenizes each body, counts
terms, computes a SHA-256 digest of the token stream and returns the top-N
terms. Results are cached by document ID. `Search` does a case-insensitive
non-overlapping substring count and returns snippets. `Stats` exposes document
count, total bytes, cache size, cache hits, heap size and goroutine count.

The workload is intentionally CPU- and allocation-heavy enough to create useful
profiles, but small enough to read.

## Walkthrough: finding the CPU problem

### 1. Start the service and load

```bash
make run
```

In another terminal:

```bash
make load
```

Watch the server's progress. `loadgen` prints the server's document count,
cache size and heap size every 5 seconds. Even in the baseline you should see
all three climb without bound.

### 2. Capture a CPU profile

```bash
# 30 seconds is the pprof default; this writes the profile to disk first
curl -fsS "http://127.0.0.1:9090/debug/pprof/profile?seconds=30" \
  -o profiles/cpu.pprof

go tool pprof -http=:8081 profiles/cpu.pprof
```

Or let pprof fetch it directly:

```bash
go tool pprof -http=:8081 "http://127.0.0.1:9090/debug/pprof/profile?seconds=30"
```

In the UI:

1. Open **Top** and sort by **flat**. This shows where CPU samples landed.
2. Open **Flame Graph**. Wide frames are expensive call paths.
3. Click a frame and use **Source** / **Peek** to see callers and annotated
   source lines.

Useful terminal equivalents:

```bash
go tool pprof -top -cum -nodecount=25 profiles/cpu.pprof
go tool pprof -peek='processing\.fingerprint' profiles/cpu.pprof
go tool pprof -list='processing\.' profiles/cpu.pprof
go tool pprof -tags profiles/cpu.pprof
go tool pprof -tagfocus='grpc_method=/docproc.v1.DocumentService/Analyze' profiles/cpu.pprof
```

### 3. What to look for in the baseline

Exact numbers depend on the machine and workload. The shapes are stable:

- **`regexp.MustCompile` / `regexp.(*Regexp).FindAllString`** in
  `processing.tokenize`: the regular expression is compiled on every call.
- **`runtime.concatstrings` and `runtime.memmove`**, called from
  `processing.fingerprint`: the token stream is joined with `+=`, which is
  quadratic in time and allocations.
- **`processing.countTerms`**: uniqueness is checked by scanning a growing
  slice for every token, which is O(n²).
- **`processing.topTerms`**: for every one of the N requested terms, the whole
  counts map is scanned again, which is O(N × distinct).
- **`strings.ToLower`** and GC/scheduler frames: secondary costs caused by the
  allocation volume. Do not chase runtime frames first; use `-cum` and `peek`
  to find the application code below them.

Some CPU profiles on macOS show a large amount of `runtime.pthread_cond_wait`
and `runtime.usleep`. That is scheduler/idle-thread noise from the platform. Use
`-focus='processing\.'` or `-ignore='runtime\.'` to clear the view, or profile
the Docker/Linux build for cleaner CPU attribution.

### 4. Confirm with source

```bash
go tool pprof -list='processing\.fingerprint' profiles/cpu.pprof
go tool pprof -list='processing\.tokenize' profiles/cpu.pprof
```

The annotated source makes the mechanism obvious: repeated `joined += token`,
a fresh `regexp.MustCompile` per document, and nested loops.

## Walkthrough: finding the memory problem

A CPU profile tells you what is running, not what is retained. For memory you
need the heap profile.

### 1. Capture heap and allocation profiles under load

```bash
curl -fsS "http://127.0.0.1:9090/debug/pprof/heap"   -o profiles/heap-inuse.pprof
curl -fsS "http://127.0.0.1:9090/debug/pprof/allocs" -o profiles/heap-alloc.pprof

go tool pprof -http=:8082 -sample_index=inuse_space profiles/heap-inuse.pprof
go tool pprof -http=:8083 -sample_index=alloc_space profiles/heap-alloc.pprof
```

Terminal:

```bash
go tool pprof -top -sample_index=inuse_space profiles/heap-inuse.pprof
go tool pprof -top -sample_index=alloc_space profiles/heap-alloc.pprof
go tool pprof -top -sample_index=alloc_objects profiles/heap-alloc.pprof
```

### 2. Read the right sample index

- **`inuse_space`** (default for `/heap`) answers "what is alive now?" Start
  here for leaks and retention. In the baseline you will see:
  - protobuf string allocation frames such as
    `protobuf/internal/impl.consumeStringValidateUTF8`, because the document
    bodies that arrived in gRPC requests are retained in the `docs` map;
  - `processing.topTerms`, because cached top-term strings are substrings of
    the lowercased document body and therefore keep the whole body alive;
  - `processing.fingerprint` and `runtime.concatstrings` transient garbage
    that has not been collected yet.
- **`alloc_space`** answers "what has been allocated in total?" Start here
  for GC pressure. In the baseline, `processing.fingerprint` normally
  dominates by an enormous margin because of the `+=` string building,
  followed by regexp and `countTerms` allocations.
- **`inuse_objects` / `alloc_objects`** are the same views by object count.

The allocation site is not always the retention site. Use `peek` to see the
call path and `list` to see the exact line:

```bash
go tool pprof -peek='processing\.fingerprint' profiles/heap-alloc.pprof
go tool pprof -list='processing\.fingerprint' profiles/heap-alloc.pprof
```

### 3. Watch growth over time

```bash
curl -s http://127.0.0.1:9090/stats
```

Repeat the call while load is running. On `main`, `documents`, `cache_entries`
and `heap_alloc_bytes` increase together. That is the signature of an unbounded
store and an unbounded cache, not a fixed overhead.

For a direct before/after comparison:

```bash
curl -fsS http://127.0.0.1:9090/debug/pprof/heap -o profiles/heap-before.pprof
# let load run for another 30 seconds
curl -fsS http://127.0.0.1:9090/debug/pprof/heap -o profiles/heap-after.pprof

go tool pprof -http=:8084 \
  -diff_base=profiles/heap-before.pprof \
  profiles/heap-after.pprof
```

Negative values in the diff are reductions. If the baseline is leaking, the
positive differences will be concentrated in document/string/cache allocation
sites.

### 4. GC symptoms

```bash
GODEBUG=gctrace=1 make run
```

High GC frequency with a growing heap means the process is spending more time
collecting garbage and still cannot keep up with retention. `GOMEMLIMIT` and
`GOGC` can change the shape of the curve, but they do not fix a leak; profile
first, then fix the retention.

## Optimizing CPU: `optimized_cpu`

```bash
git switch optimized_cpu
git diff main..optimized_cpu -- internal/processing
```

The changes are driven by the CPU profile and preserve the same API:

1. **Compile the tokenizer once.** `tokenize` used `regexp.MustCompile` on
   every document. The optimized branch uses a package-level pattern and an
   ASCII scanner, removing regexp compilation and backtracking from the hot
   path.
2. **Count terms in one pass.** `countTerms` replaces the O(n²) uniqueness
   slice scan with a map lookup. The counts map already exists, so it is the
   natural source of truth for `len(counts)`.
3. **Select top terms with one sort.** `topTerms` builds a slice and sorts it
   once (O(n log n)) instead of repeatedly scanning the map (O(N × distinct)).
4. **Build the digest incrementally.** `fingerprint` streams tokens into a
   `sha256` hasher with `io.WriteString`, eliminating the quadratic `+=`
   concatenation and its enormous allocation volume.

Run the same workload and compare:

```bash
git switch optimized_cpu
make demo

go tool pprof -top -diff_base=profiles/main-cpu.pprof \
  profiles/optimized_cpu-cpu.pprof
go tool pprof -http=:8084 \
  -diff_base=profiles/main-cpu.pprof \
  profiles/optimized_cpu-cpu.pprof
```

What to expect:

- regexp frames disappear or shrink dramatically;
- `countTerms` and `topTerms` flat time drops;
- `concatstrings` and GC pressure drop because fingerprint no longer allocates
  a new string per token;
- gRPC, protobuf, runtime and remaining work become a larger share of a much
  smaller total.

This branch intentionally does **not** fix retention. The document store and
analysis cache are still unbounded, and cached top terms still reference
lowercased bodies. A heap profile on this branch will still grow.

## Optimizing memory: `optimized_memory`

```bash
git switch optimized_memory
git diff main..optimized_memory -- internal/processing
git diff main..optimized_memory -- cmd/server
```

The changes are driven by `inuse_space`, `alloc_space` and `/stats` growth:

1. **Bound the document store.** `Config.MaxDocuments` (default 10,000) uses
   FIFO eviction. Evicted documents are removed from the analysis cache too, so
   the two structures cannot drift apart.
2. **Bound the cache with LRU.** `Config.CacheEntries` (default 4,096) uses a
   `map` + `container/list` LRU. Cache hits move an entry to the front; inserts
   evict the least recently used entry. The cache has its own mutex, separate
   from the document store.
3. **Do not retain whole documents through cached terms.** Top terms are
   cloned with `strings.Clone` before they are stored. A term is a few bytes;
   without cloning it is a slice into the lowercased body and keeps the entire
   body alive.
4. **Hoist regexp compilation and preallocate.** The tokenizer compiles its
   pattern once. Summary, document, ID and hit slices are created with known
   capacity.
5. **Replace quadratic string building.** `fingerprint` uses a pre-sized
   `strings.Builder` before hashing.
6. **Avoid per-search body copies.** Search uses an ASCII case-insensitive
   scanner instead of `strings.ToLower` on every document. This trades a
   little CPU for much lower allocation volume.
7. **Report evictions.** `StatsResponse.evictions` makes the bound observable
   from the load generator.

Run the same workload and compare:

```bash
git switch optimized_memory
make demo

curl -s http://127.0.0.1:9090/stats   # while the server is running

go tool pprof -top -sample_index=inuse_space \
  -diff_base=profiles/main-heap-inuse.pprof \
  profiles/optimized_memory-heap-inuse.pprof

go tool pprof -top -sample_index=alloc_space \
  -diff_base=profiles/main-heap-alloc.pprof \
  profiles/optimized_memory-heap-alloc.pprof
```

What to expect:

- `documents` and `cache_entries` plateau instead of growing forever;
- `evictions` becomes non-zero;
- `heap_alloc_bytes` stabilizes rather than climbing linearly;
- `alloc_space` from regexp, fingerprint and slice growth drops sharply;
- `inuse_space` no longer includes an ever-growing protobuf/string population.

This branch intentionally does **not** replace the O(N × distinct) top-terms
selection or the O(n²) term-counting algorithm. Those remain visible in CPU
profiles; they are the other branch's lesson.

## Comparing branches properly

Profiles are not benchmarks. The optimized branches do more work per second
because each operation is cheaper, so a fixed-duration profile can show *more*
total allocations even when per-operation allocation went down. Compare:

1. **Per-operation benchmarks** with the same input (see below).
2. **Profiles with identical workload parameters**, using `-diff_base`.
3. **Saturation metrics** such as ops/sec from the load generator.

Run the benchmark on each branch:

```bash
git switch main
make bench | tee profiles/bench-main.txt

git switch optimized_cpu
make bench | tee profiles/bench-optimized-cpu.txt

git switch optimized_memory
make bench | tee profiles/bench-optimized-memory.txt

# Optional, if you want statistical comparison
go run golang.org/x/perf/cmd/benchstat@latest \
  profiles/bench-main.txt profiles/bench-optimized-cpu.txt
```

`BenchmarkAnalyzeDocument` calls the uncached analysis path directly, so the
cache does not hide the work. `-benchmem` reports bytes and allocations per
operation. Expect `optimized_cpu` to reduce `ns/op`; expect
`optimized_memory` to reduce `B/op` and `allocs/op`. Some overlap is normal:
less allocation also means less GC work and often less CPU.

### Reference run

These are illustrative numbers from one Apple M3 Pro, not a guarantee. The
benchmark body is about 3.6 KB and requests the top 10 terms; each row is the
median of five runs. Run the commands above on your machine to get your own
baseline.

`BenchmarkAnalyzeDocument`:

| Branch | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| `main` | 225,000 | 1,793,000 | 1,713 |
| `optimized_cpu` | 16,600 | 53,000 | 15 |
| `optimized_memory` | 78,000 | 69,400 | 590 |

`BenchmarkSearch` (one stored document, mixed-case body):

| Branch | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| `main` | 4,800 | 3,192 | 4 |
| `optimized_cpu` | 4,800 | 3,192 | 4 |
| `optimized_memory` | 2,600 | 120 | 3 |

The memory branch trades some CPU for much lower per-search allocation; the
CPU branch does not touch search at all. The analyze benchmark shows the more
important lesson: the CPU branch wins on time and allocations, while the memory
branch keeps more explicit retention controls and still removes most of the
baseline's allocation volume.

## Configuration

### Server

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `-grpc-addr` | `GRPC_ADDR` | `:50051` | gRPC listen address |
| `-admin-addr` | `ADMIN_ADDR` | `:9090` | admin HTTP listen address |
| `-block-profile-rate` | `BLOCK_PROFILE_RATE` | `0` | `runtime.SetBlockProfileRate`; 1 records every blocking event |
| `-mutex-profile-fraction` | `MUTEX_PROFILE_FRACTION` | `0` | `runtime.SetMutexProfileFraction`; 1 records every contention event |
| `-max-recv-bytes` | `MAX_RECV_BYTES` | `8388608` | maximum gRPC receive message size |
| - | `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

On `optimized_memory` only:

| Environment | Default | Meaning |
| --- | --- | --- |
| `MAX_DOCUMENTS` | `10000` | maximum stored documents (FIFO eviction) |
| `CACHE_ENTRIES` | `4096` | maximum cached analyses (LRU eviction) |

### Load generator

| Flag | Default | Meaning |
| --- | --- | --- |
| `-addr` | `127.0.0.1:50051` | gRPC server address |
| `-mode` | `mixed` | `mixed`, `ingest`, `analyze` or `search` |
| `-duration` | `2m` | workload duration; `0` runs until Ctrl-C |
| `-concurrency` | `8` | concurrent workers |
| `-doc-bytes` | `4096` | approximate body size |
| `-seed-docs` | `100` | documents ingested before load starts |
| `-top-terms` | `10` | Analyze top-N |
| `-search-limit` | `10` | Search result limit |
| `-rpc-timeout` | `30s` | per-RPC deadline |
| `-progress` | `5s` | stats/progress interval |

Useful focused workloads:

```bash
# Ingest only: watch the document store and heap grow
go run ./cmd/loadgen -mode ingest -duration 1m -concurrency 2 -doc-bytes 2048

# Ingest + analyze every fresh document: CPU-heavy, cache misses
go run ./cmd/loadgen -mode analyze -duration 1m -concurrency 4

# Search only: allocation-heavy when bodies are copied for case folding
go run ./cmd/loadgen -mode search -duration 30s -concurrency 4
```

## Docker

```bash
# Build and start the service (detached)
docker compose up --build -d

# In another terminal, drive load and capture a profile from the host
docker compose --profile load up --build   # foreground: server + loadgen
curl -fsS "http://127.0.0.1:9090/debug/pprof/profile?seconds=30" \
  -o profiles/docker-cpu.pprof
go tool pprof -http=:8081 profiles/docker-cpu.pprof

docker compose --profile load down
```

Profiles captured in the container are ordinary Go pprof profiles and can be
inspected on the host. CPU attribution is usually cleaner on Linux than on
macOS because the runtime and scheduler behave differently. Keep the admin port
bound to localhost or a private network; `/debug/pprof` is not an authenticated
public API.

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| 404 on port 50051 | pprof is on the admin port, not gRPC | Use `:9090` |
| Empty or tiny CPU profile | Service was idle during capture | Run `make load` for longer than the profile window |
| `dot: executable file not found` | Graphviz missing | `brew install graphviz`, or use the flame graph |
| `bind: address already in use` | Previous server still running | `lsof -i :9090`, stop it, or set `ADMIN_ADDR`/`ADMIN_URL` |
| Heap grows extremely fast | Baseline is intentionally leaky | Use shorter/smaller load; the bounds exist on `optimized_memory` |
| `DeadlineExceeded` under load | Baseline does too much work per request | Reduce concurrency/document size or profile a smaller window |
| `make demo` refuses to start | Something already serves `/healthz` | Stop it or override the ports (see script variables) |
| `protoc: command not found` | Trying to regenerate protos | Install `protoc`; generated code is already committed |
| Runtime frames dominate CPU top | Idle-thread/platform noise | Use `-focus`/`-ignore`, or use the Docker/Linux build |

## Intentional issues (spoilers)

Read this after you have looked at the profiles once.

| Issue | Location | Profile evidence | Fixed in |
| --- | --- | --- | --- |
| Regexp compiled per document | `processing.tokenize` | CPU flat in `regexp.MustCompile`; `alloc_space` in regexp | both optimization branches |
| O(n²) uniqueness scan | `processing.countTerms` | CPU flat in `countTerms`; large unique slice in `alloc_space` | both |
| O(N × distinct) top-N scan | `processing.topTerms` | CPU in `topTerms` | `optimized_cpu` |
| O(n²) string concatenation | `processing.fingerprint` | Huge `alloc_space`; `concatstrings`/`memmove` in CPU | `optimized_cpu` (incremental hash), `optimized_memory` (builder) |
| Unbounded document store | `processing.Ingest` / `docs` | `inuse_space` grows linearly; `/stats` documents never plateau | `optimized_memory` |
| Unbounded analysis cache | `processing.Analyze` / `cache` | `inuse_space` grows; `/stats` cache entries never plateau | `optimized_memory` |
| Cached terms retain full bodies | `processing.topTerms` | `inuse_space` in `topTerms`; strings much larger than terms | `optimized_memory` via `strings.Clone` |
| Whole-body lowercase per search | `processing.Search` | `alloc_space` in `strings.ToLower` | `optimized_memory` |
| Missing slice preallocation | `Analyze`, `Ingest`, `Search` | `alloc_space` / `alloc_objects` from slice growth | `optimized_memory` |

The gRPC/protobuf layer also allocates. It is real production overhead and is
visible in the profiles, but it is not one of the intentional application bugs.

## Next steps

- Add a profile type for a specific document class with pprof labels.
- Capture a `runtime/trace` while the service is busy and inspect scheduler
  latency around GC pauses.
- Add Prometheus metrics alongside `/stats`.
- Try continuous profiling (for example Pyroscope or Grafana Alloy) against a
  long-running instance.
- Set `GOMEMLIMIT` on the baseline and watch how GC behavior changes, then
  explain why it still cannot fix an unbounded data structure.
