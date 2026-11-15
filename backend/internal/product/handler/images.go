package handler

import (
	"context"
	"google.golang.org/protobuf/types/known/timestamppb"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/product/service"
	"time"
)

// WithImages 注入图片上传及只读访问能力。
func (s *Server) WithImages(images *service.Images) *Server { s.images = images; return s }

// PrepareImageUpload 适配上传声明，身份由商品服务独立验签。
func (s *Server) PrepareImageUpload(ctx context.Context, r *productv1.PrepareImageUploadRequest) (*productv1.PrepareImageUploadResponse, error) {
	if s.images == nil {
		return s.UnimplementedProductServiceServer.PrepareImageUpload(ctx, r)
	}
	u, url, err := s.images.Prepare(ctx, r.GetProductId(), r.GetSizeBytes(), r.GetContentType())
	if err != nil {
		return nil, err
	}
	return &productv1.PrepareImageUploadResponse{UploadId: u.ID, UploadUrl: url, ExpiresAt: timestamppb.New(u.ExpiresAt)}, nil
}

// CompleteImageUpload 返回完成事务后的最新版本及图片记录。
func (s *Server) CompleteImageUpload(ctx context.Context, r *productv1.CompleteImageUploadRequest) (*productv1.ProductResponse, error) {
	if s.images == nil {
		return s.UnimplementedProductServiceServer.CompleteImageUpload(ctx, r)
	}
	detail, err := s.images.Complete(ctx, r.GetProductId(), r.GetUploadId())
	if err != nil {
		return nil, err
	}
	p := productProto(detail.Product)
	for _, im := range detail.Images {
		p.Images = append(p.Images, &productv1.ProductImage{Id: im.ID, ObjectKey: im.ObjectKey, SortOrder: im.SortOrder})
	}
	return &productv1.ProductResponse{Product: p}, nil
}

// GetProductImages 仅返回当前调用者可见商品的短期只读地址。
func (s *Server) GetProductImages(ctx context.Context, r *productv1.GetProductRequest) (*productv1.GetProductImagesResponse, error) {
	if s.images == nil {
		return s.UnimplementedProductServiceServer.GetProductImages(ctx, r)
	}
	images, err := s.images.Access(ctx, r.GetId())
	if err != nil {
		return nil, err
	}
	out := &productv1.GetProductImagesResponse{ExpiresAt: timestamppb.New(time.Now().Add(5 * time.Minute))}
	for _, im := range images {
		out.Images = append(out.Images, &productv1.ImageAccess{Id: im.ID, Url: im.URL, SortOrder: im.SortOrder})
	}
	return out, nil
}
