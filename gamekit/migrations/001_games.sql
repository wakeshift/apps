-- A game app's own database (Nightfall's, Secret Agent's): its games, who
-- sits where, each room's log (which is also the outbox of the moderator's
-- announcements), and what players have had recently.

-- games is one row per game. state is the game's own state (JSON); the
-- columns are what the kit queries: matchmaking (open), the ticker
-- (deadline_at), the room's lifecycle (ended_at, closed_at, room_closed),
-- and who's posting its announcements (posting_until).
CREATE TABLE games (
  id            text PRIMARY KEY,
  phase         text NOT NULL,
  open          boolean NOT NULL DEFAULT true,
  max_seats     int NOT NULL,
  names         text[] NOT NULL DEFAULT '{}',
  room_ready    boolean NOT NULL DEFAULT false,
  state         jsonb NOT NULL,
  deadline_at   timestamptz,
  ended_at      timestamptz,
  closed_at     timestamptz,
  room_closed   boolean NOT NULL DEFAULT false,
  stopped       text, -- why the game had to stop, if it did
  posting_until timestamptz,
  ticking_until timestamptz, -- a ticker has claimed its due step
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX games_open ON games (created_at) WHERE open;
CREATE INDEX games_due ON games (deadline_at) WHERE deadline_at IS NOT NULL;

-- seats: a player (one conversation: workspace/agent/session) in a game.
-- handle is null while the seat is reserved (the player is being added to
-- the room); a reservation older than a minute is abandoned.
CREATE TABLE seats (
  game_id     text NOT NULL REFERENCES games ON DELETE CASCADE,
  player      text NOT NULL,
  workspace   text NOT NULL,
  agent       text NOT NULL,
  session     text NOT NULL,
  name        text NOT NULL,
  handle      text,
  reserved_at timestamptz NOT NULL,
  seated_at   timestamptz,
  PRIMARY KEY (game_id, player)
);
CREATE INDEX seats_player ON seats (player, reserved_at DESC);
CREATE INDEX seats_agent ON seats (workspace, agent, seated_at DESC) WHERE handle IS NOT NULL;

-- room_log is everything said in a room. The moderator's entries
-- (from_handle '') are posted to the room after the transaction that wrote
-- them commits; posted_at is null until then. Players' are logged first, then posted (and the entry removed if posting fails).
CREATE TABLE room_log (
  game_id     text NOT NULL REFERENCES games ON DELETE CASCADE,
  seq         bigint GENERATED ALWAYS AS IDENTITY,
  at          timestamptz NOT NULL,
  from_handle text NOT NULL DEFAULT '',
  subject     text NOT NULL DEFAULT '',
  text        text NOT NULL,
  mentions    text[] NOT NULL DEFAULT '{}',
  posted_at   timestamptz,
  attempts    int NOT NULL DEFAULT 0,
  PRIMARY KEY (game_id, seq)
);
CREATE INDEX room_log_unposted ON room_log (at) WHERE posted_at IS NULL AND attempts < 8;

-- recent is what a player has had lately, by kind (Secret Agent's
-- codewords), so a game can avoid repeating it.
CREATE TABLE recent (
  player     text NOT NULL,
  kind       text NOT NULL,
  items      text[] NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (player, kind)
);
CREATE INDEX recent_updated ON recent (updated_at);
