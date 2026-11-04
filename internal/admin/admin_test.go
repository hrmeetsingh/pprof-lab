package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/harmeetsingh/pprof-lab/internal/processing"
)

func TestHandler(t *testing.T) {
	handler := NewHandler(processing.NewProcessor())

	tests := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{name: "health", path: "/healthz", wantStatus: http.StatusOK},
		{name: "stats", path: "/stats", wantStatus: http.StatusOK},
		{name: "pprof index", path: "/debug/pprof/", wantStatus: http.StatusOK},
		{name: "pprof goroutine", path: "/debug/pprof/goroutine?debug=1", wantStatus: http.StatusOK},
		{name: "unknown", path: "/nope", wantStatus: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != tt.wantStatus {
				t.Fatalf("GET %s = %d, want %d", tt.path, rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestStatsJSON(t *testing.T) {
	handler := NewHandler(processing.NewProcessor())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stats", nil))

	var stats processing.Stats
	if err := json.NewDecoder(rec.Body).Decode(&stats); err != nil {
		t.Fatalf("decoding /stats response: %v", err)
	}
	if stats.Goroutines <= 0 || stats.HeapAllocBytes == 0 {
		t.Errorf("stats = %+v, want populated runtime values", stats)
	}
}
