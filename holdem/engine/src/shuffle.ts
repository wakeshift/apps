// The checkable shuffle.
//
// Each hand gets a fresh 32-byte seed from a CSPRNG. At the start of the hand
// only its commitment, SHA-256 of the seed, is public. The deck is a
// Fisher–Yates shuffle of ORDERED_DECK driven by HMAC-SHA256(seed,
// "<hand id>:<block>") blocks, read four bytes at a time, with rejection
// sampling so every permutation is equally likely. When the hand ends the
// seed is revealed, and anyone can recompute the deck with shuffleDeck and
// check it against the commitment with verifyShuffle.

import { createHash, createHmac, randomBytes } from "node:crypto";
import { type Card, ORDERED_DECK } from "./cards.js";
import { EngineError } from "./errors.js";

const SEED = /^[0-9a-f]{64}$/;

/** A fresh seed: 32 random bytes, as hex. */
export function newSeed(): string {
  return randomBytes(32).toString("hex");
}

/** The commitment published before the deal: SHA-256 of the seed's bytes, as hex. */
export function commitmentOf(seed: string): string {
  checkSeed(seed);
  return createHash("sha256").update(Buffer.from(seed, "hex")).digest("hex");
}

function checkSeed(seed: string): void {
  if (!SEED.test(seed)) throw new EngineError("bad_seed", "a seed is 64 lowercase hex characters");
}

/** The shuffled deck for a seed and hand ID. Deterministic. */
export function shuffleDeck(seed: string, handId: string): Card[] {
  checkSeed(seed);
  const key = Buffer.from(seed, "hex");
  let block = 0;
  let buf = Buffer.alloc(0);
  let pos = 0;
  const next32 = (): number => {
    if (pos + 4 > buf.length) {
      buf = createHmac("sha256", key).update(`${handId}:${block++}`).digest();
      pos = 0;
    }
    const v = buf.readUInt32BE(pos);
    pos += 4;
    return v;
  };
  const deck = [...ORDERED_DECK];
  for (let i = deck.length - 1; i > 0; i--) {
    const n = i + 1;
    const limit = Math.floor(0x1_0000_0000 / n) * n; // the largest multiple of n that fits
    let x = next32();
    while (x >= limit) x = next32();
    const j = x % n;
    [deck[i], deck[j]] = [deck[j]!, deck[i]!];
  }
  return deck;
}

export interface ShuffleCheck {
  ok: boolean;
  /** Why not, if not. */
  problem?: string;
  deck?: Card[];
}

/** Checks a revealed seed against the hand's commitment, and returns the deck it makes. */
export function verifyShuffle(seed: string, commitment: string, handId: string): ShuffleCheck {
  if (!SEED.test(seed)) return { ok: false, problem: "the seed isn't 64 lowercase hex characters" };
  if (commitmentOf(seed) !== commitment) return { ok: false, problem: "the seed doesn't match the commitment" };
  return { ok: true, deck: shuffleDeck(seed, handId) };
}
