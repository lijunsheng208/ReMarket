package auth

import (
	"os"
	"time"
)

// JWTConfigFromEnv 从已导出的环境变量读取配置，不自动加载 .env，不回显密钥。
func JWTConfigFromEnv() (JWTConfig, error) {
	ttl, err := time.ParseDuration(os.Getenv("JWT_ACCESS_TTL"))
	if err != nil {
		return JWTConfig{}, ErrJWTConfig
	}
	config := JWTConfig{Secret: os.Getenv("JWT_SECRET"), Issuer: os.Getenv("JWT_ISSUER"), Audience: os.Getenv("JWT_AUDIENCE"), AccessTTL: ttl}
	if _, err := NewJWTManager(config); err != nil {
		return JWTConfig{}, err
	}
	return config, nil
}
