package gateway

import (
	"context"
	"github.com/gin-gonic/gin"
	chatv1 "remarket/backend/gen/chat/v1"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/internal/chat/node"
	"remarket/backend/pkg/auth"
	"remarket/backend/pkg/grpcx"
	"strings"
	"time"
)

// AddChatRealtimeRoutes 注册票据和 WebSocket；调用方退出时必须关闭返回的 Node。
func AddChatRealtimeRoutes(router *gin.Engine, chats chatv1.ChatServiceClient, users CurrentUserClient, verifier auth.TokenVerifier, origins []string, timeout time.Duration) *node.Node {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	hub := node.New(chats, users, verifier, origins)
	router.POST("/api/v1/chat/tickets", bearerAuth(verifier), func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if !hub.OriginAllowed(c.Request) {
			writeError(c, 403, "PermissionDenied", "来源不允许")
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		if _, err := users.GetCurrentUser(ctx, &userv1.GetCurrentUserRequest{}); err != nil {
			writeRPCError(c, err)
			return
		}
		raw := strings.Fields(c.GetHeader("Authorization"))[1]
		value, expires, err := hub.Issue(raw)
		if err != nil {
			writeRPCError(c, grpcx.Error(err))
			return
		}
		c.JSON(200, gin.H{"ticket": value, "expires_at": expires.UTC().Format(time.RFC3339Nano)})
	})
	router.GET("/api/v1/chat/ws", func(c *gin.Context) { hub.ServeHTTP(c.Writer, c.Request) })
	return hub
}
