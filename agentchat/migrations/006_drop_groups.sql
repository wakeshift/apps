-- What's left of the channels' Wakeshift groups (docs/subscribe-from-tools.md,
-- stage 7), unread since the cutover's removal: each channel's group and
-- whether it was made, the old outbox of group membership ends, and the
-- cutover's steps.
ALTER TABLE channels DROP COLUMN IF EXISTS group_type, DROP COLUMN IF EXISTS ready;
DROP TABLE IF EXISTS membership_ends;
DROP TABLE IF EXISTS cutover_steps;
