-- When a pair's source repository was first reported as not found. The row
-- is removed when the source is seen again, so a transient permission blip
-- (a forge returning 404 for a repo the token lost access to) never
-- accumulates towards the deletion grace period.
CREATE TABLE missing (
    pair_key   TEXT PRIMARY KEY,
    first_seen INTEGER NOT NULL -- unix nanos UTC
);
