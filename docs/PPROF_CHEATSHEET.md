# pprof cheat sheet

All commands below assume the admin server is reachable at
`http://127.0.0.1:9090` and profiles are stored in `./profiles`.

## Endpoints exposed by this project

| Endpoint | Profile | Default sample type |
| --- | --- | --- |
| `/debug/pprof/` | Index of all profiles | - |
| `/debug/pprof/profile?seconds=30` | CPU | `cpu` (nanoseconds) |
| `/debug/pprof/heap` | Heap | `inuse_space` |
| `/debug/pprof/allocs` | Allocations | `alloc_space` |
| `/debug/pprof/goroutine` | Goroutine stacks | `goroutine` |
| `/debug/pprof/block` | Blocking events | `contentions` / `delay` |
| `/debug/pprof/mutex` | Mutex contention | `contentions` / `delay` |
| `/debug/pprof/threadcreate` | Thread creation stacks | `threadcreate` |
| `/debug/pprof/trace?seconds=5` | Execution trace | - |
| `/debug/pprof/cmdline` | Process command line | - |
| `/debug/pprof/symbol` | Symbol lookup | - |

Block and mutex profiles are disabled by default. Start the server with
`BLOCK_PROFILE_RATE=1` and/or `MUTEX_PROFILE_FRACTION=1` to record them.

## Capture profiles

```bash
# While the server is running and load is being generated
curl -fsS "http://127.0.0.1:9090/debug/pprof/profile?seconds=30" -o profiles/cpu.pprof
curl -fsS "http://127.0.0.1:9090/debug/pprof/heap"                 -o profiles/heap.pprof
curl -fsS "http://127.0.0.1:9090/debug/pprof/allocs"               -o profiles/allocs.pprof
curl -fsS "http://127.0.0.1:9090/debug/pprof/goroutine"            -o profiles/goroutine.pprof
curl -fsS "http://127.0.0.1:9090/debug/pprof/trace?seconds=5"      -o profiles/trace.out
```

Or let `go tool pprof` fetch and open a profile in one step:

```bash
go tool pprof -http=:8081 "http://127.0.0.1:9090/debug/pprof/profile?seconds=30"
go tool pprof -http=:8082 "http://127.0.0.1:9090/debug/pprof/heap"
```

## Open the UI

```bash
go tool pprof -http=:8081 profiles/cpu.pprof
go tool pprof -http=:8082 -sample_index=inuse_space profiles/heap.pprof
go tool pprof -http=:8083 -sample_index=alloc_space profiles/allocs.pprof
```

The UI has a graph, flame graph, top table, source view and call tree. The
graph view needs Graphviz (`dot`); the flame graph and top table do not.

## Terminal commands

```bash
# Flat time: where samples were actually spending CPU
go tool pprof -top -nodecount=25 profiles/cpu.pprof

# Cumulative time: which call paths include the hot functions
go tool pprof -top -cum -nodecount=25 profiles/cpu.pprof

# Source lines for a function or package (regexp)
go tool pprof -list='processing\.fingerprint' profiles/cpu.pprof
go tool pprof -list='processing\.' profiles/cpu.pprof

# Who calls / is called by a function
go tool pprof -peek='processing\.topTerms' profiles/cpu.pprof

# Focus on one call subtree or exclude noise
go tool pprof -top -focus='processing\.Analyze' profiles/cpu.pprof
go tool pprof -top -ignore='runtime\.' profiles/cpu.pprof

# Different heap views
go tool pprof -top -sample_index=inuse_space profiles/heap.pprof
go tool pprof -top -sample_index=inuse_objects profiles/heap.pprof
go tool pprof -top -sample_index=alloc_space profiles/allocs.pprof
go tool pprof -top -sample_index=alloc_objects profiles/allocs.pprof

# Readable units
go tool pprof -top -unit=MB -sample_index=inuse_space profiles/heap.pprof
```

## Heap profiles: which sample index?

| Sample index | Answers |
| --- | --- |
| `inuse_space` | What is alive right now, in bytes? Start here for leaks and retained memory. |
| `inuse_objects` | What is alive right now, as object count? Useful for many small objects. |
| `alloc_space` | What has been allocated in total, in bytes? Start here for GC pressure and allocation hot spots. |
| `alloc_objects` | What has been allocated in total, as object count? |

A function can dominate `alloc_space` without dominating `inuse_space`: it
allocates a lot of short-lived garbage. Conversely, a leak can be small in
`alloc_space` but large in `inuse_space`.

Heap profiles are sampled. The default sampling rate is one sample per 512 KiB
allocated, so small differences are noise. Compare profiles taken under the
same load, and use larger sample windows for stable numbers.

## Compare two profiles

```bash
# main vs optimized branch, same workload and duration
go tool pprof -top -diff_base=profiles/main-cpu.pprof profiles/optimized_cpu-cpu.pprof

go tool pprof -http=:8084 \
  -diff_base=profiles/main-heap-inuse.pprof \
  profiles/optimized_memory-heap-inuse.pprof
```

`-diff_base` shows what changed relative to the baseline: negative values are
improvements. `-base` subtracts raw samples and is useful when the two profiles
have the same sample configuration.

## Labels

The server wraps every unary RPC in `pprof.Do` with a `grpc_method` label:

```bash
go tool pprof -tags profiles/cpu.pprof
go tool pprof -tagfocus='grpc_method=/docproc.v1.DocumentService/Analyze' profiles/cpu.pprof
go tool pprof -tagshow='grpc_method' -top profiles/cpu.pprof
```

Only work executed inside the labeled handler is attributed. Profiles are
sampled per goroutine, so labels are a low-overhead way to separate endpoints.

## Goroutines, blocking and mutexes

```bash
# Human-readable goroutine dump (also useful through the pprof UI)
curl -s "http://127.0.0.1:9090/debug/pprof/goroutine?debug=1"

# Block/mutex profiles must be enabled at process start
BLOCK_PROFILE_RATE=1 MUTEX_PROFILE_FRACTION=1 make run
curl -fsS "http://127.0.0.1:9090/debug/pprof/block" -o profiles/block.pprof
curl -fsS "http://127.0.0.1:9090/debug/pprof/mutex" -o profiles/mutex.pprof
go tool pprof -http=:8085 profiles/block.pprof
```

Block profile delay is cumulative across blocked goroutines. Mutex profile
contention is sampled; a fraction of 1 means "record every contention event"
and adds overhead.

## Execution trace

```bash
curl -fsS "http://127.0.0.1:9090/debug/pprof/trace?seconds=5" -o profiles/trace.out
go tool trace profiles/trace.out
```

Use traces when pprof CPU is not enough: scheduler latency, GC pauses,
goroutine blocking and syscall behavior are easier to see in a trace.

## Common mistakes

- Profiling an idle service: CPU samples are only taken while code is running.
  Generate load for the whole capture window.
- Forgetting `?seconds=30`: the CPU endpoint defaults to 30 seconds, which can
  look like a hang if you expected an instant snapshot.
- Reading `alloc_space` as "memory in use": use `inuse_space` for retention.
- Chasing runtime/GC frames: they are usually symptoms. Use `-cum` and `-peek`
  to find the application code that allocates or blocks.
- Comparing profiles from different workloads. Keep concurrency, document size
  and duration fixed.
- Exposing `/debug/pprof` on a public interface. It reveals internals and can
  be used for CPU exhaustion. Keep it on a private admin port.
