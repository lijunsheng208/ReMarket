// Package handler 适配用户 gRPC 请求与响应，不承担业务规则或数据库操作。
package handler

import (
	"context"
	"google.golang.org/protobuf/types/known/timestamppb"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/internal/user/model"
	"remarket/backend/internal/user/service"
	"remarket/backend/pkg/apperror"
)

type Registrar interface {
	Register(context.Context, service.RegisterInput) (*model.User, error)
}
type Server struct {
	userv1.UnimplementedUserServiceServer
	users Registrar
}

// New 创建用户 RPC 适配器；未覆盖的登录等方法继续返回 Unimplemented。
func New(users Registrar) *Server { return &Server{users: users} }

// Register 转换注册输入并构造不包含密码哈希的响应，错误交给通用拦截器映射。
func (s *Server) Register(ctx context.Context, req *userv1.RegisterRequest) (*userv1.UserResponse, error) {
	if req == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "注册请求不能为空"}
	}
	user, err := s.users.Register(ctx, service.RegisterInput{Username: req.Username, Password: req.Password, Nickname: req.Nickname})
	if err != nil {
		return nil, err
	}
	// 显式列出可公开字段，不把数据库模型直接作为网络响应。
	result := &userv1.User{Id: user.ID, Username: user.Username, Nickname: user.Nickname, Role: user.Role, Status: user.Status, CreatedAt: timestamppb.New(user.CreatedAt)}
	if user.AvatarObjectKey != nil {
		result.AvatarObjectKey = *user.AvatarObjectKey
	}
	return &userv1.UserResponse{User: result}, nil
}
