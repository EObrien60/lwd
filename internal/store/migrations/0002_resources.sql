-- M2: platform resources and their backups. See docs/lwd2/DESIGN.md "M2".

-- One row per declared resource of an app environment. Names are unique per
-- kind so two app-envs can never be handed the same database or bucket.
CREATE TABLE resources (
    app         text NOT NULL,
    env         text NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('database', 'bucket')),
    host        text NOT NULL REFERENCES hosts(name),   -- where it was last provisioned
    name        text NOT NULL,
    credentials bytea NOT NULL,                         -- AES-GCM JSON (internal/secrets)
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (app, env, kind),
    UNIQUE (kind, name)
);

CREATE TABLE backups (
    id          bigserial PRIMARY KEY,
    app         text NOT NULL,
    env         text NOT NULL,
    host        text NOT NULL,
    database    text NOT NULL,
    file        text NOT NULL DEFAULT '',               -- base name on the host
    bytes       bigint NOT NULL DEFAULT 0,
    sha256      text NOT NULL DEFAULT '',
    kind        text NOT NULL CHECK (kind IN ('manual', 'scheduled')),
    status      text NOT NULL CHECK (status IN ('succeeded', 'failed')),
    error       text NOT NULL DEFAULT '',
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);
CREATE INDEX backups_app_env_id ON backups (app, env, id DESC);
CREATE INDEX backups_failed ON backups (started_at) WHERE status = 'failed';
