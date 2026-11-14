-- Identity of an owned destination: the forge's repository id and the
-- normalised base URL of the destination forge when ownership was recorded.
-- A destination whose id or forge URL no longer matches was replaced (deleted
-- and re-created by hand, restored from a backup) or the forge's URL was
-- re-pointed at another server, and is no longer treated as owned. Rows from
-- before this migration have 0 / '' and are backfilled once from the current
-- destination (trust on first use).
ALTER TABLE ownership ADD COLUMN dst_repo_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ownership ADD COLUMN forge_url TEXT NOT NULL DEFAULT '';
-- When the source was last seen missing (unix nanos UTC, 0 = unknown). A gap
-- between observations longer than the allowed maximum restarts the
-- deletion grace period, so the grace measures continuous observation.
ALTER TABLE missing ADD COLUMN last_seen INTEGER NOT NULL DEFAULT 0;
-- Set while gitmirror has unarchived the destination for a sync and not yet
-- restored its archived state, so a crash in between is repaired by the next
-- run.
ALTER TABLE pairs ADD COLUMN unarchived INTEGER NOT NULL DEFAULT 0;
