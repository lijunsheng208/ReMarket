-- 在 Product 库按版本执行一次；不自动删除业务数据。
CREATE TABLE product_cache_invalidations (
 id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
 product_id BIGINT NOT NULL,
 aggregate_version BIGINT NOT NULL,
 status VARCHAR(16) NOT NULL DEFAULT 'PENDING',
 attempts BIGINT NOT NULL DEFAULT 0,
 next_attempt_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 locked_until DATETIME(6) NULL,
 lock_token VARCHAR(32) NULL,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 completed_at DATETIME(6) NULL,
 last_error VARCHAR(64) NULL,
 UNIQUE KEY uk_cache_product_version(product_id, aggregate_version),
 KEY idx_cache_pending(status, next_attempt_at, id),
 KEY idx_cache_lease(status, locked_until, id),
 CONSTRAINT chk_cache_status CHECK(status IN ('PENDING','PROCESSING','DONE')),
 CONSTRAINT chk_cache_ids CHECK(product_id > 0 AND aggregate_version > 0 AND attempts >= 0)
) ENGINE=InnoDB;
