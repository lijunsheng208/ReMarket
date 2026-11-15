package service

import (
	"context"
	"remarket/backend/internal/user/model"
	"remarket/backend/internal/user/repository"
	"remarket/backend/pkg/apperror"
	"remarket/backend/pkg/auth"
)

type ProfileRepository interface {
	UserByIDReader
	ProfileStats(context.Context, int64, int64) (*repository.FollowStats, error)
}
type ProfileService struct{ repo ProfileRepository }
type Profile struct {
	User   *model.User
	Stats  *repository.FollowStats
	IsSelf bool
}

// NewProfile 创建公开资料查询业务，身份仍由上游验签组件写入 Context。
func NewProfile(repo ProfileRepository) *ProfileService { return &ProfileService{repo: repo} }

// Get 返回启用账号的公开资料、实时统计与关系，拒绝使用过期或禁用调用者身份。
func (s *ProfileService) Get(ctx context.Context, id int64) (*Profile, error) {
	if id <= 0 {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "用户 ID 无效"}
	}
	viewer := int64(0)
	if identity, ok := auth.IdentityFromContext(ctx); ok {
		if _, err := NewCurrentUser(s.repo).GetCurrentUser(ctx); err != nil {
			return nil, err
		}
		viewer = identity.UserID
	}
	user, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if user.Status != model.StatusActive {
		return nil, repository.ErrUserNotFound
	}
	stats, err := s.repo.ProfileStats(ctx, id, viewer)
	if err != nil {
		return nil, err
	}
	return &Profile{User: user, Stats: stats, IsSelf: viewer == id}, nil
}
