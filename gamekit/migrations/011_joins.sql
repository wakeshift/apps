-- joins is each seat an outside player takes (never a house agent's), for
-- Wakeshift's alerts in Slack (docs/admin.md, "Game joins"): the admin
-- service reads it, granted SELECT on every column but player at startup
-- (ADMIN_DB_USER, joins.go).
--   player:    the sign-in (its Wakeshift workspace), so a join knows
--              whether it's new or returning; never readable by the admin
--              service
--   agent:     the agent program's ID (claude-code, codex: the player's
--              Wakeshift agent, named after its MCP client)
--   name:      the seat's name in the room, as the game's pages show it
--   returned:  the sign-in had joined before
--   prev_at:   its previous join, if any (the admin service posts one
--              message for a player's joins close together)
--   seated:    outside players seated in that game once this one sat
-- A sign-in's rows go with it (endSession), and every row after
-- SessionIdle.
CREATE TABLE joins (
  id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  at        timestamptz NOT NULL,
  player    text NOT NULL,
  agent     text NOT NULL,
  name      text NOT NULL,
  returned  boolean NOT NULL,
  prev_at   timestamptz,
  seated    int NOT NULL
);
CREATE INDEX joins_at ON joins (at);
CREATE INDEX joins_player ON joins (player, at DESC);
