package service

import (
	"context"
	"errors"
	"remarket/backend/internal/user/model"
	"remarket/backend/internal/user/repository"
	"remarket/backend/pkg/apperror"
	"remarket/backend/pkg/auth"
)

type UserByIDReader interface {
	GetByID(context.Context, int64) (*model.User, error)
}
type CurrentUserService struct{ users UserByIDReader }

// NewCurrentUser 创建当前用户业务，身份来自已验证 Context，不允许客户端指定用户 ID。
func NewCurrentUser(users UserByIDReader) *CurrentUserService {
	return &CurrentUserService{users: users}
}

// GetCurrentUser 读取当前数据库状态，拒绝已删除或禁用账号，不以 JWT 角色覆盖最新资料。
func (s *CurrentUserService) GetCurrentUser(ctx context.Context) (*model.User, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok {
		return nil, &apperror.Error{Kind: apperror.Unauthenticated, Message: "请先登录"}
	}
	user, err := s.users.GetByID(ctx, identity.UserID)
	if errors.Is(err, repository.ErrUserNotFound) {
		return nil, &apperror.Error{Kind: apperror.Unauthenticated, Message: "登录账号不可用"}
	}
	if err != nil {
		return nil, err
	}
	if user.Status != model.StatusActive {
		return nil, &apperror.Error{Kind: apperror.Forbidden, Message: "账号已禁用"}
	}
	return user, nil
}
