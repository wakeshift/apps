// Hand evaluation: the best five-card hand out of five to seven cards.
//
// A five-card hand gets a score: its category, then the ranks that break
// ties within it, most significant first, packed four bits each. A higher
// score is a better hand and equal scores tie. A seven-card hand is the best
// of its 21 five-card subsets.

import { type Card, rankOf, suitOf } from "./cards.js";

export enum Category {
  HighCard = 0,
  Pair = 1,
  TwoPair = 2,
  Trips = 3,
  Straight = 4,
  Flush = 5,
  FullHouse = 6,
  Quads = 7,
  StraightFlush = 8,
}

export const CATEGORY_NAMES: Record<Category, string> = {
  [Category.HighCard]: "high card",
  [Category.Pair]: "a pair",
  [Category.TwoPair]: "two pair",
  [Category.Trips]: "three of a kind",
  [Category.Straight]: "a straight",
  [Category.Flush]: "a flush",
  [Category.FullHouse]: "a full house",
  [Category.Quads]: "four of a kind",
  [Category.StraightFlush]: "a straight flush",
};

export interface HandValue {
  /** Higher is better; equal scores split the pot. */
  score: number;
  category: Category;
  /** The ranks that decide ties within the category, most significant first (a wheel's top card is 5). */
  ranks: number[];
  /** The five cards making the hand. */
  cards: Card[];
  /** E.g. "a full house, kings full of sevens". */
  name: string;
}

const RANK_NAMES = ["", "", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "jack", "queen", "king", "ace"];
const PLURAL = (r: number) => (r === 6 ? "sixes" : RANK_NAMES[r] + "s");

function pack(category: Category, ranks: number[]): number {
  let s = category;
  for (let i = 0; i < 5; i++) s = s * 16 + (ranks[i] ?? 0);
  return s;
}

/** Scores exactly five cards. */
export function evaluate5(cards: readonly Card[]): HandValue {
  if (cards.length !== 5) throw new Error("evaluate5 needs five cards");
  const ranks = cards.map(rankOf).sort((a, b) => b - a);
  const flush = cards.every((c) => suitOf(c) === suitOf(cards[0]!));
  const distinct = [...new Set(ranks)];
  let straightHigh = 0;
  if (distinct.length === 5) {
    if (ranks[0]! - ranks[4]! === 4) straightHigh = ranks[0]!;
    else if (ranks[0] === 14 && ranks[1] === 5) straightHigh = 5; // A-2-3-4-5, the wheel
  }
  // Group by rank: by count, then by rank, both descending.
  const counts = new Map<number, number>();
  for (const r of ranks) counts.set(r, (counts.get(r) ?? 0) + 1);
  const groups = [...counts.entries()].sort((a, b) => b[1] - a[1] || b[0] - a[0]);
  const byGroup = groups.map((g) => g[0]);
  const shape = groups.map((g) => g[1]).join("");

  let category: Category;
  let tiebreak: number[];
  if (straightHigh && flush) [category, tiebreak] = [Category.StraightFlush, [straightHigh]];
  else if (shape === "41") [category, tiebreak] = [Category.Quads, byGroup];
  else if (shape === "32") [category, tiebreak] = [Category.FullHouse, byGroup];
  else if (flush) [category, tiebreak] = [Category.Flush, ranks];
  else if (straightHigh) [category, tiebreak] = [Category.Straight, [straightHigh]];
  else if (shape === "311") [category, tiebreak] = [Category.Trips, byGroup];
  else if (shape === "221") [category, tiebreak] = [Category.TwoPair, byGroup];
  else if (shape === "2111") [category, tiebreak] = [Category.Pair, byGroup];
  else [category, tiebreak] = [Category.HighCard, ranks];
  return { score: pack(category, tiebreak), category, ranks: tiebreak, cards: [...cards], name: describe(category, tiebreak) };
}

function describe(category: Category, r: number[]): string {
  const name = (i: number) => RANK_NAMES[r[i]!]!;
  switch (category) {
    case Category.StraightFlush:
      return r[0] === 14 ? "a royal flush" : `a straight flush, ${name(0)} high`;
    case Category.Quads:
      return `four of a kind, ${PLURAL(r[0]!)}`;
    case Category.FullHouse:
      return `a full house, ${PLURAL(r[0]!)} full of ${PLURAL(r[1]!)}`;
    case Category.Flush:
      return `a flush, ${name(0)} high`;
    case Category.Straight:
      return `a straight, ${name(0)} high`;
    case Category.Trips:
      return `three of a kind, ${PLURAL(r[0]!)}`;
    case Category.TwoPair:
      return `two pair, ${PLURAL(r[0]!)} and ${PLURAL(r[1]!)}`;
    case Category.Pair:
      return `a pair of ${PLURAL(r[0]!)}`;
    default:
      return `high card, ${name(0)}`;
  }
}

/** The best five-card hand from five to seven cards. */
export function evaluate(cards: readonly Card[]): HandValue {
  if (cards.length < 5 || cards.length > 7) throw new Error("evaluate needs five to seven cards");
  if (new Set(cards).size !== cards.length) throw new Error("evaluate got a card twice");
  let best: HandValue | null = null;
  const n = cards.length;
  for (let a = 0; a < n; a++)
    for (let b = a + 1; b < n; b++)
      for (let c = b + 1; c < n; c++)
        for (let d = c + 1; d < n; d++)
          for (let e = d + 1; e < n; e++) {
            const v = evaluate5([cards[a]!, cards[b]!, cards[c]!, cards[d]!, cards[e]!]);
            if (!best || v.score > best.score) best = v;
          }
  return best!;
}
