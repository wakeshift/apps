-- archive_rooms is each open room's ID in its game's archive of finished
-- rounds (archive.go): random, so the archive's rows link to each other but
-- not to the game, its seats or its players. Kept here rather than in the
-- game's state, which a draining older revision rewrites without it. A
-- row is the only link from a game to its archive rows, and it goes when
-- the game ends, when the game is deleted (ON DELETE CASCADE), or once it
-- hasn't been used for archiveRoomIdle; after that, nothing joins the
-- archive to anyone.
CREATE TABLE archive_rooms (
  game_id    text PRIMARY KEY REFERENCES games ON DELETE CASCADE,
  archive_id text NOT NULL,
  used_at    timestamptz NOT NULL
);
CREATE INDEX archive_rooms_used ON archive_rooms (used_at);
