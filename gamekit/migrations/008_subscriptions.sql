-- The games off Wakeshift's groups (docs/subscribe-from-tools.md, "The
-- games on one update event"): the kit keeps who's seated and each seat's
-- conversation's game.update subscription, and room_log is the outbox of
-- what each seated player is sent, by subscription ID (or, for a client
-- that subscribes itself, to its native subscriptions).
--
-- seats.subscription is Wakeshift's ID for the seat's conversation's
-- game.update subscription ('' for a native seat, and for a seat whose
-- subscription ended); subscriber is the user it belongs to (PlayerUser).
ALTER TABLE seats ADD COLUMN subscription text NOT NULL DEFAULT '';
ALTER TABLE seats ADD COLUMN subscriber text NOT NULL DEFAULT '';
CREATE INDEX seats_subscription ON seats (subscription) WHERE subscription <> '';
-- Handles are the kit's now (the agent's name, then -2, -3: allocated under
-- the game's lock). Not a unique index: seats from before this migration
-- took their handles from the room's group, which could reuse a departed
-- member's handle, and a unique index would refuse to build over them.
CREATE INDEX seats_handle ON seats (game_id, handle) WHERE handle IS NOT NULL;

-- room_log as the outbox:
--   recipients: the first emit's recipients, resent as they were on a retry
--               (Wakeshift dedupes by the emit's ID and keeps the first's
--               list): {"m": [...], "o": [...]}, the mentioned and the other
--               players, each {"user", "subscription"} or {"native": user}.
--   tries:      failed emits (rate_limited doesn't count); 8 and it's given up.
--   retry_at:   not before (a rate-limited emit waits for the next minute).
--   from_agent: the speaker's agent (workspace/agent), for the say limit (30
--               a minute per agent, across its seats and games).
-- A player's entry is written with attempts = 8, so a draining revision from
-- before this one (whose outbox is posted_at IS NULL AND attempts < 8, posting
-- every entry as the moderator) never posts it.
ALTER TABLE room_log ADD COLUMN recipients jsonb;
ALTER TABLE room_log ADD COLUMN tries int NOT NULL DEFAULT 0;
ALTER TABLE room_log ADD COLUMN retry_at timestamptz;
ALTER TABLE room_log ADD COLUMN from_agent text;
CREATE INDEX room_log_outbox ON room_log (at) WHERE posted_at IS NULL AND tries < 8;
CREATE INDEX room_log_said ON room_log (from_agent, at) WHERE from_agent IS NOT NULL;

-- The move off Wakeshift's groups: each step once (cutover.go). revision is
-- the deploy that ran it; the row "lease" is the instance running the steps.
CREATE TABLE cutover_steps (
  step     text PRIMARY KEY,
  revision text NOT NULL DEFAULT '',
  done_at  timestamptz NOT NULL DEFAULT now()
);
