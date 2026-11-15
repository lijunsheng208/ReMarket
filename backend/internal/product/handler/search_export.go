package handler

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/product/service"
	"remarket/backend/pkg/apperror"
	"strings"
)

// WithSearchExport 注入维护窗口快照导出能力。
func (s *Server) WithSearchExport(exporter *service.SearchExporter) *Server {
	s.exporter = exporter
	return s
}

// ExportSearchSnapshot 仅适配内部协议，凭据由独立拦截器验证。
func (s *Server) ExportSearchSnapshot(ctx context.Context, r *productv1.ExportSearchSnapshotRequest) (*productv1.ExportSearchSnapshotResponse, error) {
	if s.exporter == nil {
		return s.UnimplementedProductServiceServer.ExportSearchSnapshot(ctx, r)
	}
	if r == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "导出请求不能为空"}
	}
	rows, upper, next, err := s.exporter.Export(ctx, r.AfterId, r.UpperId, int(r.PageSize))
	if err != nil {
		return nil, err
	}
	result := &productv1.ExportSearchSnapshotResponse{UpperId: upper, NextAfterId: next}
	for i := range rows {
		result.Products = append(result.Products, productProto(&rows[i]))
	}
	return result, nil
}

// NewSearchMaintenance 在用户/Order 鉴权之前隔离导出凭据，并暂停所有商品写 RPC。
// 维护开关为进程启动配置；所有 Product 副本必须一致开启，切换前等待旧进程在途请求退出。
func NewSearchMaintenance(token, jwtSecret, orderToken string, paused bool, next grpc.UnaryServerInterceptor) (grpc.UnaryServerInterceptor, error) {
	if token != "" && (len(token) < 32 || strings.ContainsAny(token, " \t\r\n") || token == jwtSecret || token == orderToken) {
		return nil, errors.New("Search 导出凭据必须独立、至少 32 字节且无空白")
	}
	expected := sha256.Sum256([]byte(token))
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod == productv1.ProductService_ExportSearchSnapshot_FullMethodName {
			md, _ := metadata.FromIncomingContext(ctx)
			values := md.Get("authorization")
			valid := false
			if len(values) == 1 {
				parts := strings.Fields(values[0])
				if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
					actual := sha256.Sum256([]byte(parts[1]))
					valid = token != "" && subtle.ConstantTimeCompare(expected[:], actual[:]) == 1
				}
			}
			if !valid {
				return nil, status.Error(codes.Unauthenticated, "请提供有效的导出凭据")
			}
			return handler(ctx, req)
		}
		if paused {
			switch info.FullMethod {
			case productv1.ProductService_GetProduct_FullMethodName, productv1.ProductService_ListProducts_FullMethodName, productv1.ProductService_ListMyProducts_FullMethodName, productv1.ProductService_ListCategories_FullMethodName, productv1.ProductService_GetReservation_FullMethodName, "/grpc.health.v1.Health/Check":
			default:
				return nil, status.Error(codes.Unavailable, "商品正在维护，请稍后重试")
			}
		}
		return next(ctx, req, info, handler)
	}, nil
}
