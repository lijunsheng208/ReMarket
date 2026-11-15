package handler

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	userv1 "remarket/backend/gen/user/v1"
	"strings"
)

// NewFeedAuth 仅为粉丝 ID RPC 接受独立服务凭据，不将服务身份转换成用户身份。
func NewFeedAuth(token, jwtSecret string, next grpc.UnaryServerInterceptor) (grpc.UnaryServerInterceptor, error) {
	if token != "" && (len(token) < 32 || strings.ContainsAny(token, " \t\r\n") || token == jwtSecret) {
		return nil, fmt.Errorf("Feed 服务凭据必须独立且至少 32 字节")
	}
	expected := sha256.Sum256([]byte(token))
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if info.FullMethod != userv1.UserService_ListFeedFollowers_FullMethodName {
			return next(ctx, req, info, h)
		}
		md, _ := metadata.FromIncomingContext(ctx)
		v := md.Get("authorization")
		if token == "" || len(v) != 1 {
			return nil, status.Error(codes.Unauthenticated, "服务凭据无效")
		}
		parts := strings.Fields(v[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return nil, status.Error(codes.Unauthenticated, "服务凭据无效")
		}
		actual := sha256.Sum256([]byte(parts[1]))
		if subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
			return nil, status.Error(codes.Unauthenticated, "服务凭据无效")
		}
		return h(ctx, req)
	}, nil
}

// ListFeedFollowers 返回内部粉丝 ID 页，普通 JWT 不具备调用权限。
func (s *Server) ListFeedFollowers(ctx context.Context, r *userv1.ListFeedFollowersRequest) (*userv1.ListFeedFollowersResponse, error) {
	if s.follow == nil {
		return s.UnimplementedUserServiceServer.ListFeedFollowers(ctx, r)
	}
	page, err := s.follow.FeedFollowers(ctx, r.GetSellerId(), r.GetAfterId(), int(r.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &userv1.ListFeedFollowersResponse{UserIds: page.IDs, NextAfterId: page.Next, Total: page.Total}, nil
}
