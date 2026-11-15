package service

import (
	"context"
	"remarket/backend/internal/user/model"
	"remarket/backend/internal/user/repository"
	"remarket/backend/pkg/apperror"
)

type FollowRepository interface {
	UserByIDReader
	SetFollow(context.Context, int64, int64, bool) error
	ListFollows(context.Context, int64, int64, int, bool) ([]repository.FollowUser, error)
}
type FollowService struct{ repo FollowRepository }

// NewFollow 创建关注业务，关系由 User 服务独占管理。
func NewFollow(repo FollowRepository) *FollowService { return &FollowService{repo: repo} }

// Set 校验可信登录身份、目标状态与禁止自关注规则，再执行幂等写入。
func (s *FollowService) Set(ctx context.Context, target int64, enabled bool) error {
	actor, err := NewCurrentUser(s.repo).GetCurrentUser(ctx)
	if err != nil {
		return err
	}
	if target <= 0 || actor.ID == target {
		return &apperror.Error{Kind: apperror.Invalid, Message: "关注目标无效"}
	}
	user, err := s.repo.GetByID(ctx, target)
	if err != nil {
		return err
	}
	if enabled && user.Status != model.StatusActive {
		return &apperror.Error{Kind: apperror.Forbidden, Message: "目标账号不可用"}
	}
	return s.repo.SetFollow(ctx, actor.ID, target, enabled)
}

// List 校验账号存在及分页边界，多取一条判断下一页。
func (s *FollowService) List(ctx context.Context, target, after int64, size int, followers bool) ([]repository.FollowUser, int64, error) {
	if _, err := NewCurrentUser(s.repo).GetCurrentUser(ctx); err != nil {
		return nil, 0, err
	}
	if target <= 0 || after < 0 || size < 0 || size > 100 {
		return nil, 0, &apperror.Error{Kind: apperror.Invalid, Message: "分页参数无效"}
	}
	if size == 0 {
		size = 20
	}
	if _, err := s.repo.GetByID(ctx, target); err != nil {
		return nil, 0, err
	}
	rows, err := s.repo.ListFollows(ctx, target, after, size+1, followers)
	if err != nil {
		return nil, 0, err
	}
	var next int64
	if len(rows) > size {
		rows = rows[:size]
		next = rows[len(rows)-1].ID
	}
	return rows, next, nil
}
