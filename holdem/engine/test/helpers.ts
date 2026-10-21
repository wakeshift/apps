// Test helpers: seeds from a label, a seeded PRNG, and rigging a hand's cards.

import { createHash } from "node:crypto";
import { type Card, ORDERED_DECK } from "../src/cards.js";
import { createTable, sit, type TableConfig, type TableState } from "../src/table.js";

export const seedOf = (label: string): string => createHash("sha256").update(label).digest("hex");

/** mulberry32: a small seeded PRNG for choosing actions in fuzz tests. */
export function prng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

export const CONFIG: TableConfig = { smallBlind: 1, bigBlind: 2, minBuyIn: 20, maxBuyIn: 400, seats: 9 };

/** A table with players seated in seats 0..n-1 (or the given seats) with the given stacks. */
export function table(stacks: number[], config: Partial<TableConfig> = {}, seats?: number[]): TableState {
  let s = createTable({ ...CONFIG, ...config });
  stacks.forEach((stack, i) => {
    s = sit(s, seats?.[i] ?? i, `p${i}`, Math.max(stack, s.config.minBuyIn));
    if (stack < s.config.minBuyIn) {
      // Below the minimum buy-in, for short-stack tests: set it directly.
      s.ledger.in -= s.seats[seats?.[i] ?? i]!.stack - stack;
      s.seats[seats?.[i] ?? i]!.stack = stack;
    }
  });
  return s;
}

/**
 * Rigs the hand in progress (just started, preflop): sets players' hole cards
 * and the board to come, filling the rest of the deck so cards stay unique.
 */
export function rig(state: TableState, holes: Record<string, [Card, Card]>, board: Card[]): TableState {
  const s = structuredClone(state);
  const h = s.hand!;
  const used = new Set([...Object.values(holes).flat(), ...board]);
  const rest = ORDERED_DECK.filter((c) => !used.has(c));
  for (const p of h.players) p.hole = holes[p.player] ? [...holes[p.player]!] : [rest.shift()!, rest.shift()!];
  const n = h.players.length;
  const deck: Card[] = [];
  for (let i = 0; i < 2 * n; i++) deck.push("??");
  const filler = () => rest.shift()!;
  deck.push(filler(), board[0]!, board[1]!, board[2]!, filler(), board[3]!, filler(), board[4]!);
  while (rest.length) deck.push(rest.shift()!);
  h.deck = deck;
  return s;
}

/** Every card-shaped string anywhere in a value. */
export function cardsIn(v: unknown, out: Set<string> = new Set()): Set<string> {
  if (typeof v === "string" && /^[2-9TJQKA][cdhs]$/.test(v)) out.add(v);
  else if (Array.isArray(v)) for (const x of v) cardsIn(x, out);
  else if (v && typeof v === "object") for (const x of Object.values(v)) cardsIn(x, out);
  return out;
}
