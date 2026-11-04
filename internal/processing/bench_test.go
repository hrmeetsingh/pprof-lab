package processing

import (
	"strings"
	"testing"
)

func benchmarkDoc() Document {
	return Document{
		ID:    "bench-doc",
		Title: "Benchmark",
		Body: strings.Repeat(
			"The Quick Brown Fox jumps over the lazy Dog while Gophers write Go code ",
			40,
		),
	}
}

// BenchmarkAnalyzeDocument measures the uncached analysis path. Run it on each
// branch to compare the effect of the optimizations:
//
//	go test -run '^$' -bench AnalyzeDocument -benchmem -count 5 ./internal/processing
func BenchmarkAnalyzeDocument(b *testing.B) {
	doc := benchmarkDoc()
	b.SetBytes(int64(len(doc.Body)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = analyzeDocument(doc, 10)
	}
}

// BenchmarkSearch measures search against a single stored document.
func BenchmarkSearch(b *testing.B) {
	p := NewProcessor()
	doc := benchmarkDoc()
	if _, err := p.Ingest([]Document{doc}); err != nil {
		b.Fatal(err)
	}
	ids := []string{doc.ID}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Search("gophers", ids, 10); err != nil {
			b.Fatal(err)
		}
	}
}
