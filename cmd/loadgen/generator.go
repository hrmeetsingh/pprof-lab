package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync/atomic"

	docprocv1 "github.com/harmeetsingh/pprof-lab/gen/docproc/v1"
)

// The vocabulary is deliberately small and skewed: a handful of common words
// show up far more often than the rest, which gives Analyze a realistic
// term-frequency distribution.
var vocabulary = []string{
	"go", "service", "profile", "memory", "cpu", "request", "document", "cache",
	"server", "client", "heap", "goroutine", "trace", "sample", "buffer", "index",
	"stream", "batch", "token", "digest", "search", "query", "result", "worker",
	"latency", "throughput", "allocation", "garbage", "collector", "runtime",
	"stack", "flame", "graph", "label", "mutex", "block", "channel", "context",
	"deadline", "retry", "payload", "message", "protobuf", "grpc", "transport",
	"connection", "listener", "admin", "metric", "histogram", "counter", "limit",
	"eviction", "summary", "term", "count", "unique", "snippet", "match", "score",
	"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta",
}

type generator struct {
	rng      *rand.Rand
	docBytes int
	prefix   string
	seq      atomic.Int64
}

func newGenerator(seed int64, docBytes int, prefix string) *generator {
	if docBytes < 128 {
		docBytes = 128
	}
	return &generator{
		rng:      rand.New(rand.NewSource(seed)),
		docBytes: docBytes,
		prefix:   prefix,
	}
}

func (g *generator) nextID() string {
	return fmt.Sprintf("%s-%d", g.prefix, g.seq.Add(1))
}

func (g *generator) newDocument(id string) *docprocv1.Document {
	var body strings.Builder
	body.Grow(g.docBytes + 64)

	for body.Len() < g.docBytes {
		words := 8 + g.rng.Intn(10)
		for i := 0; i < words; i++ {
			if i > 0 {
				body.WriteByte(' ')
			}
			body.WriteString(g.word())
		}
		body.WriteString(". ")
	}

	return &docprocv1.Document{
		Id:    id,
		Title: "Synthetic document " + id,
		Body:  body.String(),
	}
}

func (g *generator) word() string {
	// 60% of tokens come from the first 12 words, producing a clear top-terms
	// distribution without making the document unrealistically repetitive.
	var word string
	if g.rng.Intn(100) < 60 {
		word = vocabulary[g.rng.Intn(12)]
	} else {
		word = vocabulary[g.rng.Intn(len(vocabulary))]
	}

	// Mixed case makes case-insensitive tokenization and search realistic.
	// It also ensures strings.ToLower actually has work to do.
	if g.rng.Intn(100) < 20 {
		return strings.ToUpper(word)
	}
	return word
}

func (g *generator) query() string {
	return vocabulary[g.rng.Intn(12)]
}
