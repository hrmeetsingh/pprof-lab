# pprof-lab: a hands-on Go profiling manual

A small gRPC document-processing service with deliberate CPU and memory
problems. You will run it under load, capture profiles, find the code
responsible, fix one axis at a time, and verify the fix with profiles and
benchmarks.

This README is a lab manual, not just an API reference. Follow it from top to
bottom with the repository open. Every lab tells you what to run, what to look
for, which code to read, and how to check your understanding.

> **Branch: `main` (baseline).** This branch intentionally contains CPU
> hotspots and unbounded memory growth. The point is to find them with
> profiles, not to ship them.

## How to use this manual

- Set aside 60-90 minutes for the full path. If you only have five minutes,
  run `make demo`, then read [Lab 1](#lab-1-cpu---where-does-the-time-go) and
  its sample output.
- Run every command from the repository root.
- Use three terminals:

  | Terminal | Purpose | Command |
  | --- | --- | --- |
  | 1 | the service | `make run` |
  | 2 | traffic | `make load` |
  | 3 | capture and inspect profiles | `curl`, `go tool pprof` |

- Labs 1-3 run on `main`. Lab 4 switches to `optimized_cpu`. Lab 5 switches to
  `optimized_memory`. Stop the server with `Ctrl-C` before switching branches.
- "Read the code" means opening the named file and finding the function. The
  snippets here are excerpts, not the whole implementation.
- Do the checkpoint questions before opening the answer block. They are there
  to make the profile evidence stick.
- The same manual is present on every branch; the banner at the top tells you
  where you are and what that branch changes.

### Choose a path

| Path | Best for | Start |
| --- | --- | --- |
| Guided local | Learning the full workflow | [Lab 0](#lab-0-get-the-service-running) |
| Fast demo | Seeing profiles immediately | `make demo`, then [Lab 1](#lab-1-cpu---where-does-the-time-go) |
| Docker | No local Go toolchain, or cleaner Linux CPU profiles | [Docker](#docker) |

### Lab map

| Lab | Question | Evidence | Branch |
| --- | --- | --- | --- |
| [0](#lab-0-get-the-service-running) | Can I run it? | `/healthz`, `/stats` | `main` |
| [1](#lab-1-cpu---where-does-the-time-go) | Where does CPU time go? | CPU profile | `main` |
| [2](#lab-2-memory---what-is-retained) | What keeps the heap alive? | heap/allocs | `main` |
| [3](#lab-3-beyond-cpu-and-heap) | What about waiting and scheduling? | goroutine, block, mutex, trace | `main` |
| [4](#lab-4-fix-the-cpu-path) | Can I remove the CPU hotspots? | CPU diff, benchmarks | `optimized_cpu` |
| [5](#lab-5-fix-the-memory-path) | Can I bound retention? | heap diff, `/stats` | `optimized_memory` |
| [6](#lab-6-compare-the-branches-and-write-your-conclusion) | How do I prove the result? | benchmarks, profile diff | all |

## Branches

| Branch | Purpose |
| --- | --- |
| `main` | Baseline service with CPU hotspots and unbounded memory growth. |
| `optimized_cpu` | Starts from `main` and fixes CPU hotspots found with CPU profiles. |
| `optimized_memory` | Starts from `main` and fixes allocation/retention problems found with heap profiles. |

Each optimization branch changes only the axis named in the branch. That keeps
the lesson clear: you can see which profile drove which change.

```
main ──┬── optimized_cpu
       └── optimized_memory
```

Run `git branch --show-current` to see where you are. The first two labs are
about collecting evidence; do not switch branches until you have looked at the
baseline profiles yourself.

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

## What you are profiling

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

### Follow one request through the code

Open these files side by side while you read the profiles. This is the path
that produces most of the samples you are about to see.

1. **Ingest.** `cmd/loadgen/main.go` creates a document and calls the gRPC
   client. `internal/docproc/service.go` converts the protobuf message into a
   `processing.Document`. `internal/processing/processor.go` stores it in the
   `docs` map in `(*Processor).Ingest`.
2. **Analyze.** `(*Processor).Analyze` first checks the `cache` map. On a miss
   it calls `analyzeDocument`, which runs:
   - `tokenize` - lowercase and split the body into terms;
   - `countTerms` - count every term;
   - `topTerms` - select the most frequent terms;
   - `fingerprint` - hash the token stream.
3. **Cache.** The resulting `Summary` is stored in the `cache` map, keyed by
   document ID. The next Analyze for that document can reuse it.
4. **Search.** `(*Processor).Search` lowercases each document body, counts
   non-overlapping occurrences of the query, and returns snippets.
5. **Stats.** The admin server calls `(*Processor).Stats`, which reads the
   maps and `runtime.MemStats` and returns a JSON snapshot.

### Project layout

| Path | What lives there |
| --- | --- |
| `cmd/server/` | gRPC server, admin server, interceptors, flags |
| `cmd/loadgen/` | synthetic gRPC client and workload generator |
| `internal/processing/` | document store, cache, tokenizer, analysis, search |
| `internal/docproc/` | protobuf/domain mapping and gRPC handlers |
| `internal/admin/` | `/debug/pprof`, `/stats`, `/healthz` |
| `gen/docproc/v1/` | generated protobuf/gRPC code (committed) |
| `proto/docproc/v1/` | service contract |
| `docs/` | pprof cheat sheet |
| `scripts/demo.sh` | end-to-end profile capture |
| `profiles/` | local profile output (ignored by git) |

### The stats endpoint

`http://127.0.0.1:9090/stats` returns the exported Go field names:

| JSON key | Meaning | Baseline behavior |
| --- | --- | --- |
| `Documents` | documents currently stored | grows forever |
| `TotalBytes` | title + body bytes currently stored | grows forever |
| `Analyses` | analyses computed after a cache miss | grows with traffic |
| `CacheHits` | analyses served from cache | grows with repeated IDs |
| `CacheEntries` | cached summaries | grows forever |
| `HeapAllocBytes` | live heap after the last GC | climbs |
| `HeapInuseBytes` | heap spans currently in use | climbs |
| `Goroutines` | live goroutines | mostly stable |
| `Evictions` | documents evicted by a bound | always `0` on `main` |

The gRPC `Stats` response uses the proto field names instead
(`documents`, `cache_entries`, `heap_alloc_bytes`, and so on). The load
generator prints those lowercase names in its progress line.

## Requirements

- Go 1.26+ (the `go` directive in `go.mod` is 1.26; newer toolchains can
  auto-download).
- `curl`.
- Free TCP ports `50051` and `9090`.
- Optional: Docker + Docker Compose for the container path.
- Optional: Graphviz (`dot`) for pprof's graph view. Top, flame graph and
  source views work without it.
- Optional: `protoc` only if you change the `.proto` file and run `make proto`.
  Generated code is committed.

## Lab 0: Get the service running

**Question:** can I run the service, drive traffic, and see its state?

### Run it

```bash
go version          # expect Go 1.26 or newer
go test ./...       # should pass before you start changing anything
make run            # terminal 1
```

Expected startup output (addresses may differ):

```text
level=INFO msg="admin server listening" addr=[::]:9090 pprof=http://[::]:9090/debug/pprof/ stats=http://[::]:9090/stats
level=INFO msg="gRPC server listening" addr=[::]:50051 block_profile_rate=0 mutex_profile_fraction=0
```

In terminal 2, check the admin endpoints:

```bash
curl -fsS http://127.0.0.1:9090/healthz
curl -fsS http://127.0.0.1:9090/stats
```

The health check returns `ok`. The first stats snapshot looks like this:

```json
{"Documents":0,"TotalBytes":0,"Analyses":0,"CacheHits":0,"CacheEntries":0,"HeapAllocBytes":842432,"HeapInuseBytes":1671168,"Goroutines":7,"Evictions":0}
```

Now generate a short workload in terminal 3:

```bash
make load
```

`make load` uses 30 seconds, 3 workers and 2 KB documents by default. The load
generator prints progress every five seconds:

```text
mode=mixed concurrency=3 doc_bytes=2048 seeded=100 target=127.0.0.1:50051
progress: ingest=... analyze=... search=... errors=0 | server docs=... cache=... heap=...
done in 30.0s: ingest=... analyze=... search=... ops=... errors=...
server stats: docs=... cache=... analyses=... cache_hits=... heap=... goroutines=... evictions=0
```

### Read the code

- `cmd/server/main.go` starts the gRPC listener and the admin listener, and
  registers the unary interceptors.
- `internal/admin/admin.go` wires `/debug/pprof`, `/stats` and `/healthz` onto
  the admin mux.
- `internal/docproc/service.go` maps protobuf requests to `processing`
  methods.

### Checkpoint 0

1. Which port serves pprof, and which port serves gRPC?
2. Why is pprof on a separate listener instead of the gRPC server?
3. What is the difference between `Documents`, `CacheEntries` and
   `Analyses`?
4. Why is `Evictions` always zero on this branch?

<details>
<summary>Answers</summary>

1. pprof is on `9090`; gRPC is on `50051`.
2. The admin listener can be bound to localhost or a private network, kept
   off the public API, and protected independently. pprof also uses ordinary
   HTTP, while gRPC uses HTTP/2 framing.
3. `Documents` counts stored documents. `CacheEntries` counts cached analysis
   results. `Analyses` counts cache misses that actually ran the analysis
   pipeline.
4. The baseline has no document or cache bound, so nothing is ever evicted.

</details>

## Lab 1: CPU - where does the time go?

**Question:** why is `Analyze` slow, and which lines are responsible?

### Capture a CPU profile under load

Keep the server from Lab 0 running, or start it again with `make run`. In
terminal 2, use a load run that will outlive the profile:

```bash
LOAD_DURATION=2m make load
```

In terminal 3, capture 20 seconds of CPU samples:

```bash
mkdir -p profiles
curl -fsS "http://127.0.0.1:9090/debug/pprof/profile?seconds=20" \
  -o profiles/lab-cpu.pprof

go tool pprof -http=:8081 profiles/lab-cpu.pprof
```

The `curl` call blocks for the whole sample window. That is normal: the Go
runtime is collecting samples, and the response is not complete until the
window ends. Open <http://localhost:8081> and try:

1. **Top** sorted by **flat** - where samples landed.
2. **Flame Graph** - which call paths are wide.
3. **Source** after clicking a frame - the annotated lines.
4. **Peek** - callers and callees of a frame.

### Read it in the terminal

```bash
go tool pprof -top -nodecount=15 profiles/lab-cpu.pprof

go tool pprof -top -cum -focus='pprof-lab' -nodecount=15 \
  profiles/lab-cpu.pprof

go tool pprof -tags profiles/lab-cpu.pprof
```

A trimmed sample from a Docker/Linux run looks like this. Your percentages
and total sample time will differ; the application frames are the lesson.

```text
Type: cpu
Duration: 6s, Total samples = 11400ms (189.99%)
      flat  flat%   sum%        cum   cum%
    1160ms 10.18% 10.18%     1380ms 12.11%  strings.ToLower
     760ms  6.67% 25.00%      760ms  6.67%  runtime.memmove
     400ms  3.51% 34.82%      400ms  3.51%  internal/bytealg.IndexByteString
     200ms  1.75% 47.81%     1800ms 15.79%  runtime.concatstrings
     170ms  1.49% 52.63%      540ms  4.74%  regexp.(*Regexp).tryBacktrack
     150ms  1.32% 55.44%      510ms  4.47%  processing.countTerms
```

The `-tags` output attributes samples to RPCs:

```text
grpc_method: Total 5.87s of 11.40s (51.49%)
             3.82s (33.51%): /docproc.v1.DocumentService/Analyze
             2.04s (17.89%): /docproc.v1.DocumentService/Search
             10ms (0.088%): /docproc.v1.DocumentService/Ingest
```

### Read the profile like a detective

- **Flat** is time spent in the function itself. **Cum** includes its callees.
  Start with flat to find expensive primitives, then use cum and peek to find
  the application code that called them.
- `runtime.concatstrings` and `runtime.memmove` are symptoms, not the cause.
  Ask: "who is concatenating strings?"
- `strings.ToLower` can come from tokenization or from search. Use tags or
  `-peek` to tell which endpoint is responsible.
- Ignore scheduler and syscall frames first. On macOS you may also see
  `runtime.pthread_cond_wait` and `runtime.usleep`; that is idle-thread noise.

Useful follow-ups:

```bash
go tool pprof -peek='runtime\.concatstrings' profiles/lab-cpu.pprof
go tool pprof -list='processing\.fingerprint' profiles/lab-cpu.pprof
go tool pprof -list='processing\.tokenize' profiles/lab-cpu.pprof
go tool pprof -list='processing\.countTerms' profiles/lab-cpu.pprof
go tool pprof -list='processing\.topTerms' profiles/lab-cpu.pprof
go tool pprof -tagfocus='grpc_method=/docproc.v1.DocumentService/Analyze' \
  profiles/lab-cpu.pprof
```

### Read the code behind the profile

Open `internal/processing/processor.go`. The baseline contains four deliberate
CPU problems.

**1. The regexp is compiled for every document** in `tokenize`:

```go
func tokenize(text string) []string {
	re := regexp.MustCompile(`[A-Za-z0-9]+`)
	lower := strings.ToLower(text)
	return re.FindAllString(lower, -1)
}
```

Profile evidence: `regexp.MustCompile` and `regexp.(*Regexp).tryBacktrack`
appear in CPU profiles, and regexp allocations appear in `alloc_space`.
Compilation is pure overhead repeated once per document.

**2. Uniqueness is checked with a growing slice** in `countTerms`:

```go
for _, token := range tokens {
	counts[token]++
	found := false
	for _, existing := range unique {
		if existing == token {
			found = true
			break
		}
	}
	if !found {
		unique = append(unique, token)
	}
}
```

Profile evidence: `processing.countTerms` has flat CPU, and the `unique`
slice shows up in allocation profiles. The scan is O(n²) when a document has
many distinct terms.

**3. Top-N selection rescans the whole map** in `topTerms`:

```go
for len(result) < n {
	bestTerm := ""
	bestCount := -1
	for term, count := range counts {
		// ... find the best term not already used ...
	}
	used[bestTerm] = struct{}{}
	result = append(result, TermCount{Term: bestTerm, Count: int64(bestCount)})
}
```

Profile evidence: `processing.topTerms` is visible for documents with many
distinct terms. The work is O(N × distinct) for N requested terms.

**4. The digest is built with `+=`** in `fingerprint`:

```go
joined := ""
for i, token := range tokens {
	if i > 0 {
		joined += " "
	}
	joined += token
}
sum := sha256.Sum256([]byte(joined))
```

Profile evidence: `runtime.concatstrings` and `runtime.memmove` dominate, and
`alloc_space` attributes an enormous amount of allocation to this loop. Each
`+=` may allocate a new, larger string.

### Checkpoint 1

1. Which function compiles a regular expression per call?
2. Why do `concatstrings` and `memmove` show up under CPU even though the
   program is "just hashing"?
3. `countTerms` already has a `counts` map. Why does it also maintain a
   `unique` slice?
4. Which of the four hotspots would you expect to matter most for a document
   with few repeated words? Why?

<details>
<summary>Answers</summary>

1. `processing.tokenize`.
2. `fingerprint` builds the joined token stream with `+=`. Each concatenation
   copies the previous string and the new token, so total copying is
   quadratic and shows up as `memmove`/`concatstrings`.
3. The baseline uses the slice to count unique terms. It is redundant: the
   number of keys in the `counts` map is already the unique-term count.
4. `countTerms` and `topTerms` do more work as distinct terms grow. A document
   with few repeats has many distinct terms, so the O(n²) scan and the
   O(N × distinct) top-N scan are both exaggerated.

</details>

## Lab 2: Memory - what is retained?

**Question:** why does the heap keep growing, and what is holding the memory
alive?

### Capture heap and allocation profiles

Keep the load running from Lab 1, or start a fresh run:

```bash
LOAD_DURATION=2m make load
```

Capture both views:

```bash
curl -fsS "http://127.0.0.1:9090/debug/pprof/heap" \
  -o profiles/lab-heap-inuse.pprof
curl -fsS "http://127.0.0.1:9090/debug/pprof/allocs" \
  -o profiles/lab-heap-alloc.pprof

go tool pprof -http=:8082 -sample_index=inuse_space \
  profiles/lab-heap-inuse.pprof
go tool pprof -http=:8083 -sample_index=alloc_space \
  profiles/lab-heap-alloc.pprof
```

Terminal equivalents:

```bash
go tool pprof -top -sample_index=inuse_space profiles/lab-heap-inuse.pprof
go tool pprof -top -sample_index=alloc_space profiles/lab-heap-alloc.pprof
go tool pprof -top -sample_index=alloc_objects profiles/lab-heap-alloc.pprof
```

A trimmed `inuse_space` sample from the same Docker run:

```text
Showing nodes accounting for 176.05MB, 99.44% of 177.05MB total
      flat  flat%   sum%        cum   cum%
   93.20MB 52.64% 52.64%    93.20MB 52.64%  google.golang.org/protobuf/internal/impl.consumeStringValidateUTF8
   56.62MB 31.98% 84.62%    56.62MB 31.98%  internal/bytealg.MakeNoZero
      10MB  5.65% 90.27%       10MB  5.65%  processing.topTerms
    6.97MB  3.94% 94.21%     6.97MB  3.94%  processing.(*Processor).Ingest
```

Do not read too much into the exact frames. The lesson is that live bytes are
concentrated in document bodies, strings, and cached summaries, and the total
keeps rising while load runs.

### Pick the right sample index

| Index | Question it answers | Use it for |
| --- | --- | --- |
| `inuse_space` | What is alive right now? | leaks and retention |
| `alloc_space` | What has been allocated in total? | GC pressure and churn |
| `inuse_objects` | How many live objects? | object-count leaks |
| `alloc_objects` | How many allocations in total? | allocation-rate problems |

The allocation site is not always the retention site. `alloc_space` can point
at `fingerprint` because it allocates heavily, while `inuse_space` points at
the maps and cached strings that keep memory alive. Use both.

### Watch retention live

While load is running, sample `/stats` a few times:

```bash
for i in 1 2 3; do
  curl -s http://127.0.0.1:9090/stats
  echo
  sleep 10
done
```

On `main`, the interesting fields move together:

| Time | Documents | CacheEntries | HeapAllocBytes | Evictions |
| --- | ---: | ---: | ---: | ---: |
| 0s | 0 | 0 | ~1 MB | 0 |
| 10s | growing | growing | growing | 0 |
| 20s | growing | growing | growing | 0 |

Record your own numbers here:

| Time | Documents | CacheEntries | HeapAllocBytes | Evictions |
| --- | ---: | ---: | ---: | ---: |
| 0s | | | | |
| 10s | | | | |
| 20s | | | | |

If all three counters climb without a plateau, you are looking at retention,
not a fixed startup cost.

### Read the code behind the profile

Open `internal/processing/processor.go` and look at the `Processor` struct:

```go
type Processor struct {
	mu         sync.RWMutex
	docs       map[string]Document
	cache      map[string]*Summary
	totalBytes int64
	// ...
}
```

Neither map has a size limit. `Ingest` adds to `docs`; `Analyze` adds to
`cache`:

```go
func (p *Processor) Ingest(docs []Document) ([]string, error) {
	// ...
	for _, doc := range docs {
		p.docs[doc.ID] = doc
		p.totalBytes += int64(len(doc.Title) + len(doc.Body))
		ids = append(ids, doc.ID)
	}
	return ids, nil
}

func (p *Processor) putCached(id string, summary *Summary) {
	p.mu.Lock()
	p.cache[id] = summary
	p.mu.Unlock()
}
```

There is a second, subtler retention path. `tokenize` lowercases the whole
body and slices it into terms:

```go
lower := strings.ToLower(text)
return re.FindAllString(lower, -1)
```

Those term strings share the backing array of `lower`. `topTerms` puts the
winning terms into a `Summary`, and the cache keeps the summary:

```go
result = append(result, TermCount{Term: bestTerm, Count: int64(bestCount)})
```

So a cached term of a few bytes can keep an entire lowercased document body
alive. This is why `inuse_space` shows `processing.topTerms` even though the
returned terms are small.

Search also allocates a full lowercased copy of every document on every
query:

```go
haystack := strings.ToLower(doc.Body)
score := strings.Count(haystack, needle)
```

That is allocation churn rather than permanent retention, but it drives GC
pressure and shows up in `alloc_space`.

### Compare two heap snapshots

With load still running:

```bash
curl -fsS http://127.0.0.1:9090/debug/pprof/heap -o profiles/heap-before.pprof
# let load run for another 20-30 seconds
curl -fsS http://127.0.0.1:9090/debug/pprof/heap -o profiles/heap-after.pprof

go tool pprof -http=:8084 \
  -diff_base=profiles/heap-before.pprof \
  profiles/heap-after.pprof
```

Positive values are memory added between the snapshots. On the baseline they
are concentrated in document bodies, cached summaries and strings. Negative
values are reductions.

### Checkpoint 2

1. What is the difference between `inuse_space` and `alloc_space`?
2. Why can a cached term that is only a few bytes long retain a whole
   document body?
3. Which two maps grow without bound on `main`?
4. Would setting `GOGC` or `GOMEMLIMIT` fix the growth? Why or why not?

<details>
<summary>Answers</summary>

1. `inuse_space` is live heap at the moment of the snapshot. `alloc_space` is
   cumulative allocation since the process started (sampled).
2. Go substrings share the backing array of the original string. A term
   produced from the lowercased body keeps that entire backing array alive as
   long as the term is reachable from the cache.
3. `Processor.docs` and `Processor.cache`.
4. No. They change when and how aggressively the GC runs. They cannot free
   data that is still reachable from an unbounded map. A memory limit may turn
   the leak into an OOM or a GC death spiral instead.

</details>

## Lab 3: Beyond CPU and heap

**Question:** what do the other profile types tell me about this service?

These profiles are optional, but they are useful when a service is slow for a
reason other than raw CPU or retained memory.

### Goroutines

```bash
curl -fsS http://127.0.0.1:9090/debug/pprof/goroutine \
  -o profiles/lab-goroutine.pprof
go tool pprof -top profiles/lab-goroutine.pprof
go tool pprof -traces profiles/lab-goroutine.pprof | head -80
```

Expect a small number of gRPC handler goroutines, accept loops, the admin
server, and runtime workers. A growing goroutine count is a different kind of
leak: look for handlers that never return or goroutines blocked forever.

### Block and mutex profiles

These are disabled by default because they cost something. Restart the server
with both enabled:

```bash
BLOCK_PROFILE_RATE=1 MUTEX_PROFILE_FRACTION=1 make run
```

Generate load, then capture:

```bash
curl -fsS http://127.0.0.1:9090/debug/pprof/block \
  -o profiles/lab-block.pprof
curl -fsS http://127.0.0.1:9090/debug/pprof/mutex \
  -o profiles/lab-mutex.pprof

go tool pprof -top profiles/lab-block.pprof
go tool pprof -top profiles/lab-mutex.pprof
```

The block profile reports time goroutines spent blocked (channel, select,
mutex, syscall). The mutex profile reports contention on mutexes. The
processor uses a single `RWMutex` for the document store, so look for readers
and writers waiting on it under high concurrency.

### Execution trace

```bash
curl -fsS "http://127.0.0.1:9090/debug/pprof/trace?seconds=5" \
  -o profiles/lab-trace.out
go tool trace profiles/lab-trace.out
```

The trace shows scheduler activity, GC pauses, goroutine blocking and
unblocking over time. It complements a CPU profile: pprof tells you where CPU
went, while the trace tells you what was runnable, blocked or waiting.

### Checkpoint 3

1. Why are block and mutex profiling disabled by default?
2. What is the difference between the block profile and the mutex profile?
3. When would a trace be more useful than a CPU profile?

<details>
<summary>Answers</summary>

1. They add runtime overhead and can change the behavior you are measuring.
   Enable them for a targeted investigation, then turn them off.
2. The block profile measures time spent blocked on synchronization and
   syscalls. The mutex profile measures contention specifically on mutexes.
3. When the problem is latency, scheduling, GC pauses, or goroutines waiting
   rather than CPU time. A busy service can have low CPU and still be slow
   because work is blocked.

</details>

## Lab 4: Fix the CPU path

Stop the server. You are about to change branches and rebuild.

**Question:** can I remove the CPU hotspots without changing the API or
accidentally fixing memory retention?

### Create a matched baseline

The comparison in this lab uses the 6-second CPU profile produced by
`make demo`. Create that baseline before switching branches (skip this if
`profiles/main-cpu.pprof` already exists):

```bash
make demo
```

This also creates `profiles/main-heap-inuse.pprof` and
`profiles/main-heap-alloc.pprof`, which Lab 5 uses.

### Inspect the branch

```bash
git switch optimized_cpu
git branch --show-current
git diff main..optimized_cpu -- internal/processing
```

The branch changes one axis: CPU. The document store and cache are still
unbounded on purpose.

### Read the fixes

**1. A hand-written ASCII tokenizer replaces the per-call regexp:**

```go
func tokenize(text string) []string {
	lower := strings.ToLower(text)
	tokens := make([]string, 0, len(lower)/6)
	start := -1
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			tokens = append(tokens, lower[start:i])
			start = -1
		}
	}
	if start >= 0 {
		tokens = append(tokens, lower[start:])
	}
	return tokens
}
```

**2. `countTerms` uses the map it already has:**

```go
func countTerms(tokens []string) (map[string]int, int) {
	counts := make(map[string]int, len(tokens))
	for _, token := range tokens {
		counts[token]++
	}
	return counts, len(counts)
}
```

**3. `topTerms` sorts once instead of rescanning the map N times:**

```go
terms := make([]termFrequency, 0, len(counts))
for term, count := range counts {
	terms = append(terms, termFrequency{term: term, count: count})
}
sort.Slice(terms, func(i, j int) bool {
	if terms[i].count != terms[j].count {
		return terms[i].count > terms[j].count
	}
	return terms[i].term < terms[j].term
})
```

**4. `fingerprint` streams tokens into the hasher:**

```go
func fingerprint(tokens []string) string {
	h := sha256.New()
	buf := make([]byte, 0, 4096)
	for i, token := range tokens {
		if i > 0 {
			buf = append(buf, ' ')
		}
		buf = append(buf, token...)
		if len(buf) >= cap(buf) {
			_, _ = h.Write(buf)
			buf = buf[:0]
		}
	}
	// ... flush the remainder and return hex.EncodeToString(h.Sum(nil))
}
```

The digest is identical to the baseline; only the intermediate representation
changed.

### Re-run the experiment

```bash
make demo
```

`make demo` starts the service, warms it up, captures six seconds of CPU and
a heap snapshot, then prints the top tables. Because you are on
`optimized_cpu`, the artifacts are prefixed with the branch name:

```text
profiles/optimized_cpu-cpu.pprof
profiles/optimized_cpu-heap-inuse.pprof
profiles/optimized_cpu-heap-alloc.pprof
profiles/optimized_cpu-goroutine.pprof
profiles/optimized_cpu-stats.json
profiles/optimized_cpu-server.log
profiles/optimized_cpu-loadgen.log
```

Compare against the baseline capture:

```bash
go tool pprof -top -diff_base=profiles/main-cpu.pprof \
  profiles/optimized_cpu-cpu.pprof

go tool pprof -http=:8084 -diff_base=profiles/main-cpu.pprof \
  profiles/optimized_cpu-cpu.pprof
```

What to expect:

- regexp frames disappear or shrink dramatically;
- `countTerms` and `topTerms` flat time drops;
- `concatstrings` and GC pressure drop because `fingerprint` no longer builds
  a new string per token;
- gRPC, protobuf, runtime and remaining work become a larger share of a much
  smaller total.

### Prove it with a benchmark

```bash
make bench | tee profiles/bench-optimized-cpu.txt
```

`BenchmarkAnalyzeDocument` calls the uncached analysis path directly, so the
cache does not hide the work. Look at `ns/op`, `B/op` and `allocs/op`.

### What did not change

The document store and analysis cache are still unbounded. Cached terms still
share the lowercased body. A heap profile on this branch will still grow, and
`Evictions` is still zero. That is the next lab's job.

### Checkpoint 4

1. Why is the hand-written tokenizer faster than the regexp version?
2. Why can `countTerms` return `len(counts)` instead of maintaining a
   separate unique slice?
3. Did this branch fix the memory growth? What is the evidence?
4. Why might a fixed-duration run on this branch allocate *more* total bytes
   than the baseline?

<details>
<summary>Answers</summary>

1. It avoids compiling a regexp per document and avoids regexp backtracking
   and its allocations. It is specialized to the ASCII token rules of this
   service.
2. The map has exactly one key per distinct term, so its length is the unique
   count. The separate slice was redundant work.
3. No. The store and cache are unchanged and still unbounded; heap profiles
   still climb and `Evictions` remains zero.
4. The service completes more operations in the same wall-clock window, so
   cumulative allocation can be higher even though allocation per operation
   is lower. Compare per-operation benchmarks and use `-diff_base` with
   identical workloads.

</details>

## Lab 5: Fix the memory path

Stop the server, then switch branches.

**Question:** can I bound retention and reduce allocation without changing the
API or rewriting the CPU algorithms?

### Inspect the branch

```bash
git switch optimized_memory
git branch --show-current
git diff main..optimized_memory -- internal/processing cmd/server
```

The branch changes memory behavior: bounds, retention, allocation volume and
search copies. The O(N × distinct) top-terms selection is deliberately left in
place for the CPU branch.

### Read the fixes

**1. Bounds are explicit configuration:**

```go
type Config struct {
	MaxDocuments int
	CacheEntries int
}

func DefaultConfig() Config {
	return Config{
		MaxDocuments: 10_000,
		CacheEntries: 4_096,
	}
}
```

The server reads `MAX_DOCUMENTS` and `CACHE_ENTRIES` and passes them to
`NewProcessorWithConfig`.

**2. The document store uses FIFO eviction:**

```go
func (p *Processor) evictLocked() {
	for len(p.docs) > p.maxDocs {
		oldest := p.order.Front()
		// ...
		id, _ := oldest.Value.(string)
		p.order.Remove(oldest)

		doc, ok := p.docs[id]
		if !ok {
			continue
		}
		delete(p.docs, id)
		p.totalBytes -= int64(len(doc.Title) + len(doc.Body))
		p.cache.delete(id)
		p.evictions.Add(1)
	}
}
```

Evicting a document also removes its cached analysis, so the store and cache
cannot drift apart.

**3. The cache is a bounded LRU:**

```go
func (c *summaryCache) get(key string) (*Summary, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	element, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(element)
	return element.Value.(*cacheEntry).summary, true
}
```

`put` inserts at the front and removes from the back when the cache exceeds
its capacity. The cache has its own mutex, separate from the document store.

**4. Cached terms are detached from the body:**

```go
result = append(result, TermCount{
	Term:  strings.Clone(bestTerm),
	Count: int64(bestCount),
})
```

`strings.Clone` copies the few bytes of the term, so the cache no longer keeps
the entire lowercased body alive.

**5. Search avoids the full-body lowercase copy:**

```go
score, first := countFoldASCII(doc.Body, needle)
```

`countFoldASCII` compares ASCII bytes case-insensitively and counts
non-overlapping matches without allocating a new string per document.

**6. The tokenizer is hoisted and `fingerprint` uses a pre-sized builder:**

```go
tokenPattern = regexp.MustCompile(`[A-Za-z0-9]+`)

var joined strings.Builder
joined.Grow(size)
for i, token := range tokens {
	if i > 0 {
		joined.WriteByte(' ')
	}
	joined.WriteString(token)
}
```

The regexp is compiled once at package initialization. The builder allocates
one buffer instead of one string per token.

### Re-run the experiment

```bash
make demo
```

Watch the load generator's progress line. On this branch the store and cache
should plateau, and `evictions` should become non-zero once the bounds are
reached. You can also poll stats while the demo runs:

```bash
curl -s http://127.0.0.1:9090/stats
```

Expected shape:

```json
{"Documents":10000,"TotalBytes":...,"Analyses":...,"CacheHits":...,"CacheEntries":4096,"HeapAllocBytes":...,"HeapInuseBytes":...,"Goroutines":...,"Evictions":...}
```

The exact numbers depend on duration and concurrency. The important part is
that `Documents` stops at `MAX_DOCUMENTS`, `CacheEntries` stops at
`CACHE_ENTRIES`, and `Evictions` increases.

The comparison below uses `profiles/main-heap-*.pprof` from the matched
baseline in Lab 4. If those files do not exist, run `make demo` on `main`
first.

Compare heap profiles:

```bash
go tool pprof -top -sample_index=inuse_space \
  -diff_base=profiles/main-heap-inuse.pprof \
  profiles/optimized_memory-heap-inuse.pprof

go tool pprof -top -sample_index=alloc_space \
  -diff_base=profiles/main-heap-alloc.pprof \
  profiles/optimized_memory-heap-alloc.pprof
```

What to expect:

- `Documents` and `CacheEntries` plateau instead of growing forever;
- `Evictions` becomes non-zero;
- `HeapAllocBytes` stabilizes rather than climbing linearly;
- `alloc_space` from regexp, fingerprint and slice growth drops sharply;
- `inuse_space` no longer includes an ever-growing protobuf/string population.

### Prove it with a benchmark

```bash
make bench | tee profiles/bench-optimized-memory.txt
```

Expect lower `B/op` and `allocs/op` than `main`, especially for Search. Some
CPU cost may remain in the intentionally unchanged top-terms scan.

### What did not change

Top-term selection still rescans the counts map once per requested term. CPU
profiles on this branch still show `processing.topTerms` for documents with
many distinct terms. That is the CPU branch's lesson.

### Checkpoint 5

1. Why is FIFO eviction enough for the document store, but not for the cache?
2. Why must the cache entry be removed when its document is evicted?
3. What does `strings.Clone` prevent, and why is it cheap here?
4. Why does bounding memory not automatically make CPU profiles flat?

<details>
<summary>Answers</summary>

1. Documents are inserted and aged in order, so FIFO is a simple bound. Cache
   entries are read repeatedly and have different access patterns, so an LRU
   keeps the useful entries. Either way, the important property is the bound.
2. Otherwise the cache could retain a summary, and its strings, for a document
   that no longer exists in the store. The two structures would grow apart.
3. It prevents a cached term from sharing the backing array of the lowercased
   body. Cloning a few bytes is cheap compared with retaining a whole document.
4. The CPU algorithms are unchanged. Bounds and lower allocation reduce GC
   work, but O(n²) counting and O(N × distinct) selection still cost CPU.

</details>

## Lab 6: Compare the branches and write your conclusion

**Question:** which branch is better, and how do I prove it without fooling
myself?

Profiles are not benchmarks. The optimized branches do more work per second
because each operation is cheaper, so a fixed-duration profile can show *more*
total allocations even when allocation per operation went down. Compare:

1. **Per-operation benchmarks** with the same input.
2. **Profiles with identical workload parameters**, using `-diff_base`.
3. **Saturation metrics** such as ops/sec from the load generator.

### Run the benchmark on each branch

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

The memory branch trades some CPU for much lower per-search allocation. The
CPU branch does not touch search at all. The analyze benchmark shows the more
important lesson: the CPU branch wins on time and allocations, while the memory
branch keeps explicit retention controls and still removes most of the
baseline's allocation volume.

### Write your lab report

Fill this in with your own measurements:

| Metric | `main` | `optimized_cpu` | `optimized_memory` |
| --- | ---: | ---: | ---: |
| Analyze ns/op | | | |
| Analyze B/op | | | |
| Analyze allocs/op | | | |
| Search ns/op | | | |
| Search B/op | | | |
| Peak documents | | | |
| Peak cache entries | | | |
| Evictions | | | |

Then answer, in one paragraph each:

1. Which profile made the CPU problem obvious?
2. Which profile made the retention problem obvious?
3. Which fix would you ship first if you could only ship one? Why?
4. Which fix is riskier, and what would you test before deploying it?

### Checkpoint 6

1. Why can per-operation allocations go down while total allocations in a
   fixed-duration profile go up?
2. Why is `-diff_base` useful, and what does a negative value mean?
3. Why can a memory fix improve CPU time without changing any CPU algorithm?

<details>
<summary>Answers</summary>

1. The optimized service completes more operations in the same wall-clock
   window. Per-operation work fell, but the number of operations rose.
2. It compares two profiles from the same kind of workload. Negative values
   mean the second profile uses fewer resources at that call site.
3. Less allocation means less GC work, less memory zeroing and fewer cache
   misses. CPU time includes the cost of managing memory.

</details>

## Reference: Make targets

| Target | What it does |
| --- | --- |
| `make build` | builds the server and load generator into `bin/` |
| `make run` | runs the gRPC server (gRPC `:50051`, admin/pprof `:9090`) |
| `make load` | builds and runs the load generator for 30s |
| `make test` | runs all tests |
| `make race` | runs tests with the race detector |
| `make bench` | runs the processing benchmarks with `-benchmem -count=5` |
| `make demo` | starts the server, drives load, captures profiles for the branch |
| `make profile-cpu` | captures a 30s CPU profile and opens the pprof UI |
| `make profile-heap` | captures heap and opens the `inuse_space` UI |
| `make profile-allocs` | captures allocs and opens the `alloc_space` UI |
| `make stats` | prints the JSON stats snapshot |
| `make docker-build` | builds the Docker image |
| `make docker-up` | starts the service with Docker Compose |
| `make docker-load` | starts the service and load generator in Docker |
| `make docker-down` | stops Docker services, including the load profile |
| `make clean` | removes local binaries and profile artifacts |

## Reference: pprof recipes

Replace the file name with the profile you captured. The full command
reference is in [`docs/PPROF_CHEATSHEET.md`](docs/PPROF_CHEATSHEET.md).

| Goal | Command |
| --- | --- |
| CPU top by flat time | `go tool pprof -top profiles/cpu.pprof` |
| CPU top by cumulative time | `go tool pprof -top -cum profiles/cpu.pprof` |
| Open the UI | `go tool pprof -http=:8081 profiles/cpu.pprof` |
| Focus on application code | `go tool pprof -top -focus='pprof-lab' profiles/cpu.pprof` |
| Annotated source | `go tool pprof -list='processing\.tokenize' profiles/cpu.pprof` |
| Callers and callees | `go tool pprof -peek='processing\.fingerprint' profiles/cpu.pprof` |
| RPC labels | `go tool pprof -tags profiles/cpu.pprof` |
| Filter by RPC | `go tool pprof -tagfocus='grpc_method=/docproc.v1.DocumentService/Analyze' profiles/cpu.pprof` |
| Heap, live space | `go tool pprof -top -sample_index=inuse_space profiles/heap.pprof` |
| Heap, allocated space | `go tool pprof -top -sample_index=alloc_space profiles/allocs.pprof` |
| Heap, live objects | `go tool pprof -top -sample_index=inuse_objects profiles/heap.pprof` |
| Heap, allocated objects | `go tool pprof -top -sample_index=alloc_objects profiles/allocs.pprof` |
| Compare profiles | `go tool pprof -top -diff_base=before.pprof after.pprof` |
| Capture CPU from URL | `go tool pprof -http=:8081 'http://127.0.0.1:9090/debug/pprof/profile?seconds=20'` |

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

Docker gives you a Linux CPU profile without changing your host setup.

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

For a short load without the five-minute foreground profile:

```bash
docker compose --profile load run --rm loadgen \
  -addr docproc:50051 -mode mixed -duration 15s -concurrency 2 -doc-bytes 2048
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
| pprof UI shows no source | Binary was built with `-trimpath` or source moved | Use `list` from the repo root; source paths are relative |

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
