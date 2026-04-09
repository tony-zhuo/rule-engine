CREATE TABLE IF NOT EXISTS processed_events (
    event_id    VARCHAR(64)  PRIMARY KEY,
    attempts    INT          NOT NULL DEFAULT 0,
    status      VARCHAR(16)  NOT NULL DEFAULT 'pending',
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
