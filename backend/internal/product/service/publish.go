package service

import (
	"context"
	"math"
	"remarket/backend/internal/product/events"
	"remarket/backend/internal/product/model"
	"remarket/backend/pkg/apperror"
	"remarket/backend/pkg/auth"
	"time"
)

type PublicationRepository interface {
	WithPublication(context.Context, int64, func(*model.Product, *model.Category) (*model.OutboxEvent, error)) (*model.Product, error)
}
type PublishService struct{ products PublicationRepository }

// NewPublisher 创建发布业务，事务及持久化通过 Repository 注入。
func NewPublisher(products PublicationRepository) *PublishService {
	return &PublishService{products: products}
}

// Publish 校验所有权、版本和状态，在事务中完成首次上架及发布事件记录。
func (s *PublishService) Publish(ctx context.Context, id, expectedVersion int64) (*model.Product, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok {
		return nil, &apperror.Error{Kind: apperror.Unauthenticated, Message: "请先登录"}
	}
	if id <= 0 || expectedVersion <= 0 {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "商品 ID 和版本必须为正整数"}
	}
	return s.products.WithPublication(ctx, id, func(product *model.Product, category *model.Category) (*model.OutboxEvent, error) {
		if product.SellerID != identity.UserID {
			return nil, &apperror.Error{Kind: apperror.Forbidden, Message: "只能发布自己的商品"}
		}
		if product.AggregateVersion != expectedVersion {
			return nil, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "商品已变化，请刷新后重试"}
		}
		// 相同当前版本重复发布不加版本、不重复生成首次发布事件。
		if product.Status == model.StatusOnSale {
			return nil, nil
		}
		if product.Status != model.StatusDraft || product.FirstPublishedAt != nil {
			return nil, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "当前商品状态不允许首次发布"}
		}
		if !category.Enabled {
			return nil, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "商品分类已停用"}
		}
		if product.AggregateVersion == math.MaxInt64 {
			return nil, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "商品版本已达上限"}
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		product.Status = model.StatusOnSale
		product.AggregateVersion++
		product.FirstPublishedAt = &now
		product.UpdatedAt = now
		// 构造器再次检查快照合法性；历史首次发布判断已由上方状态规则完成。
		return events.NewPublishedOutbox(*product, now)
	})
}
