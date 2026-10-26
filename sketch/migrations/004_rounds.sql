-- sketch_rounds keeps every finished round for good, anonymised: the word,
-- the drawing and what happened when, so the drawings can be looked at
-- later (archive.go). Nothing in a row names or links to a player: players
-- are roles in that round ("drawer", "guesser 1"), with only whether each
-- was a house agent and, for a house agent, its model. No handle,
-- workspace, agent, session, user or guest ID, and no address. room is the
-- room's archive ID, random, not the game's ID (which seats and sketch_best
-- carry), and played_on is the round's day, not its time, so a row can't be
-- matched to a leaderboard row by when it was written. Guesses are the
-- players' own text, cut to the game's guess limit, with any player's
-- handle in them replaced by its role. A round voided for writing isn't
-- kept (its drawing may hold text). Rows are never deleted, and no request
-- about a player's data can find them: they carry nothing of the player.
CREATE TABLE sketch_rounds (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  room          text NOT NULL,
  round         int NOT NULL,
  played_on     date NOT NULL,  -- the round's day (UTC)
  word          text NOT NULL,
  tier          text NOT NULL,
  outcome       text NOT NULL,  -- guessed, timeout, forfeit or void
  first_draw_ms int,            -- the first drawing, from the round's start (null: none)
  guessed_ms    int,            -- the right guess, from the round's start (null: none)
  length_ms     int NOT NULL,   -- the round's start to its end
  players       jsonb NOT NULL, -- [{role, house, model}]
  drawing       jsonb NOT NULL, -- the marks, as the canvas stores them (never SVG)
  timeline      jsonb NOT NULL  -- [{ms, kind: draw | hint | guess, ...}], in order
);
CREATE INDEX sketch_rounds_played ON sketch_rounds (played_on);
