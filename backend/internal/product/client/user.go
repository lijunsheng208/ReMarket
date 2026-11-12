// Package client 提供 Product 对其他业务服务的协议适配。
package client

import (
	"context"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/pkg/auth"
)

type UserVerifier struct{ users userv1.UserServiceClient }

// NewUserVerifier 创建管理员身份核实适配器，凭据由 gRPC Client 拦截器传递。
func NewUserVerifier(users userv1.UserServiceClient) *UserVerifier {
	return &UserVerifier{users: users}
}

// IsCurrentAdmin 核实当前账号与角色，不依赖 Token 的旧角色授权访问私有商品。
func (v *UserVerifier) IsCurrentAdmin(ctx context.Context) (bool, error) {
	result, err := v.users.GetCurrentUser(ctx, &userv1.GetCurrentUserRequest{})
	if err != nil {
		return false, err
	}
	identity, ok := auth.IdentityFromContext(ctx)
	return ok && result.User != nil && result.User.Id == identity.UserID && result.User.Role == "ADMIN" && result.User.Status == "ACTIVE", nil
}
