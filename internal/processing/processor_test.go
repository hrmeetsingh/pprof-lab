package processing

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func mustIngest(t *testing.T, p *Processor, docs ...Document) []string {
	t.Helper()
	ids, err := p.Ingest(docs)
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	return ids
}

func TestIngestAndAnalyze(t *testing.T) {
	p := NewProcessor()
	body := "the quick brown fox jumps over the lazy dog the end"
	ids := mustIngest(t, p, Document{ID: "doc-1", Title: "Animals", Body: body})
	if len(ids) != 1 || ids[0] != "doc-1" {
		t.Fatalf("Ingest() ids = %v, want [doc-1]", ids)
	}

	summaries, err := p.Analyze(ids, 3)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("Analyze() returned %d summaries, want 1", len(summaries))
	}

	got := summaries[0]
	if got.DocumentID != "doc-1" {
		t.Errorf("DocumentID = %q, want doc-1", got.DocumentID)
	}
	if got.WordCount != 11 {
		t.Errorf("WordCount = %d, want 11", got.WordCount)
	}
	if got.UniqueTerms != 9 {
		t.Errorf("UniqueTerms = %d, want 9", got.UniqueTerms)
	}

	wantDigest := sha256.Sum256([]byte(body))
	if got.Digest != hex.EncodeToString(wantDigest[:]) {
		t.Errorf("Digest = %q, want %q", got.Digest, hex.EncodeToString(wantDigest[:]))
	}

	if len(got.TopTerms) != 3 {
		t.Fatalf("len(TopTerms) = %d, want 3", len(got.TopTerms))
	}
	if got.TopTerms[0].Term != "the" || got.TopTerms[0].Count != 3 {
		t.Errorf("TopTerms[0] = %+v, want the/3", got.TopTerms[0])
	}
	if got.TopTerms[1].Term != "brown" || got.TopTerms[1].Count != 1 {
		t.Errorf("TopTerms[1] = %+v, want brown/1", got.TopTerms[1])
	}
	if got.TopTerms[2].Term != "dog" || got.TopTerms[2].Count != 1 {
		t.Errorf("TopTerms[2] = %+v, want dog/1", got.TopTerms[2])
	}
}

func TestAnalysisCache(t *testing.T) {
	p := NewProcessor()
	ids := mustIngest(t, p, Document{ID: "doc-1", Body: "one two two three three three"})

	if _, err := p.Analyze(ids, 5); err != nil {
		t.Fatalf("first Analyze() error = %v", err)
	}
	if _, err := p.Analyze(ids, 5); err != nil {
		t.Fatalf("second Analyze() error = %v", err)
	}

	stats := p.Stats()
	if stats.Analyses != 1 {
		t.Errorf("Analyses = %d, want 1 (second call should hit the cache)", stats.Analyses)
	}
	if stats.CacheHits != 1 {
		t.Errorf("CacheHits = %d, want 1", stats.CacheHits)
	}
	if stats.CacheEntries != 1 {
		t.Errorf("CacheEntries = %d, want 1", stats.CacheEntries)
	}
}

func TestIngestRejectsDuplicate(t *testing.T) {
	p := NewProcessor()
	mustIngest(t, p, Document{ID: "doc-1", Body: "hello world"})

	_, err := p.Ingest([]Document{
		{ID: "doc-2", Body: "second document"},
		{ID: "doc-1", Body: "duplicate"},
	})
	if !errors.Is(err, ErrDuplicateDocument) {
		t.Fatalf("Ingest() error = %v, want ErrDuplicateDocument", err)
	}
	if _, err := p.Analyze([]string{"doc-2"}, 1); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("Analyze(doc-2) error = %v, want ErrDocumentNotFound (ingest must be all-or-nothing)", err)
	}
}

func TestIngestValidation(t *testing.T) {
	tests := []struct {
		name string
		docs []Document
	}{
		{name: "empty batch"},
		{name: "empty id", docs: []Document{{Body: "body"}}},
		{name: "empty body", docs: []Document{{ID: "id"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewProcessor()
			if _, err := p.Ingest(tt.docs); !errors.Is(err, ErrInvalidDocument) {
				t.Fatalf("Ingest() error = %v, want ErrInvalidDocument", err)
			}
		})
	}
}

func TestAnalyzeAllDocuments(t *testing.T) {
	p := NewProcessor()
	mustIngest(t, p,
		Document{ID: "b", Body: "beta beta"},
		Document{ID: "a", Body: "alpha"},
	)

	summaries, err := p.Analyze(nil, 0)
	if err != nil {
		t.Fatalf("Analyze(nil) error = %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("Analyze(nil) returned %d summaries, want 2", len(summaries))
	}
	if summaries[0].DocumentID != "a" || summaries[1].DocumentID != "b" {
		t.Errorf("Analyze(nil) order = [%s %s], want [a b]", summaries[0].DocumentID, summaries[1].DocumentID)
	}
	if len(summaries[0].TopTerms) != 1 || summaries[0].TopTerms[0].Term != "alpha" {
		t.Errorf("top terms were not capped by the number of distinct terms: %+v", summaries[0].TopTerms)
	}
}

func TestAnalyzeMissingDocument(t *testing.T) {
	p := NewProcessor()
	if _, err := p.Analyze([]string{"missing"}, 5); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("Analyze() error = %v, want ErrDocumentNotFound", err)
	}
}

func TestTopTermsCap(t *testing.T) {
	p := NewProcessor()
	body := "a b c d e f g h i j k l m n o p q r s t u v w x y z"
	mustIngest(t, p, Document{ID: "doc", Body: body})

	summaries, err := p.Analyze([]string{"doc"}, 1000)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if got := len(summaries[0].TopTerms); got != 26 {
		t.Errorf("len(TopTerms) = %d, want 26 (all terms, capped at distinct count)", got)
	}
}

func TestSearch(t *testing.T) {
	p := NewProcessor()
	mustIngest(t, p,
		Document{ID: "doc-1", Body: "Go is fun. go go gophers."},
		Document{ID: "doc-2", Body: "No match here."},
	)

	hits, err := p.Search("GO", nil, 1)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("Search() returned %d hits, want 1", len(hits))
	}
	if hits[0].DocumentID != "doc-1" {
		t.Errorf("DocumentID = %q, want doc-1", hits[0].DocumentID)
	}
	if hits[0].Score != 4 {
		t.Errorf("Score = %d, want 4", hits[0].Score)
	}
	if hits[0].Snippet == "" {
		t.Error("Snippet is empty")
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	p := NewProcessor()
	if _, err := p.Search("   ", nil, 10); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("Search() error = %v, want ErrEmptyQuery", err)
	}
}

func TestConcurrentUse(t *testing.T) {
	p := NewProcessor()
	mustIngest(t, p, Document{ID: "shared", Body: "shared document with words"})

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("worker-%d", i)
			if _, err := p.Ingest([]Document{{ID: id, Body: "alpha beta gamma " + id}}); err != nil {
				errs <- err
				return
			}
			if _, err := p.Analyze([]string{id, "shared"}, 5); err != nil {
				errs <- err
				return
			}
			if _, err := p.Search("alpha", []string{id, "shared"}, 5); err != nil {
				errs <- err
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent operation failed: %v", err)
	}

	if got := p.Stats().Documents; got != workers+1 {
		t.Errorf("Documents = %d, want %d", got, workers+1)
	}
}
