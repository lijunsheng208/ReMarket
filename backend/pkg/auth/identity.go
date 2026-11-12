package auth

import (
	"context"
	"strconv"
)

type identityKey struct{}

// Identity 是通过 JWT 验签取得的身份，角色仅代表签发时的角色快照。
type Identity struct {
	UserID int64
	Role   string
}

// TokenVerifier 定义令牌验证能力，不接受直接从请求头构造的身份。
type TokenVerifier interface {
	VerifyAccessToken(string) (*AccessClaims, error)
}

// Authenticate 验证令牌后派生身份上下文，不导出跳过验签的身份写入入口。
func Authenticate(ctx context.Context, raw string, verifier TokenVerifier) (context.Context, error) {
	claims, err := verifier.VerifyAccessToken(raw)
	if err != nil {
		return nil, ErrInvalidToken
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || id <= 0 {
		return nil, ErrInvalidToken
	}
	return context.WithValue(ctx, identityKey{}, Identity{UserID: id, Role: claims.Role}), nil
}

// IdentityFromContext 读取已验证身份的值副本，不读取 metadata 中的用户 ID 或角色。
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityKey{}).(Identity)
	return identity, ok && identity.UserID > 0
}
