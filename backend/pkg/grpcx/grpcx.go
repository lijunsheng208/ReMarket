// Package grpcx 提供一元 gRPC 调用的通用通信基础能力。
package grpcx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"remarket/backend/pkg/apperror"
)

type requestIDKey struct{}

// RequestID 从当前请求上下文读取请求标识；未设置时返回空字符串。
func RequestID(ctx context.Context) string { id, _ := ctx.Value(requestIDKey{}).(string); return id }

// WithRequestID 派生携带请求标识的上下文，保留原有取消信号与截止时间。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// Error 将业务错误、取消及超时转换为 gRPC 状态，隐藏未知内部错误。
func Error(err error) error {
	if err == nil {
		return nil
	}
	// 优先识别取消及超时，保留调用链的中止语义。
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "request canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	}
	// 已明确构造的 gRPC 状态直接透传；业务错误通过下方规则映射。
	if _, ok := status.FromError(err); ok {
		return err
	}
	var business *apperror.Error
	if errors.As(err, &business) {
		code := map[apperror.Kind]codes.Code{apperror.Invalid: codes.InvalidArgument, apperror.NotFound: codes.NotFound, apperror.Conflict: codes.AlreadyExists, apperror.Unauthenticated: codes.Unauthenticated, apperror.Forbidden: codes.PermissionDenied, apperror.FailedPrecondition: codes.FailedPrecondition, apperror.Unavailable: codes.Unavailable}[business.Kind]
		if code != codes.OK {
			return status.Error(code, business.Message)
		}
	}
	// 未知错误不向客户端暴露数据库连接、SQL 等内部细节。
	return status.Error(codes.Internal, "internal server error")
}

// validID 限制请求标识的长度和字符集，避免不可信内容进入日志与响应头。
func validID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.", c)) {
			return false
		}
	}
	return true
}

// newID 使用安全随机数生成请求标识；随机源失败时由拦截器恢复 panic。
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// ServerInterceptor 统一处理请求标识、超时、错误转换与 panic 恢复。
// 传入的认证凭据尚未验证，业务层必须通过后续认证模块建立可信身份。
func ServerInterceptor(logger *slog.Logger, timeout time.Duration) grpc.UnaryServerInterceptor {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (response any, err error) {
		start := time.Now()
		// 在执行 Handler 前安装恢复逻辑，仅能捕获当前 goroutine 的 panic。
		defer func() {
			if recover() != nil {
				err = status.Error(codes.Internal, "internal server error")
				response = nil
			}
			if err != nil {
				logger.ErrorContext(ctx, "RPC failed", "method", info.FullMethod, "request_id", RequestID(ctx), "code", status.Code(Error(err)).String())
			}
			logger.DebugContext(ctx, "RPC completed", "method", info.FullMethod, "duration", time.Since(start))
			err = Error(err)
		}()
		// 自定义 Context 值不能直接跨网络，需从 metadata 恢复请求标识。
		md, _ := metadata.FromIncomingContext(ctx)
		id := ""
		if values := md.Get("x-request-id"); len(values) > 0 {
			id = values[0]
		}
		if !validID(id) {
			id = newID()
		}
		// 同时写入本地 Context 和响应头，方便日志及客户端关联同一次请求。
		ctx = WithRequestID(ctx, id)
		_ = grpc.SetHeader(ctx, metadata.Pairs("x-request-id", id))
		// 基于原 Context 缩短期限，不延长上游更早的 Deadline；业务需响应取消。
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		return handler(ctx, req)
	}
}

// ClientInterceptor 设置调用超时，并从入站元数据中补充允许转发的字段。
// 已显式设置的出站元数据优先，不将 x-user-id、x-role 解释为可信身份。
func ClientInterceptor(timeout time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		out, _ := metadata.FromOutgoingContext(ctx)
		// 复制后再修改，避免污染上游复用的 metadata。
		out = out.Copy()
		incoming, _ := metadata.FromIncomingContext(ctx)
		// 只从入站请求补充凭据与追踪字段，传递 Token 不代表已完成鉴权。
		for _, key := range []string{"authorization", "traceparent", "tracestate"} {
			if len(out.Get(key)) == 0 && len(incoming.Get(key)) > 0 {
				out.Set(key, incoming.Get(key)...)
			}
		}
		if id := RequestID(ctx); validID(id) {
			out.Set("x-request-id", id)
		}
		ctx = metadata.NewOutgoingContext(ctx, out)
		// 基于原 Context 缩短期限，不延长上游更早的 Deadline；业务需响应取消。
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		return invoke(ctx, method, req, reply, cc, opts...)
	}
}

// Dial 创建惰性客户端连接，实际连通性需通过 RPC 验证。
// credential 为空时使用明文连接，仅适用于本地开发；timeout 为单次调用超时。
func Dial(target string, credential credentials.TransportCredentials, timeout time.Duration) (*grpc.ClientConn, error) {
	if credential == nil {
		credential = insecure.NewCredentials()
	}
	return grpc.NewClient(target, grpc.WithTransportCredentials(credential), grpc.WithChainUnaryInterceptor(ClientInterceptor(timeout)))
}

// NewServer 创建带通用拦截器的服务端，将单次请求处理时间限制为最多五秒。
func NewServer(logger *slog.Logger) *grpc.Server {
	return grpc.NewServer(grpc.ChainUnaryInterceptor(ServerInterceptor(logger, 5*time.Second)))
}

// Serve 注册进程健康检查并启动服务，上下文取消时执行有时限的优雅关闭。
// 当前骨架不将具体业务服务的健康状态标记为 SERVING。
func Serve(ctx context.Context, listener net.Listener, server *grpc.Server) error {
	h := health.NewServer()
	healthpb.RegisterHealthServer(server, h)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
			return
		case <-ctx.Done():
		}
		// 先标记健康检查不可用，再停止接收新请求并等待现有调用结束。
		h.Shutdown()
		stopped := make(chan struct{})
		go func() { server.GracefulStop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			// 超过宽限期强制关闭，避免退出流程被未结束的请求永久阻塞。
			server.Stop()
		}
	}()
	return server.Serve(listener)
}
