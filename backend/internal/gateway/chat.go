package gateway

import (
	"context"
	"github.com/gin-gonic/gin"
	chatv1 "remarket/backend/gen/chat/v1"
	"remarket/backend/internal/chat/node"
	"remarket/backend/pkg/auth"
	"strconv"
	"time"
)

// AddChatRoutes 注册需认证的会话管理与历史查询，不接受客户端指定参与者。
func AddChatRoutes(router *gin.Engine, chats chatv1.ChatServiceClient, verifier auth.TokenVerifier, timeout time.Duration, hubs ...*node.Node) {
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
		until, ok := chatQueryInt(c, "sync_until_seq", 64)
		if !ok {
			return
		}
		_, afterPresent := c.Request.URL.Query()["after_seq"]
		_, beforePresent := c.Request.URL.Query()["before_seq"]
		if (afterPresent && beforePresent) || (!afterPresent && until != 0) {
			writeError(c, 400, "InvalidArgument", "聊天分页参数互斥")
			return
		}
		request := &chatv1.ListMessagesRequest{ConversationId: id, PageSize: int32(size), BeforeSeq: before, SyncUntilSeq: until}
		if afterPresent {
			request.AfterSeq = &after
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		result, err := chats.ListMessages(ctx, request)
		if err != nil {
			writeRPCError(c, err)
			return
		}
		writeProto(c, 200, result)
	})
	router.POST("/api/v1/conversations/:id/read", bearerAuth(verifier), func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			writeError(c, 400, "InvalidArgument", "会话 ID 无效")
			return
		}
		var input struct {
			LastReadSeq *string `json:"last_read_seq"`
		}
		if !decodeJSON(c, &input) {
			return
		}
		if input.LastReadSeq == nil {
			writeError(c, 400, "InvalidArgument", "缺少已读位置")
			return
		}
		seq, err := strconv.ParseInt(*input.LastReadSeq, 10, 64)
		if err != nil || seq < 0 {
			writeError(c, 400, "InvalidArgument", "已读位置无效")
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		result, err := chats.ReadConversation(ctx, &chatv1.ReadConversationRequest{ConversationId: id, LastReadSeq: seq})
		if err != nil {
			writeRPCError(c, err)
			return
		}
		if result.GetState() == nil {
			writeError(c, 500, "Internal", "服务内部错误")
			return
		}
		writeProto(c, 200, result.State)
		// 游标以数据库返回值为准；通知是尽力投递，不能影响已提交的已读状态。
		if len(hubs) > 0 && hubs[0] != nil {
			hubs[0].NotifyRead(c.Request.Context(), result)
		}
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
