// Package model 定义 User 服务拥有的业务数据模型。
package model

import "time"

const (
	RoleUser       = "USER"
	RoleAdmin      = "ADMIN"
	StatusActive   = "ACTIVE"
	StatusDisabled = "DISABLED"
)

// User 映射已有 users 表，不通过 AutoMigrate 创建或更新表结构。
type User struct {
	ID              int64 `gorm:"primaryKey;autoIncrement"`
	Username        string
	PasswordHash    string `json:"-"`
	Nickname        string
	AvatarObjectKey *string
	Role            string
	Status          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// TableName 明确模型对应的表名，不依赖默认复数规则。
func (User) TableName() string { return "users" }
