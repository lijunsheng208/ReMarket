-- 仅在 remarket_order 执行；本迁移不自动运行，不建立跨服务外键。
-- MySQL 8.4，时间统一 UTC；DDL 非事务性，失败后须检查已执行语句。
CREATE TABLE orders (
  id BIGINT NOT NULL AUTO_INCREMENT,
  buyer_id BIGINT NOT NULL,
  seller_id BIGINT NOT NULL,
  product_id BIGINT NOT NULL,
  price_cent BIGINT NOT NULL,
  status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'PENDING',
  idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  request_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  buyer_confirmed BOOLEAN NOT NULL DEFAULT FALSE,
  seller_confirmed BOOLEAN NOT NULL DEFAULT FALSE,
  aggregate_version BIGINT NOT NULL DEFAULT 1,
  expires_at DATETIME(6) NOT NULL,
  closed_at DATETIME(6) NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  UNIQUE KEY uk_orders_buyer_idempotency (buyer_id, idempotency_key),
  KEY idx_orders_buyer (buyer_id, created_at, id),
  KEY idx_orders_seller (seller_id, created_at, id),
  KEY idx_orders_product (product_id, id),
  KEY idx_orders_expiry (status, expires_at, id),
  CONSTRAINT chk_orders_participants CHECK (buyer_id > 0 AND seller_id > 0 AND buyer_id <> seller_id),
  CONSTRAINT chk_orders_product CHECK (product_id > 0),
  CONSTRAINT chk_orders_price CHECK (price_cent > 0),
  CONSTRAINT chk_orders_version CHECK (aggregate_version > 0),
  CONSTRAINT chk_orders_key CHECK (CHAR_LENGTH(idempotency_key) > 0),
  CONSTRAINT chk_orders_status CHECK (status IN ('PENDING','RESERVED','CONFIRMING','CANCELLING','EXPIRING','COMPLETED','CANCELLED','EXPIRED','FAILED')),
  CONSTRAINT chk_orders_flags CHECK (buyer_confirmed IN (0,1) AND seller_confirmed IN (0,1)),
  CONSTRAINT chk_orders_confirmation CHECK (status NOT IN ('CONFIRMING','COMPLETED') OR (buyer_confirmed = TRUE AND seller_confirmed = TRUE)),
  CONSTRAINT chk_orders_expiry CHECK (expires_at > created_at),
  CONSTRAINT chk_orders_closed CHECK (
    (status IN ('COMPLETED','CANCELLED','EXPIRED','FAILED') AND closed_at IS NOT NULL) OR
    (status NOT IN ('COMPLETED','CANCELLED','EXPIRED','FAILED') AND closed_at IS NULL)
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 操作意图与订单状态同事务写入；RPC 始终在事务提交后执行。
CREATE TABLE order_operations (
  id BIGINT NOT NULL AUTO_INCREMENT,
  operation_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  order_id BIGINT NOT NULL,
  operation_type VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'PENDING',
  retry_count INT NOT NULL DEFAULT 0,
  next_retry_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  locked_until DATETIME(6) NULL,
  lock_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
  last_error VARCHAR(1024) NULL,
  finished_at DATETIME(6) NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  UNIQUE KEY uk_operations_operation (operation_id),
  UNIQUE KEY uk_operations_order_type (order_id, operation_type),
  KEY idx_operations_retry (status, next_retry_at, id),
  KEY idx_operations_lease (status, locked_until, id),
  CONSTRAINT fk_operations_order FOREIGN KEY (order_id) REFERENCES orders(id),
  CONSTRAINT chk_operations_type CHECK (operation_type IN ('RESERVE','RELEASE','CONFIRM')),
  CONSTRAINT chk_operations_status CHECK (status IN ('PENDING','PROCESSING','RETRY','SUCCEEDED','FAILED')),
  CONSTRAINT chk_operations_retry CHECK (retry_count >= 0),
  CONSTRAINT chk_operations_lease CHECK (
    (status = 'PROCESSING' AND locked_until IS NOT NULL AND lock_token IS NOT NULL) OR
    (status <> 'PROCESSING' AND locked_until IS NULL AND lock_token IS NULL)
  ),
  CONSTRAINT chk_operations_finished CHECK (
    (status IN ('SUCCEEDED','FAILED') AND finished_at IS NOT NULL) OR
    (status NOT IN ('SUCCEEDED','FAILED') AND finished_at IS NULL)
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 与商品 Outbox 保持一致的技术结构，数据仅属于 Order 服务。
CREATE TABLE outbox_events (
  id BIGINT NOT NULL AUTO_INCREMENT,
  event_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  event_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  aggregate_type VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  aggregate_id BIGINT NOT NULL,
  aggregate_version BIGINT NOT NULL,
  schema_version INT NOT NULL DEFAULT 1,
  payload JSON NOT NULL,
  occurred_at DATETIME(6) NOT NULL,
  publish_status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'PENDING',
  attempts INT NOT NULL DEFAULT 0,
  next_attempt_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  locked_until DATETIME(6) NULL,
  lock_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
  published_at DATETIME(6) NULL,
  last_error VARCHAR(1024) NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  UNIQUE KEY uk_outbox_event (event_id),
  UNIQUE KEY uk_outbox_aggregate_event (aggregate_type, aggregate_id, aggregate_version, event_type),
  KEY idx_outbox_pending (publish_status, next_attempt_at, id),
  KEY idx_outbox_lease (publish_status, locked_until, id),
  CONSTRAINT chk_outbox_status CHECK (publish_status IN ('PENDING','PROCESSING','PUBLISHED','FAILED')),
  CONSTRAINT chk_outbox_versions CHECK (aggregate_version > 0 AND schema_version > 0 AND attempts >= 0),
  CONSTRAINT chk_outbox_lease CHECK (publish_status <> 'PROCESSING' OR (locked_until IS NOT NULL AND lock_token IS NOT NULL)),
  CONSTRAINT chk_outbox_published CHECK (publish_status <> 'PUBLISHED' OR published_at IS NOT NULL)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
