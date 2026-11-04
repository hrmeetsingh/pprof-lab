// Package processing implements the document analysis pipeline used by the
// demo service.
//
// This branch contains the memory optimizations. The changes were chosen from
// heap and allocation profiles captured on the main branch: the store and cache
// grew without bound, cached terms retained entire lowercased documents, and
// several hot paths either recompiled regexps or grew slices repeatedly.
//
// The CPU-heavy O(n * distinct) top-terms scan is intentionally left in place;
// see optimized_cpu for that work.
package processing

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
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

	// Compiling the pattern once removes a large per-document allocation that
	// dominated the baseline allocation profile.
	tokenPattern = regexp.MustCompile(`[A-Za-z0-9]+`)
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

// Config bounds the two data structures that grew without limit in main.
type Config struct {
	// MaxDocuments is the number of documents kept in memory. The oldest
	// document is evicted first (FIFO).
	MaxDocuments int
	// CacheEntries is the number of analysis results kept in the LRU cache.
	CacheEntries int
}

// DefaultConfig returns safe defaults for the demo service.
func DefaultConfig() Config {
	return Config{
		MaxDocuments: 10_000,
		CacheEntries: 4_096,
	}
}

// Processor stores a bounded number of documents and analysis results.
type Processor struct {
	docsMu     sync.RWMutex
	docs       map[string]Document
	order      *list.List // document IDs in insertion order
	totalBytes int64
	maxDocs    int

	cache *summaryCache

	analyses  atomic.Int64
	cacheHits atomic.Int64
	evictions atomic.Int64
}

// NewProcessor returns a processor with DefaultConfig.
func NewProcessor() *Processor {
	return NewProcessorWithConfig(DefaultConfig())
}

// NewProcessorWithConfig returns a processor with explicit bounds. Zero or
// negative values fall back to DefaultConfig.
func NewProcessorWithConfig(cfg Config) *Processor {
	defaults := DefaultConfig()
	if cfg.MaxDocuments <= 0 {
		cfg.MaxDocuments = defaults.MaxDocuments
	}
	if cfg.CacheEntries <= 0 {
		cfg.CacheEntries = defaults.CacheEntries
	}

	return &Processor{
		docs:    make(map[string]Document, cfg.MaxDocuments),
		order:   list.New(),
		maxDocs: cfg.MaxDocuments,
		cache:   newSummaryCache(cfg.CacheEntries),
	}
}

// Ingest validates and stores a batch of documents. The operation is
// all-or-nothing. When the store is full, the oldest documents are evicted,
// together with their cached analyses.
func (p *Processor) Ingest(docs []Document) ([]string, error) {
	if len(docs) == 0 {
		return nil, fmt.Errorf("%w: at least one document is required", ErrInvalidDocument)
	}

	p.docsMu.Lock()
	defer p.docsMu.Unlock()

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

	ids := make([]string, 0, len(docs))
	for _, doc := range docs {
		p.docs[doc.ID] = doc
		p.order.PushBack(doc.ID)
		p.totalBytes += int64(len(doc.Title) + len(doc.Body))
		ids = append(ids, doc.ID)
	}
	p.evictLocked()
	return ids, nil
}

// Analyze returns a summary for every requested document. An empty ids slice
// means "all documents". Results are served from the bounded LRU cache.
func (p *Processor) Analyze(ids []string, topN int) ([]Summary, error) {
	docs, err := p.snapshot(ids)
	if err != nil {
		return nil, err
	}

	summaries := make([]Summary, 0, len(docs))
	for _, doc := range docs {
		if cached, ok := p.cache.get(doc.ID); ok {
			p.cacheHits.Add(1)
			summaries = append(summaries, *cached)
			continue
		}

		summary := analyzeDocument(doc, topN)
		p.cache.put(doc.ID, summary)
		p.analyses.Add(1)
		summaries = append(summaries, *summary)
	}
	return summaries, nil
}

// Search finds documents containing query. Matching is case-insensitive and
// non-overlapping, like strings.Count, but does not allocate a lowercased copy
// of every document body.
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
	hits := make([]Hit, 0, min(len(docs), limit))
	for _, doc := range docs {
		score, first := countFoldASCII(doc.Body, needle)
		if score == 0 {
			continue
		}

		start := first - 60
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
	p.docsMu.RLock()
	documents := len(p.docs)
	totalBytes := p.totalBytes
	p.docsMu.RUnlock()

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	return Stats{
		Documents:      documents,
		TotalBytes:     totalBytes,
		Analyses:       p.analyses.Load(),
		CacheHits:      p.cacheHits.Load(),
		CacheEntries:   p.cache.len(),
		HeapAllocBytes: mem.HeapAlloc,
		HeapInuseBytes: mem.HeapInuse,
		Goroutines:     runtime.NumGoroutine(),
		Evictions:      p.evictions.Load(),
	}
}

func (p *Processor) snapshot(ids []string) ([]Document, error) {
	p.docsMu.RLock()
	defer p.docsMu.RUnlock()

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

// evictLocked removes oldest documents until the store is within its bound.
// p.docsMu must be held for writing. Cache eviction also happens here so a
// document that is gone from the store cannot keep an analysis alive.
func (p *Processor) evictLocked() {
	for len(p.docs) > p.maxDocs {
		oldest := p.order.Front()
		if oldest == nil {
			return
		}
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

// summaryCache is a bounded LRU cache. Get mutates recency, so a plain Mutex
// is used rather than an RWMutex.
type summaryCache struct {
	mu       sync.Mutex
	capacity int
	order    *list.List
	entries  map[string]*list.Element
}

type cacheEntry struct {
	key     string
	summary *Summary
}

func newSummaryCache(capacity int) *summaryCache {
	return &summaryCache{
		capacity: capacity,
		order:    list.New(),
		entries:  make(map[string]*list.Element, capacity),
	}
}

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

func (c *summaryCache) put(key string, summary *Summary) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if element, ok := c.entries[key]; ok {
		element.Value.(*cacheEntry).summary = summary
		c.order.MoveToFront(element)
		return
	}

	element := c.order.PushFront(&cacheEntry{key: key, summary: summary})
	c.entries[key] = element
	for c.order.Len() > c.capacity {
		oldest := c.order.Back()
		if oldest == nil {
			return
		}
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*cacheEntry).key)
	}
}

func (c *summaryCache) delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	element, ok := c.entries[key]
	if !ok {
		return
	}
	c.order.Remove(element)
	delete(c.entries, key)
}

func (c *summaryCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
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

// tokenize lowercases the body and extracts ASCII words. The regexp is
// package-level, so it is compiled once per process instead of once per
// document.
func tokenize(text string) []string {
	lower := strings.ToLower(text)
	return tokenPattern.FindAllString(lower, -1)
}

// countTerms counts tokens in one pass. The baseline kept a separate `unique`
// slice containing every token, which was a large allocation and an O(n^2)
// scan. The counts map already has exactly one key per unique term.
func countTerms(tokens []string) (map[string]int, int) {
	counts := make(map[string]int, len(tokens))
	for _, token := range tokens {
		counts[token]++
	}
	return counts, len(counts)
}

// topTerms selects the n most frequent terms. The selection algorithm is
// deliberately unchanged from main (the CPU branch fixes it); only the
// allocations are reduced and the cached term strings are detached from the
// lowercased body.
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

	used := make(map[string]struct{}, n)
	result := make([]TermCount, 0, n)
	for len(result) < n {
		bestTerm := ""
		bestCount := -1
		for term, count := range counts {
			if _, taken := used[term]; taken {
				continue
			}
			if count > bestCount || (count == bestCount && term < bestTerm) {
				bestTerm = term
				bestCount = count
			}
		}
		if bestCount < 0 {
			break
		}
		used[bestTerm] = struct{}{}
		// bestTerm points into the lowercased document body. Cloning the few
		// bytes we return prevents the cache from retaining the whole body.
		result = append(result, TermCount{Term: strings.Clone(bestTerm), Count: int64(bestCount)})
	}
	return result
}

// fingerprint hashes the joined token stream using a pre-sized Builder. The
// baseline built the string with +=, allocating a new string on every
// iteration.
func fingerprint(tokens []string) string {
	size := 0
	for _, token := range tokens {
		size += len(token) + 1
	}

	var joined strings.Builder
	joined.Grow(size)
	for i, token := range tokens {
		if i > 0 {
			joined.WriteByte(' ')
		}
		joined.WriteString(token)
	}

	sum := sha256.Sum256([]byte(joined.String()))
	return hex.EncodeToString(sum[:])
}

// countFoldASCII counts non-overlapping occurrences of lowerNeedle in haystack
// without allocating a lowercased copy of haystack. Documents and queries are
// ASCII (the tokenizer is ASCII as well); this helper is not a substitute for
// full Unicode case folding.
func countFoldASCII(haystack, lowerNeedle string) (score int, first int) {
	first = -1
	if lowerNeedle == "" {
		return 0, first
	}

	for i := 0; i+len(lowerNeedle) <= len(haystack); {
		if equalFoldASCII(haystack[i:i+len(lowerNeedle)], lowerNeedle) {
			if first < 0 {
				first = i
			}
			score++
			i += len(lowerNeedle)
			continue
		}
		i++
	}
	return score, first
}

func equalFoldASCII(candidate, lowerNeedle string) bool {
	for i := 0; i < len(candidate); i++ {
		c := candidate[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lowerNeedle[i] {
			return false
		}
	}
	return true
}
