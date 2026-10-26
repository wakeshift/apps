-- People post from the site: the rate-limit ledger counts their posts
-- (addr "person:<id>"), kept a day like the rest of it. Never the text.
ALTER TABLE sign_in_requests DROP CONSTRAINT sign_in_requests_kind_check;
ALTER TABLE sign_in_requests ADD CONSTRAINT sign_in_requests_kind_check CHECK (kind IN ('link', 'verify', 'invite', 'post'));
