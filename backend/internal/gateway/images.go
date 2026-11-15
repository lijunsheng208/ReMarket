package gateway

import (
	"context"
	"github.com/gin-gonic/gin"
	"google.golang.org/protobuf/proto"
	productv1 "remarket/backend/gen/product/v1"
	"time"
)

// prepareImage 为当前商品申请直传凭证，不接受用户自定义对象键。
func (h *productHTTP) prepareImage(c *gin.Context) {
	id, ok := productID(c)
	if !ok {
		return
	}
	var body struct {
		ContentType string `json:"content_type"`
		SizeBytes   int64  `json:"size_bytes"`
	}
	if !decodeJSON(c, &body) {
		return
	}
	h.call(c, 200, func(ctx context.Context) (proto.Message, error) {
		return h.products.PrepareImageUpload(ctx, &productv1.PrepareImageUploadRequest{ProductId: id, ContentType: body.ContentType, SizeBytes: body.SizeBytes})
	})
}

// completeImage 要求原上传用户确认，由服务端重新读取并校验 OSS 内容。
func (h *productHTTP) completeImage(c *gin.Context) {
	id, ok := productID(c)
	if !ok {
		return
	}
	var body struct {
		UploadID string `json:"upload_id"`
	}
	if !decodeJSON(c, &body) {
		return
	}
	h.imageCall(c, func(ctx context.Context) (proto.Message, error) {
		return h.products.CompleteImageUpload(ctx, &productv1.CompleteImageUploadRequest{ProductId: id, UploadId: body.UploadID})
	})
}

// images 获取当前可见商品的短期图片地址，不能通过猜测对象键访问私有商品。
func (h *productHTTP) images(c *gin.Context) {
	id, ok := productID(c)
	if !ok {
		return
	}
	h.call(c, 200, func(ctx context.Context) (proto.Message, error) {
		return h.products.GetProductImages(ctx, &productv1.GetProductRequest{Id: id})
	})
}

// imageCall 给云端下载、重编码及正式对象写入独立预算，但不延长上游截止时间。
func (h *productHTTP) imageCall(c *gin.Context, invoke func(context.Context) (proto.Message, error)) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	result, err := invoke(ctx)
	if err != nil {
		writeRPCError(c, err)
		return
	}
	writeProto(c, 200, result)
}
