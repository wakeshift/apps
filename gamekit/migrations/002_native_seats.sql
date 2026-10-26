-- Seats taken by clients that subscribe to events themselves (MCP Events:
-- ChatGPT). The game's room can't wake them: they're woken by game.update,
-- and they're present only while that subscription is live.
--   native_at:    when the player last subscribed (or refreshed); NULL:
--                 not since it joined.
--   native_seq:   the last room_log entry sent to it (a cursor: the room's
--                 log is game.update's outbox), so nothing is lost to a
--                 failure or a restart, and each seat goes at its own pace.
--   native_retry: when to try again after a failed send.
-- games.updating_until is which instance is sending a game's updates.
ALTER TABLE seats ADD COLUMN native boolean NOT NULL DEFAULT false;
ALTER TABLE seats ADD COLUMN native_at timestamptz;
ALTER TABLE seats ADD COLUMN native_seq bigint NOT NULL DEFAULT 0;
ALTER TABLE seats ADD COLUMN native_retry timestamptz;
CREATE INDEX seats_native ON seats (game_id) WHERE native AND native_at IS NOT NULL;
ALTER TABLE games ADD COLUMN updating_until timestamptz;
