package service

import (
	"context"
	"remarket/backend/internal/product/model"
	"remarket/backend/internal/product/repository"
	"remarket/backend/pkg/apperror"
	"remarket/backend/pkg/auth"
)

type QueryRepository interface {
	ListBySeller(context.Context, int64, int64, int, int) ([]model.Product, int64, error)
	GetByID(context.Context, int64) (*model.Product, error)
	ListOnSale(context.Context, int64, int, int) ([]model.Product, int64, error)
	ListImages(context.Context, int64) ([]model.ProductImage, error)
}
type AdminVerifier interface {
	IsCurrentAdmin(context.Context) (bool, error)
}
type QueryService struct {
	products QueryRepository
	admins   AdminVerifier
}
type ProductDetail struct {
	Product *model.Product
	Images  []model.ProductImage
}

// NewQuery 创建查询服务，管理员可见性由 User Service 的最新身份核实。
func NewQuery(products QueryRepository, admins AdminVerifier) *QueryService {
	return &QueryService{products: products, admins: admins}
}

// List 校验分页及分类，仅查询在售商品；同一排序仍不保证跨请求翻页快照不变化。
func (s *QueryService) List(ctx context.Context, categoryID int64, page, pageSize int32) ([]model.Product, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if categoryID < 0 || page < 0 || pageSize < 0 || pageSize > 100 {
		return nil, 0, &apperror.Error{Kind: apperror.Invalid, Message: "分页或分类参数无效"}
	}
	if page == 0 {
		page = 1
	}
	if pageSize == 0 {
		pageSize = 20
	}
	offset := int64(page-1) * int64(pageSize)
	// GORM Offset 接受 int，显式检查平台转换，避免溢出退化成无偏移查询。
	if int64(int(offset)) != offset {
		return nil, 0, &apperror.Error{Kind: apperror.Invalid, Message: "分页偏移过大"}
	}
	return s.products.ListOnSale(ctx, categoryID, int(offset), int(pageSize))
}

// ListMine 从可信身份取得卖家 ID，返回其全部状态商品，包括草稿和已下架。
func (s *QueryService) ListMine(ctx context.Context, categoryID int64, page, pageSize int32) ([]model.Product, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok {
		return nil, 0, &apperror.Error{Kind: apperror.Unauthenticated, Message: "请先登录"}
	}
	if categoryID < 0 || page < 0 || pageSize < 0 || pageSize > 100 {
		return nil, 0, &apperror.Error{Kind: apperror.Invalid, Message: "分页或分类参数无效"}
	}
	if page == 0 {
		page = 1
	}
	if pageSize == 0 {
		pageSize = 20
	}
	offset := int64(page-1) * int64(pageSize)
	// GORM Offset 接受 int，显式检查平台转换，避免溢出退化成无偏移查询。
	if int64(int(offset)) != offset {
		return nil, 0, &apperror.Error{Kind: apperror.Invalid, Message: "分页偏移过大"}
	}
	return s.products.ListBySeller(ctx, identity.UserID, categoryID, int(offset), int(pageSize))
}

// GetDetail 在读取图片前校验可见性，无权查看的非在售商品统一返回不存在。
func (s *QueryService) GetDetail(ctx context.Context, id int64) (*ProductDetail, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "商品 ID 必须为正整数"}
	}
	product, err := s.products.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if product.Status != model.StatusOnSale {
		identity, ok := auth.IdentityFromContext(ctx)
		allowed := ok && identity.UserID == product.SellerID
		// Token 中 ADMIN 只是候选身份，必须向 User Service 核实最新角色及账号状态。
		if !allowed && ok && identity.Role == "ADMIN" && s.admins != nil {
			allowed, err = s.admins.IsCurrentAdmin(ctx)
			if err != nil {
				return nil, err
			}
		}
		if !allowed {
			return nil, repository.ErrProductNotFound
		}
	}
	images, err := s.products.ListImages(ctx, id)
	if err != nil {
		return nil, err
	}
	return &ProductDetail{Product: product, Images: images}, nil
}
