package repository

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"remarket/backend/internal/product/model"
	"remarket/backend/pkg/apperror"
)

// CreateImageUpload 在商品锁内执行所有权及状态校验，保存受限上传会话。
func (r *ProductRepository) CreateImageUpload(ctx context.Context, u *model.ImageUpload, check func(*model.Product) error) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var p model.Product
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", u.ProductID).Take(&p).Error; err != nil {
			return mapImageRead(err)
		}
		if err := check(&p); err != nil {
			return err
		}
		return tx.Create(u).Error
	})
}

// mapImageRead 不向外暴露数据库读取细节。
func mapImageRead(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &apperror.Error{Kind: apperror.NotFound, Message: "商品或上传会话不存在"}
	}
	return err
}

// GetImageUpload 使用不透明会话 ID 查询，不允许客户端指定对象键。
func (r *ProductRepository) GetImageUpload(ctx context.Context, id string) (*model.ImageUpload, error) {
	var u model.ImageUpload
	if err := r.db.WithContext(ctx).Where("id = ?", id).Take(&u).Error; err != nil {
		return nil, mapImageRead(err)
	}
	return &u, nil
}

// CommitImage 固定先锁商品再锁会话，图片、商品版本、Outbox 和缓存失效任务同事务提交。
func (r *ProductRepository) CommitImage(ctx context.Context, u *model.ImageUpload, image *model.ProductImage, change func(*model.Product, *model.ImageUpload, int64) (*model.OutboxEvent, error)) (*model.Product, string, error) {
	var p model.Product
	key := ""
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", u.ProductID).Take(&p).Error; err != nil {
			return mapImageRead(err)
		}
		var current model.ImageUpload
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", u.ID).Take(&current).Error; err != nil {
			return mapImageRead(err)
		}
		if current.CompletedKey != "" {
			key = current.CompletedKey
			return nil
		}
		var count int64
		if err := tx.Model(&model.ProductImage{}).Where("product_id = ?", p.ID).Count(&count).Error; err != nil {
			return err
		}
		event, err := change(&p, &current, count)
		if err != nil {
			return err
		}
		image.ProductID = p.ID
		image.SortOrder = int32(count)
		if err := tx.Create(image).Error; err != nil {
			return err
		}
		if err := tx.Model(&p).Updates(map[string]any{"aggregate_version": p.AggregateVersion, "updated_at": p.UpdatedAt}).Error; err != nil {
			return err
		}
		if event != nil {
			if err := tx.Create(event).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&current).Update("completed_key", image.ObjectKey).Error; err != nil {
			return err
		}
		key = image.ObjectKey
		return enqueueInvalidation(tx, p.ID, p.AggregateVersion)
	})
	return &p, key, err
}
