-- 预占重试必须复用首次商品版本，不能重新查询后改写。
-- 历史订单缺少可信版本，保留 NULL；恢复执行器须拒绝猜测版本。
ALTER TABLE orders
  ADD COLUMN expected_product_version BIGINT NULL,
  ADD CONSTRAINT chk_orders_product_version CHECK (expected_product_version IS NULL OR expected_product_version > 0);
