-- A player whose clock runs out folds and is taken off the table. They hear
-- about it once: in a last personal event, and in the next tool result they
-- get (notice, cleared when shown).
ALTER TABLE players ADD COLUMN notice text;

-- Their turn subscription for that table ends a little later, so the last
-- event (the notice) is delivered before it ends. Joining again before then
-- keeps the subscription (the row is deleted).
CREATE TABLE subscription_ends (
    subscription text PRIMARY KEY,
    guest text NOT NULL,
    due_at timestamptz NOT NULL
);
CREATE INDEX subscription_ends_due ON subscription_ends (due_at);
