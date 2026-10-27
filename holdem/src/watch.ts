// A player's watch link: /watch/<secret>, a page showing the player's table
// with their own cards, for the person behind the agent. It's the player's,
// not the seat's: it follows them to whatever table they sit at, across
// reconnects, rejoins and table changes, and says so while they're off the
// table. The secret is 128 bits derived (HMAC with HOLDEM_SECRET) from the
// player and the link's minting (a nonce and the time), so the agent gets
// the same link every time it asks, for 30 days; a new one (asked for, or
// after 30 days) is a new minting, and the old link stops working. Only the
// secret's SHA-256 is stored (watch_links), to find whose it is; the secret
// itself is never stored or logged.

import { createHash, createHmac } from "node:crypto";
import type { WatchMint } from "./casino.js";

/** A watch link's secret: 16 bytes of the HMAC, base64url (22 characters). */
export function watchSecret(key: string, player: string, w: WatchMint): string {
  return createHmac("sha256", key)
    .update(`holdem/watch-player\0${player}\0${w.nonce}\0${w.mintedAt}`)
    .digest()
    .subarray(0, 16)
    .toString("base64url");
}

/** What's stored to find a watch link's player. */
export const watchHash = (secret: string) => createHash("sha256").update(`holdem/watch\0${secret}`).digest("hex");

/** A watch link's path pattern: what the server routes, and what log lines must never carry. */
export const WATCH_PATH = /^\/(?:api\/)?watch\/([A-Za-z0-9_-]{22})(?:\/live)?$/;

/** A request path fit for a log line: a watch link's secret taken out. */
export const loggable = (path: string) => path.replace(/\/watch\/[^/?#]*/g, "/watch/…");
