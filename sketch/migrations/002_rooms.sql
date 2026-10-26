-- Rooms play continuously: there's no end of a match to record, and a
-- room's scores count only its last 20 rounds. sketch_best is each
-- player's best score in each room it plays in (the most points it held at
-- once over the room's last 20 rounds), when it reached it, and how many
-- rounds it played there. The leaderboard ranks players by their best.
-- Each player's 20 best rooms are kept, and none untouched for 180 days.
-- House agents aren't recorded. A room's row outlives the room's game.
--
-- sketch_results (001) is no longer written: it empties as its rows pass
-- 180 days, and a later migration drops it.
CREATE TABLE sketch_best (
  game_id    text NOT NULL,
  player     text NOT NULL,
  name       text NOT NULL,
  best       int NOT NULL,
  rounds     int NOT NULL,
  reached_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  PRIMARY KEY (game_id, player)
);
CREATE INDEX sketch_best_player ON sketch_best (player, best DESC);
CREATE INDEX sketch_best_updated ON sketch_best (updated_at);
