// Package repository 提供 Order 自有数据的持久化访问，不跨库读取商品或用户。
package repository

import (
	"context"
	"database/sql"
	"errors"
	"gorm.io/gorm"
	"remarket/backend/internal/order/model"
	"remarket/backend/pkg/apperror"
)

type Repository struct{ db *gorm.DB }

// New 创建订单 Repository，不执行自动迁移。
func New(db *gorm.DB) *Repository { return &Repository{db: db} }

// GetForParticipant 在 SQL 中约束买卖双方，非参与人与不存在统一返回 NotFound。
func (r *Repository) GetForParticipant(ctx context.Context, id, userID int64) (*model.Order, error) {
	var result model.Order
	err := r.db.WithContext(ctx).Where("id = ? AND (buyer_id = ? OR seller_id = ?)", id, userID, userID).Take(&result).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, &apperror.Error{Kind: apperror.NotFound, Message: "订单不存在"}
	}
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// ListForParticipant 使用同一只读快照查询列表与总数，稳定按创建时间及 ID 倒序。
func (r *Repository) ListForParticipant(ctx context.Context, userID int64, asSeller bool, offset, limit int) ([]model.Order, int64, error) {
	result := make([]model.Order, 0)
	var total int64
	// 列名只由 Service 的布尔角色决定，不拼接用户输入。
	column := "buyer_id"
	if asSeller {
		column = "seller_id"
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&model.Order{}).Where(column+" = ?", userID)
		if err := query.Count(&total).Error; err != nil {
			return err
		}
		return query.Order("created_at DESC, id DESC").Offset(offset).Limit(limit).Find(&result).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return result, total, err
}
