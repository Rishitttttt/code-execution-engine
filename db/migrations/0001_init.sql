CREATE TYPE submission_status AS ENUM (
    'queued',
    'running',
    'completed',
    'failed'
);

CREATE TABLE submissions (
    id              UUID PRIMARY KEY,
    language        TEXT NOT NULL,
    code            TEXT NOT NULL,
    stdin           TEXT NOT NULL DEFAULT '',
    webhook_url     TEXT,
    status          submission_status NOT NULL DEFAULT 'queued',

    stdout          TEXT,
    stderr          TEXT,
    exit_code       INTEGER,
    compile_output  TEXT,

    cpu_time_ms     BIGINT,
    wall_time_ms    BIGINT,
    memory_kb       BIGINT,

    timed_out       BOOLEAN NOT NULL DEFAULT FALSE,
    oom_killed      BOOLEAN NOT NULL DEFAULT FALSE,

    -- Set only for infrastructure failures (image pull, daemon down, ...),
    -- never for a user program that merely exited non-zero.
    error           TEXT,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ
);

CREATE INDEX submissions_status_idx ON submissions (status);
CREATE INDEX submissions_created_at_idx ON submissions (created_at DESC);
