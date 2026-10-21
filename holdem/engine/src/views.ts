// What players and spectators may see. The public view and the log never
// contain hole cards except those shown at showdown; a player's view adds
// only their own. The seed appears only once the hand is complete.

import type { Card } from "./cards.js";
import { type HandEvent, type HandResult, handActive, type Legal, legalActions, openPots, type Pot, type Street, type TableConfig, type TableState } from "./table.js";

export interface PublicSeat {
  seat: number;
  player: string;
  stack: number;
  sittingOut: boolean;
  /** Dealt into the hand in progress and hasn't folded. */
  inHand: boolean;
}

export interface PublicHandPlayer {
  seat: number;
  player: string;
  stack: number;
  committed: number;
  total: number;
  folded: boolean;
  allIn: boolean;
}

export interface PublicHand {
  id: string;
  number: number;
  /** SHA-256 of the seed, published before the deal. */
  commitment: string;
  street: Street;
  button: number;
  smallBlindSeat: number;
  bigBlindSeat: number;
  board: Card[];
  /** Every chip in the middle, this street's bets included. */
  pot: number;
  /** The main pot, then side pots above all-in players, from what's been put in so far. */
  pots: Pot[];
  currentBet: number;
  minRaise: number;
  toAct: { seat: number; player: string } | null;
  players: PublicHandPlayer[];
  /** Hands shown at showdown, in the order shown. */
  shown: { seat: number; player: string; cards: Card[]; hand: string }[];
  log: HandEvent[];
  /** Once complete. */
  result: HandResult | null;
}

export interface PublicView {
  config: TableConfig;
  button: number;
  seats: (PublicSeat | null)[];
  hand: PublicHand | null;
}

export interface PlayerView extends PublicView {
  you: { player: string; seat: number | null; hole: Card[] };
  /** Your choices, when it's your turn. */
  legal: Legal | null;
}

export function publicView(s: TableState): PublicView {
  const h = s.hand;
  const active = handActive(s);
  const seats = s.seats.map((x, i): PublicSeat | null => {
    if (!x) return null;
    const hp = active ? h!.players.find((p) => p.seat === i && p.player === x.player && !p.left) : undefined;
    return { seat: i, player: x.player, stack: hp ? hp.stack : x.stack, sittingOut: x.sittingOut, inHand: !!hp && !hp.folded };
  });
  return { config: { ...s.config }, button: s.button, seats, hand: h ? publicHand(h, active) : null };
}

function publicHand(h: NonNullable<TableState["hand"]>, active: boolean): PublicHand {
  const toActPlayer = h.toAct === null ? null : h.players.find((p) => p.seat === h.toAct)!;
  return {
    id: h.id,
    number: h.number,
    commitment: h.commitment,
    street: h.street,
    button: h.button,
    smallBlindSeat: h.smallBlindSeat,
    bigBlindSeat: h.bigBlindSeat,
    board: [...h.board],
    pot: h.players.reduce((sum, p) => sum + p.total, 0),
    pots: active ? openPots(h.players) : [],
    currentBet: h.currentBet,
    minRaise: h.minRaise,
    toAct: toActPlayer ? { seat: toActPlayer.seat, player: toActPlayer.player } : null,
    players: h.players.map((p) => ({ seat: p.seat, player: p.player, stack: p.stack, committed: p.committed, total: p.total, folded: p.folded, allIn: p.allIn })),
    shown: h.log.flatMap((e) => (e.type === "show" ? [{ seat: e.seat, player: e.player, cards: [...e.cards], hand: e.hand }] : [])),
    log: structuredClone(h.log),
    result: h.result ? structuredClone(h.result) : null,
  };
}

/** The public view plus this player's own hole cards and, on their turn, their choices. */
export function playerView(s: TableState, player: string): PlayerView {
  const view = publicView(s);
  const seat = s.seats.findIndex((x) => x?.player === player);
  const hp = s.hand?.players.find((p) => p.player === player && !p.left);
  return {
    ...view,
    you: { player, seat: seat >= 0 ? seat : null, hole: hp ? [...hp.hole] : [] },
    legal: legalActions(s, player),
  };
}

/** The hand's public log: a replay without anyone's unshown cards. */
export function history(s: TableState): HandEvent[] {
  return s.hand ? structuredClone(s.hand.log) : [];
}
