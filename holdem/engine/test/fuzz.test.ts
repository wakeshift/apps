// Random play over thousands of hands. After every step: chips are conserved,
// there's always someone to act until the hand ends (and they have a legal
// action), no view shows a hole card that hasn't been shown, the state
// survives a JSON round trip, and every finished hand verifies from its log.

import assert from "node:assert/strict";
import { test } from "node:test";
import type { Card } from "../src/cards.js";
import {
  type Action,
  act,
  addChips,
  checkInvariants,
  createTable,
  handActive,
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
import { playerView, publicView } from "../src/views.js";
import { cardsIn, prng, seedOf } from "./helpers.js";

function randomAction(s: TableState, rand: () => number): Action | "timeout" {
  const who = toAct(s)!;
  const legal = legalActions(s, who.player)!;
  const options: Action[] = [{ type: "fold" }];
  if (legal.check) options.push({ type: "check" }, { type: "check" });
  if (legal.call !== null) options.push({ type: "call" }, { type: "call" }, { type: "call" });
  const pick = (r: { min: number; max: number }) => r.min + Math.floor(rand() ** 3 * (r.max - r.min + 1));
  if (legal.bet) options.push({ type: "bet", amount: pick(legal.bet) }, { type: "bet", amount: legal.bet.min });
  if (legal.raise) options.push({ type: "raise", amount: pick(legal.raise) }, { type: "raise", amount: legal.raise.min });
  if (legal.allIn !== null && rand() < 0.3) options.push({ type: "allin" });
  if (rand() < 0.03) return "timeout";
  return options[Math.floor(rand() * options.length)]!;
}

/** Hole cards that no view of the state may contain (for viewers other than their owner). */
function hiddenCards(s: TableState): Map<string, Card[]> {
  const h = s.hand;
  const out = new Map<string, Card[]>();
  if (!h) return out;
  const shown = new Set(h.log.flatMap((e) => (e.type === "show" ? [e.player] : [])));
  for (const p of h.players) if (!shown.has(p.player)) out.set(p.player, p.hole);
  return out;
}

function checkViews(s: TableState): void {
  const hidden = hiddenCards(s);
  const all = [...hidden.values()].flat();
  const pub = cardsIn(publicView(s));
  for (const c of all) assert.ok(!pub.has(c), "a hidden hole card in the public view");
  for (const seat of s.seats) {
    if (!seat) continue;
    const mine = cardsIn(playerView(s, seat.player));
    for (const [owner, cards] of hidden) if (owner !== seat.player) for (const c of cards) assert.ok(!mine.has(c), "someone else's hole card in a player view");
  }
  if (handActive(s)) assert.ok(!JSON.stringify(publicView(s)).includes(s.hand!.seed), "the seed before the hand ended");
}

function play(tableSeed: number, hands: number): { hands: number; actions: number; showdowns: number; sidePots: number } {
  const rand = prng(tableSeed);
  const seats = 2 + Math.floor(rand() * 8);
  const bb = 2 * (1 + Math.floor(rand() * 5));
  let s = createTable({ smallBlind: bb / 2, bigBlind: bb, minBuyIn: bb * 10, maxBuyIn: bb * 200, seats });
  let names = 0;
  const seatSomeone = () => {
    const free = s.seats.flatMap((x, i) => (x ? [] : [i]));
    if (!free.length) return;
    const buyIn = s.config.minBuyIn + Math.floor(rand() * (s.config.maxBuyIn - s.config.minBuyIn + 1));
    s = sit(s, free[Math.floor(rand() * free.length)]!, `t${tableSeed}p${names++}`, buyIn);
  };
  const n0 = 2 + Math.floor(rand() * (seats - 1));
  for (let i = 0; i < n0; i++) seatSomeone();
  let played = 0;
  let actions = 0;
  let showdowns = 0;
  let sidePots = 0;
  for (let k = 0; k < hands; k++) {
    // Between hands: people come and go, top up, sit out.
    const r = rand();
    const seated = s.seats.filter((x) => x !== null);
    if (r < 0.1 || seated.length < 2) seatSomeone();
    else if (r < 0.15 && seated.length > 2) s = leave(s, seated[Math.floor(rand() * seated.length)]!.player).state;
    else if (r < 0.2) {
      const x = seated[Math.floor(rand() * seated.length)]!;
      if (x.stack < s.config.maxBuyIn) s = addChips(s, x.player, 1 + Math.floor(rand() * (s.config.maxBuyIn - x.stack)));
    } else if (r < 0.25) {
      const x = seated[Math.floor(rand() * seated.length)]!;
      s = sitOut(s, x.player, !x.sittingOut);
    }
    checkInvariants(s);
    const ready = s.seats.filter((x) => x && !x.sittingOut && x.stack > 0).length;
    if (ready < 2) {
      for (const x of s.seats) if (x?.sittingOut) s = sitOut(s, x.player, false);
      if (s.seats.filter((x) => x && x.stack > 0).length < 2) seatSomeone();
      if (s.seats.filter((x) => x && x.stack > 0).length < 2) seatSomeone();
    }
    s = startHand(s, { seed: seedOf(`fuzz ${tableSeed} ${k}`), handId: `${tableSeed}-${k}` });
    played++;
    checkInvariants(s);
    checkViews(s);
    let steps = 0;
    while (handActive(s)) {
      assert.ok(++steps < 500, "a hand that never ends");
      const who = toAct(s);
      assert.ok(who, "an unfinished hand with nobody to act");
      const legal = legalActions(s, who.player);
      assert.ok(legal, "the player to act has no choices");
      // Mid-hand, folded players sometimes leave, and newcomers sit down.
      if (rand() < 0.02) {
        const folded = s.hand!.players.find((p) => p.folded && !p.left);
        if (folded) s = leave(s, folded.player).state;
      }
      if (rand() < 0.01) seatSomeone();
      const a = randomAction(s, rand);
      s = a === "timeout" ? timeout(s) : act(s, who.player, a);
      actions++;
      checkInvariants(s);
      checkViews(s);
      if (rand() < 0.05) {
        const copy = JSON.parse(JSON.stringify(s)) as TableState;
        assert.deepEqual(copy, s, "the state survives JSON");
        s = copy;
      }
    }
    const result = s.hand!.result!;
    if (result.showdown) showdowns++;
    if (result.pots.length > 1) sidePots++;
    // Pots add up to what everyone put in, minus what came back.
    const net = Object.values(result.net).reduce((a, b) => a + b, 0);
    assert.equal(net, 0, "a hand's net results sum to zero");
    const check = verifyHand(s.hand!.log);
    assert.ok(check.ok, check.problems.join("; "));
  }
  return { hands: played, actions, showdowns, sidePots };
}

test("random play: 200 tables of 25 hands", () => {
  let totals = { hands: 0, actions: 0, showdowns: 0, sidePots: 0 };
  for (let t = 1; t <= 200; t++) {
    const r = play(t, 25);
    totals = { hands: totals.hands + r.hands, actions: totals.actions + r.actions, showdowns: totals.showdowns + r.showdowns, sidePots: totals.sidePots + r.sidePots };
  }
  assert.equal(totals.hands, 5000);
  // The random play reaches the interesting cases, not just folds.
  assert.ok(totals.showdowns > 500, `showdowns ${totals.showdowns}`);
  assert.ok(totals.sidePots > 50, `side pots ${totals.sidePots}`);
  console.log(`fuzz: ${JSON.stringify(totals)}`);
});

test("illegal actions are always refused and change nothing", () => {
  const rand = prng(99);
  let s = createTable({ smallBlind: 1, bigBlind: 2, minBuyIn: 20, maxBuyIn: 200, seats: 6 });
  for (let i = 0; i < 6; i++) s = sit(s, i, `x${i}`, 20 + Math.floor(rand() * 181));
  for (let k = 0; k < 300; k++) {
    if (s.seats.filter((x) => x && x.stack > 0).length < 2) break;
    s = startHand(s, { seed: seedOf(`illegal ${k}`), handId: String(k) });
    while (handActive(s)) {
      const who = toAct(s)!;
      const legal = legalActions(s, who.player)!;
      const before = JSON.stringify(s);
      const bad: Action[] = [];
      if (!legal.check) bad.push({ type: "check" });
      if (legal.call === null) bad.push({ type: "call" });
      if (!legal.bet) bad.push({ type: "bet", amount: 2 });
      else bad.push({ type: "bet", amount: legal.bet.min - 1 }, { type: "bet", amount: legal.bet.max + 1 }, { type: "bet", amount: legal.bet.min + 0.5 });
      if (!legal.raise) bad.push({ type: "raise", amount: 1000 });
      else bad.push({ type: "raise", amount: legal.raise.min - 1 }, { type: "raise", amount: legal.raise.max + 1 });
      if (legal.allIn === null) bad.push({ type: "allin" });
      for (const b of bad) {
        assert.throws(() => act(s, who.player, b), (e: unknown) => e instanceof Error && cardsIn((e as Error).message.split(/[^A-Za-z0-9]+/)).size === 0);
        assert.equal(JSON.stringify(s), before);
      }
      const other = s.hand!.players.find((p) => p.player !== who.player && !p.folded);
      if (other) assert.throws(() => act(s, other.player, { type: "fold" }));
      const a = randomAction(s, rand);
      s = a === "timeout" ? timeout(s) : act(s, who.player, a);
      checkInvariants(s);
    }
  }
});
