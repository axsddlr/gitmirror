CREATE TABLE ownership (
    dst_forge  TEXT NOT NULL,
    dst_repo   TEXT NOT NULL, -- lowercase
    how        TEXT NOT NULL CHECK (how IN ('created', 'adopted')),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (dst_forge, dst_repo)
);

CREATE TABLE pairs (
    key                  TEXT PRIMARY KEY,
    rule                 TEXT NOT NULL,
    src_forge            TEXT NOT NULL,
    src_repo             TEXT NOT NULL,
    dst_forge            TEXT NOT NULL,
    dst_repo             TEXT NOT NULL,
    last_digest          TEXT NOT NULL DEFAULT '',
    last_ref_count       INTEGER NOT NULL DEFAULT 0,
    last_success         INTEGER NOT NULL DEFAULT 0, -- unix nanos UTC, 0 = never
    last_attempt         INTEGER NOT NULL DEFAULT 0,
    last_error           TEXT NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE runs (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    pair_key TEXT NOT NULL REFERENCES pairs(key) ON DELETE CASCADE,
    trigger  TEXT NOT NULL,
    started  INTEGER NOT NULL,
    finished INTEGER NOT NULL DEFAULT 0,
    status   TEXT NOT NULL,
    steps    TEXT NOT NULL DEFAULT '[]', -- JSON
    error    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX runs_pair_idx ON runs(pair_key, id);
CREATE INDEX runs_status_idx ON runs(status);

CREATE TABLE release_map (
    pair_key TEXT NOT NULL,
    src_id   INTEGER NOT NULL,
    dst_id   INTEGER NOT NULL,
    PRIMARY KEY (pair_key, src_id)
);
