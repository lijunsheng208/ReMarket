package repository

import (
	"context"
	"database/sql"
	"errors"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"remarket/backend/internal/product/model"
	"remarket/backend/pkg/apperror"
	"time"
)

// WithReserve 先锁商品再查预占，以条件更新、预占插入和 Outbox 写入组成同一事务。
// READ COMMITTED 避免不同商品争用同一新订单键时的缺失记录间隙锁，唯一键负责最终仲裁。
func (r *ProductRepository) WithReserve(ctx context.Context, productID, orderID int64, change func(*model.Product, *model.ProductReservation, time.Time) (*model.ProductReservation, *model.OutboxEvent, error)) (*model.ProductReservation, error) {
	var result *model.ProductReservation
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var product model.Product
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", productID).Take(&product).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return &apperror.Error{Kind: apperror.NotFound, Message: "商品不存在"}
			}
			return err
		}
		var existing model.ProductReservation
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_id = ?", orderID).Take(&existing).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var previous *model.ProductReservation
		if err == nil {
			previous = &existing
		}
		// 取得锁后才取数据库时间，避免等锁期间越过期限仍预占成功。
		var now time.Time
		if err := tx.Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&now); err != nil {
			return err
		}
		version := product.AggregateVersion
		var event *model.OutboxEvent
		result, event, err = change(&product, previous, now)
		if err != nil {
			return err
		}
		if event == nil {
			return nil
		}
		updated := tx.Model(&model.Product{}).Where("id = ? AND status = ? AND aggregate_version = ? AND seller_id = ? AND price_cent = ?", productID, model.StatusOnSale, version, product.SellerID, product.PriceCent).Updates(map[string]any{"status": product.Status, "aggregate_version": product.AggregateVersion, "updated_at": product.UpdatedAt})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return errors.New("商品条件预占失败")
		}
		if err := tx.Create(result).Error; err != nil {
			return err
		}
		if err := tx.Create(event).Error; err != nil {
			return err
		}
		return enqueueInvalidation(tx, product.ID, product.AggregateVersion)
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	var duplicate *driver.MySQLError
	if errors.Is(err, gorm.ErrDuplicatedKey) || (errors.As(err, &duplicate) && duplicate.Number == 1062) {
		// 同订单跨商品竞争可能在插入时失败；回滚后读取赢家，复用业务回调比较参数。
		existing, readErr := r.GetReservation(ctx, orderID)
		if readErr != nil {
			return nil, err
		}
		result, _, err = change(nil, existing, time.Time{})
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}
