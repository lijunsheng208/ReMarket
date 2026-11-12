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
	login LoginProvider
}

// New 创建用户 RPC 适配器；未覆盖的登录等方法继续返回 Unimplemented。
func New(users Registrar) *Server { return &Server{users: users} }

// LoginProvider 定义登录业务契约，使传输层不依赖具体业务实现。
type LoginProvider interface {
	Login(context.Context, service.LoginInput) (*service.LoginResult, error)
}

// NewWithLogin 注入注册与登录业务，其余用户 RPC 保持未实现。
func NewWithLogin(users Registrar, login LoginProvider) *Server {
	return &Server{users: users, login: login}
}

// Login 转换登录输入并返回令牌及公开用户信息，不输出密码或哈希。
func (s *Server) Login(ctx context.Context, req *userv1.LoginRequest) (*userv1.LoginResponse, error) {
	if s.login == nil {
		return s.UnimplementedUserServiceServer.Login(ctx, req)
	}
	if req == nil {
		return nil, service.ErrLoginFailed
	}
	result, err := s.login.Login(ctx, service.LoginInput{Username: req.Username, Password: req.Password})
	if err != nil {
		return nil, err
	}
	return &userv1.LoginResponse{AccessToken: result.AccessToken, ExpiresAt: timestamppb.New(result.ExpiresAt), User: publicUser(result.User)}, nil
}

// publicUser 显式构造可公开字段，避免数据库密码哈希泄漏到响应。
func publicUser(user *model.User) *userv1.User {
	result := &userv1.User{Id: user.ID, Username: user.Username, Nickname: user.Nickname, Role: user.Role, Status: user.Status, CreatedAt: timestamppb.New(user.CreatedAt)}
	if user.AvatarObjectKey != nil {
		result.AvatarObjectKey = *user.AvatarObjectKey
	}
	return result
}

// Register 转换注册输入并构造不包含密码哈希的响应，错误交给通用拦截器映射。
func (s *Server) Register(ctx context.Context, req *userv1.RegisterRequest) (*userv1.UserResponse, error) {
	if req == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "注册请求不能为空"}
	}
	user, err := s.users.Register(ctx, service.RegisterInput{Username: req.Username, Password: req.Password, Nickname: req.Nickname})
	if err != nil {
		return nil, err
	}
	return &userv1.UserResponse{User: publicUser(user)}, nil
}
