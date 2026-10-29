-- What's left of the rooms' Wakeshift groups (docs/subscribe-from-tools.md,
-- stage 7): the cutover's steps, and the column that said a game's room
-- group was still to be closed, both unread since the cutover's removal;
-- and 002's native-seat columns, never read.
DROP TABLE IF EXISTS cutover_steps;
ALTER TABLE games DROP COLUMN IF EXISTS room_ready, DROP COLUMN IF EXISTS updating_until;
ALTER TABLE seats DROP COLUMN IF EXISTS native_seq, DROP COLUMN IF EXISTS native_retry;
