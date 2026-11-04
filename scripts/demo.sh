#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

GRPC_LISTEN_ADDR="${GRPC_LISTEN_ADDR:-:50051}"
ADMIN_LISTEN_ADDR="${ADMIN_LISTEN_ADDR:-:9090}"
LOAD_ADDR="${LOAD_ADDR:-127.0.0.1:50051}"
ADMIN_URL="${ADMIN_URL:-http://127.0.0.1:9090}"
LOAD_DURATION="${LOAD_DURATION:-30s}"
PROFILE_SECONDS="${PROFILE_SECONDS:-8}"
OUT_DIR="${OUT_DIR:-profiles}"
CONCURRENCY="${CONCURRENCY:-4}"
DOC_BYTES="${DOC_BYTES:-2048}"

BRANCH="$(git branch --show-current 2>/dev/null || true)"
BRANCH="${BRANCH:-local}"
BRANCH="${BRANCH//\//-}"
PREFIX="$OUT_DIR/$BRANCH"

mkdir -p "$OUT_DIR/bin"
SERVER_BIN="$OUT_DIR/bin/docproc"
LOADGEN_BIN="$OUT_DIR/bin/loadgen"

go build -trimpath -o "$SERVER_BIN" ./cmd/server
go build -trimpath -o "$LOADGEN_BIN" ./cmd/loadgen

if curl -fsS --max-time 1 "$ADMIN_URL/healthz" >/dev/null 2>&1; then
  echo "error: $ADMIN_URL is already serving /healthz." >&2
  echo "Stop the running server or override ADMIN_URL, ADMIN_LISTEN_ADDR and LOAD_ADDR." >&2
  exit 1
fi

SERVER_PID=""
LOAD_PID=""
cleanup() {
  set +e
  if [[ -n "$LOAD_PID" ]] && kill -0 "$LOAD_PID" 2>/dev/null; then
    kill "$LOAD_PID" 2>/dev/null
    wait "$LOAD_PID" 2>/dev/null
  fi
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill -TERM "$SERVER_PID" 2>/dev/null
    wait "$SERVER_PID" 2>/dev/null
  fi
}
trap cleanup EXIT

echo "Starting server (gRPC $GRPC_LISTEN_ADDR, admin $ADMIN_LISTEN_ADDR, branch $BRANCH)..."
GRPC_ADDR="$GRPC_LISTEN_ADDR" ADMIN_ADDR="$ADMIN_LISTEN_ADDR" \
  "$SERVER_BIN" >"$PREFIX-server.log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 100); do
  if curl -fsS --max-time 1 "$ADMIN_URL/healthz" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "error: server exited during startup" >&2
    tail -n 50 "$PREFIX-server.log" >&2
    exit 1
  fi
  sleep 0.2
done
if ! curl -fsS --max-time 1 "$ADMIN_URL/healthz" >/dev/null 2>&1; then
  echo "error: server did not become healthy at $ADMIN_URL" >&2
  tail -n 50 "$PREFIX-server.log" >&2
  exit 1
fi

echo "Starting mixed load for $LOAD_DURATION with $CONCURRENCY workers and ${DOC_BYTES}B documents..."
"$LOADGEN_BIN" \
  -addr "$LOAD_ADDR" \
  -mode mixed \
  -duration "$LOAD_DURATION" \
  -concurrency "$CONCURRENCY" \
  -doc-bytes "$DOC_BYTES" \
  -seed-docs 100 \
  >"$PREFIX-loadgen.log" 2>&1 &
LOAD_PID=$!

sleep 5
if ! kill -0 "$LOAD_PID" 2>/dev/null; then
  echo "error: load generator exited during warm-up" >&2
  tail -n 50 "$PREFIX-loadgen.log" >&2
  exit 1
fi

echo "Capturing a ${PROFILE_SECONDS}s CPU profile (the service is under load)..."
curl -fsS "$ADMIN_URL/debug/pprof/profile?seconds=$PROFILE_SECONDS" -o "$PREFIX-cpu.pprof"
curl -fsS "$ADMIN_URL/debug/pprof/heap" -o "$PREFIX-heap-inuse.pprof"
curl -fsS "$ADMIN_URL/debug/pprof/allocs" -o "$PREFIX-heap-alloc.pprof"
curl -fsS "$ADMIN_URL/debug/pprof/goroutine" -o "$PREFIX-goroutine.pprof"
curl -fsS "$ADMIN_URL/stats" -o "$PREFIX-stats.json"

if ! wait "$LOAD_PID"; then
  echo "error: load generator failed; last log lines:" >&2
  tail -n 50 "$PREFIX-loadgen.log" >&2
  LOAD_PID=""
  exit 1
fi
LOAD_PID=""
curl -fsS "$ADMIN_URL/stats" -o "$PREFIX-stats.json"

echo
echo "=== CPU profile: top functions by flat time ==="
go tool pprof -top -nodecount=15 "$PREFIX-cpu.pprof" | sed -n '1,24p'

echo
echo "=== Heap profile: top functions by in-use space ==="
go tool pprof -top -nodecount=10 -sample_index=inuse_space "$PREFIX-heap-inuse.pprof" | sed -n '1,19p'

echo
echo "=== Alloc profile: top functions by allocated space ==="
go tool pprof -top -nodecount=10 -sample_index=alloc_space "$PREFIX-heap-alloc.pprof" | sed -n '1,19p'

echo
echo "Artifacts for branch '$BRANCH':"
echo "  $PREFIX-cpu.pprof"
echo "  $PREFIX-heap-inuse.pprof"
echo "  $PREFIX-heap-alloc.pprof"
echo "  $PREFIX-goroutine.pprof"
echo "  $PREFIX-stats.json"
echo "  $PREFIX-server.log"
echo "  $PREFIX-loadgen.log"
echo
echo "Explore the profiles offline:"
echo "  go tool pprof -http=:8081 $PREFIX-cpu.pprof"
echo "  go tool pprof -http=:8082 -sample_index=inuse_space $PREFIX-heap-inuse.pprof"
echo "  go tool pprof -http=:8083 -sample_index=alloc_space $PREFIX-heap-alloc.pprof"
