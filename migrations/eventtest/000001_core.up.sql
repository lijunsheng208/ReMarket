-- 仅在独立测试消费者数据库执行，不能放在 Product/Order 业务库。
CREATE TABLE event_inbox (
 event_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
 fingerprint CHAR(64) NOT NULL,
 round_id VARCHAR(32) NOT NULL DEFAULT '',
 stage INT NOT NULL DEFAULT 0,
 status VARCHAR(16) NOT NULL DEFAULT 'PENDING',
 updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB;
CREATE TABLE event_transfers (
 decision_id CHAR(64) PRIMARY KEY,
 target_topic VARCHAR(249) NOT NULL,
 message_key BLOB NOT NULL,
 message_value MEDIUMBLOB NOT NULL,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB;
CREATE TABLE event_views (
 aggregate_type VARCHAR(32) NOT NULL,
 aggregate_id BIGINT NOT NULL,
 aggregate_version BIGINT NOT NULL,
 event_id VARCHAR(128) NOT NULL,
 payload JSON NOT NULL,
 PRIMARY KEY(aggregate_type,aggregate_id)
) ENGINE=InnoDB;
