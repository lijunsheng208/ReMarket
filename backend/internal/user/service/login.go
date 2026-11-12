package service

import (
	"context"
	"errors"
	"remarket/backend/internal/user/model"
	"remarket/backend/internal/user/repository"
	"remarket/backend/pkg/apperror"
	"remarket/backend/pkg/auth"
	"strings"
	"time"
)

type UserReader interface {
	GetByUsername(context.Context, string) (*model.User, error)
}
type TokenIssuer interface {
	IssueAccessToken(int64, string) (string, time.Time, error)
}
type LoginService struct {
	users     UserReader
	tokens    TokenIssuer
	dummyHash string
}
type LoginInput struct{ Username, Password string }
type LoginResult struct {
	User        *model.User
	AccessToken string
	ExpiresAt   time.Time
}

var ErrLoginFailed = &apperror.Error{Kind: apperror.Unauthenticated, Message: "用户名或密码错误，或账号不可用"}

// NewLogin 创建登录服务并预生成占位哈希，避免不存在的用户完全跳过密码计算。
func NewLogin(users UserReader, tokens TokenIssuer) (*LoginService, error) {
	hash, err := auth.HashPassword("dummy-password-not-for-login")
	if err != nil {
		return nil, err
	}
	return &LoginService{users: users, tokens: tokens, dummyHash: hash}, nil
}

// Login 校验凭据与账号状态后签发令牌，不向外区分账号不存在、密码错误和禁用。
func (s *LoginService) Login(ctx context.Context, input LoginInput) (*LoginResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	username := strings.ToLower(strings.TrimSpace(input.Username))
	if !usernamePattern.MatchString(username) {
		return nil, ErrLoginFailed
	}
	user, err := s.users.GetByUsername(ctx, username)
	if err != nil && !errors.Is(err, repository.ErrUserNotFound) {
		return nil, err
	}
	missing := errors.Is(err, repository.ErrUserNotFound)
	hash := s.dummyHash
	if !missing {
		hash = user.PasswordHash
	}
	// 密码原样校验；不存在用户同样执行 bcrypt，但这不承诺严格恒定响应时间。
	passwordErr := auth.VerifyPassword(hash, input.Password)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if missing {
		return nil, ErrLoginFailed
	}
	if errors.Is(passwordErr, auth.ErrInvalidPassword) || errors.Is(passwordErr, auth.ErrPasswordMismatch) {
		return nil, ErrLoginFailed
	}
	// 损坏的存储哈希属于内部故障，不伪装成可重试的普通登录失败。
	if passwordErr != nil {
		return nil, passwordErr
	}
	if user.Status != model.StatusActive {
		return nil, ErrLoginFailed
	}
	token, expires, err := s.tokens.IssueAccessToken(user.ID, user.Role)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &LoginResult{User: user, AccessToken: token, ExpiresAt: expires}, nil
}
