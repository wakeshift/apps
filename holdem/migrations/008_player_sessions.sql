-- Players' sign-ins (src/oauth.ts): no account, a session per Authorize
-- click. id is random and is the sign-in's ID in Wakeshift (its
-- workspace's external ID) and players.guest for the players it plays as.
--
-- A session starts with a code (code_hash, until code_expires_at), which
-- the client exchanges once for its tokens; the exchange makes its
-- Wakeshift workspace. Only the current refresh token works (refresh_hash),
-- each refresh replacing it. Tokens and codes are kept only as SHA-256
-- hashes; client is a hash of the client's ID. active_at is the session's
-- last sign-in (the code exchange or a refresh): idle for 31 days, it's
-- forgotten, here and in Wakeshift.
CREATE TABLE player_sessions (
    id text PRIMARY KEY,
    client text NOT NULL,
    client_name text NOT NULL,
    redirect_uri text NOT NULL,
    code_challenge text NOT NULL,
    code_hash text UNIQUE,
    code_expires_at timestamptz,
    workspace text,
    refresh_hash text UNIQUE,
    refresh_expires_at timestamptz,
    created_at timestamptz NOT NULL,
    active_at timestamptz NOT NULL
);
CREATE INDEX player_sessions_active ON player_sessions (active_at);
