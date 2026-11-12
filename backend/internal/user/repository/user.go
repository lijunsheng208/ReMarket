// Package repository 实现 User 服务的数据访问，不承担注册、密码哈希或鉴权规则。
package repository

import (
	"context"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"remarket/backend/internal/user/model"
	"remarket/backend/pkg/apperror"
)

var (
	ErrUserNotFound   = &apperror.Error{Kind: apperror.NotFound, Message: "用户不存在"}
	ErrUsernameExists = &apperror.Error{Kind: apperror.Conflict, Message: "用户名已存在"}
)

type UserRepository struct{ db *gorm.DB }

// NewUserRepository 通过构造参数注入数据库，连接池生命周期由服务组装层管理。
func NewUserRepository(db *gorm.DB) *UserRepository { return &UserRepository{db: db} }

// CreateUserParams 只接收已哈希的密码，不接收明文、主键或管理员权限。
type CreateUserParams struct{ Username, PasswordHash, Nickname string }

// Create 创建普通启用用户，唯一性由数据库约束裁决，避免先查再写的并发竞态。
func (r *UserRepository) Create(ctx context.Context, params CreateUserParams) (*model.User, error) {
	user := &model.User{Username: params.Username, PasswordHash: params.PasswordHash, Nickname: params.Nickname, Role: model.RoleUser, Status: model.StatusActive}
	// 新记录不携带 ID；当前 users 唯一业务键是 username，因此重复键对应用户名冲突。
	if err := r.db.WithContext(ctx).Create(user).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil, ErrUsernameExists
		}
		return nil, fmt.Errorf("创建用户失败: %w", err)
	}
	return user, nil
}

// GetByUsername 按用户名查询，使用参数绑定，不在 Repository 内修改用户名大小写。
func (r *UserRepository) GetByUsername(ctx context.Context, username string) (*model.User, error) {
	var user model.User
	if err := r.db.WithContext(ctx).Where("username = ?", username).Take(&user).Error; err != nil {
		return nil, mapReadError(err)
	}
	return &user, nil
}

// GetByID 按明确的主键条件查询；零值 ID 不会意外退化成首条记录查询。
func (r *UserRepository) GetByID(ctx context.Context, id int64) (*model.User, error) {
	var user model.User
	if err := r.db.WithContext(ctx).Where("id = ?", id).Take(&user).Error; err != nil {
		return nil, mapReadError(err)
	}
	return &user, nil
}

// mapReadError 将未找到转换为业务错误，其余错误保留根因以支持取消检查与内部排障。
func mapReadError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrUserNotFound
	}
	return fmt.Errorf("查询用户失败: %w", err)
}
