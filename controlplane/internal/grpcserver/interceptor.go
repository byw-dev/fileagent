package grpcserver

import (
	"context"
	"strings"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// contextKey is the type used for context value keys in this package to avoid
// collisions with keys from other packages.
type contextKey string

const (
	// claimsContextKey is the context key used to store parsed JWT claims.
	claimsContextKey contextKey = "grpc_jwt_claims"
)

// ── Public methods exposed by the connection ───────────────────────────────

// jwtExemptMethods lists gRPC full method names that do not require a JWT.
// Register and PollApproval are exempted because they are called before the
// Agent has been issued a token.
var jwtExemptMethods = map[string]bool{
	"/fileagent.v1.AgentService/Register":    true,
	"/fileagent.v1.AgentService/PollApproval": true,
}

// jwtUnaryInterceptor is a gRPC unary server interceptor that validates the
// Bearer JWT token in the request metadata. Phase 1 provides the skeleton;
// actual token verification (secret lookup + blacklist check) is added in
// Phase 2 (T2-A1).
func jwtUnaryInterceptor(logger *zap.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		if jwtExemptMethods[info.FullMethod] {
			return handler(ctx, req)
		}
		ctx, err := authenticateGRPC(ctx, logger)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// jwtStreamInterceptor is a gRPC stream server interceptor that validates the
// Bearer JWT token in the stream metadata.
func jwtStreamInterceptor(logger *zap.Logger) grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		stream grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		if jwtExemptMethods[info.FullMethod] {
			return handler(srv, stream)
		}
		ctx, err := authenticateGRPC(stream.Context(), logger)
		if err != nil {
			return err
		}
		return handler(srv, &wrappedStream{ServerStream: stream, ctx: ctx})
	}
}

// authenticateGRPC extracts and minimally validates the Bearer token from gRPC
// metadata. Phase 1 only checks that the token is present and well-formed;
// signature verification and blacklist lookup are added in Phase 2.
func authenticateGRPC(ctx context.Context, logger *zap.Logger) (context.Context, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx, status.Error(codes.Unauthenticated, "missing metadata")
	}

	values := md.Get("authorization")
	if len(values) == 0 {
		return ctx, status.Error(codes.Unauthenticated, "missing authorization header")
	}

	bearer := values[0]
	if !strings.HasPrefix(bearer, "Bearer ") {
		return ctx, status.Error(codes.Unauthenticated, "authorization header must use Bearer scheme")
	}

	token := strings.TrimPrefix(bearer, "Bearer ")
	if token == "" {
		return ctx, status.Error(codes.Unauthenticated, "empty token")
	}

	// TODO (Phase 2 T2-A1): verify JWT signature and check blacklist via Redis.
	logger.Debug("jwt interceptor: token present (verification skipped in Phase 1)",
		zap.Int("token_len", len(token)))

	return ctx, nil
}

// ── Logging interceptors ──────────────────────────────────────────────────────

// loggingUnaryInterceptor logs unary RPC calls with method name and outcome.
func loggingUnaryInterceptor(logger *zap.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		resp, err := handler(ctx, req)
		if err != nil {
			logger.Warn("gRPC unary call failed",
				zap.String("method", info.FullMethod),
				zap.Error(err),
			)
		} else {
			logger.Debug("gRPC unary call succeeded",
				zap.String("method", info.FullMethod),
			)
		}
		return resp, err
	}
}

// loggingStreamInterceptor logs streaming RPC connections.
func loggingStreamInterceptor(logger *zap.Logger) grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		stream grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		logger.Debug("gRPC stream started", zap.String("method", info.FullMethod))
		err := handler(srv, stream)
		if err != nil {
			logger.Warn("gRPC stream ended with error",
				zap.String("method", info.FullMethod),
				zap.Error(err),
			)
		} else {
			logger.Debug("gRPC stream completed", zap.String("method", info.FullMethod))
		}
		return err
	}
}

// ── wrappedStream ─────────────────────────────────────────────────────────────

// wrappedStream wraps a grpc.ServerStream to inject a modified context.
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

// Context returns the injected context.
func (w *wrappedStream) Context() context.Context { return w.ctx }
