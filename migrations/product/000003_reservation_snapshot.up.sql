-- 在已应用 000001/000002 的 remarket_product 执行一次。
-- 历史记录不从当前商品反推下单快照；三项均 NULL 表示历史未知，禁止伪造回填。
ALTER TABLE product_reservations
  ADD COLUMN expected_seller_id BIGINT NULL,
  ADD COLUMN expected_price_cent BIGINT NULL,
  ADD COLUMN expected_product_version BIGINT NULL,
  ADD CONSTRAINT chk_reservations_snapshot CHECK (
    (expected_seller_id IS NULL AND expected_price_cent IS NULL AND expected_product_version IS NULL) OR
    (expected_seller_id IS NOT NULL AND expected_seller_id > 0
     AND expected_seller_id <> buyer_id
     AND expected_price_cent IS NOT NULL AND expected_price_cent > 0
     AND expected_product_version IS NOT NULL AND expected_product_version > 0)
  );
