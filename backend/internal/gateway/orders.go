package gateway

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	orderv1 "remarket/backend/gen/order/v1"
	"remarket/backend/pkg/auth"
)

type orderHTTP struct {
	orders  orderv1.OrderServiceClient
	timeout time.Duration
}

// AddOrderRoutes 挂载创建、详情和买卖列表，所有入口强制验签并转发原用户凭据。
func AddOrderRoutes(router *gin.Engine, orders orderv1.OrderServiceClient, verifier auth.TokenVerifier, timeout time.Duration) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	h := &orderHTTP{orders: orders, timeout: timeout}
	group := router.Group("/api/v1/orders", bearerAuth(verifier))
	group.POST("", h.create)
	group.GET("", h.list)
	group.GET("/:id", h.detail)
	group.POST("/:id/cancel", h.cancel)
	group.POST("/:id/confirm", h.confirm)
}

// create 只从唯一 Idempotency-Key 请求头读取幂等键，不接受请求体中的用户、金额或幂等字段。
func (h *orderHTTP) create(c *gin.Context) {
	keys := c.Request.Header.Values("Idempotency-Key")
	if len(keys) != 1 || len(keys[0]) < 1 || len(keys[0]) > 128 {
		writeError(c, 400, "InvalidArgument", "必须提供唯一且长度为 1～128 的 Idempotency-Key")
		return
	}
	for _, b := range []byte(keys[0]) {
		if b < 33 || b > 126 {
			writeError(c, 400, "InvalidArgument", "幂等键仅允许无空格可打印 ASCII 字符")
			return
		}
	}
	var input struct {
		ProductID int64 `json:"product_id,string"`
	}
	if !decodeJSON(c, &input) {
		return
	}
	if input.ProductID <= 0 {
		writeError(c, 400, "InvalidArgument", "商品 ID 必须为正整数")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	result, err := h.orders.CreateOrder(ctx, &orderv1.CreateOrderRequest{ProductId: input.ProductID, IdempotencyKey: keys[0]})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	if result == nil || result.Order == nil || result.Order.Id <= 0 {
		writeError(c, 500, "Internal", "订单响应无效")
		return
	}
	c.Header("Location", "/api/v1/orders/"+strconv.FormatInt(result.Order.Id, 10))
	code := http.StatusOK
	// 无法区分首次与重放，不使用 201；PENDING 只表示已接受，客户端应查询订单结果。
	if result.Order.Status == orderv1.OrderStatus_ORDER_STATUS_PENDING {
		code = http.StatusAccepted
	}
	writeProto(c, code, result)
}

// detail 校验有符号 BIGINT 路径，归属授权由 Order 根据独立验签身份执行。
func (h *orderHTTP) detail(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, 400, "InvalidArgument", "订单 ID 无效")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	result, err := h.orders.GetOrder(ctx, &orderv1.GetOrderRequest{Id: id})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	writeProto(c, http.StatusOK, result)
}

// list 严格校验角色、重复及未知查询参数，不能指定其他用户读取订单。
func (h *orderHTTP) list(c *gin.Context) {
	query := c.Request.URL.Query()
	request := &orderv1.ListOrdersRequest{}
	for key, values := range query {
		if len(values) != 1 {
			writeError(c, 400, "InvalidArgument", "查询参数不能重复")
			return
		}
		switch key {
		case "role":
			switch values[0] {
			case "buyer":
				request.Role = orderv1.OrderRole_ORDER_ROLE_BUYER
			case "seller":
				request.Role = orderv1.OrderRole_ORDER_ROLE_SELLER
			default:
				writeError(c, 400, "InvalidArgument", "请选择 buyer 或 seller")
				return
			}
		case "page", "page_size":
			value, err := strconv.ParseInt(values[0], 10, 32)
			if err != nil || value < 1 || (key == "page_size" && value > 100) {
				writeError(c, 400, "InvalidArgument", "分页参数无效")
				return
			}
			if key == "page" {
				request.Page = int32(value)
			} else {
				request.PageSize = int32(value)
			}
		default:
			writeError(c, 400, "InvalidArgument", "不支持的查询参数")
			return
		}
	}
	if request.Role == orderv1.OrderRole_ORDER_ROLE_UNSPECIFIED {
		writeError(c, 400, "InvalidArgument", "必须选择 buyer 或 seller")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	result, err := h.orders.ListOrders(ctx, request)
	if err != nil {
		writeRPCError(c, err)
		return
	}
	writeProto(c, http.StatusOK, result)
}

// cancel 持久化取消决定，处理中返回 202；重复请求保留真实状态。
func (h *orderHTTP) cancel(c *gin.Context) { h.decision(c, false) }

// confirm 记录当前用户确认，双方确认后处理中返回 202。
func (h *orderHTTP) confirm(c *gin.Context) { h.decision(c, true) }

// decision 校验路径及空请求体，避免静默接受客户端伪造的参与者或状态字段。
func (h *orderHTTP) decision(c *gin.Context, confirm bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, 400, "InvalidArgument", "订单 ID 无效")
		return
	}
	// 此接口不需要业务参数；拒绝请求体，不解析或传递用户声明。
	body := http.MaxBytesReader(c.Writer, c.Request.Body, 1)
	defer body.Close()
	var b [1]byte
	if n, err := body.Read(b[:]); n > 0 || (err != nil && err != io.EOF) {
		writeError(c, 400, "InvalidArgument", "此接口不接受请求体")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()
	var result *orderv1.OrderResponse
	if confirm {
		result, err = h.orders.ConfirmOrder(ctx, &orderv1.ConfirmOrderRequest{Id: id})
	} else {
		result, err = h.orders.CancelOrder(ctx, &orderv1.CancelOrderRequest{Id: id})
	}
	if err != nil {
		writeRPCError(c, err)
		return
	}
	if result == nil || result.Order == nil {
		writeError(c, 500, "Internal", "订单响应无效")
		return
	}
	code := http.StatusOK
	if result.Order.Status == orderv1.OrderStatus_ORDER_STATUS_CONFIRMING || result.Order.Status == orderv1.OrderStatus_ORDER_STATUS_CANCELLING {
		code = http.StatusAccepted
	}
	c.Header("Location", "/api/v1/orders/"+strconv.FormatInt(result.Order.Id, 10))
	writeProto(c, code, result)
}
