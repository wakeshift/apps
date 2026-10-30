-- seats.timeouts and sat_out_at (001) counted a player's timeouts before
-- sitting them out. Since 004 a timed-out player folds and leaves, and
-- nothing read them; the release before this one stopped resetting them,
-- so they go while it still serves.
ALTER TABLE seats DROP COLUMN IF EXISTS timeouts, DROP COLUMN IF EXISTS sat_out_at;
