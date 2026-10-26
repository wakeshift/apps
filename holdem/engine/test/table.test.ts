import assert from "node:assert/strict";
import { test } from "node:test";
import { EngineError } from "../src/errors.js";
import {
  act,
  type ActionType,
  addChips,
  checkInvariants,
  createTable,
  leave,
  legalActions,
  sit,
  sitOut,
  startHand,
  type TableState,
  timeout,
  toAct,
} from "../src/table.js";
import { verifyHand } from "../src/verify.js";
import { history, playerView, publicView } from "../src/views.js";
import { cardsIn, CONFIG, rig, seedOf, table } from "./helpers.js";

/** Acts as whoever's turn it is, checking it's the expected player, and the invariants after. */
function go(s: TableState, player: string, type: ActionType, amount?: number): TableState {
  assert.equal(toAct(s)?.player, player, `expected ${player} to act`);
  const next = act(s, player, { type, ...(amount !== undefined ? { amount } : {}) });
  checkInvariants(next);
  return next;
}

const start = (s: TableState, label = "hand") => {
  const next = startHand(s, { seed: seedOf(label) });
  checkInvariants(next);
  return next;
};

const stack = (s: TableState, player: string) => s.seats.find((x) => x?.player === player)?.stack;
const engineError = (code: string) => (e: unknown) => e instanceof EngineError && e.code === code;

test("table settings are checked", () => {
  assert.throws(() => createTable({ ...CONFIG, seats: 1 }), engineError("bad_config"));
  assert.throws(() => createTable({ ...CONFIG, seats: 10 }), engineError("bad_config"));
  assert.throws(() => createTable({ ...CONFIG, bigBlind: 0, smallBlind: 0 }), engineError("bad_config"));
  assert.throws(() => createTable({ ...CONFIG, smallBlind: 3 }), engineError("bad_config"));
  assert.throws(() => createTable({ ...CONFIG, minBuyIn: 1 }), engineError("bad_config"));
  assert.throws(() => createTable({ ...CONFIG, maxBuyIn: 10 }), engineError("bad_config"));
  assert.throws(() => createTable({ ...CONFIG, bigBlind: 2.5 }), engineError("bad_config"));
  assert.equal(createTable(CONFIG).seats.length, 9);
});

test("sitting down, topping up, sitting out and leaving", () => {
  let s = createTable(CONFIG);
  s = sit(s, 3, "ada", 100);
  assert.throws(() => sit(s, 3, "bob", 100), engineError("seat_taken"));
  assert.throws(() => sit(s, 4, "ada", 100), engineError("already_seated"));
  assert.throws(() => sit(s, 9, "bob", 100), engineError("bad_seat"));
  assert.throws(() => sit(s, 4, "bob", 10), engineError("bad_amount"));
  assert.throws(() => sit(s, 4, "bob", 401), engineError("bad_amount"));
  s = sit(s, 4, "bob", 50);
  s = addChips(s, "bob", 350);
  assert.throws(() => addChips(s, "bob", 1), engineError("bad_amount"));
  assert.throws(() => addChips(s, "cy", 1), engineError("not_seated"));
  s = sitOut(s, "bob", true);
  assert.throws(() => startHand(s), engineError("not_enough_players"));
  s = sitOut(s, "bob", false);
  const { state, cashOut } = leave(s, "bob");
  assert.equal(cashOut, 400);
  assert.equal(state.seats[4], null);
  assert.deepEqual(state.ledger, { in: 500, out: 400 });
  checkInvariants(state);
});

test("heads-up: the button posts the small blind, acts first before the flop and last after it", () => {
  let s = start(table([100, 100]));
  const h = s.hand!;
  assert.equal(h.button, 0);
  assert.equal(h.smallBlindSeat, 0);
  assert.equal(h.bigBlindSeat, 1);
  assert.deepEqual(h.players.map((p) => p.committed), [1, 2]);
  s = go(s, "p0", "call");
  s = go(s, "p1", "check"); // the big blind's option
  assert.equal(s.hand!.street, "flop");
  s = go(s, "p1", "check"); // after the flop the big blind acts first
  s = go(s, "p0", "check");
  assert.equal(s.hand!.street, "turn");
  // The next hand moves the button.
  s = go(s, "p1", "bet", 10);
  s = go(s, "p0", "fold");
  assert.equal(s.hand!.street, "complete");
  s = start(s, "next");
  assert.equal(s.hand!.button, 1);
  assert.equal(toAct(s)?.player, "p1");
});

test("three or more: blinds left of the button, the seat after the big blind acts first, the big blind has the option", () => {
  let s = start(table([100, 100, 100, 100]));
  const h = s.hand!;
  assert.deepEqual([h.button, h.smallBlindSeat, h.bigBlindSeat], [0, 1, 2]);
  s = go(s, "p3", "call");
  s = go(s, "p0", "call");
  s = go(s, "p1", "call");
  const legal = legalActions(s, "p2")!;
  assert.equal(legal.check, true);
  assert.deepEqual(legal.raise, { min: 4, max: 100 });
  s = go(s, "p2", "check");
  assert.equal(s.hand!.street, "flop");
  assert.equal(toAct(s)?.player, "p1"); // first live seat left of the button
});

test("minimum bets and raises", () => {
  let s = start(table([100, 100, 100]));
  // Button p0 is first to act three-handed (after the big blind).
  assert.deepEqual(legalActions(s, "p0")!.raise, { min: 4, max: 100 });
  assert.throws(() => act(s, "p0", { type: "raise", amount: 3 }), engineError("illegal_action"));
  assert.throws(() => act(s, "p0", { type: "bet", amount: 4 }), engineError("illegal_action")); // there's already a bet: it's a raise
  s = go(s, "p0", "raise", 6); // raise by 4
  assert.deepEqual(legalActions(s, "p1")!.raise, { min: 10, max: 100 });
  assert.throws(() => act(s, "p1", { type: "raise", amount: 9 }), engineError("illegal_action"));
  s = go(s, "p1", "raise", 16); // raise by 10
  assert.deepEqual(legalActions(s, "p2")!.raise, { min: 26, max: 100 });
  s = go(s, "p2", "fold");
  s = go(s, "p0", "call");
  assert.equal(s.hand!.street, "flop");
  // After the flop the minimum bet is the big blind.
  assert.deepEqual(legalActions(s, "p1")!.bet, { min: 2, max: 84 });
  assert.equal(legalActions(s, "p1")!.raise, null);
  assert.throws(() => act(s, "p1", { type: "bet", amount: 1 }), engineError("illegal_action"));
  assert.throws(() => act(s, "p1", { type: "call" }), engineError("illegal_action"));
  s = go(s, "p1", "bet", 5);
  assert.deepEqual(legalActions(s, "p0")!.raise, { min: 10, max: 84 });
  assert.throws(() => act(s, "p0", { type: "check" }), engineError("illegal_action"));
  assert.throws(() => act(s, "p1", { type: "fold" }), engineError("not_your_turn"));
});

test("check-raise: a player who checked may raise a full bet behind them", () => {
  let s = start(table([100, 100, 100]));
  s = go(s, "p0", "call");
  s = go(s, "p1", "call");
  s = go(s, "p2", "check");
  s = go(s, "p1", "check");
  s = go(s, "p2", "check");
  s = go(s, "p0", "bet", 6);
  assert.deepEqual(legalActions(s, "p1")!.raise, { min: 12, max: 98 });
  s = go(s, "p1", "raise", 20);
  assert.deepEqual(legalActions(s, "p2")!.raise, { min: 34, max: 98 });
});

test("an all-in short of a full raise doesn't reopen the betting for players who already acted", () => {
  let s = start(table([100, 100, 14]));
  s = go(s, "p0", "raise", 10); // a raise of 8
  s = go(s, "p1", "call");
  s = go(s, "p2", "allin"); // to 14: 4 more, less than a full raise of 8
  assert.equal(s.hand!.currentBet, 14);
  assert.equal(s.hand!.minRaise, 8);
  const legal = legalActions(s, "p0")!;
  assert.equal(legal.call, 4);
  assert.equal(legal.raise, null);
  assert.equal(legal.allIn, null);
  assert.throws(() => act(s, "p0", { type: "raise", amount: 30 }), engineError("illegal_action"));
  assert.throws(() => act(s, "p0", { type: "allin" }), engineError("illegal_action"));
  s = go(s, "p0", "call");
  s = go(s, "p1", "call");
  assert.equal(s.hand!.street, "flop");
});

test("a player who hasn't acted can still raise after a short all-in, by a full raise on top", () => {
  let s = start(table([100, 100, 100, 14]));
  // Button p0, small blind p1, big blind p2, p3 first.
  s = go(s, "p3", "allin"); // to 14: a full raise of 12 from 2
  s = go(s, "p0", "raise", 30); // raise of 16
  const legal = legalActions(s, "p1")!;
  assert.deepEqual(legal.raise, { min: 46, max: 100 });
  s = go(s, "p1", "call");
  s = go(s, "p2", "allin"); // 100: a full raise
  assert.deepEqual(legalActions(s, "p0")!.allIn, 100); // reopened
});

test("while betting is open, side pots start only above an all-in player", () => {
  let s = start(table([20, 100, 100]));
  // The blinds: one pot, though the big blind hasn't been called.
  assert.deepEqual(publicView(s).hand!.pots, [{ amount: 3, eligible: ["p0", "p1", "p2"] }]);
  s = go(s, "p0", "allin");
  s = go(s, "p1", "raise", 50);
  // p0's 20 from everyone so far is the main pot; the rest is a side pot p0 can't win.
  assert.deepEqual(publicView(s).hand!.pots, [
    { amount: 20 + 20 + 2, eligible: ["p0", "p1", "p2"] },
    { amount: 30, eligible: ["p1", "p2"] },
  ]);
});

test("side pots with three all-ins, and the uncalled part of the biggest stack comes back", () => {
  let s = start(table([20, 40, 60, 100]));
  s = rig(s, { p0: ["Ac", "Ad"], p1: ["Kc", "Kd"], p2: ["Qc", "Qd"], p3: ["8c", "4d"] }, ["2c", "7d", "9h", "Js", "3s"]);
  s = go(s, "p3", "allin");
  s = go(s, "p0", "allin");
  s = go(s, "p1", "allin");
  s = go(s, "p2", "allin");
  const r = s.hand!.result!;
  assert.equal(s.hand!.street, "complete");
  assert.equal(r.showdown, true);
  assert.deepEqual(
    r.pots.map((p) => [p.amount, p.eligible, p.winners]),
    [
      [80, ["p0", "p1", "p2", "p3"], ["p0"]],
      [60, ["p1", "p2", "p3"], ["p1"]],
      [40, ["p2", "p3"], ["p2"]],
    ],
  );
  assert.ok(s.hand!.log.some((e) => e.type === "return" && e.player === "p3" && e.amount === 40));
  assert.deepEqual([stack(s, "p0"), stack(s, "p1"), stack(s, "p2"), stack(s, "p3")], [80, 60, 40, 40]);
  assert.deepEqual(r.net, { p0: 60, p1: 20, p2: -20, p3: -60 });
  assert.equal(s.hand!.board.length, 5);
});

test("split pots, with the odd chip to the first winner left of the button", () => {
  let s = start(table([100, 100, 100]));
  s = rig(s, {}, ["As", "Ks", "Qs", "Js", "Ts"]); // a royal flush on the board: everyone ties
  s = go(s, "p0", "call");
  s = go(s, "p1", "fold"); // leaves its small blind: the pot is 5
  s = go(s, "p2", "check");
  for (const street of ["flop", "turn", "river"]) {
    assert.equal(s.hand!.street, street);
    s = go(s, "p2", "check");
    s = go(s, "p0", "check");
  }
  const r = s.hand!.result!;
  assert.deepEqual(r.pots, [{ amount: 5, eligible: ["p0", "p2"], winners: ["p2", "p0"], shares: { p2: 3, p0: 2 }, hand: "a royal flush" }]);
  assert.deepEqual(r.net, { p0: 0, p1: -1, p2: 1 });
});

test("a three-way split: two odd chips go clockwise from the button", () => {
  let s = start(table([100, 100, 100, 100]));
  s = rig(s, {}, ["As", "Ks", "Qs", "Js", "Ts"]);
  s = go(s, "p3", "call");
  s = go(s, "p0", "call");
  s = go(s, "p1", "call");
  s = go(s, "p2", "check"); // 8 in the pot
  s = go(s, "p1", "bet", 2);
  s = go(s, "p2", "call");
  s = go(s, "p3", "fold");
  s = go(s, "p0", "call"); // 14: three ways is 4 each and two over
  for (const _ of ["turn", "river"]) {
    s = go(s, "p1", "check");
    s = go(s, "p2", "check");
    s = go(s, "p0", "check");
  }
  const r = s.hand!.result!;
  assert.equal(r.pots[0]!.amount, 14);
  assert.deepEqual(r.pots[0]!.winners, ["p1", "p2", "p0"]);
  assert.deepEqual(r.pots[0]!.shares, { p1: 5, p2: 5, p0: 4 });
});

test("uncalled bets come back when everyone folds", () => {
  let s = start(table([100, 100]));
  s = go(s, "p0", "call");
  s = go(s, "p1", "check");
  s = go(s, "p1", "bet", 10);
  s = go(s, "p0", "fold");
  const r = s.hand!.result!;
  assert.equal(r.showdown, false);
  assert.ok(s.hand!.log.some((e) => e.type === "return" && e.player === "p1" && e.amount === 10));
  assert.deepEqual(r.net, { p0: -2, p1: 2 });
  assert.deepEqual(r.shown, []);
});

test("folding round to the big blind: it wins the small blind, its own blind comes back", () => {
  let s = start(table([100, 100, 100]));
  s = go(s, "p0", "fold");
  s = go(s, "p1", "fold");
  const r = s.hand!.result!;
  assert.deepEqual(r.net, { p0: 0, p1: -1, p2: 1 });
  assert.ok(s.hand!.log.some((e) => e.type === "return" && e.player === "p2" && e.amount === 1));
});

test("a timed-out player folds, even where a check was free; chips are conserved", () => {
  let s = start(table([100, 100]));
  s = timeout(s); // p0 faces the big blind: folds
  assert.equal(s.hand!.street, "complete");
  const fold = s.hand!.log.find((e) => e.type === "action")!;
  assert.deepEqual([fold.type === "action" && fold.action, fold.type === "action" && fold.timeout], ["fold", true]);
  checkInvariants(s);
  s = start(s, "2");
  s = go(s, "p1", "call");
  s = timeout(s); // p0, the big blind, could check: folds anyway
  assert.equal(s.hand!.street, "complete");
  assert.ok(s.hand!.log.some((e) => e.type === "action" && e.action === "fold" && e.timeout));
  assert.ok(!s.hand!.log.some((e) => e.type === "action" && e.action === "check" && e.timeout));
  checkInvariants(s);
  assert.throws(() => timeout({ ...s, hand: null }), engineError("no_hand"));
});

test("a short big blind posts all in; the call is still the full big blind, and the extra comes back", () => {
  let s = start(table([100, 1]));
  assert.equal(s.hand!.players[1]!.allIn, true);
  assert.equal(s.hand!.currentBet, 2);
  const legal = legalActions(s, "p0")!;
  assert.equal(legal.call, 1);
  assert.equal(legal.raise, null, "nobody left to raise against");
  assert.equal(legal.allIn, null);
  s = go(s, "p0", "call");
  assert.equal(s.hand!.street, "complete");
  assert.ok(s.hand!.log.some((e) => e.type === "return" && e.player === "p0" && e.amount === 1));
  assert.equal(s.hand!.board.length, 5);
  checkInvariants(s);
});

test("a short stack's opening bet below the big blind is an all-in only", () => {
  let s = start(table([100, 3]));
  s = go(s, "p0", "call");
  s = go(s, "p1", "check");
  // p1 has 1 chip left: below the minimum bet of 2.
  const legal = legalActions(s, "p1")!;
  assert.equal(legal.bet, null);
  assert.equal(legal.allIn, 1);
  s = go(s, "p1", "allin");
  assert.equal(s.hand!.currentBet, 1);
  const p0 = legalActions(s, "p0")!;
  assert.equal(p0.call, 1);
  assert.equal(p0.raise, null);
  s = go(s, "p0", "call");
  assert.equal(s.hand!.street, "complete");
});

test("players with no chips left lose their seat", () => {
  let s = start(table([100, 40]));
  s = rig(s, { p0: ["Ac", "Ad"], p1: ["7c", "2d"] }, ["Ah", "Kd", "9h", "4s", "3s"]);
  s = go(s, "p0", "allin");
  s = go(s, "p1", "allin");
  const r = s.hand!.result!;
  assert.deepEqual(r.busted, ["p1"]);
  assert.equal(s.seats[1], null);
  assert.equal(stack(s, "p0"), 140);
  assert.throws(() => startHand(s), engineError("not_enough_players"));
});

test("players who sit down during a hand are dealt in from the next; sitting out skips a hand", () => {
  let s = start(table([100, 100]));
  s = sit(s, 5, "late", 100);
  assert.equal(s.hand!.players.length, 2);
  assert.equal(publicView(s).seats[5]!.inHand, false);
  s = go(s, "p0", "fold");
  s = sitOut(s, "p0", true);
  s = start(s, "2");
  assert.deepEqual(s.hand!.players.map((p) => p.player), ["p1", "late"]);
  checkInvariants(s);
});

test("leaving: not while live in a hand; after folding, paid out at once", () => {
  let s = start(table([100, 100, 100]));
  assert.throws(() => leave(s, "p0"), engineError("still_in_hand"));
  s = go(s, "p0", "fold");
  const out = leave(s, "p0");
  assert.equal(out.cashOut, 100);
  s = out.state;
  checkInvariants(s);
  s = go(s, "p1", "call");
  s = go(s, "p2", "check");
  assert.equal(publicView(s).seats[0], null);
  // Someone takes the empty seat mid-hand.
  s = sit(s, 0, "new", 50);
  checkInvariants(s);
  s = go(s, "p1", "bet", 4);
  s = go(s, "p2", "fold");
  checkInvariants(s);
  assert.equal(s.hand!.street, "complete");
  assert.deepEqual(s.ledger, { in: 350, out: 100 });
});

test("names: a safe charset, no special object keys, and not reused within a hand", () => {
  let s = createTable(CONFIG);
  for (const bad of ["", "__proto__", "constructor", "has space", "x".repeat(65), "<b>", "a\nb"])
    assert.throws(() => sit(s, 0, bad, 100), engineError("bad_name"), JSON.stringify(bad));
  s = sit(s, 0, "ws:ada@claude-code", 100);
  s = sit(s, 1, "bob", 100);
  s = sit(s, 2, "cy", 100);
  s = start(s);
  s = go(s, "ws:ada@claude-code", "fold");
  s = leave(s, "ws:ada@claude-code").state;
  // The name is still in this hand's records: someone else can't take it until the hand ends.
  assert.throws(() => sit(s, 0, "ws:ada@claude-code", 100), engineError("already_seated"));
  s = go(s, "bob", "fold");
  assert.equal(s.hand!.street, "complete");
  s = sit(s, 0, "ws:ada@claude-code", 100);
  checkInvariants(s);
});

test("showdown order: the last river aggressor shows first, else the first live seat left of the button", () => {
  const play = (riverBet: boolean) => {
    let s = start(table([100, 100, 100]));
    s = go(s, "p0", "call");
    s = go(s, "p1", "call");
    s = go(s, "p2", "check");
    for (const _ of ["flop", "turn"]) {
      s = go(s, "p1", "check");
      s = go(s, "p2", "check");
      s = go(s, "p0", "check");
    }
    s = go(s, "p1", "check");
    if (riverBet) {
      s = go(s, "p2", "bet", 2);
      s = go(s, "p0", "call");
      s = go(s, "p1", "call");
    } else {
      s = go(s, "p2", "check");
      s = go(s, "p0", "check");
    }
    return s.hand!.result!.shown.map((x) => x.player);
  };
  assert.deepEqual(play(true), ["p2", "p0", "p1"]);
  assert.deepEqual(play(false), ["p1", "p2", "p0"]);
});

test("the button moves to the next player with chips, skipping empty seats", () => {
  let s = table([100, 100, 100], {}, [1, 4, 7]);
  s = start(s, "a");
  assert.equal(s.hand!.button, 1);
  s = go(s, "p0", "fold"); // seat 1 (button) is first to act three-handed
  s = go(s, "p1", "fold");
  s = start(s, "b");
  assert.equal(s.hand!.button, 4);
  s = go(s, "p1", "fold");
  s = go(s, "p2", "fold");
  s = start(s, "c");
  assert.equal(s.hand!.button, 7);
  s = go(s, "p2", "fold");
  // The button's next seat empties: it skips to the next player with chips.
  s = leave(s, "p2").state;
  s = go(s, "p0", "fold");
  s = sit(s, 2, "p9", 100);
  s = start(s, "d");
  assert.equal(s.hand!.button, 1);
});

test("a stored state picks up where it left off", () => {
  let a = start(table([100, 100, 100]), "resume");
  a = go(a, "p0", "raise", 6);
  const b = JSON.parse(JSON.stringify(a)) as TableState;
  assert.deepEqual(b, a);
  const next = (s: TableState) => act(act(s, "p1", { type: "call" }), "p2", { type: "call" });
  assert.deepEqual(next(b), next(a));
});

test("a finished hand checks out against its commitment from the public log", () => {
  let s = start(table([100, 100, 100]), "verify");
  s = go(s, "p0", "call");
  s = go(s, "p1", "call");
  s = go(s, "p2", "check");
  while (s.hand!.street !== "complete") s = act(s, toAct(s)!.player, { type: "check" });
  const log = history(s);
  assert.deepEqual(verifyHand(log), { ok: true, problems: [] });
  assert.equal(s.hand!.result!.shown.length, 3);
  const tamperedSeed = log.map((e) => (e.type === "end" ? { ...e, seed: seedOf("other") } : e));
  assert.equal(verifyHand(tamperedSeed).ok, false);
  const tamperedBoard = log.map((e) => (e.type === "deal" && e.street === "river" ? { ...e, cards: ["2c"] } : e));
  const check = verifyHand(tamperedBoard);
  assert.equal(check.ok, false);
  assert.match(check.problems.join(), /river/);
  assert.equal(verifyHand(log.filter((e) => e.type !== "end")).ok, false);
  // A log with a street left out, repeated or out of order doesn't verify.
  const noRiver = log.filter((e) => !(e.type === "deal" && e.street === "river"));
  assert.match(verifyHand(noRiver).problems.join(), /showdown without the whole board/);
  const flop = log.find((e) => e.type === "deal" && e.street === "flop")!;
  assert.match(verifyHand([...log, flop]).problems.join(), /once each, in order/);
  const swapped = log.map((e) => (e.type === "deal" && e.street === "turn" ? log.find((x) => x.type === "deal" && x.street === "river")! : e.type === "deal" && e.street === "river" ? log.find((x) => x.type === "deal" && x.street === "turn")! : e));
  assert.equal(verifyHand(swapped).ok, false);
  const twoEnds = [...log, log[log.length - 1]!];
  assert.match(verifyHand(twoEnds).problems.join(), /exactly one start and one end/);
  // A hand that ended before the river verifies with the streets it dealt.
  let f = start(table([100, 100]), "early");
  f = go(f, "p0", "call");
  f = go(f, "p1", "check");
  f = go(f, "p1", "bet", 4);
  f = go(f, "p0", "fold");
  assert.deepEqual(verifyHand(history(f)), { ok: true, problems: [] });
});

test("views: nobody sees cards they shouldn't", () => {
  let s = start(table([100, 100, 100]), "views");
  const holes = Object.fromEntries(s.hand!.players.map((p) => [p.player, p.hole]));
  const pub = publicView(s);
  assert.equal(cardsIn(pub).size, 0, "no cards before the flop");
  assert.equal(pub.hand!.commitment, s.hand!.commitment);
  assert.ok(!JSON.stringify(pub).includes(s.hand!.seed), "the seed stays secret");
  const mine = playerView(s, "p1");
  assert.deepEqual(mine.you.hole, holes.p1);
  assert.deepEqual([...cardsIn(mine)].sort(), [...holes.p1!].sort());
  assert.equal(mine.legal, null, "not p1's turn");
  assert.notEqual(playerView(s, "p0").legal, null);
  // Errors name no cards.
  for (const bad of [{ type: "raise", amount: 1 }, { type: "check" }, { type: "dance" }] as const) {
    try {
      act(s, "p0", bad as never);
      assert.fail("should refuse");
    } catch (e) {
      assert.equal(cardsIn((e as Error).message.split(/\W+/)).size, 0);
    }
  }
  // p2 folds; the rest go to showdown: p2's cards are never shown.
  s = go(s, "p0", "call");
  s = go(s, "p1", "call");
  s = go(s, "p2", "fold");
  while (s.hand!.street !== "complete") s = act(s, toAct(s)!.player, { type: "check" });
  const end = publicView(s);
  const seen = cardsIn(end);
  for (const c of holes.p2!) assert.ok(!seen.has(c), "folded cards stay hidden");
  for (const c of [...holes.p0!, ...holes.p1!]) assert.ok(seen.has(c), "showdown cards are shown");
  assert.equal(end.hand!.result!.seed, s.hand!.seed, "the seed is revealed at the end");
});
