-- 上传会话限定目标商品、所有者、类型、大小与有效期，正式对象与临时对象使用不同键。
CREATE TABLE image_uploads (
 id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 product_id BIGINT NOT NULL,
 owner_id BIGINT NOT NULL,
 content_type VARCHAR(32) NOT NULL,
 size_bytes BIGINT NOT NULL,
 expires_at DATETIME(6) NOT NULL,
 completed_key VARCHAR(512) NOT NULL DEFAULT '',
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (id),
 KEY idx_image_upload_expiry (expires_at),
 CONSTRAINT fk_image_upload_product FOREIGN KEY (product_id) REFERENCES products(id),
 CONSTRAINT chk_image_upload_size CHECK (size_bytes > 0 AND size_bytes <= 5242880)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
ALTER TABLE product_images
 ADD COLUMN content_type VARCHAR(32) NOT NULL DEFAULT '',
 ADD COLUMN size_bytes BIGINT NOT NULL DEFAULT 0,
 ADD COLUMN width INT NOT NULL DEFAULT 0,
 ADD COLUMN height INT NOT NULL DEFAULT 0;
