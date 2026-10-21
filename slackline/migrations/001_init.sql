-- Slackline keeps what it needs to route messages and let people manage
-- their agents: Slack workspaces that installed it (bot tokens sealed),
-- people who signed in, the agents they connected (and their OAuth refresh
-- tokens, hashed), which Slack threads belong to which agent conversation,
-- each agent's channel rules, and an audit log. It never stores message
-- text.

CREATE TABLE teams (
    team_id      text PRIMARY KEY,
    team_name    text NOT NULL,
    bot_token    bytea NOT NULL, -- sealed (AES-256-GCM)
    bot_user_id  text NOT NULL,
    installed_by text NOT NULL DEFAULT '',
    installed_at timestamptz NOT NULL DEFAULT now()
);

-- Someone who signed in with Slack. session_version ends every web session
-- when it changes (sign out everywhere).
CREATE TABLE users (
    team_id         text NOT NULL,
    user_id         text NOT NULL,
    name            text NOT NULL DEFAULT '',
    team_name       text NOT NULL DEFAULT '',
    dm_channel      text NOT NULL DEFAULT '',
    session_version integer NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_sign_in_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_id)
);

-- An agent someone connected (one OAuth client install: Claude Code on a
-- laptop, a Codex). Its access tokens name it; revoking it stops them all.
CREATE TABLE agents (
    id          text PRIMARY KEY,
    team_id     text NOT NULL,
    user_id     text NOT NULL,
    client_id   text NOT NULL, -- the OAuth client (dynamic registration), hashed
    client_name text NOT NULL,
    label       text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz,
    FOREIGN KEY (team_id, user_id) REFERENCES users ON DELETE CASCADE
);
CREATE INDEX agents_user ON agents (team_id, user_id);

-- Authorization codes, single use, short-lived; hashed.
CREATE TABLE oauth_codes (
    code_hash      bytea PRIMARY KEY,
    agent_id       text NOT NULL REFERENCES agents ON DELETE CASCADE,
    client_id      text NOT NULL,
    redirect_uri   text NOT NULL,
    code_challenge text NOT NULL,
    expires_at     timestamptz NOT NULL
);

-- Refresh tokens, hashed; each use replaces the token.
CREATE TABLE refresh_tokens (
    token_hash bytea PRIMARY KEY,
    agent_id   text NOT NULL REFERENCES agents ON DELETE CASCADE,
    expires_at timestamptz NOT NULL
);
CREATE INDEX refresh_tokens_agent ON refresh_tokens (agent_id);

-- One conversation of an agent that called watch: its thread in the
-- person's DM with the app, and the topic it's subscribed to.
CREATE TABLE listeners (
    id           text PRIMARY KEY,
    agent_id     text NOT NULL REFERENCES agents ON DELETE CASCADE,
    dm_channel   text NOT NULL,
    dm_thread_ts text NOT NULL,
    label        text NOT NULL,
    subscription text NOT NULL DEFAULT '', -- its Wakeshift subscription (ts_…)
    sub_ended    boolean NOT NULL DEFAULT false, -- Wakeshift confirmed it ended (after a revoke)
    created_at   timestamptz NOT NULL DEFAULT now(),
    active_at    timestamptz NOT NULL DEFAULT now(),
    closed_at    timestamptz
);
CREATE INDEX listeners_agent ON listeners (agent_id);

-- Which listener a conversation already has, by an HMAC of its session key
-- or Codex thread (never the key itself).
CREATE TABLE conversations (
    key_hash    bytea PRIMARY KEY,
    listener_id text NOT NULL REFERENCES listeners ON DELETE CASCADE
);

-- Slack threads that lead to listeners: each one's DM thread, and threads
-- in channels it posted in (several agents can post in one thread). Replies
-- there go to them, each under its own channel rule.
CREATE TABLE threads (
    team_id     text NOT NULL,
    channel_id  text NOT NULL,
    thread_ts   text NOT NULL,
    listener_id text NOT NULL REFERENCES listeners ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, channel_id, thread_ts, listener_id)
);

-- Where an agent listens besides its DM: a channel, every message or only
-- @-mentions of the app, from its person only or from anyone there.
CREATE TABLE channel_rules (
    agent_id     text NOT NULL REFERENCES agents ON DELETE CASCADE,
    channel_id   text NOT NULL,
    channel_name text NOT NULL,
    mode         text NOT NULL CHECK (mode IN ('mentions', 'all')),
    senders      text NOT NULL CHECK (senders IN ('me', 'anyone')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, channel_id)
);
CREATE INDEX channel_rules_channel ON channel_rules (channel_id);

-- What people did with their agents and rules (never message text).
CREATE TABLE audit_log (
    id      bigserial PRIMARY KEY,
    team_id text NOT NULL,
    user_id text NOT NULL,
    action  text NOT NULL,
    detail  jsonb NOT NULL DEFAULT '{}',
    at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_user ON audit_log (team_id, user_id, id DESC);
