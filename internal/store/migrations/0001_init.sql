-- LWD 2.0 controller schema. See docs/lwd2/DESIGN.md "Controller".

CREATE TABLE hosts (
    name       text PRIMARY KEY,
    addr       text NOT NULL,
    token_enc  bytea NOT NULL,              -- node bearer token, AES-GCM (internal/secrets)
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE apps (
    name       text PRIMARY KEY,
    manifest   text NOT NULL,               -- current lwd.toml, verbatim
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Synced from the manifest on every app apply. Rows referenced by deployments
-- outlive their removal from the manifest so history stays joinable.
CREATE TABLE environments (
    app    text NOT NULL REFERENCES apps(name),
    name   text NOT NULL,
    host   text NOT NULL REFERENCES hosts(name),
    domain text NOT NULL,
    tls    text NOT NULL CHECK (tls IN ('acme', 'internal')),
    PRIMARY KEY (app, name)
);

CREATE TABLE releases (
    id         bigserial PRIMARY KEY,
    app        text NOT NULL REFERENCES apps(name),
    commit     text NOT NULL DEFAULT '',
    tag        text NOT NULL DEFAULT '',
    manifest   text NOT NULL,               -- snapshot of apps.manifest at creation
    images     jsonb NOT NULL,              -- service -> repo@sha256:...
    actor      text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX releases_app_id ON releases (app, id DESC);

CREATE TABLE deployments (
    id          bigserial PRIMARY KEY,
    app         text NOT NULL,
    env         text NOT NULL,
    release_id  bigint NOT NULL REFERENCES releases(id),
    host        text NOT NULL,              -- copied: where this attempt ran
    kind        text NOT NULL CHECK (kind IN ('deploy', 'rollback')),
    status      text NOT NULL CHECK (status IN ('running', 'succeeded', 'failed', 'reverted')),
    phase       text NOT NULL DEFAULT '',
    message     text NOT NULL DEFAULT '',
    actor       text NOT NULL DEFAULT '',
    reason      text NOT NULL DEFAULT '',
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    result      jsonb,                      -- bundle.Result from the node
    FOREIGN KEY (app, env) REFERENCES environments (app, name)
);
CREATE INDEX deployments_app_env_id ON deployments (app, env, id DESC);
-- "Live" is the newest succeeded deployment; this keeps that lookup cheap.
CREATE INDEX deployments_succeeded ON deployments (app, env, id DESC) WHERE status = 'succeeded';
CREATE INDEX deployments_running ON deployments (id) WHERE status = 'running';

-- Append-only: each set or delete adds a version; deletes are tombstones.
CREATE TABLE secrets (
    app        text NOT NULL,
    env        text NOT NULL,
    key        text NOT NULL,
    version    int NOT NULL,
    value      bytea,                       -- AES-GCM; NULL for tombstones
    deleted    boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (app, env, key, version)
);

CREATE TABLE events (
    id            bigserial PRIMARY KEY,
    at            timestamptz NOT NULL DEFAULT now(),
    app           text NOT NULL DEFAULT '',
    env           text NOT NULL DEFAULT '',
    deployment_id bigint REFERENCES deployments(id),
    kind          text NOT NULL,
    message       text NOT NULL DEFAULT '',
    data          jsonb
);
CREATE INDEX events_app_env_id ON events (app, env, id DESC);
