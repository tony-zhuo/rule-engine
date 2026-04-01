CREATE TABLE IF NOT EXISTS behavior_logs (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    member_id   VARCHAR(128)    NOT NULL,
    platform_id VARCHAR(128)    NOT NULL DEFAULT '',
    behavior    VARCHAR(64)     NOT NULL COMMENT 'Login|Trade|CryptoWithdraw|CryptoDeposit|FiatWithdraw|FiatDeposit',
    fields      JSON            NOT NULL,
    occurred_at DATETIME(3)     NOT NULL,
    created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    INDEX idx_member_behavior_occurred (member_id, behavior, occurred_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
