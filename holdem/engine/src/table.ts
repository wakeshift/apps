// A no-limit Texas Hold'em table: seats, hands, betting and showdown.
//
// Everything here is pure: each function takes a TableState and returns a
// new one (the input is never changed), and a state is plain JSON, so the app
// can store it and carry on after a restart. Randomness comes in only as a
// hand's seed (startHand makes one with a CSPRNG unless given one), so the
// same state, seed and actions always give the same hand.
//
// Rules, and the choices made where rule books differ:
// - Button: moves one eligible seat clockwise each hand (no dead button).
//   Heads-up, the button posts the small blind, acts first before the flop
//   and last after it.
// - Blinds: a player short of a blind posts what they have, all in. The bet
//   to call before the flop is the full big blind even if the big blind is
//   short; whatever nobody matches comes back (uncalled bets are returned).
// - New players are dealt in from their first hand without posting.
// - Minimum bet: the big blind. Minimum raise: the size of the last full bet
//   or raise this street (the big blind to start). An all-in that's less than
//   a full raise doesn't reopen the betting: players who already acted since
//   the last full raise may only call or fold.
// - Nobody may bet or raise when every other live player is all in.
// - Folding is always allowed, even when checking is.
// - A timed-out player folds, even where a check was free.
// - Side pots: one layer per all-in level, merged where the same players are
//   eligible. A pot's odd chips go one each to its winners, starting from the
//   first seat left of the button.
// - Showdown: every live hand is shown (no mucking), starting with the last
//   player to bet or raise on the last betting round, else the first live
//   player left of the button. Folded hands are never shown.
// - A player with no chips after a hand loses their seat; a player who asked
//   to leave during a hand they'd folded is paid out at once.

import type { Card } from "./cards.js";
import { EngineError } from "./errors.js";
import { evaluate, type HandValue } from "./evaluate.js";
import { commitmentOf, newSeed, shuffleDeck } from "./shuffle.js";

export interface TableConfig {
  smallBlind: number;
  bigBlind: number;
  minBuyIn: number;
  maxBuyIn: number;
  /** 2 to 9. */
  seats: number;
}

export interface Seat {
  player: string;
  /** Chips at the table, outside any hand in progress (during a hand, the hand's copy is current). */
  stack: number;
  /** Sits out from the next hand until it's turned off. */
  sittingOut: boolean;
}

export type Street = "preflop" | "flop" | "turn" | "river" | "complete";
export type ActionType = "fold" | "check" | "call" | "bet" | "raise" | "allin";

export interface Action {
  type: ActionType;
  /** For bet and raise: the total the player has put in this street once the action is done ("raise to"). */
  amount?: number;
}

export interface HandPlayer {
  seat: number;
  player: string;
  /** Chips not yet put in. */
  stack: number;
  /** The stack the hand started with, before blinds. */
  startStack: number;
  hole: Card[];
  /** Put in this street. */
  committed: number;
  /** Put in this hand, all streets (after returned bets). */
  total: number;
  folded: boolean;
  allIn: boolean;
  /** Has acted since the last full bet or raise this street. Posting a blind isn't acting. */
  acted: boolean;
  /** Left the table after folding; paid out already. */
  left: boolean;
}

export type HandEvent =
  | {
      type: "start";
      hand: string;
      number: number;
      commitment: string;
      button: number;
      smallBlindSeat: number;
      bigBlindSeat: number;
      players: { seat: number; player: string; stack: number }[];
      /** Seats in the order they were dealt to (two rounds, then burn and board), for checking the deal against the deck. */
      dealOrder: number[];
    }
  | { type: "post"; seat: number; player: string; blind: "small" | "big"; amount: number; allIn: boolean }
  | { type: "action"; seat: number; player: string; action: ActionType; amount: number; to: number; allIn: boolean; timeout?: true }
  | { type: "return"; seat: number; player: string; amount: number }
  | { type: "deal"; street: "flop" | "turn" | "river"; cards: Card[] }
  | { type: "show"; seat: number; player: string; cards: Card[]; hand: string }
  | { type: "award"; pot: number; amount: number; winners: string[]; shares: Record<string, number>; hand?: string }
  | { type: "end"; seed: string; net: Record<string, number> };

export interface Pot {
  amount: number;
  /** Live players who can win it. */
  eligible: string[];
}

export interface PotAward extends Pot {
  winners: string[];
  shares: Record<string, number>;
  /** The winning hand's name, if it went to showdown. */
  hand?: string;
}

export interface HandResult {
  showdown: boolean;
  pots: PotAward[];
  shown: { seat: number; player: string; cards: Card[]; hand: string }[];
  /** Each player's stack change over the hand. */
  net: Record<string, number>;
  /** The seed, revealed: anyone can recompute the deck (verifyShuffle). */
  seed: string;
  /** Lost their seat with no chips left. */
  busted: string[];
  /** Left and were paid out at the end of the hand. */
  left: { player: string; amount: number }[];
}

export interface HandState {
  id: string;
  number: number;
  commitment: string;
  /** Secret until the hand is complete. */
  seed: string;
  deck: Card[];
  /** The next deck position to deal from. */
  next: number;
  button: number;
  smallBlindSeat: number;
  bigBlindSeat: number;
  dealOrder: number[];
  street: Street;
  board: Card[];
  /** In seat order. */
  players: HandPlayer[];
  /** The seat to act, or null when nobody is (between streets, or the hand is over). */
  toAct: number | null;
  /** The most anyone has put in this street. */
  currentBet: number;
  /** The smallest raise increment allowed. */
  minRaise: number;
  /** Last to bet or raise on the last betting round with action. */
  lastAggressor: number | null;
  log: HandEvent[];
  result: HandResult | null;
}

export interface TableState {
  config: TableConfig;
  seats: (Seat | null)[];
  /** The last hand's button, -1 before the first. */
  button: number;
  handCount: number;
  /** The hand in progress, or the last one (street "complete") until the next starts. */
  hand: HandState | null;
  /** Chips brought to the table (buy-ins, top-ups) and taken away (leaving). */
  ledger: { in: number; out: number };
}

const clone = <T>(v: T): T => structuredClone(v);

const isInt = (n: unknown): n is number => typeof n === "number" && Number.isSafeInteger(n);

// ── The table ──────────────────────────────────────────────────────────────

export function createTable(config: TableConfig): TableState {
  const { smallBlind, bigBlind, minBuyIn, maxBuyIn, seats } = config;
  if (![smallBlind, bigBlind, minBuyIn, maxBuyIn, seats].every(isInt)) throw new EngineError("bad_config", "every setting is a whole number");
  if (smallBlind < 1 || bigBlind < smallBlind) throw new EngineError("bad_config", "blinds need 1 ≤ small blind ≤ big blind");
  if (minBuyIn < bigBlind || maxBuyIn < minBuyIn) throw new EngineError("bad_config", "buy-ins need big blind ≤ minimum ≤ maximum");
  if (seats < 2 || seats > 9) throw new EngineError("bad_config", "a table has 2 to 9 seats");
  return { config: { ...config }, seats: Array(seats).fill(null), button: -1, handCount: 0, hand: null, ledger: { in: 0, out: 0 } };
}

export function handActive(s: TableState): boolean {
  return s.hand !== null && s.hand.street !== "complete";
}

function seatOf(s: TableState, player: string): number {
  return s.seats.findIndex((x) => x?.player === player);
}

function liveHandPlayer(s: TableState, player: string): HandPlayer | undefined {
  return handActive(s) ? s.hand!.players.find((p) => p.player === player && !p.left) : undefined;
}

/**
 * A player's name is their ID here, and a key in results (net, shares): 1 to
 * 64 letters, digits and . _ : @ -, and not a name objects treat specially.
 */
const NAME = /^[A-Za-z0-9._:@-]{1,64}$/;
const RESERVED = new Set(["__proto__", "constructor", "prototype", "hasOwnProperty", "toString", "valueOf"]);

/** Seats a player with a buy-in. Allowed during a hand: they're dealt in from the next one. */
export function sit(state: TableState, seat: number, player: string, buyIn: number): TableState {
  const s = clone(state);
  if (!isInt(seat) || seat < 0 || seat >= s.seats.length) throw new EngineError("bad_seat", `seat ${seat} doesn't exist`);
  if (s.seats[seat]) throw new EngineError("seat_taken", `seat ${seat} is taken`);
  if (typeof player !== "string" || !NAME.test(player) || RESERVED.has(player))
    throw new EngineError("bad_name", "a player's name is 1 to 64 letters, digits and . _ : @ -");
  // Names identify players in the hand's records, so a name stays taken
  // until the hand ends, also by someone who folded and left.
  const inHand = handActive(s) && s.hand!.players.some((p) => p.player === player);
  if (seatOf(s, player) >= 0 || inHand) throw new EngineError("already_seated", `${player} is already at this table`);
  if (!isInt(buyIn) || buyIn < s.config.minBuyIn || buyIn > s.config.maxBuyIn)
    throw new EngineError("bad_amount", `the buy-in is ${s.config.minBuyIn} to ${s.config.maxBuyIn}`);
  s.seats[seat] = { player, stack: buyIn, sittingOut: false };
  s.ledger.in += buyIn;
  return s;
}

/** Adds chips between hands, up to the maximum buy-in. */
export function addChips(state: TableState, player: string, amount: number): TableState {
  const s = clone(state);
  const i = seatOf(s, player);
  if (i < 0) throw new EngineError("not_seated", `${player} isn't at this table`);
  if (liveHandPlayer(s, player)) throw new EngineError("hand_in_progress", "chips can be added between hands");
  const seat = s.seats[i]!;
  if (!isInt(amount) || amount < 1 || seat.stack + amount > s.config.maxBuyIn)
    throw new EngineError("bad_amount", `the stack can go up to ${s.config.maxBuyIn}`);
  seat.stack += amount;
  s.ledger.in += amount;
  return s;
}

/** Sits a player out from the next hand, or back in. */
export function sitOut(state: TableState, player: string, out: boolean): TableState {
  const s = clone(state);
  const i = seatOf(s, player);
  if (i < 0) throw new EngineError("not_seated", `${player} isn't at this table`);
  s.seats[i]!.sittingOut = out;
  return s;
}

/**
 * Takes a player off the table and pays out their chips. Not while they're
 * still live in a hand: fold first (their chips in the pot stay there).
 */
export function leave(state: TableState, player: string): { state: TableState; cashOut: number } {
  const s = clone(state);
  const i = seatOf(s, player);
  if (i < 0) throw new EngineError("not_seated", `${player} isn't at this table`);
  const hp = liveHandPlayer(s, player);
  if (hp && !hp.folded) throw new EngineError("still_in_hand", `${player} is still in the hand: fold first`);
  let cashOut: number;
  if (hp) {
    cashOut = hp.stack;
    hp.stack = 0;
    hp.left = true;
  } else cashOut = s.seats[i]!.stack;
  s.seats[i] = null;
  s.ledger.out += cashOut;
  return { state: s, cashOut };
}

// ── Starting a hand ────────────────────────────────────────────────────────

function eligibleSeats(s: TableState): number[] {
  return s.seats.flatMap((x, i) => (x && !x.sittingOut && x.stack > 0 ? [i] : []));
}

/** The first seat in `seats` clockwise strictly after `from`. */
function after(seats: number[], from: number, n: number): number {
  for (let k = 1; k <= n; k++) {
    const i = (from + k) % n;
    if (seats.includes(i)) return i;
  }
  throw new Error("no seat");
}

export interface StartOptions {
  /** 64 hex characters; a fresh one from a CSPRNG if left out. */
  seed?: string;
  /** Used in the shuffle; defaults to the hand's number. Must be unique per seed. */
  handId?: string;
}

export function startHand(state: TableState, opts: StartOptions = {}): TableState {
  const s = clone(state);
  if (handActive(s)) throw new EngineError("hand_in_progress", "a hand is in progress");
  const seats = eligibleSeats(s);
  if (seats.length < 2) throw new EngineError("not_enough_players", "a hand needs two players with chips");
  const n = s.seats.length;
  const number = s.handCount + 1;
  const id = opts.handId ?? String(number);
  const seed = opts.seed ?? newSeed();
  const commitment = commitmentOf(seed);
  const deck = shuffleDeck(seed, id);

  const button = s.button < 0 ? seats[0]! : after(seats, s.button, n);
  const headsUp = seats.length === 2;
  const sbSeat = headsUp ? button : after(seats, button, n);
  const bbSeat = after(seats, sbSeat, n);
  const dealOrder: number[] = [];
  for (let k = 1; k <= n; k++) {
    const i = (button + k) % n;
    if (seats.includes(i)) dealOrder.push(i);
  }

  const players: HandPlayer[] = seats.map((i) => {
    const x = s.seats[i]!;
    return { seat: i, player: x.player, stack: x.stack, startStack: x.stack, hole: [], committed: 0, total: 0, folded: false, allIn: false, acted: false, left: false };
  });
  const at = (seat: number) => players.find((p) => p.seat === seat)!;
  let pos = 0;
  for (let round = 0; round < 2; round++) for (const seat of dealOrder) at(seat).hole.push(deck[pos++]!);

  const h: HandState = {
    id,
    number,
    commitment,
    seed,
    deck,
    next: pos,
    button,
    smallBlindSeat: sbSeat,
    bigBlindSeat: bbSeat,
    dealOrder,
    street: "preflop",
    board: [],
    players,
    toAct: null,
    currentBet: s.config.bigBlind,
    minRaise: s.config.bigBlind,
    lastAggressor: null,
    log: [],
    result: null,
  };
  h.log.push({
    type: "start",
    hand: id,
    number,
    commitment,
    button,
    smallBlindSeat: sbSeat,
    bigBlindSeat: bbSeat,
    players: players.map((p) => ({ seat: p.seat, player: p.player, stack: p.stack })),
    dealOrder,
  });
  for (const [seat, blind, size] of [[sbSeat, "small", s.config.smallBlind], [bbSeat, "big", s.config.bigBlind]] as const) {
    const p = at(seat);
    const amount = Math.min(size, p.stack);
    put(p, amount);
    h.log.push({ type: "post", seat, player: p.player, blind, amount, allIn: p.allIn });
  }
  s.button = button;
  s.handCount = number;
  s.hand = h;
  // Before the flop the first to act is the button heads-up, else the seat after the big blind.
  h.toAct = firstToAct(h, headsUp ? button : after(seats, bbSeat, n), n, true);
  if (h.toAct === null) endStreet(s);
  return s;
}

function put(p: HandPlayer, amount: number): void {
  p.stack -= amount;
  p.committed += amount;
  p.total += amount;
  if (p.stack === 0) p.allIn = true;
}

// ── Acting ─────────────────────────────────────────────────────────────────

export interface Legal {
  seat: number;
  player: string;
  /** Chips needed to call in full. */
  toCall: number;
  fold: true;
  check: boolean;
  /** Chips a call adds (less than toCall if it's all in), or null if there's nothing to call. */
  call: number | null;
  /** "Bet to" range, or null. */
  bet: { min: number; max: number } | null;
  /** "Raise to" range, or null. */
  raise: { min: number; max: number } | null;
  /** The total this street if the player goes all in, or null if that's not allowed. */
  allIn: number | null;
}

function canAct(p: HandPlayer): boolean {
  return !p.folded && !p.allIn && !p.left;
}

function legalFor(h: HandState, p: HandPlayer): Legal {
  const toCall = Math.max(0, h.currentBet - p.committed);
  const opponents = h.players.some((q) => q !== p && canAct(q));
  const canRaise = !p.acted && opponents;
  const maxTo = p.committed + p.stack;
  const bet = h.currentBet === 0 && canRaise && maxTo >= h.minRaise ? { min: h.minRaise, max: maxTo } : null;
  const minRaiseTo = h.currentBet + h.minRaise;
  const raise = h.currentBet > 0 && canRaise && maxTo >= minRaiseTo ? { min: minRaiseTo, max: maxTo } : null;
  const allIn = p.stack > 0 && ((toCall > 0 && maxTo <= h.currentBet) || canRaise) ? maxTo : null;
  return { seat: p.seat, player: p.player, toCall, fold: true, check: toCall === 0, call: toCall > 0 ? Math.min(toCall, p.stack) : null, bet, raise, allIn };
}

/** The player to act's choices, or null if it isn't that player's turn. */
export function legalActions(s: TableState, player: string): Legal | null {
  if (!handActive(s) || s.hand!.toAct === null) return null;
  const p = s.hand!.players.find((q) => q.seat === s.hand!.toAct)!;
  return p.player === player ? legalFor(s.hand!, p) : null;
}

/** Who acts next, or null if nobody does. */
export function toAct(s: TableState): { seat: number; player: string } | null {
  if (!handActive(s) || s.hand!.toAct === null) return null;
  const p = s.hand!.players.find((q) => q.seat === s.hand!.toAct)!;
  return { seat: p.seat, player: p.player };
}

export function act(state: TableState, player: string, action: Action): TableState {
  return doAct(state, player, action, false);
}

/** The player to act ran out of time: they check if they can, else fold. */
export function timeout(state: TableState): TableState {
  const who = toAct(state);
  if (!who) throw new EngineError("no_hand", "nobody is to act");
  // Out of time is a fold, even where a check was free: the app then takes
  // the player off the table.
  return doAct(state, who.player, { type: "fold" }, true);
}

function doAct(state: TableState, player: string, action: Action, timedOut: boolean): TableState {
  const s = clone(state);
  if (!handActive(s)) throw new EngineError("no_hand", "no hand is in progress");
  const h = s.hand!;
  const p = h.players.find((q) => q.seat === h.toAct);
  if (!p || p.player !== player) throw new EngineError("not_your_turn", `it's not ${player}'s turn`);
  const legal = legalFor(h, p);
  const amount = action?.amount;
  const before = p.committed;
  const refuse = (why: string) => {
    throw new EngineError("illegal_action", why);
  };
  switch (action?.type) {
    case "fold":
      p.folded = true;
      break;
    case "check":
      if (!legal.check) refuse(`${player} can't check: ${legal.toCall} to call`);
      break;
    case "call":
      if (legal.call === null) refuse(`${player} has nothing to call`);
      put(p, legal.call!);
      break;
    case "bet":
    case "raise": {
      const range = action.type === "bet" ? legal.bet : legal.raise;
      if (!range) refuse(`${player} can't ${action.type} now`);
      if (!isInt(amount) || amount < range!.min || amount > range!.max)
        refuse(`a ${action.type} is to between ${range!.min} and ${range!.max}`);
      put(p, amount! - p.committed);
      break;
    }
    case "allin":
      if (legal.allIn === null) refuse(`${player} can't go all in now: call or fold`);
      put(p, p.stack);
      break;
    default:
      refuse("the action is one of fold, check, call, bet, raise or allin");
  }
  if (p.committed > h.currentBet) {
    const raiseBy = p.committed - h.currentBet;
    if (raiseBy >= h.minRaise) {
      // A full bet or raise reopens the betting for everyone else.
      h.minRaise = raiseBy;
      for (const q of h.players) if (q !== p) q.acted = false;
    }
    h.currentBet = p.committed;
    h.lastAggressor = p.seat;
  }
  p.acted = true;
  h.log.push({
    type: "action",
    seat: p.seat,
    player,
    action: action.type,
    amount: p.committed - before,
    to: p.committed,
    allIn: p.allIn,
    ...(timedOut ? { timeout: true as const } : {}),
  });
  advance(s);
  return s;
}

function needsToAct(h: HandState, p: HandPlayer): boolean {
  if (!canAct(p)) return false;
  if (p.committed < h.currentBet) return true;
  // Matched and nobody left to bet against: nothing to decide.
  if (!h.players.some((q) => q !== p && canAct(q))) return false;
  return !p.acted;
}

/** The first seat from `start` (inclusive, if `inclusive`) clockwise that needs to act. */
function firstToAct(h: HandState, start: number, n: number, inclusive: boolean): number | null {
  for (let k = inclusive ? 0 : 1; k <= n; k++) {
    const seat = (start + k) % n;
    const p = h.players.find((q) => q.seat === seat);
    if (p && needsToAct(h, p)) return seat;
  }
  return null;
}

function live(h: HandState): HandPlayer[] {
  return h.players.filter((p) => !p.folded);
}

function advance(s: TableState): void {
  const h = s.hand!;
  if (live(h).length === 1) {
    returnUncalled(h);
    finish(s, false);
    return;
  }
  h.toAct = firstToAct(h, h.toAct!, s.seats.length, false);
  if (h.toAct === null) endStreet(s);
}

function returnUncalled(h: HandState): void {
  const sorted = [...h.players].sort((a, b) => b.committed - a.committed);
  const [top, second] = sorted;
  if (!top || !second || top.committed <= second.committed) return;
  const excess = top.committed - second.committed;
  top.committed -= excess;
  top.total -= excess;
  top.stack += excess;
  if (top.stack > 0) top.allIn = false;
  h.log.push({ type: "return", seat: top.seat, player: top.player, amount: excess });
}

function endStreet(s: TableState): void {
  const h = s.hand!;
  returnUncalled(h);
  if (live(h).length === 1) {
    finish(s, false);
    return;
  }
  const n = s.seats.length;
  const betting = () => h.players.filter(canAct).length >= 2;
  while (h.street !== "river") {
    deal(h);
    for (const p of h.players) {
      p.committed = 0;
      p.acted = false;
    }
    h.currentBet = 0;
    h.minRaise = s.config.bigBlind;
    if (betting()) {
      h.lastAggressor = null;
      h.toAct = firstToAct(h, h.button, n, false);
      if (h.toAct !== null) return;
    }
  }
  finish(s, true);
}

function deal(h: HandState): void {
  const street = h.street === "preflop" ? "flop" : h.street === "flop" ? "turn" : "river";
  h.next++; // burn
  const count = street === "flop" ? 3 : 1;
  const cards = h.deck.slice(h.next, h.next + count);
  h.next += count;
  h.board.push(...cards);
  h.street = street;
  h.log.push({ type: "deal", street, cards });
}

// ── Pots and showdown ──────────────────────────────────────────────────────

/** Side pots from what each player has put in: one per all-in level, merged where the same players are eligible. */
export function potsOf(players: readonly HandPlayer[]): Pot[] {
  const levels = [...new Set(players.map((p) => p.total).filter((t) => t > 0))].sort((a, b) => a - b);
  const pots: Pot[] = [];
  let prev = 0;
  for (const level of levels) {
    const amount = players.reduce((sum, p) => sum + Math.max(0, Math.min(p.total, level) - prev), 0);
    const eligible = players.filter((p) => !p.folded && p.total >= level).map((p) => p.player);
    prev = level;
    const last = pots[pots.length - 1];
    if (eligible.length === 0) {
      // Only folded players reached this level. Returning uncalled bets
      // prevents it, but the chips must still go somewhere.
      if (last) last.amount += amount;
      else pots.push({ amount, eligible: players.filter((p) => !p.folded).map((p) => p.player) });
    } else if (last && last.eligible.join("\n") === eligible.join("\n")) last.amount += amount;
    else pots.push({ amount, eligible });
  }
  return pots;
}

/**
 * The pots while betting is open: a side pot only above a player who is all
 * in. Bets not yet called stay in the top pot (potsOf would split them off,
 * which is right only once uncalled bets have come back).
 */
export function openPots(players: readonly HandPlayer[]): Pot[] {
  const caps = [...new Set(players.filter((p) => p.allIn && !p.folded).map((p) => p.total))].sort((a, b) => a - b);
  const pots: Pot[] = [];
  let prev = 0;
  for (const level of [...caps, Infinity]) {
    const amount = players.reduce((sum, p) => sum + Math.max(0, Math.min(p.total, level) - prev), 0);
    const eligible = players.filter((p) => !p.folded && (!p.allIn || p.total >= level)).map((p) => p.player);
    prev = level;
    if (amount === 0) continue;
    const last = pots[pots.length - 1];
    if (last && (eligible.length === 0 || last.eligible.join("\n") === eligible.join("\n"))) last.amount += amount;
    else pots.push({ amount, eligible: eligible.length ? eligible : players.filter((p) => !p.folded).map((p) => p.player) });
  }
  return pots;
}

function finish(s: TableState, showdown: boolean): void {
  const h = s.hand!;
  const n = s.seats.length;
  h.toAct = null;
  const pots = potsOf(h.players);
  const values = new Map<string, HandValue>();
  const shown: HandResult["shown"] = [];
  if (showdown) {
    const first = h.lastAggressor !== null && !h.players.find((p) => p.seat === h.lastAggressor)!.folded ? h.lastAggressor : null;
    for (const p of clockwise(h, first ?? h.button, n, first !== null)) {
      if (p.folded) continue;
      const v = evaluate([...p.hole, ...h.board]);
      values.set(p.player, v);
      shown.push({ seat: p.seat, player: p.player, cards: [...p.hole], hand: v.name });
      h.log.push({ type: "show", seat: p.seat, player: p.player, cards: [...p.hole], hand: v.name });
    }
  }
  const leftOfButton = clockwise(h, h.button, n, false);
  const awards: PotAward[] = pots.map((pot, i) => {
    let winners: string[];
    let hand: string | undefined;
    if (showdown && pot.eligible.length > 1) {
      const best = Math.max(...pot.eligible.map((x) => values.get(x)!.score));
      winners = pot.eligible.filter((x) => values.get(x)!.score === best);
      hand = values.get(winners[0]!)!.name;
    } else winners = [...pot.eligible];
    winners = leftOfButton.map((p) => p.player).filter((x) => winners.includes(x));
    const share = Math.floor(pot.amount / winners.length);
    let odd = pot.amount - share * winners.length;
    const shares: Record<string, number> = {};
    for (const w of winners) {
      shares[w] = share + (odd > 0 ? 1 : 0);
      if (odd > 0) odd--;
      h.players.find((p) => p.player === w)!.stack += shares[w]!;
    }
    h.log.push({ type: "award", pot: i, amount: pot.amount, winners, shares, ...(hand ? { hand } : {}) });
    return { ...pot, winners, shares, ...(hand ? { hand } : {}) };
  });
  for (const p of h.players) {
    p.total = 0;
    p.committed = 0;
  }

  const busted: string[] = [];
  const left: { player: string; amount: number }[] = [];
  for (const p of h.players) {
    if (p.left) {
      // Paid out when they left (they'd folded, so they can't have won since).
      if (p.stack > 0) {
        left.push({ player: p.player, amount: p.stack });
        s.ledger.out += p.stack;
        p.stack = 0;
      }
      continue;
    }
    const seat = s.seats[p.seat];
    if (!seat || seat.player !== p.player) continue;
    seat.stack = p.stack;
    if (seat.stack === 0) {
      busted.push(p.player);
      s.seats[p.seat] = null;
    }
  }
  h.street = "complete";
  h.result = { showdown, pots: awards, shown, net: netOf(h), seed: h.seed, busted, left };
  h.log.push({ type: "end", seed: h.seed, net: h.result.net });
}

/** Each player's chips won (positive) or lost over the hand, from the log: what they put in and got back. */
function netOf(h: HandState): Record<string, number> {
  const net: Record<string, number> = {};
  for (const p of h.players) net[p.player] = 0;
  for (const e of h.log) {
    if (e.type === "post" || e.type === "action") net[e.player]! -= e.amount;
    else if (e.type === "return") net[e.player]! += e.amount;
    else if (e.type === "award") for (const [w, a] of Object.entries(e.shares)) net[w]! += a;
  }
  return net;
}

/** Hand players clockwise from `from` (inclusive or not), all of them once. */
function clockwise(h: HandState, from: number, n: number, inclusive: boolean): HandPlayer[] {
  const out: HandPlayer[] = [];
  for (let k = inclusive ? 0 : 1; out.length < h.players.length && k <= n; k++) {
    const p = h.players.find((q) => q.seat === (from + k) % n);
    if (p && !out.includes(p)) out.push(p);
  }
  return out;
}

// ── Checks ─────────────────────────────────────────────────────────────────

/** Every chip at the table: stacks, plus what's in the pot during a hand. */
export function chipsInPlay(s: TableState): number {
  if (!handActive(s)) return s.seats.reduce((sum, x) => sum + (x?.stack ?? 0), 0);
  const h = s.hand!;
  const inHand = new Set(h.players.filter((p) => !p.left).map((p) => p.seat));
  let sum = 0;
  for (const p of h.players) sum += p.stack + p.total;
  s.seats.forEach((x, i) => {
    if (x && !(inHand.has(i) && h.players.find((p) => p.seat === i && !p.left)?.player === x.player)) sum += x.stack;
  });
  return sum;
}

/** Throws if chips were made or lost: the table must hold what was brought in minus what was taken away. */
export function checkInvariants(s: TableState): void {
  const expected = s.ledger.in - s.ledger.out;
  const actual = chipsInPlay(s);
  if (actual !== expected) throw new Error(`chips not conserved: ${actual} at the table, ${expected} expected`);
  const negative = s.seats.some((x) => x && x.stack < 0) || (s.hand?.players.some((p) => p.stack < 0 || p.total < 0) ?? false);
  if (negative) throw new Error("a stack went negative");
}
