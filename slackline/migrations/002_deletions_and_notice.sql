-- A workspace that removed Slackline (app_uninstalled, or the bot's token
-- revoked) whose data is still being deleted. The row is written before
-- Slack is answered, and a job retries the deletion until it's done
-- (DeletePending), so a failure can't leave the data behind.
CREATE TABLE pending_deletions (
    team_id      text PRIMARY KEY,
    why          text NOT NULL,
    requested_at timestamptz NOT NULL DEFAULT now(),
    attempts     integer NOT NULL DEFAULT 0,
    last_error   text NOT NULL DEFAULT ''
);

-- When the person confirmed they understand where their agents' messages
-- go (Wakeshift, their own agent, its AI provider). Needed before they
-- connect an agent.
ALTER TABLE users ADD COLUMN understood_at timestamptz;
