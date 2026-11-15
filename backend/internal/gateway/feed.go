package gateway

import (
	"context"
	"github.com/gin-gonic/gin"
	feedv1 "remarket/backend/gen/feed/v1"
	"remarket/backend/pkg/auth"
	"strconv"
	"time"
)

// AddFeedRoutes 注册匿名首页与需认证的关注商品流，保留上游取消和截止时间。
func AddFeedRoutes(router *gin.Engine, feeds feedv1.FeedServiceClient, verifier auth.TokenVerifier, timeout time.Duration) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	// list 将 HTTP 游标参数适配到 Feed RPC，不接受客户端用户 ID。
	list := func(following bool) gin.HandlerFunc {
		return func(c *gin.Context) {
			r := &feedv1.ListFeedRequest{}
			for _, key := range []string{"cursor", "page_size"} {
				v, ok := c.Request.URL.Query()[key]
				if !ok {
					continue
				}
				if len(v) != 1 {
					writeError(c, 400, "InvalidArgument", "Feed 分页参数无效")
					return
				}
				if key == "cursor" {
					r.Cursor = v[0]
				} else {
					n, err := strconv.ParseInt(v[0], 10, 32)
					if err != nil {
						writeError(c, 400, "InvalidArgument", "Feed 页大小无效")
						return
					}
					r.PageSize = int32(n)
				}
			}
			ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
			defer cancel()
			var result *feedv1.ListFeedResponse
			var err error
			if following {
				result, err = feeds.Following(ctx, r)
			} else {
				result, err = feeds.Home(ctx, r)
			}
			if err != nil {
				writeRPCError(c, err)
				return
			}
			writeProto(c, 200, result)
		}
	}
	router.GET("/api/v1/feed/home", optionalBearer(verifier), list(false))
	router.GET("/api/v1/feed/following", bearerAuth(verifier), list(true))
}
