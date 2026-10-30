-- Columns nothing reads, unwritten since the release before this one, so
-- this is safe while that release still serves:
--   - room_log.attempts and its index room_log_unposted (001): the outbox
--     before 008 went by them; 008's goes by tries, and a player's entry
--     no longer writes attempts = 8 to hide it from a revision before 008;
--   - seats.native_at and its index seats_native (002): when a seat went
--     native, never read (009 dropped its siblings).
DROP INDEX IF EXISTS room_log_unposted;
ALTER TABLE room_log DROP COLUMN IF EXISTS attempts;
DROP INDEX IF EXISTS seats_native;
ALTER TABLE seats DROP COLUMN IF EXISTS native_at;
