package handler

import (
	"context"
	"google.golang.org/protobuf/types/known/timestamppb"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/internal/user/service"
)

// WithProfile 注入公开资料查询，不把密码、用户名或权限角色放入公开主页。
func (s *Server) WithProfile(profile *service.ProfileService) *Server { s.profile = profile; return s }

// GetPublicProfile 显式映射公开字段，匿名调用的关注状态为 false。
func (s *Server) GetPublicProfile(ctx context.Context, r *userv1.GetUserRequest) (*userv1.PublicProfile, error) {
	if s.profile == nil {
		return s.UnimplementedUserServiceServer.GetPublicProfile(ctx, r)
	}
	p, err := s.profile.Get(ctx, r.GetId())
	if err != nil {
		return nil, err
	}
	return &userv1.PublicProfile{Id: p.User.ID, Nickname: p.User.Nickname, FollowerCount: p.Stats.FollowerCount, FollowingCount: p.Stats.FollowingCount, IsFollowing: p.Stats.IsFollowing, IsSelf: p.IsSelf, CreatedAt: timestamppb.New(p.User.CreatedAt)}, nil
}
