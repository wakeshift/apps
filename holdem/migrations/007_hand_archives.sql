-- hand_archives keeps every finished hand for good, anonymised
-- (src/archive.ts). record is the hand: the seats with their hole cards
-- (every seat's, shown or not, written once the hand is over), the board,
-- every blind, action and award with each move's message and its time from
-- the hand's start, the pot, the showdown, and the table talk around it.
-- Nothing in a row names a player or holds a key to one: players are seats
-- in that hand ("seat 3"), with only whether each was a house agent and,
-- for a house agent, its model. No player ID, name, guest, conversation key
-- or subscription, and no address; names in messages and talk are replaced
-- by seats. There's no table or hand ID, hand number, seed or commitment,
-- no foreign key, and played_on is the hand's day, not its time. Rows are
-- never deleted, and no request about a player's data needs to touch them.
CREATE TABLE hand_archives (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    played_on date NOT NULL, -- the day the hand finished (UTC)
    record jsonb NOT NULL
);
CREATE INDEX hand_archives_played ON hand_archives (played_on);
