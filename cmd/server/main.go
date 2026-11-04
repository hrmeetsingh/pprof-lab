package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	docprocv1 "github.com/hrmeetsingh/pprof-lab/gen/docproc/v1"
	"github.com/hrmeetsingh/pprof-lab/internal/admin"
	"github.com/hrmeetsingh/pprof-lab/internal/docproc"
	"github.com/hrmeetsingh/pprof-lab/internal/processing"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

func main() {
	var (
		grpcAddr            = flag.String("grpc-addr", envString("GRPC_ADDR", ":50051"), "gRPC listen address")
		adminAddr           = flag.String("admin-addr", envString("ADMIN_ADDR", ":9090"), "admin HTTP listen address (pprof, /stats, /healthz)")
		blockProfileRate    = flag.Int("block-profile-rate", envInt("BLOCK_PROFILE_RATE", 0), "runtime.SetBlockProfileRate value (0 disables block profiling)")
		mutexProfileFrac    = flag.Int("mutex-profile-fraction", envInt("MUTEX_PROFILE_FRACTION", 0), "runtime.SetMutexProfileFraction value (0 disables mutex profiling)")
		maxRecvMessageBytes = flag.Int("max-recv-bytes", envInt("MAX_RECV_BYTES", 8<<20), "maximum gRPC receive message size in bytes")
	)
	flag.Parse()

	logger := newLogger()
	slog.SetDefault(logger)

	if *blockProfileRate > 0 {
		runtime.SetBlockProfileRate(*blockProfileRate)
	}
	if *mutexProfileFrac > 0 {
		runtime.SetMutexProfileFraction(*mutexProfileFrac)
	}

	processor := processing.NewProcessor()
	service := docproc.NewService(processor)

	listener, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		logger.Error("failed to listen for gRPC", "addr", *grpcAddr, "error", err)
		os.Exit(1)
	}

	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(*maxRecvMessageBytes),
		grpc.ChainUnaryInterceptor(
			recoveryInterceptor(logger),
			loggingInterceptor(logger),
			labelsInterceptor(),
		),
	)
	docprocv1.RegisterDocumentServiceServer(grpcServer, service)

	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthServer.SetServingStatus(docprocv1.DocumentService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)
	reflection.Register(grpcServer)

	adminServer, err := admin.New(*adminAddr, processor)
	if err != nil {
		logger.Error("failed to start admin server", "addr", *adminAddr, "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("admin server listening",
			"addr", adminServer.Addr(),
			"pprof", "http://"+adminServer.Addr()+"/debug/pprof/",
			"stats", "http://"+adminServer.Addr()+"/stats",
		)
		if err := adminServer.Serve(); err != nil {
			logger.Error("admin server failed", "error", err)
			stop()
		}
	}()

	go func() {
		<-ctx.Done()
		logger.Info("shutdown requested")

		healthServer.Shutdown()
		stopped := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			logger.Warn("gRPC graceful stop timed out; forcing stop")
			grpcServer.Stop()
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := adminServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("admin server shutdown failed", "error", err)
		}
	}()

	logger.Info("gRPC server listening",
		"addr", listener.Addr().String(),
		"block_profile_rate", *blockProfileRate,
		"mutex_profile_fraction", *mutexProfileFrac,
	)

	if err := grpcServer.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		logger.Error("gRPC server failed", "error", err)
		os.Exit(1)
	}
	logger.Info("server stopped")
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func envString(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
