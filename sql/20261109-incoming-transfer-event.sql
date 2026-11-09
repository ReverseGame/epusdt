-- 专用核对表使用固定表名；不修改已有订单或支付匹配行为。
CREATE TABLE IF NOT EXISTS incoming_transfer_cursor (
    wallet VARCHAR(34) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    cursor_ms BIGINT NOT NULL
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS incoming_transfer_event (
    id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    event_key VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tx_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    event_index INT NOT NULL,
    wallet VARCHAR(34) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    sender VARCHAR(34) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    amount DECIMAL(38,6) NOT NULL,
    block_time BIGINT NOT NULL,
    state VARCHAR(16) NOT NULL DEFAULT 'PENDING',
    reason VARCHAR(512),
    attempts INT NOT NULL DEFAULT 0,
    next_attempt_at DATETIME(3) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    UNIQUE KEY uk_transfer_event(event_key),
    KEY idx_transfer_retry(state,next_attempt_at),
    KEY idx_transfer_tx(tx_hash,wallet)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
