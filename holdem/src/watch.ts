// A player's watch link: /watch/<secret>, a page showing their table with
// their own cards, for the person behind the agent. The secret is 128 bits
// derived (HMAC with HOLDEM_SECRET) from the player, their seat (table and
// when they sat down), the day and a nonce, so the agent gets the same link
// every time it asks, and the link stops working when the player leaves the
// table, at the daily reset, or when the agent asks for a new one. Only the
// secret's SHA-256 is stored (on the seat), to find whose it is; the secret
// itself is never stored or logged.

import { createHash, createHmac } from "node:crypto";

export interface WatchSeat {
  table: string;
  joinedAt: string;
  nonce: number;
  day: string;
}

/** A watch link's secret: 16 bytes of the HMAC, base64url (22 characters). */
export function watchSecret(key: string, player: string, seat: WatchSeat): string {
  return createHmac("sha256", key)
    .update(`holdem/watch\0${player}\0${seat.table}\0${seat.joinedAt}\0${seat.day}\0${seat.nonce}`)
    .digest()
    .subarray(0, 16)
    .toString("base64url");
}

/** What's stored to find a watch link's seat. */
export const watchHash = (secret: string) => createHash("sha256").update(`holdem/watch\0${secret}`).digest("hex");

/** A watch link's path pattern: what the server routes, and what log lines must never carry. */
export const WATCH_PATH = /^\/(?:api\/)?watch\/([A-Za-z0-9_-]{22})(?:\/live)?$/;

/** A request path fit for a log line: a watch link's secret taken out. */
export const loggable = (path: string) => path.replace(/\/watch\/[^/?#]*/g, "/watch/…");
