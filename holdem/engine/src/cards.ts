// Cards are two-character strings: a rank (2-9, T, J, Q, K, A) and a suit
// (c, d, h, s), e.g. "As", "Td". They're plain strings so state stays JSON.

export type Card = string;

export const RANKS = "23456789TJQKA";
export const SUITS = "cdhs";

/** The deck before shuffling: 2c 2d 2h 2s 3c … As. */
export const ORDERED_DECK: readonly Card[] = Object.freeze(
  [...RANKS].flatMap((r) => [...SUITS].map((s) => r + s)),
);

const CARD = /^[2-9TJQKA][cdhs]$/;

export function isCard(c: unknown): c is Card {
  return typeof c === "string" && CARD.test(c);
}

/** The rank's value: 2 for a two … 14 for an ace. */
export function rankOf(c: Card): number {
  return RANKS.indexOf(c[0]!) + 2;
}

export function suitOf(c: Card): string {
  return c[1]!;
}
