package grpcx

import (
	"context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"remarket/backend/pkg/auth"
	"strings"
)

// AuthInterceptor 默认保护所有方法，仅放行构造时明确列出的完整方法路径。
func AuthInterceptor(verifier auth.TokenVerifier, publicMethods ...string) grpc.UnaryServerInterceptor {
	public := make(map[string]bool, len(publicMethods))
	for _, method := range publicMethods {
		public[method] = true
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if public[info.FullMethod] {
			return handler(ctx, req)
		}
		md, _ := metadata.FromIncomingContext(ctx)
		values := md.Get("authorization")
		// 拒绝重复凭据和含空格的 Token，不接受 x-user-id/x-role 作为认证依据。
		if len(values) != 1 {
			return nil, status.Error(codes.Unauthenticated, "请提供有效的登录令牌")
		}
		parts := strings.Fields(values[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return nil, status.Error(codes.Unauthenticated, "请提供有效的登录令牌")
		}
		verified, err := auth.Authenticate(ctx, parts[1], verifier)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "请提供有效的登录令牌")
		}
		return handler(verified, req)
	}
}

// OptionalAuthInterceptor 对指定方法允许无凭据访问；一旦提供凭据仍必须验签，写方法保持强制鉴权。
func OptionalAuthInterceptor(verifier auth.TokenVerifier, optionalMethods []string, publicMethods ...string) grpc.UnaryServerInterceptor {
	optional := make(map[string]bool)
	for _, method := range optionalMethods {
		optional[method] = true
	}
	required := AuthInterceptor(verifier, publicMethods...)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		if optional[info.FullMethod] && len(md.Get("authorization")) == 0 {
			return handler(ctx, req)
		}
		return required(ctx, req, info, handler)
	}
}
