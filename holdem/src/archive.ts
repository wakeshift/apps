// Archives: each finished hand, kept for good and anonymized (hand_archives,
// migration 007). A kept hand names no player: each is its seat in that hand
// ("seat 3"), with only whether it was a house agent and, for a house agent,
// its model. No player ID, name, guest, conversation key, subscription or
// address, and nothing that joins back to the hand's own rows: no table or
// hand ID, no hand number, no seed or commitment, and the day only, never a
// time. Players' names in their messages and table talk become their seats.
// This mirrors the game kit's Anon (apps/gamekit/archive.go), which the Go
// games use.

import type { Card, HandEvent, HandState } from "../engine/src/index.js";
import { MESSAGE_MAX } from "./moderation.js";

/** A player as an archive keeps it. */
export interface ArchivePlayer {
  role: string;
  house?: true;
  /** A house agent's model; never set for anyone else. */
  model?: string;
}

/** Who a player is, to Anon: never kept, only turned into a role. */
export interface Known {
  id: string;
  name: string;
  house: boolean;
  model: string | null;
}

/** Gives a record's players their roles, and scrubs their names out of text. */
export class Anon {
  private readonly roles = new Map<string, string>(); // player ID → role
  private readonly names = new Map<string, string>(); // name → role
  private readonly counts = new Map<string, number>();
  private readonly list: ArchivePlayer[] = [];

  /** Gives p the role label as it is ("seat 3"). A player keeps the first role it's given. */
  as(p: Known, label: string): string {
    const had = this.roles.get(p.id);
    if (had) return had;
    this.roles.set(p.id, label);
    if (!this.names.has(p.name)) this.names.set(p.name, label); // a name keeps its first role, as in gamekit
    this.list.push(p.house ? { role: label, house: true, ...(p.model ? { model: p.model } : {}) } : { role: label });
    return label;
  }

  /** Gives p the next role of kind, numbered from 1 ("other player 1"). */
  numbered(p: Known, kind: string): string {
    const had = this.roles.get(p.id);
    if (had) return had;
    const n = (this.counts.get(kind) ?? 0) + 1;
    this.counts.set(kind, n);
    return this.as(p, `${kind} ${n}`);
  }

  /** The role of the player with this ID ("" if it has none). */
  role(id: string): string {
    return this.roles.get(id) ?? "";
  }

  /** Every player given a role, in the order they were given one. */
  players(): ArchivePlayer[] {
    return this.list.map((p) => ({ ...p }));
  }

  /**
   * Text with every name of a player given a role replaced by its role: in
   * any case, with its words joined by spaces, hyphens or underscores
   * ("Brave Otter", "brave-otter"), as a whole word (no letter or digit
   * beside it), longest name (in UTF-8 bytes) first. gamekit's Anon.Scrub
   * does the same (apps/gamekit/testdata/scrub.json).
   */
  scrub(text: string): string {
    const names = [...this.names.keys()].sort(
      (x, y) => Buffer.byteLength(y) - Buffer.byteLength(x) || Buffer.compare(Buffer.from(x), Buffer.from(y)),
    );
    for (const name of names) {
      const parts = name.split(new RegExp(SEPARATORS, "u")).filter(Boolean).map(escapeRegExp);
      if (!parts.length) continue; // all separators: it would match everywhere
      const re = new RegExp(`(?<![\\p{L}\\p{Nd}])${parts.join(SEPARATORS)}(?![\\p{L}\\p{Nd}])`, "giu");
      const role = this.names.get(name)!;
      text = text.replace(re, () => role);
    }
    return text;
  }
}

/** What may join a name's words in text: hyphens, underscores and spaces (any Unicode space, a no-break space too), as in gamekit. */
const SEPARATORS = "[-_\\t\\n\\f\\r\\p{Zs}]+";

const escapeRegExp = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

/** s cut to at most n characters (code points, not UTF-16 units). */
export function truncate(s: string, n: number): string {
  const cps = Array.from(s);
  return cps.length <= n ? s : cps.slice(0, n).join("");
}

/** A seat as an archived hand keeps it. */
export interface ArchivedSeat extends ArchivePlayer {
  /** The stack it started the hand with, before blinds. */
  stack: number;
  /** Its hole cards: kept here for every seat, shown or not, once the hand is over. */
  hole: Card[];
  folded: boolean;
  allIn: boolean;
  /** Its cards were shown at showdown. */
  shown: boolean;
  /** Its stack change over the hand. */
  net: number;
}

export type ArchivedEvent =
  | { kind: "post"; seat: string; blind: "small" | "big"; amount: number; allIn: boolean }
  | { kind: "action"; seat: string; action: string; amount: number; to: number; allIn: boolean; timeout?: true; message?: string; ms?: number }
  | { kind: "return"; seat: string; amount: number }
  | { kind: "deal"; street: "flop" | "turn" | "river"; cards: Card[] }
  | { kind: "show"; seat: string; cards: Card[]; hand: string }
  | { kind: "award"; pot: number; amount: number; winners: string[]; shares: Record<string, number>; hand?: string };

/** A finished hand, anonymized. */
export interface ArchivedHand {
  game: "holdem";
  /** The stake ("1-2") and the pace ("standard"). */
  stake: string;
  pace: string;
  smallBlind: number;
  bigBlind: number;
  seats: ArchivedSeat[];
  button: string;
  board: Card[];
  /** What happened, in order; a move's message and its time (ms from the hand's start) where it had one. */
  events: ArchivedEvent[];
  pot: number;
  showdown: boolean;
  /** Table talk from the end of the table's last hand to the end of this one, ms from this hand's start (negative: before it). */
  talk: { ms: number; from: string; text: string }[];
  lengthMs: number;
}

/** A move's message, by its place in the hand's log. */
export interface Note {
  idx: number;
  text: string;
  at: Date;
}

/** A line of table talk. */
export interface Talk {
  player: string;
  text: string;
  at: Date;
}

/**
 * The hand h, finished, anonymized. players are everyone it may name: its
 * players, everyone seated at the table, whoever talked, and former (who
 * sat there lately). seated is who sits where at the table now (by seat),
 * for talk by players not in the hand.
 */
export function archiveHand(o: {
  h: HandState;
  stake: string;
  pace: string;
  smallBlind: number;
  bigBlind: number;
  players: Map<string, Known>;
  seated: (string | null)[];
  /** Players who sat at the table lately, gone now. */
  former: string[];
  notes: Note[];
  talk: Talk[];
  startedAt: Date;
  endedAt: Date;
  talkMax: number;
}): ArchivedHand {
  const { h } = o;
  const anon = new Anon();
  const unknown = (id: string): Known => o.players.get(id) ?? { id, name: "", house: false, model: null };
  const label = (seat: number) => `seat ${seat + 1}`;
  for (const p of h.players) anon.as(unknown(p.player), label(p.seat));
  o.seated.forEach((id, seat) => id && anon.as(unknown(id), label(seat)));
  for (const l of o.talk) anon.numbered(unknown(l.player), "other player");
  // Only so their names come out of what's said: they aren't in the hand.
  for (const id of o.former) anon.numbered(unknown(id), "other player");
  const kept = new Map(anon.players().map((p) => [p.role, p]));
  const ms = (at: Date) => at.getTime() - o.startedAt.getTime();
  const result = h.result!;
  const shown = new Set(result.shown.map((s) => s.player));
  const seats: ArchivedSeat[] = h.players.map((p) => ({
    ...kept.get(anon.role(p.player))!,
    stack: p.startStack,
    hole: [...p.hole],
    folded: p.folded,
    allIn: p.allIn,
    shown: shown.has(p.player),
    net: result.net[p.player] ?? 0,
  }));
  const notes = new Map(o.notes.map((n) => [n.idx, n]));
  const events: ArchivedEvent[] = [];
  h.log.forEach((e: HandEvent, idx) => {
    switch (e.type) {
      case "post":
        events.push({ kind: "post", seat: anon.role(e.player), blind: e.blind, amount: e.amount, allIn: e.allIn });
        break;
      case "action": {
        const n = notes.get(idx);
        events.push({
          kind: "action",
          seat: anon.role(e.player),
          action: e.action,
          amount: e.amount,
          to: e.to,
          allIn: e.allIn,
          ...(e.timeout ? { timeout: true as const } : {}),
          ...(n ? { message: truncate(anon.scrub(n.text), MESSAGE_MAX), ms: Math.max(0, ms(n.at)) } : {}),
        });
        break;
      }
      case "return":
        events.push({ kind: "return", seat: anon.role(e.player), amount: e.amount });
        break;
      case "deal":
        events.push({ kind: "deal", street: e.street, cards: [...e.cards] });
        break;
      case "show":
        events.push({ kind: "show", seat: anon.role(e.player), cards: [...e.cards], hand: e.hand });
        break;
      case "award":
        events.push({
          kind: "award",
          pot: e.pot,
          amount: e.amount,
          winners: e.winners.map((w) => anon.role(w)),
          shares: Object.fromEntries(Object.entries(e.shares).map(([w, n]) => [anon.role(w), n])),
          ...(e.hand ? { hand: e.hand } : {}),
        });
        break;
      // start (the seats and button, kept above) and end (the seed) aren't kept as events.
    }
  });
  return {
    game: "holdem",
    stake: o.stake,
    pace: o.pace,
    smallBlind: o.smallBlind,
    bigBlind: o.bigBlind,
    seats,
    button: label(h.button),
    board: [...h.board],
    events,
    pot: result.pots.reduce((sum, a) => sum + a.amount, 0),
    showdown: result.showdown,
    talk: o.talk.map((l) => ({ ms: ms(l.at), from: anon.role(l.player), text: truncate(anon.scrub(l.text), o.talkMax) })),
    lengthMs: Math.max(0, ms(o.endedAt)),
  };
}
