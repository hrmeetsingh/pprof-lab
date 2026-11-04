// Package docproc adapts the processing package to the gRPC service contract.
package docproc

import (
	"context"
	"errors"

	docprocv1 "github.com/harmeetsingh/pprof-lab/gen/docproc/v1"
	"github.com/harmeetsingh/pprof-lab/internal/processing"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Service implements docprocv1.DocumentServiceServer.
type Service struct {
	docprocv1.UnimplementedDocumentServiceServer
	processor *processing.Processor
}

// NewService wires a processor into the gRPC API.
func NewService(processor *processing.Processor) *Service {
	return &Service{processor: processor}
}

func (s *Service) Ingest(_ context.Context, req *docprocv1.IngestRequest) (*docprocv1.IngestResponse, error) {
	if req == nil || len(req.GetDocuments()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one document is required")
	}

	docs := make([]processing.Document, 0, len(req.GetDocuments()))
	for _, doc := range req.GetDocuments() {
		if doc.GetId() == "" {
			return nil, status.Error(codes.InvalidArgument, "document id must not be empty")
		}
		docs = append(docs, processing.Document{
			ID:    doc.GetId(),
			Title: doc.GetTitle(),
			Body:  doc.GetBody(),
		})
	}

	ids, err := s.processor.Ingest(docs)
	if err != nil {
		return nil, mapError(err)
	}
	return &docprocv1.IngestResponse{
		Ids:      ids,
		Accepted: int32(len(ids)),
	}, nil
}

func (s *Service) Analyze(_ context.Context, req *docprocv1.AnalyzeRequest) (*docprocv1.AnalyzeResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request must not be nil")
	}

	summaries, err := s.processor.Analyze(req.GetDocumentIds(), int(req.GetTopTerms()))
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]*docprocv1.DocumentSummary, 0, len(summaries))
	for _, summary := range summaries {
		terms := make([]*docprocv1.TermCount, 0, len(summary.TopTerms))
		for _, term := range summary.TopTerms {
			terms = append(terms, &docprocv1.TermCount{
				Term:  term.Term,
				Count: term.Count,
			})
		}
		out = append(out, &docprocv1.DocumentSummary{
			DocumentId:  summary.DocumentID,
			WordCount:   int32(summary.WordCount),
			UniqueTerms: int32(summary.UniqueTerms),
			Digest:      summary.Digest,
			TopTerms:    terms,
		})
	}
	return &docprocv1.AnalyzeResponse{Summaries: out}, nil
}

func (s *Service) Search(_ context.Context, req *docprocv1.SearchRequest) (*docprocv1.SearchResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request must not be nil")
	}

	hits, err := s.processor.Search(req.GetQuery(), req.GetDocumentIds(), int(req.GetLimit()))
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]*docprocv1.SearchHit, 0, len(hits))
	for _, hit := range hits {
		out = append(out, &docprocv1.SearchHit{
			DocumentId: hit.DocumentID,
			Score:      int32(hit.Score),
			Snippet:    hit.Snippet,
		})
	}
	return &docprocv1.SearchResponse{Hits: out}, nil
}

func (s *Service) Stats(_ context.Context, _ *docprocv1.StatsRequest) (*docprocv1.StatsResponse, error) {
	stats := s.processor.Stats()
	return &docprocv1.StatsResponse{
		Documents:      int64(stats.Documents),
		TotalBytes:     stats.TotalBytes,
		Analyses:       stats.Analyses,
		CacheHits:      stats.CacheHits,
		CacheEntries:   int64(stats.CacheEntries),
		HeapAllocBytes: int64(stats.HeapAllocBytes),
		Goroutines:     int64(stats.Goroutines),
		Evictions:      stats.Evictions,
	}, nil
}

func mapError(err error) error {
	switch {
	case errors.Is(err, processing.ErrInvalidDocument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, processing.ErrDuplicateDocument):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, processing.ErrDocumentNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, processing.ErrEmptyQuery):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
