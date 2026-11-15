-- 仅在 Chat 自有数据库执行；不建立跨服务外键，不自动运行。
CREATE TABLE conversations (
  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  product_id BIGINT NOT NULL,
  buyer_id BIGINT NOT NULL,
  seller_id BIGINT NOT NULL,
  last_seq BIGINT NOT NULL DEFAULT 0,
  last_message_id BIGINT NULL,
  last_message_at DATETIME(6) NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY uk_product_participants (product_id, buyer_id, seller_id),
  CONSTRAINT chk_chat_participants CHECK (buyer_id <> seller_id),
  CONSTRAINT chk_chat_seq CHECK (last_seq >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE chat_messages (
  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  conversation_id BIGINT NOT NULL,
  sender_id BIGINT NOT NULL,
  client_msg_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  seq BIGINT NOT NULL,
  content TEXT NOT NULL,
  message_type VARCHAR(16) NOT NULL DEFAULT 'TEXT',
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY uk_sender_client_msg (sender_id, client_msg_id),
  UNIQUE KEY uk_conversation_seq (conversation_id, seq),
  CONSTRAINT chk_message_seq CHECK (seq > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE conversation_members (
  conversation_id BIGINT NOT NULL,
  user_id BIGINT NOT NULL,
  last_read_seq BIGINT NOT NULL DEFAULT 0,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (conversation_id, user_id),
  KEY idx_member_user (user_id, conversation_id),
  CONSTRAINT chk_read_seq CHECK (last_read_seq >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
