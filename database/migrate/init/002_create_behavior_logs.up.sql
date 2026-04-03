CREATE TABLE IF NOT EXISTS behavior_logs (
    id          BIGSERIAL       PRIMARY KEY,
    event_id    VARCHAR(36)     NOT NULL,
    member_id   VARCHAR(128)    NOT NULL,
    behavior    VARCHAR(64)     NOT NULL,
    fields      JSONB           NOT NULL,
    occurred_at TIMESTAMPTZ     NOT NULL,
    created_at  TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_behavior_logs_member_behavior_occurred
    ON behavior_logs (member_id, behavior, occurred_at);

CREATE UNIQUE INDEX IF NOT EXISTS idx_behavior_logs_event_id
    ON behavior_logs (event_id);
