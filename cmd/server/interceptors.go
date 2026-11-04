package main

import (
	"context"
	"log/slog"
	"runtime/debug"
	"runtime/pprof"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// labelsInterceptor attaches the gRPC method to every profile sample taken
// while the handler runs. Inspect the labels with:
//
//	go tool pprof -tags cpu.pprof
//	go tool pprof -tagfocus=grpc_method=/docproc.v1.DocumentService/Analyze cpu.pprof
func labelsInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		var (
			resp any
			err  error
		)
		pprof.Do(ctx, pprof.Labels("grpc_method", info.FullMethod), func(ctx context.Context) {
			resp, err = handler(ctx, req)
		})
		return resp, err
	}
}

func loggingInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		elapsed := time.Since(start)

		code := status.Code(err)
		if err != nil {
			logger.Warn("gRPC request failed",
				"method", info.FullMethod,
				"code", code.String(),
				"duration", elapsed,
				"error", err,
			)
			return resp, err
		}

		// Per-request logging is intentionally at debug level: synchronous
		// logging in a hot path distorts CPU profiles.
		logger.Debug("gRPC request",
			"method", info.FullMethod,
			"code", code.String(),
			"duration", elapsed,
		)
		return resp, nil
	}
}

func recoveryInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("panic in gRPC handler",
					"method", info.FullMethod,
					"panic", recovered,
					"stack", string(debug.Stack()),
				)
				resp = nil
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}
