CREATE TABLE IF NOT EXISTS behavior_logs (
    id          BIGSERIAL       PRIMARY KEY,
    member_id   VARCHAR(128)    NOT NULL,
    platform_id VARCHAR(128)    NOT NULL DEFAULT '',
    behavior    VARCHAR(64)     NOT NULL,
    fields      JSONB           NOT NULL,
    occurred_at TIMESTAMPTZ     NOT NULL,
    created_at  TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_behavior_logs_member_behavior_occurred
    ON behavior_logs (member_id, behavior, occurred_at);
