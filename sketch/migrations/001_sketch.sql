-- Sketch Stadium's own tables, next to the game kit's (apps/gamekit/migrations)
-- in its database. Applied by Migrate, tracked in sketch_migrations.

-- sketch_results is each player's points in each match it finished, for the
-- leaderboard: a player's last 20 matches count, and only those are kept.
-- A player is a conversation (gamekit's player key); name is how it's shown.
-- House agents' matches aren't recorded.
CREATE TABLE sketch_results (
  match_id text NOT NULL,
  player   text NOT NULL,
  name     text NOT NULL,
  points   int NOT NULL,
  ended_at timestamptz NOT NULL,
  PRIMARY KEY (match_id, player)
);
CREATE INDEX sketch_results_player ON sketch_results (player, ended_at DESC);
CREATE INDEX sketch_results_ended ON sketch_results (ended_at);

-- sketch_watch is players' watch links: the SHA-256 of each link's secret
-- (never the secret), the game and the player. A link's row goes when its
-- player leaves the match or the match ends.
CREATE TABLE sketch_watch (
  hash       text PRIMARY KEY,
  game_id    text NOT NULL REFERENCES games ON DELETE CASCADE,
  player     text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (game_id, player)
);

-- sketch_outbox is what a move leaves to send to Wakeshift once it commits
-- (Wakeshift isn't called while a game is locked): the drawer's turn event,
-- a removed player's leaving its game's room. Claimed with a lease
-- (next_at), deleted once sent, dropped after a few attempts.
CREATE TABLE sketch_outbox (
  id       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  game_id  text NOT NULL,
  kind     text NOT NULL,
  data     jsonb NOT NULL,
  attempts int NOT NULL DEFAULT 0,
  next_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sketch_outbox_due ON sketch_outbox (next_at);
