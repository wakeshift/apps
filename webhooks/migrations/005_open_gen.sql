-- The open URL's generation: "Get a new URL" on the webhook's page adds
-- one, and only a token of the current generation is taken (links.ts), so
-- the old open URL stops working at once, on every instance. The webhook,
-- its link, its subscriptions and its checked URLs stay as they were. Kept
-- with the rest of the row, until the link expires.
ALTER TABLE hooks ADD COLUMN open_gen integer NOT NULL DEFAULT 0;
