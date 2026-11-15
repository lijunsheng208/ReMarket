package service

import (
	"context"
	"remarket/backend/internal/user/repository"
	"remarket/backend/pkg/apperror"
)

type FeedFollowersRepository interface {
	ListFeedFollowers(context.Context, int64, int64, int) (*repository.FeedFollowers, error)
}

// FeedFollowers 校验内部查询边界，凭据验证由 RPC 拦截器独立执行。
func (s *FollowService) FeedFollowers(ctx context.Context, seller, after int64, size int) (*repository.FeedFollowers, error) {
	if seller <= 0 || after < 0 || size < 0 || size > 500 {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "粉丝分页参数无效"}
	}
	if size == 0 {
		size = 500
	}
	repo, ok := s.repo.(FeedFollowersRepository)
	if !ok {
		return nil, &apperror.Error{Kind: apperror.Unavailable, Message: "粉丝查询不可用"}
	}
	return repo.ListFeedFollowers(ctx, seller, after, size)
}
