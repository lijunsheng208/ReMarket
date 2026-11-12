package gateway

import (
	"context"
	"github.com/gin-gonic/gin"
	"google.golang.org/protobuf/proto"
	"net/http"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/pkg/auth"
	"strconv"
	"time"
)

// AddProductRoutes 在现有 Gateway 上挂载商品接口，写操作强制鉴权，读取可选鉴权。
func AddProductRoutes(router *gin.Engine, products productv1.ProductServiceClient, verifier auth.TokenVerifier, timeout time.Duration) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	h := &productHTTP{products: products, timeout: timeout}
	read := router.Group("/api/v1", optionalBearer(verifier))
	read.GET("/products", h.list)
	read.GET("/products/:id", h.detail)
	read.GET("/categories", h.categories)
	write := router.Group("/api/v1/products", bearerAuth(verifier))
	write.GET("/mine", h.listMine)
	write.POST("", h.create)
	write.PATCH("/:id", h.update)
	write.POST("/:id/publish", h.publish)
	write.POST("/:id/off-shelf", h.offShelf)
}

type productHTTP struct {
	products productv1.ProductServiceClient
	timeout  time.Duration
}

// optionalBearer 不提供凭据时匿名访问，提供无效凭据则拒绝，不静默降级。
func optionalBearer(verifier auth.TokenVerifier) gin.HandlerFunc {
	required := bearerAuth(verifier)
	return func(c *gin.Context) {
		if len(c.Request.Header.Values("Authorization")) == 0 {
			c.Next()
			return
		}
		required(c)
	}
}

// productID 严格解析正数有符号 BIGINT 路径参数。
func productID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, 400, "InvalidArgument", "商品 ID 无效")
		return 0, false
	}
	return id, true
}

// call 保留原 HTTP Context，统一设置超时并输出协议响应。
func (h *productHTTP) call(c *gin.Context, code int, invoke func(context.Context) (proto.Message, error)) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	result, err := invoke(ctx)
	if err != nil {
		writeRPCError(c, err)
		return
	}
	writeProto(c, code, result)
}

// create 创建草稿，不允许请求携带卖家、状态或主键。
func (h *productHTTP) create(c *gin.Context) {
	var input struct {
		CategoryID  int64  `json:"category_id,string"`
		Title       string `json:"title"`
		Description string `json:"description"`
		PriceCent   int64  `json:"price_cent,string"`
	}
	if !decodeJSON(c, &input) {
		return
	}
	h.call(c, 201, func(ctx context.Context) (proto.Message, error) {
		return h.products.CreateProduct(ctx, &productv1.CreateProductRequest{CategoryId: input.CategoryID, Title: input.Title, Description: input.Description, PriceCent: input.PriceCent})
	})
}

// list 校验数值转换后调用公开列表，负值及页大小限制继续由 Service 判断。
func (h *productHTTP) list(c *gin.Context) {
	request := &productv1.ListProductsRequest{}
	for _, key := range []string{"category_id", "page", "page_size"} {
		values, exists := c.Request.URL.Query()[key]
		if !exists {
			continue
		}
		if len(values) != 1 {
			writeError(c, 400, "InvalidArgument", "分页参数不能重复")
			return
		}
		bits := 32
		if key == "category_id" {
			bits = 64
		}
		value, err := strconv.ParseInt(values[0], 10, bits)
		if err != nil {
			writeError(c, 400, "InvalidArgument", "分页或分类参数无效")
			return
		}
		switch key {
		case "category_id":
			request.CategoryId = value
		case "page":
			request.Page = int32(value)
		case "page_size":
			request.PageSize = int32(value)
		}
	}
	h.call(c, 200, func(ctx context.Context) (proto.Message, error) { return h.products.ListProducts(ctx, request) })
}

// listMine 使用相同分页参数规则调用受保护的当前卖家列表。
func (h *productHTTP) listMine(c *gin.Context) {
	request := &productv1.ListProductsRequest{}
	for _, key := range []string{"category_id", "page", "page_size"} {
		values, exists := c.Request.URL.Query()[key]
		if !exists {
			continue
		}
		if len(values) != 1 {
			writeError(c, 400, "InvalidArgument", "分页参数不能重复")
			return
		}
		bits := 32
		if key == "category_id" {
			bits = 64
		}
		value, err := strconv.ParseInt(values[0], 10, bits)
		if err != nil {
			writeError(c, 400, "InvalidArgument", "分页或分类参数无效")
			return
		}
		switch key {
		case "category_id":
			request.CategoryId = value
		case "page":
			request.Page = int32(value)
		case "page_size":
			request.PageSize = int32(value)
		}
	}
	h.call(c, 200, func(ctx context.Context) (proto.Message, error) { return h.products.ListMyProducts(ctx, request) })
}

// detail 由 Product Service 统一判断商品的公开或所有者可见性。
func (h *productHTTP) detail(c *gin.Context) {
	id, ok := productID(c)
	if !ok {
		return
	}
	h.call(c, 200, func(ctx context.Context) (proto.Message, error) {
		return h.products.GetProduct(ctx, &productv1.GetProductRequest{Id: id})
	})
}

// categories 查询启用分类，供创建和编辑选择。
func (h *productHTTP) categories(c *gin.Context) {
	h.call(c, 200, func(ctx context.Context) (proto.Message, error) {
		return h.products.ListCategories(ctx, &productv1.ListCategoriesRequest{})
	})
}

// update 保留 optional 字段存在性；大整数以字符串输入，避免前端精度丢失。
func (h *productHTTP) update(c *gin.Context) {
	id, ok := productID(c)
	if !ok {
		return
	}
	var input struct {
		ExpectedVersion int64   `json:"expected_version,string"`
		CategoryID      *int64  `json:"category_id,string"`
		Title           *string `json:"title"`
		Description     *string `json:"description"`
		PriceCent       *int64  `json:"price_cent,string"`
	}
	if !decodeJSON(c, &input) {
		return
	}
	h.call(c, 200, func(ctx context.Context) (proto.Message, error) {
		return h.products.UpdateProduct(ctx, &productv1.UpdateProductRequest{Id: id, ExpectedVersion: input.ExpectedVersion, CategoryId: input.CategoryID, Title: input.Title, Description: input.Description, PriceCent: input.PriceCent})
	})
}

// stateChange 将发布及下架请求转换为携带预期版本的 RPC。
func (h *productHTTP) stateChange(c *gin.Context, publish bool) {
	id, ok := productID(c)
	if !ok {
		return
	}
	var input struct {
		ExpectedVersion int64 `json:"expected_version,string"`
	}
	if !decodeJSON(c, &input) {
		return
	}
	h.call(c, http.StatusOK, func(ctx context.Context) (proto.Message, error) {
		req := &productv1.ChangeProductStateRequest{Id: id, ExpectedVersion: input.ExpectedVersion}
		if publish {
			return h.products.PublishProduct(ctx, req)
		}
		return h.products.OffShelfProduct(ctx, req)
	})
}

// publish 发布草稿或重新上架，事件由商品服务事务产生。
func (h *productHTTP) publish(c *gin.Context) { h.stateChange(c, true) }

// offShelf 下架商品，管理员权限仍由 Product 调用 User Service 核实。
func (h *productHTTP) offShelf(c *gin.Context) { h.stateChange(c, false) }
