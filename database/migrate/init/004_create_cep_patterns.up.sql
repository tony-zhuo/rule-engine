CREATE TABLE IF NOT EXISTS cep_patterns (
    id          VARCHAR(64)     PRIMARY KEY,
    name        VARCHAR(255)    NOT NULL,
    description TEXT,
    states      JSONB           NOT NULL,
    status      SMALLINT        NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_cep_patterns_status ON cep_patterns (status);
