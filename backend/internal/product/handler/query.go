package handler

import (
	"context"
	"google.golang.org/protobuf/types/known/timestamppb"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/product/model"
	"remarket/backend/internal/product/service"
)

type ProductReader interface {
	List(context.Context, int64, int32, int32) ([]model.Product, int64, error)
	GetDetail(context.Context, int64) (*service.ProductDetail, error)
}

// NewWithQuery 组装草稿、发布和读取能力，编辑及下架暂未实现。
func NewWithQuery(drafts DraftCreator, publisher Publisher, reader ProductReader) *Server {
	return &Server{products: drafts, publisher: publisher, reader: reader}
}

// ListProducts 返回公开在售列表，分页规则交由 Service 校验。
func (s *Server) ListProducts(ctx context.Context, req *productv1.ListProductsRequest) (*productv1.ListProductsResponse, error) {
	if s.reader == nil {
		return s.UnimplementedProductServiceServer.ListProducts(ctx, req)
	}
	products, total, err := s.reader.List(ctx, req.GetCategoryId(), req.GetPage(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	result := &productv1.ListProductsResponse{Total: total, Products: make([]*productv1.Product, 0, len(products))}
	for i := range products {
		result.Products = append(result.Products, productProto(&products[i]))
	}
	return result, nil
}

// GetProduct 返回经过可见性校验的商品及按序图片，不能通过传入卖家字段绕过权限。
func (s *Server) GetProduct(ctx context.Context, req *productv1.GetProductRequest) (*productv1.ProductResponse, error) {
	if s.reader == nil {
		return s.UnimplementedProductServiceServer.GetProduct(ctx, req)
	}
	detail, err := s.reader.GetDetail(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	result := productProto(detail.Product)
	for _, image := range detail.Images {
		result.Images = append(result.Images, &productv1.ProductImage{Id: image.ID, ObjectKey: image.ObjectKey, SortOrder: image.SortOrder})
	}
	return &productv1.ProductResponse{Product: result}, nil
}

// productProto 显式转换数据库字符串状态，避免枚举数值强制转换造成状态误报。
func productProto(p *model.Product) *productv1.Product {
	state := map[model.ProductStatus]productv1.ProductStatus{model.StatusDraft: productv1.ProductStatus_PRODUCT_STATUS_DRAFT, model.StatusOnSale: productv1.ProductStatus_PRODUCT_STATUS_ON_SALE, model.StatusReserved: productv1.ProductStatus_PRODUCT_STATUS_RESERVED, model.StatusSold: productv1.ProductStatus_PRODUCT_STATUS_SOLD, model.StatusOffShelf: productv1.ProductStatus_PRODUCT_STATUS_OFF_SHELF}[p.Status]
	result := &productv1.Product{Id: p.ID, SellerId: p.SellerID, CategoryId: p.CategoryID, Title: p.Title, Description: p.Description, PriceCent: p.PriceCent, Status: state, AggregateVersion: p.AggregateVersion, CreatedAt: timestamppb.New(p.CreatedAt), UpdatedAt: timestamppb.New(p.UpdatedAt)}
	if p.FirstPublishedAt != nil {
		result.FirstPublishedAt = timestamppb.New(*p.FirstPublishedAt)
	}
	return result
}

// OwnProductReader 定义当前卖家分页业务。
type OwnProductReader interface {
	ListMine(context.Context, int64, int32, int32) ([]model.Product, int64, error)
}

// WithOwnList 注入当前卖家分页能力，默认要求商品服务鉴权。
func (s *Server) WithOwnList(reader OwnProductReader) *Server { s.ownReader = reader; return s }

// ListMyProducts 从 Context 获取身份，忽略客户端提供的任何卖家头。
func (s *Server) ListMyProducts(ctx context.Context, req *productv1.ListProductsRequest) (*productv1.ListProductsResponse, error) {
	if s.ownReader == nil {
		return s.UnimplementedProductServiceServer.ListMyProducts(ctx, req)
	}
	products, total, err := s.ownReader.ListMine(ctx, req.GetCategoryId(), req.GetPage(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	result := &productv1.ListProductsResponse{Total: total, Products: make([]*productv1.Product, 0, len(products))}
	for i := range products {
		result.Products = append(result.Products, productProto(&products[i]))
	}
	return result, nil
}
