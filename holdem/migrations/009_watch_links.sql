-- A player's watch link, now the player's rather than the seat's: one link
-- per player, which follows them to whatever table they sit at, across
-- reconnects, rejoins and table changes. The link is derived (HMAC with
-- HOLDEM_SECRET) from the player, nonce and minted_at; only its SHA-256 is
-- kept, to find the player. It works for 30 days from minted_at; asking for
-- a new one (or for one after that) bumps the nonce, so the old link stops
-- working. Deleting the player deletes it, and so does forgetting the
-- player's sign-in (oauth.ts).
--
-- Links made from seats (seats.watch_*) stop working: they ended at the
-- next 00:00 UTC anyway. Those columns stay until the previous revision,
-- which still writes them, is gone; a later migration drops them.
CREATE TABLE watch_links (
    player text PRIMARY KEY REFERENCES players(id) ON DELETE CASCADE,
    hash text NOT NULL UNIQUE,
    nonce integer NOT NULL,
    minted_at timestamptz NOT NULL
);
CREATE INDEX watch_links_minted ON watch_links (minted_at);
