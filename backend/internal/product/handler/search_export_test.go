package handler

import (
	"context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	productv1 "remarket/backend/gen/product/v1"
	"strings"
	"testing"
)

// TestSearchMaintenance 验证导出凭据独立和维护覆盖 Order 写入，普通 JWT 不授权导出。
func TestSearchMaintenance(t *testing.T) {
	token := strings.Repeat("s", 32)
	called := false
	next := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		return h(ctx, req)
	}
	gate, err := NewSearchMaintenance(token, strings.Repeat("j", 32), strings.Repeat("o", 32), true, next)
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(context.Context, any) (any, error) { called = true; return nil, nil }
	for _, method := range []string{productv1.ProductService_CreateProduct_FullMethodName, productv1.ProductService_ReserveProduct_FullMethodName, productv1.ProductService_ReleaseReservation_FullMethodName, productv1.ProductService_ConfirmReservation_FullMethodName} {
		_, err := gate(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: method}, invoke)
		if status.Code(err) != codes.Unavailable || called {
			t.Fatal("维护未阻止写入")
		}
	}
	_, err = gate(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: productv1.ProductService_ExportSearchSnapshot_FullMethodName}, invoke)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatal("匿名导出未拒绝")
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
	_, err = gate(ctx, nil, &grpc.UnaryServerInfo{FullMethod: productv1.ProductService_ExportSearchSnapshot_FullMethodName}, invoke)
	if err != nil || !called {
		t.Fatal("服务凭据未授权")
	}
	if _, err = NewSearchMaintenance(token, token, "", false, next); err == nil {
		t.Fatal("复用 JWT 密钥未拒绝")
	}
}
