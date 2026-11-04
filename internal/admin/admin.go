// Package admin exposes operational endpoints on a separate HTTP listener:
// health checks, JSON stats and the net/http/pprof handlers.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/hrmeetsingh/pprof-lab/internal/processing"
)

// Server is the admin HTTP server.
type Server struct {
	http     *http.Server
	listener net.Listener
}

// NewHandler builds the admin mux. It is exported so tests can use httptest.
func NewHandler(processor *processing.Processor) http.Handler {
	mux := http.NewServeMux()

	// net/http/pprof registers its handlers on http.DefaultServeMux when
	// imported. Registering explicitly keeps this server self-contained and
	// avoids leaking pprof onto any other HTTP server in the process.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(processor.Stats())
	})

	return mux
}

// New binds addr and returns a ready-to-serve admin server.
func New(addr string, processor *processing.Processor) (*Server, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	return &Server{
		listener: listener,
		http: &http.Server{
			Handler:           NewHandler(processor),
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       2 * time.Minute,
			// No WriteTimeout: /debug/pprof/profile?seconds=60 must be able
			// to keep the response open for the whole sample window.
		},
	}, nil
}

// Addr returns the bound address, which is useful when addr used port 0.
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Serve blocks until the server stops.
func (s *Server) Serve() error {
	err := s.http.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops the admin server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}
