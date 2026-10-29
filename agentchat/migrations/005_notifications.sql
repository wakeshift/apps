-- Agent Chat on one personal event, chat.notification
-- (docs/subscribe-from-tools.md, "Agent Chat on one notification event").
-- Agent Chat keeps who is in each channel, which conversations watch, and
-- the messages themselves (for 30 days from posting), and wakes each
-- recipient by emitting chat.notification to its conversation's
-- subscription. Message text is kept here now, unlike 001's "never message
-- text": a message's text, sender, channel, thread and mentions, deleted 30
-- days after it was posted (Prune), and at once with its channel or with a
-- deleted account's direct messages.

-- Who's in a channel: one conversation of an agent per row (not DMs: a DM
-- reaches its agent's watching conversations, below).
CREATE TABLE channel_agents (
    channel_id text NOT NULL REFERENCES channels ON DELETE CASCADE,
    agent_id   text NOT NULL REFERENCES agents ON DELETE CASCADE,
    session    text NOT NULL, -- its session handle (claude-code#2); '' for a client that subscribes itself
    handle     text NOT NULL,
    wake       text NOT NULL CHECK (wake IN ('all', 'mentions', 'none')),
    joined_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (channel_id, agent_id, session),
    UNIQUE (channel_id, handle)
);
CREATE INDEX channel_agents_agent ON channel_agents (agent_id, session);

-- Conversations subscribed to chat.notification (watch). session '' is the
-- native marker: a client that subscribes itself, reached as {native:
-- subscriber}. subscriber is the subscription's user (<workspace>/<agent>),
-- what an emit addresses it by with subscription.
CREATE TABLE watchers (
    agent_id     text NOT NULL REFERENCES agents ON DELETE CASCADE,
    session      text NOT NULL,
    subscription text NOT NULL DEFAULT '', -- Wakeshift's ID for it; '' for the native marker
    subscriber   text NOT NULL,
    watched_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, session)
);
CREATE INDEX watchers_subscription ON watchers (subscriber, subscription) WHERE subscription <> '';

-- A channel's messages take their place from its counter, under its row's
-- lock, so they commit in seq order and the live feed never skips one.
ALTER TABLE channels ADD COLUMN last_seq bigint NOT NULL DEFAULT 0;
-- New channels need no Wakeshift group. ready and group_type stay (the
-- previous revision reads them while it drains) until a later release drops
-- them.
ALTER TABLE channels ALTER COLUMN ready SET DEFAULT true;
ALTER TABLE channels ALTER COLUMN group_type DROP NOT NULL;

-- Messages, kept 30 days from posting.
CREATE TABLE messages (
    id           text PRIMARY KEY,
    channel_id   text NOT NULL REFERENCES channels ON DELETE CASCADE,
    seq          bigint NOT NULL,
    parent_id    text,
    thread_id    text,
    hop          integer NOT NULL DEFAULT 0, -- a chain of agent replies stops at 4
    from_kind    text NOT NULL CHECK (from_kind IN ('agent', 'person', 'app')),
    from_agent   text REFERENCES agents ON DELETE SET NULL,
    from_person  text REFERENCES people ON DELETE SET NULL,
    from_handle  text NOT NULL,
    from_session text NOT NULL DEFAULT '',
    text         text NOT NULL,
    mentions     text[] NOT NULL DEFAULT '{}',
    also_channel boolean NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (channel_id, seq)
);
CREATE INDEX messages_agent_recent ON messages (from_agent, created_at) WHERE from_agent IS NOT NULL; -- the post rate
CREATE INDEX messages_created ON messages (created_at); -- retention

-- Outbox: the emits still to make for a message, one per reason (dm,
-- mention, channel) and batch of 1,000. Written in the message's
-- transaction with its recipients, so every retry sends what the first
-- try did (Wakeshift dedupes by emit_id and keeps the first's list).
CREATE TABLE notify_outbox (
    id            bigserial PRIMARY KEY,
    message_id    text NOT NULL REFERENCES messages ON DELETE CASCADE,
    emit_id       text NOT NULL UNIQUE, -- <message id>:<reason>[:<batch>]
    reason        text NOT NULL CHECK (reason IN ('dm', 'mention', 'channel')),
    recipients    jsonb NOT NULL, -- [{"user", "subscription"} | {"native": user}]
    attempts      integer NOT NULL DEFAULT 0,
    next_at       timestamptz NOT NULL DEFAULT now(),
    claimed_until timestamptz,
    last_error    text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notify_outbox_due ON notify_outbox (next_at);

-- Outbox: a disconnected agent's chat.notification subscriptions, ended by
-- ID in Wakeshift.
CREATE TABLE subscription_ends (
    id            bigserial PRIMARY KEY,
    subscriber    text NOT NULL,
    subscription  text NOT NULL,
    attempts      integer NOT NULL DEFAULT 0,
    claimed_until timestamptz,
    last_error    text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- The move off Wakeshift's groups: each step once (cutover.go). revision
-- is the deploy that ran it.
CREATE TABLE cutover_steps (
    step     text PRIMARY KEY,
    revision text NOT NULL DEFAULT '',
    done_at  timestamptz NOT NULL DEFAULT now()
);
