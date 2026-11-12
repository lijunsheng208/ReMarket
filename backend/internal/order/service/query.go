package service

import (
	"context"
	"remarket/backend/internal/order/model"
	"remarket/backend/pkg/apperror"
	"remarket/backend/pkg/auth"
)

type QueryRepository interface {
	GetForParticipant(context.Context, int64, int64) (*model.Order, error)
	ListForParticipant(context.Context, int64, bool, int, int) ([]model.Order, int64, error)
}
type QueryService struct{ orders QueryRepository }

// NewQuery 注入订单查询持久化，不依赖 RPC 类型。
func NewQuery(orders QueryRepository) *QueryService { return &QueryService{orders: orders} }

// Get 仅按可信当前用户查询参与的订单，不开放管理员越权读取。
func (s *QueryService) Get(ctx context.Context, id int64) (*model.Order, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok {
		return nil, &apperror.Error{Kind: apperror.Unauthenticated, Message: "请先登录"}
	}
	if id <= 0 {
		return nil, &apperror.Error{Kind: apperror.Invalid, Message: "订单 ID 必须为正整数"}
	}
	return s.orders.GetForParticipant(ctx, id, identity.UserID)
}

// List 按买家或卖家角色查询当前用户订单，限制分页且避免偏移计算溢出。
func (s *QueryService) List(ctx context.Context, role string, page, pageSize int32) ([]model.Order, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok {
		return nil, 0, &apperror.Error{Kind: apperror.Unauthenticated, Message: "请先登录"}
	}
	if role != "buyer" && role != "seller" {
		return nil, 0, &apperror.Error{Kind: apperror.Invalid, Message: "必须选择买家或卖家角色"}
	}
	if page == 0 {
		page = 1
	}
	if pageSize == 0 {
		pageSize = 20
	}
	if page < 1 || pageSize < 1 || pageSize > 100 || int64(page-1)*int64(pageSize) > 2147483647 {
		return nil, 0, &apperror.Error{Kind: apperror.Invalid, Message: "分页参数无效"}
	}
	return s.orders.ListForParticipant(ctx, identity.UserID, role == "seller", int(page-1)*int(pageSize), int(pageSize))
}
