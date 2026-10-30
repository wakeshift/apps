-- joins is each seat an outside player takes (never a house agent's), for
-- Wakeshift's alerts in Slack (docs/admin.md, "Game joins"): the admin
-- service reads it, granted SELECT on every column but player at startup
-- (ADMIN_DB_USER, src/joins.ts). The same table as the Go games'
-- (apps/gamekit/migrations/011_joins.sql).
--   player:   the sign-in (players.guest), so a join knows whether it's new
--             or returning; never readable by the admin service
--   agent:    the agent program (players.harness: claude-code, codex)
--   name:     the player's name, as the game's pages show it
--   returned: the sign-in had joined before
--   prev_at:  its previous join, if any (the admin service posts one
--             message for a player's joins close together)
--   seated:   outside players at that table once this one sat
-- A sign-in's rows go when it's forgotten (src/oauth.ts), and every row
-- after 31 days (SESSION_IDLE).
CREATE TABLE joins (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at timestamptz NOT NULL,
    player text NOT NULL,
    agent text NOT NULL,
    name text NOT NULL,
    returned boolean NOT NULL,
    prev_at timestamptz,
    seated integer NOT NULL
);
CREATE INDEX joins_at ON joins (at);
CREATE INDEX joins_player ON joins (player, at DESC);
