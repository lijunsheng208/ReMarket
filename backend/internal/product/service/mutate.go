package service

import (
	"context"
	"math"
	"remarket/backend/internal/product/events"
	"remarket/backend/internal/product/model"
	"remarket/backend/pkg/apperror"
	"remarket/backend/pkg/auth"
	"strings"
	"time"
	"unicode/utf8"
)

type MutationRepository interface {
	WithMutation(context.Context, int64, int64, func(*model.Product, *model.Category) (*model.OutboxEvent, bool, error)) (*model.Product, error)
}
type MutationService struct {
	products MutationRepository
	admins   AdminVerifier
}
type UpdateInput struct {
	ID, ExpectedVersion int64
	CategoryID          *int64
	Title, Description  *string
	PriceCent           *int64
}

// NewMutation 创建编辑及下架业务，管理员权限通过实时核实适配器注入。
func NewMutation(products MutationRepository, admins AdminVerifier) *MutationService {
	return &MutationService{products: products, admins: admins}
}

// mutationIdentity 校验身份、商品 ID 和预期版本，保持修改入口一致。
func mutationIdentity(ctx context.Context, id, version int64) (auth.Identity, error) {
	if err := ctx.Err(); err != nil {
		return auth.Identity{}, err
	}
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok {
		return identity, &apperror.Error{Kind: apperror.Unauthenticated, Message: "请先登录"}
	}
	if id <= 0 || version <= 0 {
		return identity, &apperror.Error{Kind: apperror.Invalid, Message: "商品 ID 和版本必须为正整数"}
	}
	return identity, nil
}

// checkVersion 拒绝旧版本，避免并发覆盖；空操作也必须满足当前版本。
func checkVersion(p *model.Product, version int64) error {
	if p.AggregateVersion != version {
		return &apperror.Error{Kind: apperror.FailedPrecondition, Message: "商品已变化，请刷新后重试"}
	}
	return nil
}

// bumpVersion 为实际变更增加版本与更新时间，防止 int64 溢出。
func bumpVersion(p *model.Product) error {
	if p.AggregateVersion == math.MaxInt64 {
		return &apperror.Error{Kind: apperror.FailedPrecondition, Message: "商品版本已达上限"}
	}
	p.AggregateVersion++
	p.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
	return nil
}

// Update 仅允许卖家编辑可编辑状态；草稿/下架编辑不发事件，在售编辑写 updated 事件。
func (s *MutationService) Update(ctx context.Context, input UpdateInput) (*model.Product, error) {
	identity, err := mutationIdentity(ctx, input.ID, input.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	if input.CategoryID == nil && input.Title == nil && input.Description == nil && input.PriceCent == nil {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "请至少提供一个编辑字段"}
	}
	target := int64(0)
	if input.CategoryID != nil {
		target = *input.CategoryID
		if target <= 0 {
			return nil, &apperror.Error{Kind: apperror.Invalid, Message: "分类 ID 必须为正整数"}
		}
	}
	var title string
	if input.Title != nil {
		title = strings.TrimSpace(*input.Title)
		if !utf8.ValidString(title) || title == "" || utf8.RuneCountInString(title) > 128 {
			return nil, &apperror.Error{Kind: apperror.Invalid, Message: "商品标题须为 1 至 128 个字符"}
		}
	}
	if input.Description != nil && (!utf8.ValidString(*input.Description) || len(*input.Description) > 16<<10) {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "商品描述须为有效 UTF-8，且不超过 16 KiB"}
	}
	if input.PriceCent != nil && *input.PriceCent <= 0 {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "商品价格必须为正整数"}
	}
	return s.products.WithMutation(ctx, input.ID, target, func(p *model.Product, c *model.Category) (*model.OutboxEvent, bool, error) {
		if p.SellerID != identity.UserID {
			return nil, false, &apperror.Error{Kind: apperror.Forbidden, Message: "只能编辑自己的商品"}
		}
		if err := checkVersion(p, input.ExpectedVersion); err != nil {
			return nil, false, err
		}
		if p.Status != model.StatusDraft && p.Status != model.StatusOnSale && p.Status != model.StatusOffShelf {
			return nil, false, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "当前商品状态不允许编辑"}
		}
		if !c.Enabled {
			return nil, false, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "商品分类已停用"}
		}
		before := *p
		if input.CategoryID != nil {
			p.CategoryID = *input.CategoryID
		}
		if input.Title != nil {
			p.Title = title
		}
		if input.Description != nil {
			p.Description = *input.Description
		}
		if input.PriceCent != nil {
			p.PriceCent = *input.PriceCent
		}
		if before.CategoryID == p.CategoryID && before.Title == p.Title && before.Description == p.Description && before.PriceCent == p.PriceCent {
			return nil, false, nil
		}
		if err := bumpVersion(p); err != nil {
			return nil, false, err
		}
		if p.Status == model.StatusOnSale {
			event, err := events.NewUpdatedOutbox(*p, p.UpdatedAt)
			return event, true, err
		}
		return nil, true, nil
	})
}

// OffShelf 允许卖家或经核实的管理员下架在售商品，不允许下架预占或成交商品。
func (s *MutationService) OffShelf(ctx context.Context, id, version int64) (*model.Product, error) {
	identity, err := mutationIdentity(ctx, id, version)
	if err != nil {
		return nil, err
	}
	// 外部 RPC 不在持有商品锁期间执行；管理员核实失败时不降级放行。
	admin := false
	if identity.Role == "ADMIN" && s.admins != nil {
		admin, err = s.admins.IsCurrentAdmin(ctx)
		if err != nil {
			return nil, err
		}
	}
	return s.products.WithMutation(ctx, id, 0, func(p *model.Product, c *model.Category) (*model.OutboxEvent, bool, error) {
		if p.SellerID != identity.UserID && !admin {
			return nil, false, &apperror.Error{Kind: apperror.Forbidden, Message: "无权下架该商品"}
		}
		if err := checkVersion(p, version); err != nil {
			return nil, false, err
		}
		if p.Status == model.StatusOffShelf {
			return nil, false, nil
		}
		if p.Status != model.StatusOnSale {
			return nil, false, &apperror.Error{Kind: apperror.FailedPrecondition, Message: "当前商品状态不允许下架"}
		}
		p.Status = model.StatusOffShelf
		if err := bumpVersion(p); err != nil {
			return nil, false, err
		}
		event, err := events.NewOffShelfOutbox(*p, p.UpdatedAt)
		return event, true, err
	})
}
