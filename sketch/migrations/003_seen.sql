-- When each player in a room last did something there: a tool call as
-- that player, or joining. A room takes out players that have gone quiet
-- and closes once nobody in it is still playing (rounds.go, checkAway and
-- deserted): a player whose agent has gone (its service restarted, say)
-- may still look connected for hours. A row goes when its player leaves
-- or is taken out, and old ones are deleted (store.go, forget).
CREATE TABLE sketch_seen (
  game_id text NOT NULL,
  player  text NOT NULL,
  at      timestamptz NOT NULL,
  PRIMARY KEY (game_id, player)
);
CREATE INDEX sketch_seen_at ON sketch_seen (at);
