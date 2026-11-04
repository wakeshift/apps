-- Refused and rate-limited posts past the live view's lines for them (at
-- most 10 a minute per webhook on each instance, server.ts): counted per
-- webhook and minute, so the page shows one line saying how many more there
-- were, however many instances answered them. Each instance adds its own
-- count once the minute is over (or when it shuts down); seq is the live
-- view's line for the minute, updated in place as counts come in. Swept
-- after an hour (state.ts).
CREATE TABLE skipped (
    hook text NOT NULL,
    minute timestamptz NOT NULL,
    n integer NOT NULL,
    codes integer[] NOT NULL,
    seq bigint,
    PRIMARY KEY (hook, minute)
);
CREATE INDEX skipped_minute ON skipped (minute);
