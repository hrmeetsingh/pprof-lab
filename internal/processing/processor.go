// Package processing implements the document analysis pipeline used by the
// demo service.
//
// This branch contains the CPU optimizations. The changes were chosen from CPU
// profiles captured on the main branch: the tokenizer recompiled a regexp for
// every document, term counting and top-N selection were quadratic, and the
// digest builder used quadratic string concatenation. Memory retention is
// intentionally left untouched on this branch; see optimized_memory for that.
package processing

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	ErrInvalidDocument   = errors.New("invalid document")
	ErrDuplicateDocument = errors.New("document already exists")
	ErrDocumentNotFound  = errors.New("document not found")
	ErrEmptyQuery        = errors.New("query must not be empty")
)

// Document is the domain representation of a stored document.
type Document struct {
	ID    string
	Title string
	Body  string
}

// TermCount is one row in a document's top-terms list.
type TermCount struct {
	Term  string
	Count int64
}

// Summary is the result of analyzing one document.
type Summary struct {
	DocumentID  string
	WordCount   int
	UniqueTerms int
	Digest      string
	TopTerms    []TermCount
}

// Hit is one search result.
type Hit struct {
	DocumentID string
	Score      int
	Snippet    string
}

// Stats is a snapshot of processor and runtime state.
type Stats struct {
	Documents      int
	TotalBytes     int64
	Analyses       int64
	CacheHits      int64
	CacheEntries   int
	HeapAllocBytes uint64
	HeapInuseBytes uint64
	Goroutines     int
	Evictions      int64
}

// Processor stores documents and caches their analysis results.
//
// The baseline implementation keeps every document and every analysis result
// in memory for the lifetime of the process.
type Processor struct {
	mu         sync.RWMutex
	docs       map[string]Document
	cache      map[string]*Summary
	totalBytes int64

	analyses  atomic.Int64
	cacheHits atomic.Int64
}

// NewProcessor returns an empty processor.
func NewProcessor() *Processor {
	return &Processor{
		docs:  make(map[string]Document),
		cache: make(map[string]*Summary),
	}
}

// Ingest validates and stores a batch of documents. The operation is
// all-or-nothing: if any document is invalid or already exists, nothing is
// stored.
func (p *Processor) Ingest(docs []Document) ([]string, error) {
	if len(docs) == 0 {
		return nil, fmt.Errorf("%w: at least one document is required", ErrInvalidDocument)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	seen := make(map[string]struct{}, len(docs))
	for _, doc := range docs {
		if doc.ID == "" || doc.Body == "" {
			return nil, fmt.Errorf("%w: id and body are required", ErrInvalidDocument)
		}
		if _, exists := p.docs[doc.ID]; exists {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateDocument, doc.ID)
		}
		if _, exists := seen[doc.ID]; exists {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateDocument, doc.ID)
		}
		seen[doc.ID] = struct{}{}
	}

	var ids []string
	for _, doc := range docs {
		p.docs[doc.ID] = doc
		p.totalBytes += int64(len(doc.Title) + len(doc.Body))
		ids = append(ids, doc.ID)
	}
	return ids, nil
}

// Analyze returns a summary for every requested document. An empty ids slice
// means "all documents". Results are cached by document ID.
func (p *Processor) Analyze(ids []string, topN int) ([]Summary, error) {
	docs, err := p.snapshot(ids)
	if err != nil {
		return nil, err
	}

	var summaries []Summary
	for _, doc := range docs {
		if cached, ok := p.getCached(doc.ID); ok {
			p.cacheHits.Add(1)
			summaries = append(summaries, *cached)
			continue
		}

		summary := analyzeDocument(doc, topN)
		p.putCached(doc.ID, summary)
		p.analyses.Add(1)
		summaries = append(summaries, *summary)
	}
	return summaries, nil
}

// Search finds documents containing query. Matching is case-insensitive and
// non-overlapping, like strings.Count.
func (p *Processor) Search(query string, ids []string, limit int) ([]Hit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, ErrEmptyQuery
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	docs, err := p.snapshot(ids)
	if err != nil {
		return nil, err
	}

	needle := strings.ToLower(query)
	var hits []Hit
	for _, doc := range docs {
		haystack := strings.ToLower(doc.Body)
		score := strings.Count(haystack, needle)
		if score == 0 {
			continue
		}

		idx := strings.Index(haystack, needle)
		start := idx - 60
		if start < 0 {
			start = 0
		}
		end := start + 160
		if end > len(doc.Body) {
			end = len(doc.Body)
		}

		hits = append(hits, Hit{
			DocumentID: doc.ID,
			Score:      score,
			Snippet:    doc.Body[start:end],
		})
	}

	sort.SliceStable(hits, func(i, j int) bool {
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// Stats returns a snapshot of processor and Go runtime statistics.
func (p *Processor) Stats() Stats {
	p.mu.RLock()
	documents := len(p.docs)
	cacheEntries := len(p.cache)
	totalBytes := p.totalBytes
	p.mu.RUnlock()

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	return Stats{
		Documents:      documents,
		TotalBytes:     totalBytes,
		Analyses:       p.analyses.Load(),
		CacheHits:      p.cacheHits.Load(),
		CacheEntries:   cacheEntries,
		HeapAllocBytes: mem.HeapAlloc,
		HeapInuseBytes: mem.HeapInuse,
		Goroutines:     runtime.NumGoroutine(),
	}
}

func (p *Processor) snapshot(ids []string) ([]Document, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(ids) == 0 {
		ids = make([]string, 0, len(p.docs))
		for id := range p.docs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}

	docs := make([]Document, 0, len(ids))
	for _, id := range ids {
		doc, ok := p.docs[id]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrDocumentNotFound, id)
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

func (p *Processor) getCached(id string) (*Summary, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	summary, ok := p.cache[id]
	return summary, ok
}

func (p *Processor) putCached(id string, summary *Summary) {
	p.mu.Lock()
	p.cache[id] = summary
	p.mu.Unlock()
}

// analyzeDocument is the core CPU path. It is intentionally not tied to
// Processor so benchmarks can call it without exercising the cache.
func analyzeDocument(doc Document, topN int) *Summary {
	tokens := tokenize(doc.Body)
	counts, unique := countTerms(tokens)
	top := topTerms(counts, topN)

	return &Summary{
		DocumentID:  doc.ID,
		WordCount:   len(tokens),
		UniqueTerms: unique,
		Digest:      fingerprint(tokens),
		TopTerms:    top,
	}
}

// tokenize lowercases the body and extracts ASCII words.
//
// CPU profile evidence: the baseline compiled `[A-Za-z0-9]+` on every call, so
// regexp compilation and backtracking dominated the Analyze profile. This
// scanner performs the same ASCII tokenization without a regexp.
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

// countTerms counts tokens in one pass.
//
// CPU profile evidence: the baseline maintained a separate unique slice and
// scanned it for every token, which is O(n^2) for documents with many distinct
// words. The counts map already knows every distinct key, so its length is the
// unique-term count.
func countTerms(tokens []string) (map[string]int, int) {
	counts := make(map[string]int, len(tokens))
	for _, token := range tokens {
		counts[token]++
	}
	return counts, len(counts)
}

// topTerms selects the n most frequent terms with a single sort.
//
// CPU profile evidence: the baseline scanned the entire counts map once per
// requested term (O(n * distinct)). Building a slice and sorting it once is
// O(distinct log distinct) and was one of the clearest flat CPU reductions.
func topTerms(counts map[string]int, n int) []TermCount {
	if n <= 0 {
		n = 10
	}
	if n > len(counts) {
		n = len(counts)
	}
	if n == 0 {
		return nil
	}

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

	result := make([]TermCount, n)
	for i := 0; i < n; i++ {
		result[i] = TermCount{Term: terms[i].term, Count: int64(terms[i].count)}
	}
	return result
}

type termFrequency struct {
	term  string
	count int
}

// fingerprint hashes the token stream incrementally.
//
// CPU and allocation profile evidence: the baseline built the joined token
// string with +=, allocating a new, larger string on every iteration. Streaming
// the tokens into the hasher produces the identical digest without the
// quadratic intermediate string.
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
	if len(buf) > 0 {
		_, _ = h.Write(buf)
	}
	return hex.EncodeToString(h.Sum(nil))
}
