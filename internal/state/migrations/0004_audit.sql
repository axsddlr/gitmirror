-- Append-only record of destructive or ownership-changing actions on
-- destination repositories. Rows are never updated or deleted (DeletePair
-- leaves them alone), so a deleted destination keeps a durable trace.
CREATE TABLE audit (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    time      INTEGER NOT NULL, -- unix nanos UTC
    pair_key  TEXT NOT NULL,
    rule      TEXT NOT NULL,
    action    TEXT NOT NULL CHECK (action IN ('archive', 'delete', 'adopt', 'create')),
    dst_forge TEXT NOT NULL,
    dst_repo  TEXT NOT NULL,
    detail    TEXT NOT NULL DEFAULT ''
);

-- Whether a no-op reconcile outcome (destination not owned or already gone)
-- has been recorded for the current missing period, so later cycles stay
-- quiet. Reset with the row by ClearMissing.
ALTER TABLE missing ADD COLUMN reported INTEGER NOT NULL DEFAULT 0;
