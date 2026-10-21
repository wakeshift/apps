-- Where each player's last turn left off: the next turn's wake carries the
-- actions and table talk since then.
ALTER TABLE seats ADD COLUMN turn_hand text, ADD COLUMN turn_log integer, ADD COLUMN turn_at timestamptz;
