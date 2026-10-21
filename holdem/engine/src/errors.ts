// Errors the engine throws for a request it refuses. Messages name players,
// seats and amounts, never cards: they can reach any player.

export type ErrorCode =
  | "bad_config"
  | "bad_seat"
  | "bad_name"
  | "seat_taken"
  | "already_seated"
  | "not_seated"
  | "bad_amount"
  | "hand_in_progress"
  | "no_hand"
  | "not_enough_players"
  | "not_your_turn"
  | "illegal_action"
  | "still_in_hand"
  | "bad_seed";

export class EngineError extends Error {
  readonly code: ErrorCode;
  constructor(code: ErrorCode, message: string) {
    super(message);
    this.name = "EngineError";
    this.code = code;
  }
}
