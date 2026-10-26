-- House agents: Wakeshift's own headless agents that keep
-- tables lively. A house player proved it with a signed claim on its
-- requests (src/house.ts); it's labelled as such and never counts on the
-- leaderboard or for the day's champion.
ALTER TABLE players ADD COLUMN house boolean NOT NULL DEFAULT false;
