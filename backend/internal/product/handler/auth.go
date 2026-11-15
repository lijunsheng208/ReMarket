package handler

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/grpcx"
)

// NewAuthInterceptor 按完整方法路径隔离 Order 服务凭据与用户 JWT；空服务凭据关闭预占入口。
// 非空服务凭据至少 32 字节，不允许空白或与 JWT 密钥复用，错误不回显凭据。
func NewAuthInterceptor(verifier auth.TokenVerifier, serviceToken, jwtSecret string) (grpc.UnaryServerInterceptor, error) {
	if serviceToken != "" && (len(serviceToken) < 32 || strings.ContainsAny(serviceToken, " \t\r\n") || serviceToken == jwtSecret) {
		return nil, errors.New("Order 服务凭据须至少 32 字节、无空白且独立于 JWT 密钥")
	}
	userAuth := grpcx.OptionalAuthInterceptor(verifier, []string{productv1.ProductService_GetProductImages_FullMethodName, productv1.ProductService_ListFeedProducts_FullMethodName, productv1.ProductService_GetProduct_FullMethodName, productv1.ProductService_ListProducts_FullMethodName, productv1.ProductService_ListCategories_FullMethodName}, "/grpc.health.v1.Health/Check")
	expected := sha256.Sum256([]byte(serviceToken))
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		switch info.FullMethod {
		case productv1.ProductService_ReserveProduct_FullMethodName, productv1.ProductService_GetReservation_FullMethodName, productv1.ProductService_ReleaseReservation_FullMethodName, productv1.ProductService_ConfirmReservation_FullMethodName:
			md, _ := metadata.FromIncomingContext(ctx)
			values := md.Get("authorization")
			if serviceToken == "" || len(values) != 1 {
				return nil, status.Error(codes.Unauthenticated, "请提供有效的服务凭据")
			}
			parts := strings.Fields(values[0])
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				return nil, status.Error(codes.Unauthenticated, "请提供有效的服务凭据")
			}
			actual := sha256.Sum256([]byte(parts[1]))
			if subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
				return nil, status.Error(codes.Unauthenticated, "请提供有效的服务凭据")
			}
			// 服务身份不转换为用户身份；调用方声明只能用于内部预占业务。
			return next(ctx, req)
		default:
			return userAuth(ctx, req, info, next)
		}
	}, nil
}
