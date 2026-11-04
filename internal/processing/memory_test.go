package processing

import (
	"errors"
	"testing"
)

func TestDocumentEviction(t *testing.T) {
	p := NewProcessorWithConfig(Config{MaxDocuments: 2, CacheEntries: 10})
	mustIngest(t, p, Document{ID: "doc-1", Body: "one"})
	mustIngest(t, p, Document{ID: "doc-2", Body: "two"})
	mustIngest(t, p, Document{ID: "doc-3", Body: "three"})

	stats := p.Stats()
	if stats.Documents != 2 {
		t.Errorf("Documents = %d, want 2", stats.Documents)
	}
	if stats.Evictions != 1 {
		t.Errorf("Evictions = %d, want 1", stats.Evictions)
	}

	if _, err := p.Analyze([]string{"doc-1"}, 3); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("Analyze(doc-1) error = %v, want ErrDocumentNotFound", err)
	}
	for _, id := range []string{"doc-2", "doc-3"} {
		if _, err := p.Analyze([]string{id}, 3); err != nil {
			t.Errorf("Analyze(%s) error = %v", id, err)
		}
	}
}

func TestCacheEvictionIsLRU(t *testing.T) {
	p := NewProcessorWithConfig(Config{MaxDocuments: 10, CacheEntries: 2})
	mustIngest(t, p,
		Document{ID: "a", Body: "alpha alpha"},
		Document{ID: "b", Body: "beta beta"},
		Document{ID: "c", Body: "gamma gamma"},
	)

	// Fill the cache: [a], [b a], [c b]. a is now the least recently used.
	for _, id := range []string{"a", "b", "c"} {
		if _, err := p.Analyze([]string{id}, 3); err != nil {
			t.Fatalf("Analyze(%s) error = %v", id, err)
		}
	}
	if got := p.Stats().CacheEntries; got != 2 {
		t.Fatalf("CacheEntries = %d, want 2", got)
	}

	// a was evicted, so this recomputes.
	if _, err := p.Analyze([]string{"a"}, 3); err != nil {
		t.Fatalf("Analyze(a) error = %v", err)
	}
	stats := p.Stats()
	if stats.Analyses != 4 {
		t.Errorf("Analyses = %d, want 4 (a should have been recomputed)", stats.Analyses)
	}
	if stats.CacheHits != 0 {
		t.Errorf("CacheHits = %d, want 0", stats.CacheHits)
	}

	// c is still cached after a's insertion evicted b.
	if _, err := p.Analyze([]string{"c"}, 3); err != nil {
		t.Fatalf("Analyze(c) error = %v", err)
	}
	if got := p.Stats().CacheHits; got != 1 {
		t.Errorf("CacheHits = %d, want 1", got)
	}
}

func TestDocumentEvictionDropsCachedAnalysis(t *testing.T) {
	p := NewProcessorWithConfig(Config{MaxDocuments: 1, CacheEntries: 10})
	mustIngest(t, p, Document{ID: "a", Body: "alpha"})
	if _, err := p.Analyze([]string{"a"}, 3); err != nil {
		t.Fatalf("Analyze(a) error = %v", err)
	}
	if got := p.Stats().CacheEntries; got != 1 {
		t.Fatalf("CacheEntries = %d, want 1", got)
	}

	mustIngest(t, p, Document{ID: "b", Body: "beta"})
	stats := p.Stats()
	if stats.Documents != 1 || stats.Evictions != 1 {
		t.Fatalf("after ingest: documents=%d evictions=%d, want 1/1", stats.Documents, stats.Evictions)
	}
	if stats.CacheEntries != 0 {
		t.Errorf("CacheEntries = %d, want 0 after document eviction", stats.CacheEntries)
	}
}

func TestReingestAfterEviction(t *testing.T) {
	p := NewProcessorWithConfig(Config{MaxDocuments: 1, CacheEntries: 10})
	mustIngest(t, p, Document{ID: "doc", Body: "first version"})
	mustIngest(t, p, Document{ID: "other", Body: "other document"})
	mustIngest(t, p, Document{ID: "doc", Body: "second version"})

	if _, err := p.Analyze([]string{"doc"}, 3); err != nil {
		t.Fatalf("Analyze(doc) after re-ingest error = %v", err)
	}
	if _, err := p.Analyze([]string{"other"}, 3); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("Analyze(other) error = %v, want ErrDocumentNotFound", err)
	}
}

func TestCountFoldASCII(t *testing.T) {
	tests := []struct {
		name      string
		haystack  string
		needle    string
		wantScore int
		wantFirst int
	}{
		{name: "case insensitive", haystack: "Go go GOPHERS", needle: "go", wantScore: 3, wantFirst: 0},
		{name: "no match", haystack: "nothing", needle: "zz", wantScore: 0, wantFirst: -1},
		{name: "non overlapping", haystack: "aaaa", needle: "aa", wantScore: 2, wantFirst: 0},
		{name: "offset", haystack: "xxGo", needle: "go", wantScore: 1, wantFirst: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score, first := countFoldASCII(tt.haystack, tt.needle)
			if score != tt.wantScore || first != tt.wantFirst {
				t.Errorf("countFoldASCII(%q, %q) = (%d, %d), want (%d, %d)",
					tt.haystack, tt.needle, score, first, tt.wantScore, tt.wantFirst)
			}
		})
	}
}
