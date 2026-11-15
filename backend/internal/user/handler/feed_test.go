package handler

import (
	"context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	userv1 "remarket/backend/gen/user/v1"
	"strings"
	"testing"
)

// TestFeedServiceAuth 验证普通 JWT、重复凭据和空配置不能调用内部粉丝接口。
func TestFeedServiceAuth(t *testing.T) {
	token := strings.Repeat("f", 32)
	nextCalls := 0
	next := func(ctx context.Context, r any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		nextCalls++
		return h(ctx, r)
	}
	interceptor, err := NewFeedAuth(token, strings.Repeat("j", 32), next)
	if err != nil {
		t.Fatal(err)
	}
	info := &grpc.UnaryServerInfo{FullMethod: userv1.UserService_ListFeedFollowers_FullMethodName}
	handler := func(context.Context, any) (any, error) { return "ok", nil }
	for _, values := range [][]string{nil, {"Bearer jwt"}, {"Bearer " + token, "Bearer " + token}} {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{"authorization": values})
		if _, err := interceptor(ctx, nil, info, handler); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("未拒绝凭据 %v", err)
		}
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
	if _, err := interceptor(ctx, nil, info, handler); err != nil {
		t.Fatal(err)
	}
	if nextCalls != 0 {
		t.Fatal("服务令牌进入普通用户验签")
	}
	info.FullMethod = userv1.UserService_Follow_FullMethodName
	if _, err := interceptor(ctx, nil, info, handler); err != nil || nextCalls != 1 {
		t.Fatal("普通接口未交给用户认证")
	}
	if _, err := NewFeedAuth(token, token, next); err == nil {
		t.Fatal("允许与 JWT 复用")
	}
}
