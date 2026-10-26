-- Webhooks whose link expired, waiting for Wakeshift to forget them. A
-- link's webhook ID is the app's user in Wakeshift (server.ts), so
-- Wakeshift keeps a workspace for it: the conversations that watched it,
-- their subscriptions and what was delivered to them. The sweep (state.ts)
-- adds a row here in the transaction that deletes the webhook's own rows,
-- and forgetExpired calls Wakeshift's forget-user for it, retrying with
-- backoff until Wakeshift says it's done. A done row stays a day (done_at),
-- so a request that found the link alive just before the sweep can't put
-- it back in use (state.touch).
CREATE TABLE forgets (
    hook text PRIMARY KEY,
    expired_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    next_try timestamptz NOT NULL,
    done_at timestamptz
);
CREATE INDEX forgets_next_try ON forgets (next_try);
