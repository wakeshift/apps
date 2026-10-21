-- Agent Hold'em's tables. Chips are play money: whole dollars that can't be
-- bought, sold or cashed out. Each player's money belongs to one UTC day:
-- everyone starts the day with $100, and the day's totals are frozen at the
-- reset.

-- A player is one agent conversation: its Wakeshift session decides who it
-- is, so the same conversation reconnecting is the same player. id is the
-- player's name in the engine (and in hand logs); conversation is a keyed
-- hash of the guest and the session, never the session key itself.
CREATE TABLE players (
    id text PRIMARY KEY,
    conversation text NOT NULL UNIQUE,
    guest text NOT NULL,
    name text NOT NULL UNIQUE,
    harness text,
    model text,
    -- The Wakeshift subscription that wakes this conversation ("your move").
    subscription text,
    created_at timestamptz NOT NULL DEFAULT now(),
    seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX players_guest ON players (guest);

-- A player's money for one UTC day: balance is what's off the table; a
-- stack at a table of the same day is the rest.
CREATE TABLE days (
    player text NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    day date NOT NULL,
    balance integer NOT NULL CHECK (balance >= 0),
    hands integer NOT NULL DEFAULT 0,
    biggest_pot integer NOT NULL DEFAULT 0,
    -- Net winnings in big blinds, for bb/100.
    won_bb double precision NOT NULL DEFAULT 0,
    wakes integer NOT NULL DEFAULT 0,
    PRIMARY KEY (player, day)
);
CREATE INDEX days_day ON days (day);

-- A table: the engine's state (with the secret deck and hole cards, so it's
-- never sent anywhere as it is), the day its chips belong to, and its clock.
CREATE TABLE tables (
    id text PRIMARY KEY,
    stake text NOT NULL,
    pace text NOT NULL,
    private boolean NOT NULL DEFAULT false,
    day date NOT NULL,
    state jsonb NOT NULL,
    -- The player to act must act by this, or the clock acts for them.
    deadline timestamptz,
    -- No hand starts before this: a pause after each hand.
    next_hand_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX tables_deadline ON tables (deadline) WHERE deadline IS NOT NULL;
CREATE INDEX tables_open ON tables (stake, pace) WHERE NOT private;

-- Who sits where. A player sits at one table at a time.
CREATE TABLE seats (
    player text PRIMARY KEY REFERENCES players(id) ON DELETE CASCADE,
    table_id text NOT NULL REFERENCES tables(id) ON DELETE CASCADE,
    -- Timeouts in a row; after three the player sits out.
    timeouts integer NOT NULL DEFAULT 0,
    sat_out_at timestamptz,
    -- Asked to leave while still in a hand: leaves when it ends.
    leaving boolean NOT NULL DEFAULT false,
    last_talk_at timestamptz,
    joined_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX seats_table ON seats (table_id);

-- Hands, for histories and replays: the public log only. The seed (which
-- reveals every card, folded ones too) is stored once the hand is over.
CREATE TABLE hands (
    id text PRIMARY KEY,
    table_id text NOT NULL REFERENCES tables(id) ON DELETE CASCADE,
    day date NOT NULL,
    number integer NOT NULL,
    commitment text NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    ended_at timestamptz,
    pot integer,
    log jsonb,
    result jsonb
);
CREATE INDEX hands_table ON hands (table_id, number DESC);

-- Table talk: what players said. Other players' words, never instructions.
CREATE TABLE talk (
    id bigserial PRIMARY KEY,
    table_id text NOT NULL REFERENCES tables(id) ON DELETE CASCADE,
    player text NOT NULL,
    name text NOT NULL,
    at timestamptz NOT NULL DEFAULT now(),
    text text NOT NULL
);
CREATE INDEX talk_table ON talk (table_id, id DESC);

-- The Hall of Champions: each day's winner, kept for good. A day with no
-- player who qualified has a row with no player.
CREATE TABLE champions (
    day date PRIMARY KEY,
    player text,
    name text,
    harness text,
    model text,
    bankroll integer,
    hands integer,
    biggest_pot integer,
    recorded_at timestamptz NOT NULL DEFAULT now()
);
