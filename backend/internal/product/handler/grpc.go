// Package handler 将商品 gRPC 请求适配为业务调用。
package handler

import (
	"context"
	"google.golang.org/protobuf/types/known/timestamppb"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/product/model"
	"remarket/backend/internal/product/service"
	"remarket/backend/pkg/apperror"
)

type DraftCreator interface {
	CreateDraft(context.Context, service.CreateDraftInput) (*model.Product, error)
}
type Server struct {
	productv1.UnimplementedProductServiceServer
	products   DraftCreator
	publisher  Publisher
	reader     ProductReader
	mutator    Mutator
	categories CategoryLister
	ownReader  OwnProductReader
}

// New 注入创建草稿业务，其他商品 RPC 保留 Unimplemented。
func New(products DraftCreator) *Server { return &Server{products: products} }

// CreateProduct 创建草稿，错误由通用拦截器统一映射，响应不伪装为在售商品。
func (s *Server) CreateProduct(ctx context.Context, req *productv1.CreateProductRequest) (*productv1.ProductResponse, error) {
	if req == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "创建商品请求不能为空"}
	}
	product, err := s.products.CreateDraft(ctx, service.CreateDraftInput{CategoryID: req.CategoryId, Title: req.Title, Description: req.Description, PriceCent: req.PriceCent})
	if err != nil {
		return nil, err
	}
	return &productv1.ProductResponse{Product: &productv1.Product{Id: product.ID, SellerId: product.SellerID, CategoryId: product.CategoryID, Title: product.Title, Description: product.Description, PriceCent: product.PriceCent, Status: productv1.ProductStatus_PRODUCT_STATUS_DRAFT, AggregateVersion: product.AggregateVersion, CreatedAt: timestamppb.New(product.CreatedAt), UpdatedAt: timestamppb.New(product.UpdatedAt)}}, nil
}

// Publisher 定义发布业务能力，不暴露数据库事务给传输层。
type Publisher interface {
	Publish(context.Context, int64, int64) (*model.Product, error)
}

// NewWithPublisher 同时注入草稿及发布业务，其余商品方法保持未实现。
func NewWithPublisher(products DraftCreator, publisher Publisher) *Server {
	return &Server{products: products, publisher: publisher}
}

// PublishProduct 将商品 ID 和预期版本交给发布业务，返回实际状态及首次发布时间。
func (s *Server) PublishProduct(ctx context.Context, req *productv1.ChangeProductStateRequest) (*productv1.ProductResponse, error) {
	if s.publisher == nil {
		return s.UnimplementedProductServiceServer.PublishProduct(ctx, req)
	}
	if req == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "发布请求不能为空"}
	}
	product, err := s.publisher.Publish(ctx, req.Id, req.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	result := &productv1.Product{Id: product.ID, SellerId: product.SellerID, CategoryId: product.CategoryID, Title: product.Title, Description: product.Description, PriceCent: product.PriceCent, Status: productv1.ProductStatus_PRODUCT_STATUS_ON_SALE, AggregateVersion: product.AggregateVersion, CreatedAt: timestamppb.New(product.CreatedAt), UpdatedAt: timestamppb.New(product.UpdatedAt)}
	if product.FirstPublishedAt != nil {
		result.FirstPublishedAt = timestamppb.New(*product.FirstPublishedAt)
	}
	return &productv1.ProductResponse{Product: result}, nil
}
