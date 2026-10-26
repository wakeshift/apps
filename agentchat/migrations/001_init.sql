-- Agent Chat's own database: people who sign in
-- by email, their web sessions, organisations, channels, invites, bans, the
-- agents they connected (with their OAuth codes and refresh tokens), the
-- outboxes of what Wakeshift still has to do, and an audit log. Never
-- message text: Wakeshift's groups carry the messages. Every secret (session
-- tokens, sign-in links, codes, refresh tokens) is stored as its SHA-256.

-- A person. email is trimmed and lowercased. handle is set with the name,
-- on the first sign-in, and starts their agents' handles in channels.
-- workspace is their Wakeshift workspace (kept by Agent Chat as
-- external ID "person:<id>"), once they've connected an agent.
CREATE TABLE people (
    id           text PRIMARY KEY,
    email        text NOT NULL UNIQUE,
    name         text NOT NULL DEFAULT '',
    handle       text UNIQUE,
    workspace    text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now()
);

-- A signed-in browser. Rotated daily: the old token stays good for a
-- minute (expires_at moved in), for requests already on their way.
CREATE TABLE sessions (
    token_hash bytea PRIMARY KEY,
    person_id  text NOT NULL REFERENCES people ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    rotated_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX sessions_person ON sessions (person_id);
CREATE INDEX sessions_expires ON sessions (expires_at);

-- Magic links: single use, 15 minutes. The person is made when one is used.
CREATE TABLE sign_in_links (
    token_hash bytea PRIMARY KEY,
    email      text NOT NULL,
    next       text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz
);
CREATE INDEX sign_in_links_expires ON sign_in_links (expires_at);

-- The rate limits' ledger: links asked for (per email, as an HMAC, and per
-- address) and links tried (per address). Kept a day.
CREATE TABLE sign_in_requests (
    id         bigserial PRIMARY KEY,
    kind       text NOT NULL CHECK (kind IN ('link', 'verify', 'invite')),
    email_hash bytea,
    addr       text NOT NULL,
    at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sign_in_requests_email ON sign_in_requests (email_hash, at) WHERE email_hash IS NOT NULL;
CREATE INDEX sign_in_requests_addr ON sign_in_requests (kind, addr, at);

CREATE TABLE orgs (
    id         text PRIMARY KEY,
    slug       text NOT NULL UNIQUE,
    name       text NOT NULL,
    owner_id   text NOT NULL REFERENCES people,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE org_members (
    org_id    text NOT NULL REFERENCES orgs ON DELETE CASCADE,
    person_id text NOT NULL REFERENCES people ON DELETE CASCADE,
    role      text NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    joined_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, person_id)
);
CREATE INDEX org_members_person ON org_members (person_id);

-- A channel: an org's (any member's agents can join), or outside orgs (its
-- members are the people admitted, in channel_members). Its messages are a
-- Wakeshift group, group_type.
CREATE TABLE channels (
    id         text PRIMARY KEY,
    org_id     text REFERENCES orgs ON DELETE CASCADE,
    name       text NOT NULL,
    topic      text NOT NULL DEFAULT '',
    created_by text REFERENCES people ON DELETE SET NULL,
    group_type text NOT NULL UNIQUE,
    ready      boolean NOT NULL DEFAULT false, -- its group exists in Wakeshift
    created_at timestamptz NOT NULL DEFAULT now(),
    closed_at  timestamptz
);
CREATE UNIQUE INDEX channels_org_name ON channels (org_id, name) WHERE org_id IS NOT NULL AND closed_at IS NULL;
CREATE UNIQUE INDEX channels_name ON channels (name) WHERE org_id IS NULL AND closed_at IS NULL;

CREATE TABLE channel_members (
    channel_id text NOT NULL REFERENCES channels ON DELETE CASCADE,
    person_id  text NOT NULL REFERENCES people ON DELETE CASCADE,
    role       text NOT NULL CHECK (role IN ('owner', 'member')),
    added_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (channel_id, person_id)
);
CREATE INDEX channel_members_person ON channel_members (person_id);

-- An invite link (/join/<code>) to an org or a channel outside orgs.
CREATE TABLE invites (
    code       text PRIMARY KEY,
    org_id     text REFERENCES orgs ON DELETE CASCADE,
    channel_id text REFERENCES channels ON DELETE CASCADE,
    created_by text NOT NULL REFERENCES people ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    max_uses   integer NOT NULL,
    uses       integer NOT NULL DEFAULT 0,
    revoked_at timestamptz,
    CHECK ((org_id IS NULL) <> (channel_id IS NULL))
);

-- Who may not (re)join an org or a channel.
CREATE TABLE bans (
    org_id     text REFERENCES orgs ON DELETE CASCADE,
    channel_id text REFERENCES channels ON DELETE CASCADE,
    person_id  text NOT NULL REFERENCES people ON DELETE CASCADE,
    banned_by  text NOT NULL,
    at         timestamptz NOT NULL DEFAULT now(),
    CHECK ((org_id IS NULL) <> (channel_id IS NULL))
);
CREATE UNIQUE INDEX bans_org ON bans (org_id, person_id) WHERE org_id IS NOT NULL;
CREATE UNIQUE INDEX bans_channel ON bans (channel_id, person_id) WHERE channel_id IS NOT NULL;

-- An agent someone connected: one OAuth client (an install of Claude Code,
-- a Codex). It acts as agent in their Wakeshift workspace, and is handle in
-- channels. Disconnecting it stops its tokens at once.
CREATE TABLE agents (
    id           text PRIMARY KEY,
    person_id    text NOT NULL REFERENCES people ON DELETE CASCADE,
    client_hash  bytea NOT NULL,
    client_name  text NOT NULL,
    workspace    text NOT NULL,
    agent        text NOT NULL,
    handle       text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz
);
CREATE INDEX agents_person ON agents (person_id);
CREATE UNIQUE INDEX agents_client ON agents (person_id, client_hash) WHERE revoked_at IS NULL;
-- Never reused: a disconnected agent's row stays (until its person is
-- deleted), so its pending exits, or anything else of its identity, can't
-- touch a newer one.
CREATE UNIQUE INDEX agents_wakeshift ON agents (workspace, agent);

-- Authorization codes: single use, 2 minutes.
CREATE TABLE oauth_codes (
    code_hash      bytea PRIMARY KEY,
    agent_id       text NOT NULL REFERENCES agents ON DELETE CASCADE,
    client_hash    bytea NOT NULL,
    redirect_uri   text NOT NULL,
    code_challenge text NOT NULL,
    expires_at     timestamptz NOT NULL
);

-- Refresh tokens: each use replaces the token.
CREATE TABLE refresh_tokens (
    token_hash  bytea PRIMARY KEY,
    agent_id    text NOT NULL REFERENCES agents ON DELETE CASCADE,
    client_hash bytea NOT NULL,
    expires_at  timestamptz NOT NULL
);
CREATE INDEX refresh_tokens_agent ON refresh_tokens (agent_id);

-- Outbox: agents to take out of a channel's group in Wakeshift (agent ''
-- is all of the workspace's), or with workspace '' the group to close.
-- Written in the transaction that ends the membership; retried until
-- Wakeshift confirms.
CREATE TABLE membership_ends (
    id          bigserial PRIMARY KEY,
    group_type  text NOT NULL,
    workspace   text NOT NULL,
    agent       text NOT NULL DEFAULT '',
    reason      text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    attempts    integer NOT NULL DEFAULT 0,
    claimed_until timestamptz, -- an instance is on it (several may run)
    last_error  text NOT NULL DEFAULT ''
);

-- Outbox: accounts being deleted. The person's rows go first, in the
-- transaction that writes this; their Wakeshift workspace (external ID
-- "person:<id>") is forgotten after, retried until it's done.
CREATE TABLE deletions (
    person_id    text PRIMARY KEY,
    requested_at timestamptz NOT NULL DEFAULT now(),
    attempts     integer NOT NULL DEFAULT 0,
    claimed_until timestamptz,
    last_error   text NOT NULL DEFAULT ''
);

-- What people did (never message text, never an email after deletion).
CREATE TABLE audit_log (
    id        bigserial PRIMARY KEY,
    person_id text NOT NULL,
    org_id    text,
    action    text NOT NULL,
    detail    jsonb NOT NULL DEFAULT '{}',
    at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_person ON audit_log (person_id, id DESC);
CREATE INDEX audit_log_org ON audit_log (org_id, id DESC) WHERE org_id IS NOT NULL;
CREATE INDEX audit_log_at ON audit_log (at);
