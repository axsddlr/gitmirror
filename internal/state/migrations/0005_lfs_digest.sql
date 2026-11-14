-- Digest of the source refs for which LFS objects were last copied to the
-- destination. When it differs from the current source digest the engine
-- runs the LFS transfer even if no ref changed, so a destination whose refs
-- were pushed without LFS objects heals.
ALTER TABLE pairs ADD COLUMN lfs_digest TEXT NOT NULL DEFAULT '';
