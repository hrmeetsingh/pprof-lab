package docproc

import (
	"context"
	"net"
	"testing"

	docprocv1 "github.com/harmeetsingh/pprof-lab/gen/docproc/v1"
	"github.com/harmeetsingh/pprof-lab/internal/processing"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func newTestClient(t *testing.T) docprocv1.DocumentServiceClient {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	docprocv1.RegisterDocumentServiceServer(server, NewService(processing.NewProcessor()))

	go func() {
		_ = server.Serve(lis)
	}()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return docprocv1.NewDocumentServiceClient(conn)
}

func TestServiceEndToEnd(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)

	ingest, err := client.Ingest(ctx, &docprocv1.IngestRequest{
		Documents: []*docprocv1.Document{
			{Id: "doc-1", Title: "Go", Body: "go go gophers love go"},
			{Id: "doc-2", Title: "Other", Body: "nothing to see here"},
		},
	})
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	if ingest.GetAccepted() != 2 || len(ingest.GetIds()) != 2 {
		t.Fatalf("Ingest() = %+v, want 2 accepted documents", ingest)
	}

	analyze, err := client.Analyze(ctx, &docprocv1.AnalyzeRequest{
		DocumentIds: []string{"doc-1"},
		TopTerms:    2,
	})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if len(analyze.GetSummaries()) != 1 {
		t.Fatalf("Analyze() returned %d summaries, want 1", len(analyze.GetSummaries()))
	}
	summary := analyze.GetSummaries()[0]
	if summary.GetWordCount() != 5 || summary.GetUniqueTerms() != 3 {
		t.Errorf("summary counts = %d/%d, want 5/3", summary.GetWordCount(), summary.GetUniqueTerms())
	}
	if len(summary.GetTopTerms()) != 2 || summary.GetTopTerms()[0].GetTerm() != "go" {
		t.Errorf("top terms = %+v, want go first", summary.GetTopTerms())
	}

	search, err := client.Search(ctx, &docprocv1.SearchRequest{Query: "gophers", Limit: 5})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(search.GetHits()) != 1 || search.GetHits()[0].GetDocumentId() != "doc-1" {
		t.Errorf("Search() hits = %+v, want only doc-1", search.GetHits())
	}

	stats, err := client.Stats(ctx, &docprocv1.StatsRequest{})
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if stats.GetDocuments() != 2 || stats.GetHeapAllocBytes() <= 0 {
		t.Errorf("Stats() = %+v, want 2 documents and a positive heap size", stats)
	}
}

func TestServiceErrors(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)

	if _, err := client.Ingest(ctx, &docprocv1.IngestRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty Ingest() code = %v, want InvalidArgument", status.Code(err))
	}

	if _, err := client.Ingest(ctx, &docprocv1.IngestRequest{
		Documents: []*docprocv1.Document{{Id: "doc-1", Body: "hello"}},
	}); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	if _, err := client.Ingest(ctx, &docprocv1.IngestRequest{
		Documents: []*docprocv1.Document{{Id: "doc-1", Body: "again"}},
	}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate Ingest() code = %v, want AlreadyExists", status.Code(err))
	}

	if _, err := client.Analyze(ctx, &docprocv1.AnalyzeRequest{
		DocumentIds: []string{"missing"},
	}); status.Code(err) != codes.NotFound {
		t.Errorf("missing Analyze() code = %v, want NotFound", status.Code(err))
	}

	if _, err := client.Search(ctx, &docprocv1.SearchRequest{Query: " "}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty Search() code = %v, want InvalidArgument", status.Code(err))
	}
}
