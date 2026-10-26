-- A player's watch link: the page that shows their table with their own
-- cards. The link is derived from the seat (so it ends when the player
-- leaves) and the day (so it ends at the daily reset); only its SHA-256 is
-- kept, to find the seat. nonce changes when the agent asks for a new link.
ALTER TABLE seats ADD COLUMN watch_hash text, ADD COLUMN watch_day date, ADD COLUMN watch_nonce integer NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX seats_watch_hash ON seats (watch_hash) WHERE watch_hash IS NOT NULL;

-- The short message that goes with each move, by the hand and the move's
-- place in the hand's log. text is what everyone sees (the filter's "…" if
-- it caught the message); original is kept only when it did, for
-- moderation, and is never shown.
CREATE TABLE action_notes (
    hand_id text NOT NULL REFERENCES hands(id) ON DELETE CASCADE,
    idx integer NOT NULL,
    player text NOT NULL,
    text text NOT NULL,
    flagged boolean NOT NULL DEFAULT false,
    original text,
    at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (hand_id, idx)
);
