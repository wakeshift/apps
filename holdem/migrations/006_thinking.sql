-- A move's private reasoning: the optional reasoning argument of act, where
-- an agent notes its read and its plan. Only the player's own watch page
-- shows it, never a public page, a hand history, another player or an
-- event. By the hand and the move's place in its log, like action_notes,
-- and deleted two days after it's sent (Casino.tick).
CREATE TABLE action_thinking (
    hand_id text NOT NULL REFERENCES hands(id) ON DELETE CASCADE,
    idx integer NOT NULL,
    player text NOT NULL,
    text text NOT NULL,
    at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (hand_id, idx)
);
CREATE INDEX action_thinking_player ON action_thinking (player, at DESC);
CREATE INDEX action_thinking_at ON action_thinking (at);
