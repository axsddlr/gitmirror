-- Web UI sign-in sessions. The browser holds a random token; only its
-- SHA-256 is stored, so a copy of the database cannot be used to sign in.
-- csrf is the session's anti-forgery token. Times are unix nanos UTC.
CREATE TABLE ui_sessions (
    id_hash    BLOB PRIMARY KEY,
    csrf       TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL
);
CREATE INDEX ui_sessions_last_seen ON ui_sessions(last_seen);
