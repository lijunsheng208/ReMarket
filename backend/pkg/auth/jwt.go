package auth

import (
	"errors"
	"github.com/golang-jwt/jwt/v5"
	"strconv"
	"strings"
	"time"
)

var (
	ErrJWTConfig            = errors.New("JWT 配置无效")
	ErrInvalidToken         = errors.New("令牌无效或已过期")
	ErrInvalidTokenIdentity = errors.New("签发令牌的用户身份无效")
)

// JWTConfig 定义固定 HS256 算法的访问令牌配置；Secret 应为至少 32 字节的随机密钥。
type JWTConfig struct {
	Secret, Issuer, Audience string
	AccessTTL                time.Duration
}

// AccessClaims 仅包含身份及标准声明，不存储密码、哈希或敏感个人资料。
type AccessClaims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}
type JWTManager struct {
	secret           []byte
	issuer, audience string
	ttl              time.Duration
	now              func() time.Time
}

// NewJWTManager 校验配置并保存副本；不使用默认密钥，不允许由令牌选择算法。
func NewJWTManager(config JWTConfig) (*JWTManager, error) {
	if len(config.Secret) < 32 || strings.TrimSpace(config.Secret) == "" || strings.TrimSpace(config.Issuer) == "" || strings.TrimSpace(config.Audience) == "" || config.AccessTTL < time.Second || config.AccessTTL > 24*time.Hour {
		return nil, ErrJWTConfig
	}
	return &JWTManager{secret: []byte(config.Secret), issuer: config.Issuer, audience: config.Audience, ttl: config.AccessTTL, now: time.Now}, nil
}

// IssueAccessToken 为数据库确认的用户签发令牌，返回令牌及其实际精度的过期时间。
func (m *JWTManager) IssueAccessToken(userID int64, role string) (string, time.Time, error) {
	if userID <= 0 || (role != "USER" && role != "ADMIN") {
		return "", time.Time{}, ErrInvalidTokenIdentity
	}
	now := m.now().UTC().Truncate(time.Second)
	expires := now.Add(m.ttl).Truncate(time.Second)
	claims := AccessClaims{Role: role, RegisteredClaims: jwt.RegisteredClaims{Issuer: m.issuer, Audience: jwt.ClaimStrings{m.audience}, Subject: strconv.FormatInt(userID, 10), IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(expires)}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// VerifyAccessToken 验证签名、算法、发行方、受众和时间，再返回可信声明。
// 验签仅证明签发时身份；账号禁用、角色变更及令牌撤销需后续业务处理。
func (m *JWTManager) VerifyAccessToken(raw string) (*AccessClaims, error) {
	if len(raw) == 0 || len(raw) > 8192 {
		return nil, ErrInvalidToken
	}
	claims := &AccessClaims{}
	// 固定算法白名单且再次校验方法，拒绝 none 及其他 HMAC 算法；不信任头中的密钥信息。
	token, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, ErrInvalidToken
		}
		return m.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(m.issuer), jwt.WithAudience(m.audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(m.now))
	// 任意校验失败统一返回，避免调用方误用仅解析但未验证的声明。
	if err != nil || !token.Valid {
		return nil, ErrInvalidToken
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != claims.Subject || (claims.Role != "USER" && claims.Role != "ADMIN") || claims.IssuedAt == nil || claims.NotBefore == nil || !claims.ExpiresAt.After(claims.IssuedAt.Time) || claims.NotBefore.After(claims.ExpiresAt.Time) {
		return nil, ErrInvalidToken
	}
	return claims, nil
}
