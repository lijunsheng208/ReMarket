package handler

import (
	"context"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/internal/user/service"
)

// WithFollow 注入关注业务，保留现有认证构造方式。
func (s *Server) WithFollow(f *service.FollowService) *Server { s.follow = f; return s }

// Follow 适配关注请求，身份始终取自服务端认证上下文。
func (s *Server) Follow(ctx context.Context, r *userv1.FollowRequest) (*userv1.FollowResponse, error) {
	if s.follow == nil {
		return s.UnimplementedUserServiceServer.Follow(ctx, r)
	}
	err := s.follow.Set(ctx, r.GetUserId(), true)
	return &userv1.FollowResponse{}, err
}

// Unfollow 适配幂等取消关注请求。
func (s *Server) Unfollow(ctx context.Context, r *userv1.FollowRequest) (*userv1.FollowResponse, error) {
	if s.follow == nil {
		return s.UnimplementedUserServiceServer.Unfollow(ctx, r)
	}
	err := s.follow.Set(ctx, r.GetUserId(), false)
	return &userv1.FollowResponse{}, err
}

// ListFollowers 查询粉丝列表。
func (s *Server) ListFollowers(ctx context.Context, r *userv1.ListFollowsRequest) (*userv1.ListFollowsResponse, error) {
	if s.follow == nil {
		return s.UnimplementedUserServiceServer.ListFollowers(ctx, r)
	}
	return s.listFollows(ctx, r, true)
}

// ListFollowing 查询关注列表。
func (s *Server) ListFollowing(ctx context.Context, r *userv1.ListFollowsRequest) (*userv1.ListFollowsResponse, error) {
	if s.follow == nil {
		return s.UnimplementedUserServiceServer.ListFollowing(ctx, r)
	}
	return s.listFollows(ctx, r, false)
}

// listFollows 将关系和公开资料转换为协议响应。
func (s *Server) listFollows(ctx context.Context, r *userv1.ListFollowsRequest, followers bool) (*userv1.ListFollowsResponse, error) {
	rows, next, err := s.follow.List(ctx, r.GetUserId(), r.GetAfterId(), int(r.GetPageSize()), followers)
	if err != nil {
		return nil, err
	}
	out := &userv1.ListFollowsResponse{NextAfterId: next}
	for _, row := range rows {
		out.Items = append(out.Items, &userv1.FollowEntry{Id: row.ID, User: publicUser(row.User)})
	}
	return out, nil
}
