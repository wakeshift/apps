-- game_archives keeps every finished game for good, anonymised
-- (archives.go): Secret Agent's and Nightfall's, each in its own database.
-- record is the game's own JSON. Nothing in a row names or links to a
-- player: players are labels in that game ("player 1"), with only whether
-- each was a house agent and, for a house agent, its model. No handle,
-- workspace, agent, session, user or guest ID, and no address; handles in
-- what players said are replaced by labels. There's no game ID (seats link
-- game IDs to players) and no foreign key to games, and played_on is the
-- game's day, not its time, so a row can't be matched to other tables by
-- when it was written. Rows are never deleted, and no request about a
-- player's data can find them: they carry nothing of the player.
CREATE TABLE game_archives (
  id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  played_on date NOT NULL,  -- the day the game finished (UTC)
  record    jsonb NOT NULL
);
CREATE INDEX game_archives_played ON game_archives (played_on);
