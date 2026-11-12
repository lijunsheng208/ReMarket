// Package service 实现商品业务规则，不依赖 gRPC 或 Gin。
package service

import (
	"context"
	"errors"
	"remarket/backend/internal/product/model"
	"remarket/backend/internal/product/repository"
	"remarket/backend/pkg/apperror"
	"remarket/backend/pkg/auth"
	"strings"
	"unicode/utf8"
)

type DraftRepository interface {
	GetCategoryByID(context.Context, int64) (*model.Category, error)
	CreateDraft(context.Context, repository.CreateDraftParams) (*model.Product, error)
}
type Service struct{ products DraftRepository }
type CreateDraftInput struct {
	CategoryID         int64
	Title, Description string
	PriceCent          int64
}

// New 创建商品业务服务，仓储接口便于隔离测试身份及参数规则。
func New(products DraftRepository) *Service { return &Service{products: products} }

// CreateDraft 从可信身份获取卖家，校验输入及分类后创建草稿，不执行发布或写 Outbox。
func (s *Service) CreateDraft(ctx context.Context, input CreateDraftInput) (*model.Product, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok {
		return nil, &apperror.Error{Kind: apperror.Unauthenticated, Message: "请先登录"}
	}
	title := strings.TrimSpace(input.Title)
	if !utf8.ValidString(title) || title == "" || utf8.RuneCountInString(title) > 128 {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "商品标题须为 1 至 128 个字符"}
	}
	if len(input.Description) > 16<<10 || !utf8.ValidString(input.Description) {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "商品描述须为有效 UTF-8，且不超过 16 KiB"}
	}
	if input.CategoryID <= 0 || input.PriceCent <= 0 {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "分类 ID 和商品价格必须为正整数"}
	}
	category, err := s.products.GetCategoryByID(ctx, input.CategoryID)
	if errors.Is(err, repository.ErrCategoryNotFound) {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "商品分类不存在"}
	}
	if err != nil {
		return nil, err
	}
	if !category.Enabled {
		return nil, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "商品分类已停用"}
	}
	// 描述保留原样；卖家不能由客户端指定，状态和初始版本由专用仓储创建路径固定。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.products.CreateDraft(ctx, repository.CreateDraftParams{SellerID: identity.UserID, CategoryID: input.CategoryID, Title: title, Description: input.Description, PriceCent: input.PriceCent})
}
