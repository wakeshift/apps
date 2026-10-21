-- Watchers and the live view named a conversation by its Wakeshift session
-- key, which acts as the conversation: anyone with the page link could read
-- it. They name it by a public label now (an HMAC of the key, server.ts),
-- with the harness Wakeshift recognised. Nothing is lost that a conversation
-- doesn't put back the next time it calls watch.
DELETE FROM watchers;
ALTER TABLE watchers RENAME COLUMN session TO conversation;
ALTER TABLE watchers ADD COLUMN harness text;
-- Only watch and unwatch lines named a conversation.
DELETE FROM activity WHERE (body LIKE '{"kind":"watch"%' OR body LIKE '{"kind":"unwatch"%') AND body LIKE '%"session":%';
UPDATE sessions SET watched_as = NULL;
