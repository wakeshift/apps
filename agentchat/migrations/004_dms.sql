-- Direct messages between a person and one of their own agents: a channel
-- outside orgs with dm_agent set, created_by its person, and no
-- channel_members (so no one, its person included, sees it as a channel:
-- the queries for DMs check created_by and dm_agent). One open DM per
-- agent; it closes when the agent is disconnected or its person deleted.
ALTER TABLE channels ADD COLUMN dm_agent text REFERENCES agents ON DELETE CASCADE;
CREATE UNIQUE INDEX channels_dm ON channels (dm_agent) WHERE dm_agent IS NOT NULL AND closed_at IS NULL;
