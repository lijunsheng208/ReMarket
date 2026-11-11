-- Execute in remarket_product. No cross-database foreign keys.
CREATE TABLE categories (
  id BIGINT NOT NULL AUTO_INCREMENT,
  code VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  name VARCHAR(64) NOT NULL,
  sort_order INT NOT NULL DEFAULT 0,
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  UNIQUE KEY uk_categories_code (code),
  KEY idx_categories_listing (enabled, sort_order, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE products (
  id BIGINT NOT NULL AUTO_INCREMENT,
  seller_id BIGINT NOT NULL,
  category_id BIGINT NOT NULL,
  title VARCHAR(128) NOT NULL,
  description TEXT NOT NULL,
  price_cent BIGINT NOT NULL,
  status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'DRAFT',
  aggregate_version BIGINT NOT NULL DEFAULT 1,
  first_published_at DATETIME(6) NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  KEY idx_products_listing (status, first_published_at, id),
  KEY idx_products_category (category_id, status, first_published_at, id),
  KEY idx_products_seller (seller_id, created_at, id),
  CONSTRAINT fk_products_category FOREIGN KEY (category_id) REFERENCES categories(id),
  CONSTRAINT chk_products_price CHECK (price_cent > 0),
  CONSTRAINT chk_products_version CHECK (aggregate_version > 0),
  CONSTRAINT chk_products_status CHECK (status IN ('DRAFT','ON_SALE','RESERVED','SOLD','OFF_SHELF')),
  CONSTRAINT chk_products_published CHECK (status = 'DRAFT' OR first_published_at IS NOT NULL)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE product_images (
  id BIGINT NOT NULL AUTO_INCREMENT,
  product_id BIGINT NOT NULL,
  object_key VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
  sort_order INT NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  UNIQUE KEY uk_images_position (product_id, sort_order),
  CONSTRAINT fk_images_product FOREIGN KEY (product_id) REFERENCES products(id),
  CONSTRAINT chk_images_sort CHECK (sort_order >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE product_reservations (
  id BIGINT NOT NULL AUTO_INCREMENT,
  product_id BIGINT NOT NULL,
  order_id BIGINT NOT NULL,
  buyer_id BIGINT NOT NULL,
  status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'ACTIVE',
  expires_at DATETIME(6) NOT NULL,
  confirmed_at DATETIME(6) NULL,
  released_at DATETIME(6) NULL,
  release_reason VARCHAR(32) NULL,
  active_product_id BIGINT GENERATED ALWAYS AS (CASE WHEN status = 'ACTIVE' THEN product_id ELSE NULL END) STORED,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  UNIQUE KEY uk_reservations_order (order_id),
  UNIQUE KEY uk_reservations_active_product (active_product_id),
  KEY idx_reservations_expiry (status, expires_at, id),
  KEY idx_reservations_product (product_id, id),
  CONSTRAINT fk_reservations_product FOREIGN KEY (product_id) REFERENCES products(id),
  CONSTRAINT chk_reservations_status CHECK (status IN ('ACTIVE','CONFIRMED','RELEASED')),
  CONSTRAINT chk_reservations_expiry CHECK (expires_at > created_at),
  CONSTRAINT chk_reservations_terminal CHECK (
    (status = 'ACTIVE' AND confirmed_at IS NULL AND released_at IS NULL) OR
    (status = 'CONFIRMED' AND confirmed_at IS NOT NULL AND released_at IS NULL) OR
    (status = 'RELEASED' AND released_at IS NOT NULL AND confirmed_at IS NULL)
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

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
