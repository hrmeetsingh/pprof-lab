// Command loadgen generates gRPC traffic for the pprof-lab service.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	docprocv1 "github.com/hrmeetsingh/pprof-lab/gen/docproc/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

const (
	modeMixed   = "mixed"
	modeIngest  = "ingest"
	modeAnalyze = "analyze"
	modeSearch  = "search"
)

func main() {
	var (
		addr           = flag.String("addr", envString("LOADGEN_ADDR", "127.0.0.1:50051"), "gRPC server address")
		mode           = flag.String("mode", modeMixed, "workload: mixed, ingest, analyze or search")
		duration       = flag.Duration("duration", 2*time.Minute, "how long to generate load (0 means until interrupted)")
		concurrency    = flag.Int("concurrency", 8, "number of concurrent gRPC workers")
		docBytes       = flag.Int("doc-bytes", 4096, "approximate document body size in bytes")
		seedDocs       = flag.Int("seed-docs", 100, "documents ingested before the workload starts")
		seed           = flag.Int64("seed", 42, "random seed")
		rpcTimeout     = flag.Duration("rpc-timeout", 30*time.Second, "per-RPC timeout")
		topTerms       = flag.Int("top-terms", 10, "top_terms value for Analyze")
		searchLimit    = flag.Int("search-limit", 10, "limit value for Search")
		progressPeriod = flag.Duration("progress", 5*time.Second, "progress report interval")
	)
	flag.Parse()

	switch *mode {
	case modeMixed, modeIngest, modeAnalyze, modeSearch:
	default:
		log.Fatalf("invalid -mode %q, want one of: mixed, ingest, analyze, search", *mode)
	}
	if *concurrency < 1 {
		log.Fatal("-concurrency must be at least 1")
	}
	if *docBytes < 128 {
		log.Fatal("-doc-bytes must be at least 128")
	}
	if *duration < 0 {
		log.Fatal("-duration must not be negative")
	}
	if *seedDocs == 0 && *mode != modeIngest {
		*seedDocs = 50
	}

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("creating gRPC client: %v", err)
	}
	defer conn.Close()

	client := docprocv1.NewDocumentServiceClient(conn)
	baseCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	waitCtx, cancel := context.WithTimeout(baseCtx, 15*time.Second)
	if err := waitForServer(waitCtx, conn); err != nil {
		cancel()
		log.Fatalf("server at %s is not ready: %v", *addr, err)
	}
	cancel()

	runCtx := baseCtx
	if *duration > 0 {
		var cancelRun context.CancelFunc
		runCtx, cancelRun = context.WithTimeout(baseCtx, *duration)
		defer cancelRun()
	}

	runID := time.Now().UnixNano()
	metrics := &metricSet{}
	seedGen := newGenerator(*seed+1, *docBytes, fmt.Sprintf("seed-%d", runID))
	seedIDs, err := ingestSeed(runCtx, client, seedGen, *seedDocs, 20, *rpcTimeout)
	if err != nil {
		log.Fatalf("seeding documents: %v", err)
	}
	log.Printf("mode=%s concurrency=%d doc_bytes=%d seeded=%d target=%s",
		*mode, *concurrency, *docBytes, len(seedIDs), *addr)

	start := time.Now()
	progressDone := make(chan struct{})
	go reportProgress(runCtx, client, metrics, *progressPeriod, progressDone)

	var wg sync.WaitGroup
	for worker := 0; worker < *concurrency; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			gen := newGenerator(*seed+int64(worker+1)*1009, *docBytes, fmt.Sprintf("w%d-%d", worker, runID))
			recent := newRecentIDs(128)
			runWorker(runCtx, client, gen, recent, seedIDs, *mode, *rpcTimeout, *topTerms, *searchLimit, metrics)
		}(worker)
	}
	wg.Wait()
	close(progressDone)

	elapsed := time.Since(start)
	total := metrics.ingest.Load() + metrics.analyze.Load() + metrics.search.Load()
	log.Printf("done in %s: ingest=%d analyze=%d search=%d ops=%d errors=%d (%.1f ops/s)",
		elapsed.Round(time.Millisecond),
		metrics.ingest.Load(), metrics.analyze.Load(), metrics.search.Load(),
		total, metrics.errors.Load(), float64(total)/elapsed.Seconds(),
	)

	printFinalStats(conn, metrics)
}

func runWorker(
	ctx context.Context,
	client docprocv1.DocumentServiceClient,
	gen *generator,
	recent *recentIDs,
	seedIDs []string,
	mode string,
	rpcTimeout time.Duration,
	topTerms int,
	searchLimit int,
	metrics *metricSet,
) {
	var errs errorCounter
	for ctx.Err() == nil {
		switch mode {
		case modeIngest:
			if _, err := ingestOne(ctx, client, gen, recent, rpcTimeout, metrics); err != nil {
				errs.add(err)
				metrics.errors.Add(1)
			}

		case modeAnalyze:
			id, err := ingestOne(ctx, client, gen, recent, rpcTimeout, metrics)
			if err != nil {
				errs.add(err)
				metrics.errors.Add(1)
				continue
			}
			ids := []string{id}
			if recent.len() > 4 && gen.rng.Intn(100) < 25 {
				ids = append(ids, sampleIDs(gen.rng, seedIDs, recent, 2)...)
			}
			if err := analyzeIDs(ctx, client, ids, topTerms, rpcTimeout, metrics); err != nil {
				errs.add(err)
				metrics.errors.Add(1)
			}

		case modeSearch:
			ids := sampleIDs(gen.rng, seedIDs, recent, 50)
			if len(ids) == 0 {
				continue
			}
			if err := searchIDs(ctx, client, gen.query(), ids, searchLimit, rpcTimeout, metrics); err != nil {
				errs.add(err)
				metrics.errors.Add(1)
			}

		case modeMixed:
			roll := gen.rng.Float64()
			switch {
			case roll < 0.50:
				id, err := ingestOne(ctx, client, gen, recent, rpcTimeout, metrics)
				if err != nil {
					errs.add(err)
					metrics.errors.Add(1)
					continue
				}
				if err := analyzeIDs(ctx, client, []string{id}, topTerms, rpcTimeout, metrics); err != nil {
					errs.add(err)
					metrics.errors.Add(1)
				}
			case roll < 0.55:
				// Re-analyze a recent document so the cache-hit path is
				// exercised alongside fresh cache misses.
				ids := sampleIDs(gen.rng, seedIDs, recent, 1)
				if len(ids) == 0 {
					continue
				}
				if err := analyzeIDs(ctx, client, ids, topTerms, rpcTimeout, metrics); err != nil {
					errs.add(err)
					metrics.errors.Add(1)
				}
			case roll < 0.85:
				if _, err := ingestOne(ctx, client, gen, recent, rpcTimeout, metrics); err != nil {
					errs.add(err)
					metrics.errors.Add(1)
				}
			default:
				ids := sampleIDs(gen.rng, seedIDs, recent, 50)
				if len(ids) == 0 {
					continue
				}
				if err := searchIDs(ctx, client, gen.query(), ids, searchLimit, rpcTimeout, metrics); err != nil {
					errs.add(err)
					metrics.errors.Add(1)
				}
			}
		}
	}
}

func ingestOne(
	ctx context.Context,
	client docprocv1.DocumentServiceClient,
	gen *generator,
	recent *recentIDs,
	rpcTimeout time.Duration,
	metrics *metricSet,
) (string, error) {
	doc := gen.newDocument(gen.nextID())

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := client.Ingest(rpcCtx, &docprocv1.IngestRequest{Documents: []*docprocv1.Document{doc}})
	if err != nil {
		return "", fmt.Errorf("Ingest: %w", err)
	}
	ids := resp.GetIds()
	if len(ids) != 1 {
		return "", fmt.Errorf("Ingest returned %d ids, want 1", len(ids))
	}
	recent.add(ids[0])
	metrics.ingest.Add(1)
	return ids[0], nil
}

func analyzeIDs(
	ctx context.Context,
	client docprocv1.DocumentServiceClient,
	ids []string,
	topTerms int,
	rpcTimeout time.Duration,
	metrics *metricSet,
) error {
	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	_, err := client.Analyze(rpcCtx, &docprocv1.AnalyzeRequest{
		DocumentIds: ids,
		TopTerms:    int32(topTerms),
	})
	if err != nil {
		return fmt.Errorf("Analyze: %w", err)
	}
	metrics.analyze.Add(1)
	return nil
}

func searchIDs(
	ctx context.Context,
	client docprocv1.DocumentServiceClient,
	query string,
	ids []string,
	limit int,
	rpcTimeout time.Duration,
	metrics *metricSet,
) error {
	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	_, err := client.Search(rpcCtx, &docprocv1.SearchRequest{
		Query:       query,
		DocumentIds: ids,
		Limit:       int32(limit),
	})
	if err != nil {
		return fmt.Errorf("Search: %w", err)
	}
	metrics.search.Add(1)
	return nil
}

func ingestSeed(
	ctx context.Context,
	client docprocv1.DocumentServiceClient,
	gen *generator,
	count int,
	batchSize int,
	rpcTimeout time.Duration,
) ([]string, error) {
	ids := make([]string, 0, count)
	for start := 0; start < count; start += batchSize {
		end := min(start+batchSize, count)
		docs := make([]*docprocv1.Document, 0, end-start)
		for i := start; i < end; i++ {
			docs = append(docs, gen.newDocument(gen.nextID()))
		}

		rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
		resp, err := client.Ingest(rpcCtx, &docprocv1.IngestRequest{Documents: docs})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("Ingest seed batch: %w", err)
		}
		ids = append(ids, resp.GetIds()...)
	}
	return ids, nil
}

func waitForServer(ctx context.Context, conn *grpc.ClientConn) error {
	client := healthpb.NewHealthClient(conn)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		resp, err := client.Check(ctx, &healthpb.HealthCheckRequest{})
		if err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("health status %s", resp.GetStatus())
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("last health check error: %w", lastErr)
		case <-ticker.C:
		}
	}
}

func sampleIDs(rng *rand.Rand, seedIDs []string, recent *recentIDs, n int) []string {
	pool := make([]string, 0, len(seedIDs)+recent.len())
	pool = append(pool, seedIDs...)
	pool = append(pool, recent.snapshot()...)
	if len(pool) == 0 {
		return nil
	}
	rng.Shuffle(len(pool), func(i, j int) {
		pool[i], pool[j] = pool[j], pool[i]
	})
	if len(pool) > n {
		pool = pool[:n]
	}
	return pool
}

func reportProgress(ctx context.Context, client docprocv1.DocumentServiceClient, metrics *metricSet, period time.Duration, done <-chan struct{}) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			statsCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			stats, err := client.Stats(statsCtx, &docprocv1.StatsRequest{})
			cancel()
			if err != nil {
				log.Printf("progress: ingest=%d analyze=%d search=%d errors=%d (stats: %v)",
					metrics.ingest.Load(), metrics.analyze.Load(), metrics.search.Load(), metrics.errors.Load(), err)
				continue
			}
			log.Printf("progress: ingest=%d analyze=%d search=%d errors=%d | server docs=%d cache=%d heap=%.1fMB goroutines=%d",
				metrics.ingest.Load(), metrics.analyze.Load(), metrics.search.Load(), metrics.errors.Load(),
				stats.GetDocuments(), stats.GetCacheEntries(),
				float64(stats.GetHeapAllocBytes())/(1024*1024), stats.GetGoroutines(),
			)
		}
	}
}

func printFinalStats(conn *grpc.ClientConn, metrics *metricSet) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stats, err := docprocv1.NewDocumentServiceClient(conn).Stats(ctx, &docprocv1.StatsRequest{})
	if err != nil {
		log.Printf("final stats unavailable: %v", err)
		return
	}
	log.Printf("server stats: docs=%d cache=%d analyses=%d cache_hits=%d heap=%.1fMB goroutines=%d evictions=%d",
		stats.GetDocuments(), stats.GetCacheEntries(), stats.GetAnalyses(), stats.GetCacheHits(),
		float64(stats.GetHeapAllocBytes())/(1024*1024), stats.GetGoroutines(), stats.GetEvictions(),
	)
}

type metricSet struct {
	ingest  atomic.Int64
	analyze atomic.Int64
	search  atomic.Int64
	errors  atomic.Int64
}

type errorCounter struct {
	shown atomic.Int64
}

func (e *errorCounter) add(err error) {
	if e.shown.Add(1) <= 5 {
		log.Printf("rpc error: %v", err)
	}
}

type recentIDs struct {
	mu   sync.Mutex
	ids  []string
	max  int
	next int
}

func newRecentIDs(max int) *recentIDs {
	return &recentIDs{max: max}
}

func (r *recentIDs) add(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ids) < r.max {
		r.ids = append(r.ids, id)
		return
	}
	r.ids[r.next] = id
	r.next = (r.next + 1) % r.max
}

func (r *recentIDs) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ids)
}

func (r *recentIDs) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.ids))
	copy(out, r.ids)
	return out
}

func envString(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
