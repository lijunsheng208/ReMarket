package handler

import (
	"context"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/product/model"
	"remarket/backend/internal/product/service"
	"remarket/backend/pkg/apperror"
)

type Mutator interface {
	Update(context.Context, service.UpdateInput) (*model.Product, error)
	OffShelf(context.Context, int64, int64) (*model.Product, error)
}

// NewComplete 组装当前商品读写能力，不包含订单预占业务。
func NewComplete(drafts DraftCreator, publisher Publisher, reader ProductReader, mutator Mutator) *Server {
	return &Server{products: drafts, publisher: publisher, reader: reader, mutator: mutator}
}

// UpdateProduct 保留 optional 字段的存在性，空字符串与未提供字段具有不同语义。
func (s *Server) UpdateProduct(ctx context.Context, req *productv1.UpdateProductRequest) (*productv1.ProductResponse, error) {
	if s.mutator == nil {
		return s.UnimplementedProductServiceServer.UpdateProduct(ctx, req)
	}
	if req == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "编辑请求不能为空"}
	}
	product, err := s.mutator.Update(ctx, service.UpdateInput{ID: req.Id, ExpectedVersion: req.ExpectedVersion, CategoryID: req.CategoryId, Title: req.Title, Description: req.Description, PriceCent: req.PriceCent})
	if err != nil {
		return nil, err
	}
	return &productv1.ProductResponse{Product: productProto(product)}, nil
}

// OffShelfProduct 转发下架请求，所有权及当前管理员校验由业务层负责。
func (s *Server) OffShelfProduct(ctx context.Context, req *productv1.ChangeProductStateRequest) (*productv1.ProductResponse, error) {
	if s.mutator == nil {
		return s.UnimplementedProductServiceServer.OffShelfProduct(ctx, req)
	}
	if req == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "下架请求不能为空"}
	}
	product, err := s.mutator.OffShelf(ctx, req.Id, req.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	return &productv1.ProductResponse{Product: productProto(product)}, nil
}
