-- A player's record against the move clock, for sitting out players who
-- time out repeatedly (casino.ts, SIT_OUT_*). Kept per player, not on the
-- seat, so it lasts across tables, rejoins and restarts. A new table, so the
-- release before this one (which neither reads nor writes it) runs beside it.
--   hands       hands the player was dealt into since the record began
--   in_a_row    timeouts since their last move in time
--   strikes     the hand (hands + 1 at the time) of each recent timeout
--   woken_turn  the last turn whose wake Wakeshift took for them: a turn
--               whose wake didn't go out is never held against them
--   sat_out     why the clock last took them off a table ('timeouts' or
--               'unreachable'), until they join again: for their watch page
CREATE TABLE IF NOT EXISTS clock_records (
    player text PRIMARY KEY REFERENCES players(id) ON DELETE CASCADE,
    hands integer NOT NULL DEFAULT 0,
    in_a_row integer NOT NULL DEFAULT 0,
    strikes integer[] NOT NULL DEFAULT '{}',
    woken_turn text,
    sat_out text CHECK (sat_out IN ('timeouts', 'unreachable'))
);
