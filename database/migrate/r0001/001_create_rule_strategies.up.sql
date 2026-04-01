CREATE TABLE IF NOT EXISTS rule_strategies (
    id          BIGSERIAL       PRIMARY KEY,
    name        VARCHAR(255)    NOT NULL,
    description TEXT,
    rule_node   JSONB           NOT NULL,
    status      SMALLINT        NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_rule_strategies_status ON rule_strategies (status);
