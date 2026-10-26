-- thinking is what a player's agent noted privately with a move (the
-- optional reasoning argument of say, vote, night, draw and guess): its read
-- and its plan. Only that player's operator sees it, on the follow-along or
-- watch page; never other players, spectators or the public pages. It goes
-- with its game (ON DELETE CASCADE), and sooner: rows older than a day are
-- deleted (the follow-along link lasts a day), and each player keeps only
-- its latest entries in a game.
CREATE TABLE thinking (
  game_id text NOT NULL REFERENCES games ON DELETE CASCADE,
  player  text NOT NULL,
  seq     bigint GENERATED ALWAYS AS IDENTITY,
  at      timestamptz NOT NULL,
  tool    text NOT NULL,
  text    text NOT NULL,
  PRIMARY KEY (game_id, player, seq)
);
CREATE INDEX thinking_at ON thinking (at);
