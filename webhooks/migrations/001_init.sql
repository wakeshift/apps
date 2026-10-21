-- What the try app keeps per webhook, so it survives restarts and works on
-- any number of instances. Links themselves are still stored nowhere: these
-- rows are keyed by the webhook ID a link names. Everything here is bounded
-- and swept (state.ts) as the privacy policy says.

-- One row per webhook that's been used: when (links expire after 7 days
-- without use) and the checked URL the user last picked.
CREATE TABLE hooks (
    hook text PRIMARY KEY,
    last_use timestamptz NOT NULL,
    checked_check text,
    checked_url text
);

-- The live view: the last 50 lines per webhook. seq orders them across
-- instances (the page dedupes by it).
CREATE TABLE activity (
    seq bigserial PRIMARY KEY,
    hook text NOT NULL,
    at timestamptz NOT NULL,
    body text NOT NULL -- JSON (text, not jsonb: jsonb refuses \u0000, which posts can hold)
);
CREATE INDEX activity_hook ON activity (hook, seq DESC);

-- The last 10 posts per webhook in full, for the recent_posts tool.
CREATE TABLE posts (
    seq bigint PRIMARY KEY,
    hook text NOT NULL,
    at timestamptz NOT NULL,
    body text NOT NULL -- JSON: its text and data
);
CREATE INDEX posts_hook ON posts (hook, seq DESC);

-- The conversations that called watch.
CREATE TABLE watchers (
    hook text NOT NULL,
    session text NOT NULL,
    client text NOT NULL,
    since timestamptz NOT NULL,
    PRIMARY KEY (hook, session)
);

-- Counters for the limits, one per key and window (until: when it ends).
-- Keys: <hook>/<check>/h and /d (a webhook URL's hour and day), global/h,
-- ip/<hmac of the address>/h (new links).
CREATE TABLE counters (
    key text NOT NULL,
    until timestamptz NOT NULL,
    n integer NOT NULL,
    PRIMARY KEY (key, until)
);
CREATE INDEX counters_until ON counters (until);

-- MCP sessions: what initialize said, so any instance can serve the
-- session's next request (the client's name decides how it's woken).
CREATE TABLE sessions (
    id text PRIMARY KEY,
    hook text NOT NULL,
    -- initialize's clientInfo and capabilities, as JSON
    client text NOT NULL,
    capabilities text NOT NULL,
    protocol_version text NOT NULL,
    watched_as text,
    created_at timestamptz NOT NULL,
    last_seen timestamptz NOT NULL
);
CREATE INDEX sessions_hook ON sessions (hook, last_seen DESC);
CREATE INDEX sessions_last_seen ON sessions (last_seen);

-- Counts a post against a webhook URL's day and hour and the global hour,
-- all or none. Returns 0 if allowed, else which limit it's over: 1 the day,
-- 2 the hour, 3 the global hour. The URL's rows are locked before the
-- global one, always in that order, so concurrent calls can't deadlock,
-- and a URL over its own limit never waits on the global row.
CREATE FUNCTION take(k_day text, k_hour text, k_global text, day_end timestamptz, hour_end timestamptz,
                     per_day integer, per_hour integer, global_per_hour integer) RETURNS integer
LANGUAGE plpgsql AS $$
DECLARE
    d integer;
    h integer;
    g integer;
BEGIN
    INSERT INTO counters (key, until, n) VALUES (k_day, day_end, 0), (k_hour, hour_end, 0) ON CONFLICT DO NOTHING;
    SELECT n INTO d FROM counters WHERE key = k_day AND until = day_end FOR UPDATE;
    SELECT n INTO h FROM counters WHERE key = k_hour AND until = hour_end FOR UPDATE;
    IF d >= per_day THEN RETURN 1; END IF;
    IF h >= per_hour THEN RETURN 2; END IF;
    INSERT INTO counters (key, until, n) VALUES (k_global, hour_end, 0) ON CONFLICT DO NOTHING;
    SELECT n INTO g FROM counters WHERE key = k_global AND until = hour_end FOR UPDATE;
    IF g >= global_per_hour THEN RETURN 3; END IF;
    UPDATE counters SET n = n + 1
     WHERE (key = k_day AND until = day_end) OR (key = k_hour AND until = hour_end) OR (key = k_global AND until = hour_end);
    RETURN 0;
END
$$;
