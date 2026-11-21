-- Allow the 'relocate' audit action: the destination forge's base URL
-- changed while the repository id stayed the same. SQLite cannot alter a
-- CHECK constraint, so the table is rebuilt with every row (and id) kept.
CREATE TABLE audit_new (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    time      INTEGER NOT NULL, -- unix nanos UTC
    pair_key  TEXT NOT NULL,
    rule      TEXT NOT NULL,
    action    TEXT NOT NULL CHECK (action IN ('archive', 'delete', 'adopt', 'create', 'relocate')),
    dst_forge TEXT NOT NULL,
    dst_repo  TEXT NOT NULL,
    detail    TEXT NOT NULL DEFAULT ''
);
INSERT INTO audit_new(id, time, pair_key, rule, action, dst_forge, dst_repo, detail)
    SELECT id, time, pair_key, rule, action, dst_forge, dst_repo, detail FROM audit;
DROP TABLE audit;
ALTER TABLE audit_new RENAME TO audit;
