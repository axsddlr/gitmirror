-- Forges and rules managed in the web UI (config-file entries are never
-- stored here). The forge token is sealed by the app layer (secretbox); the
-- store never sees it in plaintext. A rule is the JSON of config.Rule as the
-- user entered it, before defaults are applied. Times are unix nanos UTC.
CREATE TABLE ui_forges (
    name       TEXT PRIMARY KEY CHECK (name <> ''),
    type       TEXT NOT NULL,
    url        TEXT NOT NULL DEFAULT '',
    username   TEXT NOT NULL DEFAULT '',
    token      BLOB,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE TABLE ui_rules (
    name       TEXT PRIMARY KEY CHECK (name <> ''),
    rule       TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
-- Small key/value settings owned by the web UI (webhook secret, external
-- URL, admin password hash, ...).
CREATE TABLE settings (
    key   TEXT PRIMARY KEY CHECK (key <> ''),
    value BLOB
);
