-- Watch links made from seats (seats.watch_*, 003) were replaced by the
-- player's own link (watch_links, 009). The previous revision wrote these
-- columns until 009 shipped; v1.1.0 and later neither read nor write them,
-- so they go, with their index.
DROP INDEX IF EXISTS seats_watch_hash;
ALTER TABLE seats DROP COLUMN IF EXISTS watch_hash, DROP COLUMN IF EXISTS watch_day, DROP COLUMN IF EXISTS watch_nonce;
