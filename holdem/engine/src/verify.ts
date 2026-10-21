// Checking a finished hand from its public log alone: the revealed seed
// matches the commitment published at the start, and the board and every
// shown hand are the cards that seed's deck puts in those places.
//
// The deal: two cards to each player in dealOrder (one each, twice), then
// burn, flop (3), burn, turn, burn, river.

import type { Card } from "./cards.js";
import { verifyShuffle } from "./shuffle.js";
import type { HandEvent } from "./table.js";

export interface HandCheck {
  ok: boolean;
  problems: string[];
}

export function verifyHand(log: readonly HandEvent[]): HandCheck {
  const problems: string[] = [];
  const start = log.find((e) => e.type === "start");
  const end = log.find((e) => e.type === "end");
  if (!start || start.type !== "start") return { ok: false, problems: ["the log has no start"] };
  if (!end || end.type !== "end") return { ok: false, problems: ["the hand isn't finished: no seed yet"] };
  const shuffle = verifyShuffle(end.seed, start.commitment, start.hand);
  if (!shuffle.ok) return { ok: false, problems: [shuffle.problem!] };
  const deck = shuffle.deck!;
  const n = start.dealOrder.length;
  const holeOf = (seat: number): Card[] | null => {
    const k = start.dealOrder.indexOf(seat);
    return k < 0 ? null : [deck[k]!, deck[n + k]!];
  };
  const places: Record<string, number[]> = { flop: [2 * n + 1, 2 * n + 2, 2 * n + 3], turn: [2 * n + 5], river: [2 * n + 7] };
  // The streets come in order, once each; a hand that reached showdown dealt all three.
  const streets = log.flatMap((e) => (e.type === "deal" ? [e.street] : []));
  const order = ["flop", "turn", "river"];
  if (streets.some((x, i) => x !== order[i])) problems.push("the streets aren't dealt once each, in order");
  if (log.some((e) => e.type === "show") && streets.length !== 3) problems.push("the hand went to showdown without the whole board");
  if (log.filter((e) => e.type === "start").length !== 1 || log.filter((e) => e.type === "end").length !== 1)
    problems.push("the log needs exactly one start and one end");
  for (const e of log) {
    if (e.type === "deal") {
      const expected = places[e.street]!.map((i) => deck[i]!);
      if (expected.join() !== e.cards.join()) problems.push(`the ${e.street} isn't what the deck deals`);
    } else if (e.type === "show") {
      const expected = holeOf(e.seat);
      if (!expected || [...expected].sort().join() !== [...e.cards].sort().join()) problems.push(`seat ${e.seat}'s shown cards aren't what the deck dealt it`);
    }
  }
  return { ok: problems.length === 0, problems };
}
