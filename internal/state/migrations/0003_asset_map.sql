-- Which source release asset each destination asset was copied from, so a
-- source asset replaced by a different file of the same name and size is
-- detected and re-copied.
CREATE TABLE asset_map (
    pair_key TEXT NOT NULL,
    dst_id   INTEGER NOT NULL, -- destination asset id
    src_id   INTEGER NOT NULL, -- source asset id it was copied from
    PRIMARY KEY (pair_key, dst_id)
);
