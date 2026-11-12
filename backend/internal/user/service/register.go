// Package service 实现用户业务规则，不依赖具体传输协议。
package service

import (
	"context"
	"errors"
	"regexp"
	"remarket/backend/internal/user/model"
	"remarket/backend/internal/user/repository"
	"remarket/backend/pkg/apperror"
	"remarket/backend/pkg/auth"
	"strings"
	"unicode"
	"unicode/utf8"
)

type UserCreator interface {
	Create(context.Context, repository.CreateUserParams) (*model.User, error)
}
type Service struct{ users UserCreator }
type RegisterInput struct{ Username, Password, Nickname string }

var usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{2,31}$`)

// New 创建用户业务服务，通过接口注入 Repository 以便独立测试业务规则。
func New(users UserCreator) *Service { return &Service{users: users} }

// Register 校验并规范化注册输入，密码哈希后创建普通用户；唯一冲突由 Repository 处理。
func (s *Service) Register(ctx context.Context, input RegisterInput) (*model.User, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 用户名规范化一次后用于存储；密码原样保留，不裁剪空格。
	username := strings.ToLower(strings.TrimSpace(input.Username))
	if !usernamePattern.MatchString(username) {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "用户名须为 3 至 32 位，以英文字母开头，仅含字母、数字和下划线"}
	}
	nickname := strings.TrimSpace(input.Nickname)
	if !utf8.ValidString(nickname) || utf8.RuneCountInString(nickname) < 1 || utf8.RuneCountInString(nickname) > 64 || strings.ContainsFunc(nickname, unicode.IsControl) {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "昵称须为 1 至 64 个字符，且不能包含控制字符"}
	}
	hash, err := auth.HashPassword(input.Password)
	if errors.Is(err, auth.ErrInvalidPassword) {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: err.Error()}
	}
	if err != nil {
		return nil, err
	}
	// bcrypt 无法中途取消；计算结束后再次检查，避免已取消请求继续入库。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.users.Create(ctx, repository.CreateUserParams{Username: username, PasswordHash: hash, Nickname: nickname})
}
