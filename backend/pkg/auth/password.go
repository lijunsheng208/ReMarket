// Package auth 提供认证相关的基础组件。
package auth

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const (
	// MinPasswordCharacters 是当前注册密码策略的最少 Unicode 字符数。
	MinPasswordCharacters = 8
	// MaxPasswordBytes 是 bcrypt 支持的密码字节上限，不是字符数。
	MaxPasswordBytes = 72
)

var (
	ErrInvalidPassword     = errors.New("密码必须为有效的 UTF-8 编码，至少包含 8 个字符且不超过 72 字节")
	ErrPasswordMismatch    = errors.New("密码不匹配")
	ErrInvalidPasswordHash = errors.New("存储的密码哈希无效")
)

// HashPassword 校验密码并生成 bcrypt 哈希；只返回可存储的哈希，不记录原文。
func HashPassword(password string) (string, error) {
	if err := validatePassword(password); err != nil {
		return "", err
	}
	// bcrypt 自行生成随机盐并将盐和计算成本编码进哈希，无需单独保存盐。
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("generate password hash: %w", err)
	}
	return string(hash), nil
}

// VerifyPassword 校验原文与已存哈希；区分输入无效、不匹配和存储哈希损坏。
// 登录层应将输入无效及不匹配转换为统一登录失败提示，损坏哈希作为内部错误处理。
func VerifyPassword(hash, password string) error {
	// 校验时同样拒绝超长密码，避免 bcrypt 只处理前 72 字节导致前缀匹配。
	if err := validatePassword(password); err != nil {
		return err
	}
	// 哈希由数据库读取而非客户端提供；固定检查编码长度，提前拒绝明显损坏的数据。
	if len(hash) != 60 {
		return ErrInvalidPasswordHash
	}
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return ErrPasswordMismatch
	}
	if err != nil {
		return ErrInvalidPasswordHash
	}
	return nil
}

// validatePassword 统一限制 UTF-8、字符数和字节数，不裁剪空白或修改密码内容。
func validatePassword(password string) error {
	// 先检查字节上限，再遍历字符，限制对超长不可信输入的处理成本。
	if len(password) > MaxPasswordBytes || !utf8.ValidString(password) || utf8.RuneCountInString(password) < MinPasswordCharacters {
		return ErrInvalidPassword
	}
	return nil
}
