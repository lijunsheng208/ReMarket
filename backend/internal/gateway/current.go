package gateway

import (
	"context"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"net/http"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/pkg/auth"
	"strings"
	"time"
)

type CurrentUserClient interface {
	GetCurrentUser(context.Context, *userv1.GetCurrentUserRequest, ...grpc.CallOption) (*userv1.UserResponse, error)
}

// NewAuthenticatedRouter 在匿名路由上增加当前用户接口，要求调用方注入已配置的验签组件。
func NewAuthenticatedRouter(users UserClient, current CurrentUserClient, verifier auth.TokenVerifier, timeout time.Duration) *gin.Engine {
	router := NewRouter(users, timeout)
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	router.GET("/api/v1/users/me", bearerAuth(verifier), func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		result, err := current.GetCurrentUser(ctx, &userv1.GetCurrentUserRequest{})
		if err != nil {
			writeRPCError(c, err)
			return
		}
		writeProto(c, http.StatusOK, result)
	})
	return router
}

// bearerAuth 独立验签后写入可信身份，再转发原 Token 供 User Service 再次验证。
func bearerAuth(verifier auth.TokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		values := c.Request.Header.Values("Authorization")
		if len(values) != 1 {
			rejectBearer(c)
			return
		}
		parts := strings.Fields(values[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			rejectBearer(c)
			return
		}
		ctx, err := auth.Authenticate(c.Request.Context(), parts[1], verifier)
		if err != nil {
			rejectBearer(c)
			return
		}
		// 仅传递已校验的原 Token，不将 HTTP 用户 ID 或角色头转换为内部可信身份。
		md, _ := metadata.FromOutgoingContext(ctx)
		md = md.Copy()
		md.Set("authorization", "Bearer "+parts[1])
		c.Request = c.Request.WithContext(metadata.NewOutgoingContext(ctx, md))
		c.Next()
	}
}

// rejectBearer 返回统一认证失败和 Bearer challenge，不暴露 Token 的具体失败原因。
func rejectBearer(c *gin.Context) {
	c.Header("WWW-Authenticate", "Bearer")
	writeError(c, http.StatusUnauthorized, "Unauthenticated", "请提供有效的登录令牌")
	c.Abort()
}
