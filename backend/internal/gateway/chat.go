package gateway

import (
	"context"
	"github.com/gin-gonic/gin"
	chatv1 "remarket/backend/gen/chat/v1"
	"remarket/backend/pkg/auth"
	"strconv"
	"time"
)

// AddChatRoutes 注册需认证的会话管理与历史查询，不接受客户端指定参与者。
func AddChatRoutes(router *gin.Engine, chats chatv1.ChatServiceClient, verifier auth.TokenVerifier, timeout time.Duration) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	router.POST("/api/v1/conversations", bearerAuth(verifier), func(c *gin.Context) {
		var input struct {
			ProductID string `json:"product_id"`
		}
		if !decodeJSON(c, &input) {
			return
		}
		id, err := strconv.ParseInt(input.ProductID, 10, 64)
		if err != nil || id <= 0 {
			writeError(c, 400, "InvalidArgument", "商品 ID 无效")
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		result, err := chats.CreateConversation(ctx, &chatv1.CreateConversationRequest{ProductId: id})
		if err != nil {
			writeRPCError(c, err)
			return
		}
		writeProto(c, 200, result)
	})
	router.GET("/api/v1/conversations", bearerAuth(verifier), func(c *gin.Context) {
		size, ok := chatQueryInt(c, "page_size", 32)
		if !ok {
			return
		}
		cursor := c.Query("cursor")
		if len(c.Request.URL.Query()["cursor"]) > 1 {
			writeError(c, 400, "InvalidArgument", "游标无效")
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		result, err := chats.ListConversations(ctx, &chatv1.ListConversationsRequest{Cursor: cursor, PageSize: int32(size)})
		if err != nil {
			writeRPCError(c, err)
			return
		}
		writeProto(c, 200, result)
	})
	router.GET("/api/v1/conversations/:id/messages", bearerAuth(verifier), func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			writeError(c, 400, "InvalidArgument", "会话 ID 无效")
			return
		}
		size, ok := chatQueryInt(c, "page_size", 32)
		if !ok {
			return
		}
		before, ok := chatQueryInt(c, "before_seq", 64)
		if !ok {
			return
		}
		after, ok := chatQueryInt(c, "after_seq", 64)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		result, err := chats.ListMessages(ctx, &chatv1.ListMessagesRequest{ConversationId: id, PageSize: int32(size), BeforeSeq: before, AfterSeq: after})
		if err != nil {
			writeRPCError(c, err)
			return
		}
		writeProto(c, 200, result)
	})
}

// chatQueryInt 拒绝重复、空值和溢出的整数查询参数。
func chatQueryInt(c *gin.Context, key string, bits int) (int64, bool) {
	values, ok := c.Request.URL.Query()[key]
	if !ok {
		return 0, true
	}
	if len(values) == 1 {
		n, err := strconv.ParseInt(values[0], 10, bits)
		if err == nil && n >= 0 {
			return n, true
		}
	}
	writeError(c, 400, "InvalidArgument", "聊天分页参数无效")
	return 0, false
}
