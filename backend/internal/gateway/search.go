package gateway

import (
	"context"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"net/url"
	searchv1 "remarket/backend/gen/search/v1"
	"strconv"
	"time"
)

// SearchClient 仅暴露公开搜索 RPC，便于验证 Gateway 适配边界。
type SearchClient interface {
	SearchProducts(context.Context, *searchv1.SearchProductsRequest, ...grpc.CallOption) (*searchv1.SearchProductsResponse, error)
}

// AddSearchRoutes 注册匿名只读搜索；不转发任意身份头或用户凭据。
func AddSearchRoutes(router *gin.Engine, client SearchClient, timeout time.Duration) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	h := &searchHTTP{client: client, timeout: timeout}
	router.GET("/api/v1/search/products", h.search)
}

type searchHTTP struct {
	client  SearchClient
	timeout time.Duration
}

// search 严格拒绝未知、重复及非法整数参数，再调用 Search 业务校验。
func (h *searchHTTP) search(c *gin.Context) {
	values, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		writeError(c, 400, "InvalidArgument", "查询参数无效")
		return
	}
	r := &searchv1.SearchProductsRequest{}
	for k, vs := range values {
		if len(vs) != 1 {
			writeError(c, 400, "InvalidArgument", "查询参数不能重复")
			return
		}
		v := vs[0]
		switch k {
		case "query":
			r.Query = v
		case "cursor":
			r.Cursor = v
		case "sort":
			sorts := map[string]searchv1.SearchSort{"relevance": 1, "newest": 2, "price_asc": 3, "price_desc": 4}
			sort, ok := sorts[v]
			if !ok {
				writeError(c, 400, "InvalidArgument", "排序无效")
				return
			}
			r.Sort = sort
		case "category_id", "min_price_cent", "max_price_cent", "page_size":
			if v == "" {
				writeError(c, 400, "InvalidArgument", "数字参数不能为空")
				return
			}
			for _, ch := range v {
				if ch < '0' || ch > '9' {
					writeError(c, 400, "InvalidArgument", "数字参数无效")
					return
				}
			}
			bits := 64
			if k == "page_size" {
				bits = 32
			}
			n, e := strconv.ParseInt(v, 10, bits)
			if e != nil {
				writeError(c, 400, "InvalidArgument", "数字参数溢出")
				return
			}
			switch k {
			case "category_id":
				r.CategoryId = &n
			case "min_price_cent":
				r.MinPriceCent = &n
			case "max_price_cent":
				r.MaxPriceCent = &n
			case "page_size":
				r.PageSize = int32(n)
			}
		default:
			writeError(c, 400, "InvalidArgument", "未知查询参数")
			return
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	response, err := h.client.SearchProducts(ctx, r)
	if err != nil {
		writeRPCError(c, err)
		return
	}
	writeProto(c, 200, response)
}
