package gateway

import (
	"context"
	"github.com/gin-gonic/gin"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/pkg/auth"
	"strconv"
	"time"
)

// AddFollowRoutes 注册关注接口，全部要求有效登录凭据并继续向 User 服务传递令牌。
func AddFollowRoutes(router *gin.Engine, users userv1.UserServiceClient, verifier auth.TokenVerifier, timeout time.Duration) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	group := router.Group("/api/v1/users", bearerAuth(verifier))
	// handle 统一校验路径及分页，超时从上游上下文派生。
	handle := func(operation string) gin.HandlerFunc {
		return func(c *gin.Context) {
			id, err := strconv.ParseInt(c.Param("id"), 10, 64)
			if err != nil || id <= 0 {
				writeError(c, 400, "InvalidArgument", "用户 ID 无效")
				return
			}
			ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
			defer cancel()
			if operation == "follow" || operation == "unfollow" {
				req := &userv1.FollowRequest{UserId: id}
				var result *userv1.FollowResponse
				if operation == "follow" {
					result, err = users.Follow(ctx, req)
				} else {
					result, err = users.Unfollow(ctx, req)
				}
				if err != nil {
					writeRPCError(c, err)
					return
				}
				writeProto(c, 200, result)
				return
			}
			req := &userv1.ListFollowsRequest{UserId: id}
			for _, key := range []string{"after_id", "page_size"} {
				values, exists := c.Request.URL.Query()[key]
				if !exists {
					continue
				}
				if len(values) != 1 {
					writeError(c, 400, "InvalidArgument", "分页参数无效")
					return
				}
				value, e := strconv.ParseInt(values[0], 10, 64)
				if e != nil || value < 0 || (key == "page_size" && value > 100) {
					writeError(c, 400, "InvalidArgument", "分页参数无效")
					return
				}
				if key == "after_id" {
					req.AfterId = value
				} else {
					req.PageSize = int32(value)
				}
			}
			var result *userv1.ListFollowsResponse
			if operation == "followers" {
				result, err = users.ListFollowers(ctx, req)
			} else {
				result, err = users.ListFollowing(ctx, req)
			}
			if err != nil {
				writeRPCError(c, err)
				return
			}
			writeProto(c, 200, result)
		}
	}
	group.POST("/:id/follow", handle("follow"))
	group.DELETE("/:id/follow", handle("unfollow"))
	group.GET("/:id/followers", handle("followers"))
	group.GET("/:id/following", handle("following"))
}
