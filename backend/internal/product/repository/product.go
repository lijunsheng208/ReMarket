// Package repository 实现 Product 服务的数据访问，不承担权限、发布或状态转换规则。
package repository

import (
	"context"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"remarket/backend/internal/product/model"
	"remarket/backend/pkg/apperror"
)

var (
	ErrProductNotFound   = &apperror.Error{Kind: apperror.NotFound, Message: "商品不存在"}
	ErrCategoryNotFound  = &apperror.Error{Kind: apperror.NotFound, Message: "分类不存在"}
	ErrCategoryReference = &apperror.Error{Kind: apperror.Invalid, Message: "商品分类不存在"}
)

type ProductRepository struct{ db *gorm.DB }

// CreateDraftParams 不允许指定商品主键、状态、版本或首次发布时间。
// SellerID 必须由后续 Service 从可信身份中取得，不能直接信任 HTTP 输入。
type CreateDraftParams struct {
	SellerID, CategoryID int64
	Title, Description   string
	PriceCent            int64
}

// NewProductRepository 注入本服务数据库，连接池生命周期由组装层管理。
func NewProductRepository(db *gorm.DB) *ProductRepository { return &ProductRepository{db: db} }

// CreateDraft 显式写入 DRAFT 和初始版本 1，不产生发布事件。
func (r *ProductRepository) CreateDraft(ctx context.Context, params CreateDraftParams) (*model.Product, error) {
	product := &model.Product{SellerID: params.SellerID, CategoryID: params.CategoryID, Title: params.Title, Description: params.Description, PriceCent: params.PriceCent, Status: model.StatusDraft, AggregateVersion: 1}
	if err := r.db.WithContext(ctx).Create(product).Error; err != nil {
		// 当前商品表的外键仅引用分类；账号引用不跨库建立外键。
		if errors.Is(err, gorm.ErrForeignKeyViolated) {
			return nil, ErrCategoryReference
		}
		return nil, fmt.Errorf("创建商品草稿失败: %w", err)
	}
	return product, nil
}

// GetByID 读取完整商品行，不做可见性过滤；对外返回前必须经过 Service 权限校验。
func (r *ProductRepository) GetByID(ctx context.Context, id int64) (*model.Product, error) {
	var product model.Product
	if err := r.db.WithContext(ctx).Where("id = ?", id).Take(&product).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrProductNotFound
		}
		return nil, fmt.Errorf("查询商品失败: %w", err)
	}
	return &product, nil
}

// GetCategoryByID 返回包含启用状态的分类，是否允许使用由 Service 判断。
func (r *ProductRepository) GetCategoryByID(ctx context.Context, id int64) (*model.Category, error) {
	var category model.Category
	if err := r.db.WithContext(ctx).Where("id = ?", id).Take(&category).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrCategoryNotFound
		}
		return nil, fmt.Errorf("查询分类失败: %w", err)
	}
	return &category, nil
}

// ListEnabledCategories 按排序值和 ID 返回启用分类，空结果返回空切片。
func (r *ProductRepository) ListEnabledCategories(ctx context.Context) ([]model.Category, error) {
	categories := make([]model.Category, 0)
	if err := r.db.WithContext(ctx).Where("enabled = ?", true).Order("sort_order ASC").Order("id ASC").Find(&categories).Error; err != nil {
		return nil, fmt.Errorf("查询分类列表失败: %w", err)
	}
	return categories, nil
}
