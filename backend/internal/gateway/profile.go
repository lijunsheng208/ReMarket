package gateway

import (
	"context"
	"github.com/gin-gonic/gin"
	productv1 "remarket/backend/gen/product/v1"
	userv1 "remarket/backend/gen/user/v1"
	"remarket/backend/pkg/auth"
	"strconv"
	"time"
)

// AddProfileRoutes 注册公开主页、关系查询与卖家在售商品，身份由可选认证独立校验。
func AddProfileRoutes(router *gin.Engine, users userv1.UserServiceClient, products productv1.ProductServiceClient, verifier auth.TokenVerifier, timeout time.Duration) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	group := router.Group("/api/v1/users", optionalBearer(verifier))
	// profile 统一校验路径 ID，失败不退化成其他用户或首页。
	profile := func(c *gin.Context) (context.Context, context.CancelFunc, int64, bool) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			writeError(c, 400, "InvalidArgument", "用户 ID 无效")
			return nil, nil, 0, false
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		return ctx, cancel, id, true
	}
	// get 返回关系与计数，匿名请求不携带伪造身份。
	get := func(c *gin.Context) {
		ctx, cancel, id, ok := profile(c)
		if !ok {
			return
		}
		defer cancel()
		result, err := users.GetPublicProfile(ctx, &userv1.GetUserRequest{Id: id})
		if err != nil {
			writeRPCError(c, err)
			return
		}
		writeProto(c, 200, result)
	}
	group.GET("/:id/profile", get)
	group.GET("/:id/follow", get)
	group.GET("/:id/products", func(c *gin.Context) {
		ctx, cancel, id, ok := profile(c)
		if !ok {
			return
		}
		defer cancel()
		if _, err := users.GetPublicProfile(ctx, &userv1.GetUserRequest{Id: id}); err != nil {
			writeRPCError(c, err)
			return
		}
		r := &productv1.ListFeedProductsRequest{SellerIds: []int64{id}}
		for _, key := range []string{"page_size", "cursor"} {
			values, exists := c.Request.URL.Query()[key]
			if !exists {
				continue
			}
			if len(values) != 1 {
				writeError(c, 400, "InvalidArgument", "分页参数无效")
				return
			}
			if key == "cursor" {
				r.Cursor = values[0]
			} else {
				size, err := strconv.ParseInt(values[0], 10, 32)
				if err != nil {
					writeError(c, 400, "InvalidArgument", "页大小无效")
					return
				}
				r.PageSize = int32(size)
			}
		}
		result, err := products.ListFeedProducts(ctx, r)
		if err != nil {
			writeRPCError(c, err)
			return
		}
		writeProto(c, 200, result)
	})
}
