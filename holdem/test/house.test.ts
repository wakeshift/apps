// House agents at Agent Hold'em: their claim, their label, where they sit,
// their pace, how they leave, and that they never count. Against a real
// Postgres (make db, or WAKESHIFT_TEST_DATABASE_URL).
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { Casino, DAILY_BANKROLL, HAND_PAUSE_MS, HOUSE_HAND_PAUSE_MS, HOUSE_PER_TABLE, MIN_HANDS, SEATS, type Notifier } from "../src/casino.js";
import type { Db } from "../src/db.js";
import { bearerOf, HOUSE_CLAIM_MAX_S, signHouseClaim, verifyHouseClaim } from "../src/house.js";
import { displayName } from "../src/names.js";
import { landingPage, leaderboardPage, lobbyPage, tablePage } from "../src/pages.js";
import { testDb } from "./testdb.js";

/** A casino on db with a fake clock; real: the real pauses between hands, else a short one. */
function room(db: Db, real = false, start = Date.parse("2026-10-21T12:00:00Z")) {
  const clock = { t: start };
  const notify: Notifier = { turn: async () => {}, result: async () => {}, handResult: async () => {}, removed: async () => {}, endSubscription: async () => {} };
  const casino = new Casino({ db, notify, now: () => clock.t, ...(real ? {} : { handPauseMs: 1000 }) });
  return { casino, clock };
}

const real = (casino: Casino, n: number) => casino.identify(`c_real_${n}`, `guest:real${n}`, { harness: "claude-code", subscription: `sub_${n}` });
const house = (casino: Casino, n: number, model = n % 2 ? "Haiku 4.5" : "GPT-6 Luna") => casino.identify(`c_house_${n}`, `guest:house${n}`, { harness: "house", model, house: true });

/** Seats per table, as [real, house] counts. */
async function tables(casino: Casino): Promise<Map<string, [number, number]>> {
  const out = new Map<string, [number, number]>();
  for (const t of await casino.houseLobby()) out.set(t.id, [t.players.filter((p) => !p.house).length, t.players.filter((p) => p.house).length]);
  return out;
}

// ── The claim ──────────────────────────────────────────────────────────────

test("a house claim: the shared vectors hold, and nothing else passes", () => {
  const v = JSON.parse(readFileSync(new URL("../../../gamekit/testdata/house-claims.json", import.meta.url), "utf8")) as {
    secret: string;
    token: string;
    now: number;
    valid: { game: string; agent: string; model: string; header: string }[];
    invalid: { why: string; game: string; now: number; header: string; token?: string }[];
  };
  for (const c of v.valid) assert.deepEqual(verifyHouseClaim(v.secret, c.game, c.header, v.token, v.now * 1000), { agent: c.agent, model: c.model }, c.game);
  for (const c of v.invalid) assert.equal(verifyHouseClaim(v.secret, c.game, c.header, c.token ?? v.token, c.now * 1000), undefined, c.why);
  const ok = v.valid[0]!;
  assert.equal(verifyHouseClaim(undefined, ok.game, ok.header, v.token, v.now * 1000), undefined, "a game without HOUSE_SECRET accepts none");
  assert.equal(verifyHouseClaim("another-secret-0123456789abcdef01234567", ok.game, ok.header, v.token, v.now * 1000), undefined, "another secret");
  assert.equal(verifyHouseClaim(v.secret, ok.game, [ok.header], v.token, v.now * 1000), undefined, "a repeated header");
  const now = Date.now();
  const fresh = signHouseClaim(v.secret, "holdem", { agent: "h7", model: "Haiku 4.5" }, "tok", Math.floor(now / 1000) + HOUSE_CLAIM_MAX_S);
  assert.deepEqual(verifyHouseClaim(v.secret, "holdem", fresh, "tok", now), { agent: "h7", model: "Haiku 4.5" });
  assert.equal(verifyHouseClaim(v.secret, "holdem", fresh, "another guest's token", now), undefined, "a leaked claim");
});

test("the bearer token a claim is checked against", () => {
  assert.equal(bearerOf("Bearer abc"), "abc");
  assert.equal(bearerOf("Basic abc"), undefined);
  assert.equal(bearerOf("Bearer "), undefined);
  assert.equal(bearerOf(["Bearer a", "Bearer b"]), undefined);
  assert.equal(bearerOf(undefined), undefined);
});

// ── The label ──────────────────────────────────────────────────────────────

test("a house agent is labelled House with its model for agents, but not on the pages; a guest calling itself house isn't", async (t) => {
  const { casino } = room(await testDb(t));
  const h = await house(casino, 1, "Haiku 4.5");
  assert.equal(h.house, true);
  assert.equal(displayName(h), `${h.name} · House · Haiku 4.5`);
  // Spectators see a house agent like any other player: no House label.
  const r = await real(casino, 1);
  const { table } = await casino.join(r.id, {});
  assert.equal((await casino.join(h.id, {})).table, table);
  const v = (await casino.spectate(table))!;
  const seat = v.seats.find((s) => s?.name === h.name)!;
  assert.equal(seat.harness, null);
  assert.equal(seat.label, `${h.name} · Haiku 4.5`);
  const lobby = await casino.lobby();
  const pages = {
    view: JSON.stringify(v),
    table: tablePage(v, { url: `https://holdem.test/tables/${table}`, recent: [], live: `/api/tables/${table}/live` }),
    lobby: lobbyPage(lobby),
    landing: landingPage({ mcpUrl: "https://holdem.test/mcp", liveTables: lobby.length, tables: lobby, resetIn: 60, top: [], featured: v }),
    leaderboard: leaderboardPage({ day: casino.today(), resetIn: 60, standings: [], champions: [], minHands: MIN_HANDS }),
  };
  for (const [name, body] of Object.entries(pages)) assert.ok(!body.includes("House"), `${name} shows House`);
  const fake = await casino.identify("c_fake", "guest:fake", { harness: "house", model: "Haiku 4.5" });
  assert.equal(fake.house, false);
  assert.equal(fake.harness, null);
  assert.equal(displayName(fake), `${fake.name} · Haiku 4.5`);
});

// ── Never on the leaderboard ───────────────────────────────────────────────

test("house agents are left off the leaderboard, the Hall of Champions and the day's winner", async (t) => {
  const { casino } = room(await testDb(t));
  const [r, h] = [await real(casino, 1), await house(casino, 1)];
  await casino.db.query("INSERT INTO days (player, day, balance, hands, won_bb, biggest_pot) VALUES ($1, '2026-10-20', 900, $3, 40, 300), ($2, '2026-10-20', 150, $3, 5, 40)", [h.id, r.id, MIN_HANDS + 5]);
  await casino.db.query("INSERT INTO days (player, day, balance, hands, won_bb, biggest_pot) VALUES ($1, '2026-10-21', 700, 9, 40, 300), ($2, '2026-10-21', 120, 3, 5, 40)", [h.id, r.id]);
  assert.deepEqual((await casino.leaderboard("2026-10-21")).map((s) => s.id), [r.id]);
  await casino.recordChampions();
  assert.equal((await casino.champion("2026-10-20"))?.player, r.id);
  assert.deepEqual((await casino.champions()).map((c) => c.player), [r.id]);
});

// ── Where they sit ─────────────────────────────────────────────────────────

test("house agents join real agents who are waiting, at most four to a table, always leaving a seat free", async (t) => {
  const { casino } = room(await testDb(t));
  const r = await real(casino, 1);
  const alone = (await casino.join(r.id, {})).table;
  const hs = [];
  for (let i = 0; i < 9; i++) hs.push(await house(casino, i));
  const seats = [];
  for (const h of hs) seats.push((await casino.join(h.id, {})).table);
  // The waiting agent got company first.
  assert.equal(seats[0], alone);
  const t1 = await tables(casino);
  assert.deepEqual(t1.get(alone), [1, HOUSE_PER_TABLE]);
  for (const [id, [reals, houses]] of t1) {
    assert.ok(houses <= HOUSE_PER_TABLE, `${id}: ${houses} house agents`);
    assert.ok(reals + houses < SEATS, `${id}: no seat left for a real agent`);
  }
  assert.equal(t1.size, 3, "nine house agents: the waiting table, then house tables of four and one");
  // Real agents joining take the seat left free: the busiest table first,
  // then a house table.
  assert.equal((await casino.join((await real(casino, 2)).id, {})).table, alone);
  const at = (await casino.join((await real(casino, 3)).id, {})).table;
  assert.notEqual(at, alone);
  assert.deepEqual((await tables(casino)).get(at), [1, HOUSE_PER_TABLE]);
});

test("a house-only table pauses a little longer between hands; a real agent sitting down brings the next hand forward", async (t) => {
  const { casino, clock } = room(await testDb(t));
  const [a, b] = [await house(casino, 1), await house(casino, 2)];
  const { table } = await casino.join(a.id, {});
  await casino.join(b.id, {});
  // Fold the first hand away.
  for (let i = 0; i < 4; i++) {
    for (const id of [a.id, b.id]) {
      const s = await casino.state(id);
      if (s.table?.hand?.yourTurn && !s.table.hand.result) await casino.act(id, { type: "fold" }, s.table.hand.turn!);
    }
  }
  const next = async () => (await casino.db.query<{ next_hand_at: Date | null }>("SELECT next_hand_at FROM tables WHERE id = $1", [table])).rows[0]!.next_hand_at!.getTime();
  assert.equal(await next(), clock.t + HOUSE_HAND_PAUSE_MS);
  const r = await real(casino, 1);
  assert.equal((await casino.join(r.id, {})).table, table, "the real agent sits with the house agents");
  assert.equal(await next(), clock.t + 1000);
});

test("with a real agent seated among house agents, every next hand is dealt one short pause after the last ends", async (t) => {
  // The real pauses and the real clock: tick once a second, as main.ts does.
  const { casino, clock } = room(await testDb(t), true);
  const hs = [await house(casino, 1), await house(casino, 2), await house(casino, 3)];
  const { table } = await casino.join(hs[0]!.id, {});
  for (const h of hs.slice(1)) await casino.join(h.id, {});
  const r = await real(casino, 1);
  const ids = [...hs.map((h) => h.id), r.id];
  const row = async () =>
    (await casino.db.query<{ state: { hand: { number: number; street: string } | null }; next_hand_at: Date | null }>("SELECT state, next_hand_at FROM tables WHERE id = $1", [table])).rows[0]!;
  /** Plays the hand in progress to its end (checking or calling, a few seconds a move), the real agent joining after its first move if asked. */
  const playOut = async (joinMidHand = false) => {
    for (let moves = 0; moves < 60; moves++) {
      const x = await row();
      if (!x.state.hand || x.state.hand.street === "complete") return;
      for (const id of ids) {
        const s = await casino.state(id);
        if (!s.table?.hand?.yourTurn) continue;
        clock.t += 3000;
        await casino.act(id, { type: s.table.legal!.choices.includes("check") ? "check" : "call" }, s.table.hand.turn!);
        break;
      }
      if (joinMidHand && moves === 0) assert.equal((await casino.join(r.id, {})).table, table, "the real agent sits down mid-hand");
    }
    assert.fail("the hand didn't end");
  };
  /** Ticks once a second until a new hand is dealt; the seconds it took. */
  const untilDealt = async (after: number) => {
    const ended = clock.t;
    for (let s = 1; s <= 200; s++) {
      clock.t += 1000;
      await casino.tick();
      const h = (await row()).state.hand;
      if (h && h.number > after && h.street !== "complete") return (clock.t - ended) / 1000;
    }
    assert.fail("no hand was dealt");
  };
  // The house agents alone: the first hand comes at once, the next a house pause after it.
  await playOut();
  const first = (await row()).state.hand!.number;
  assert.equal((await row()).next_hand_at!.getTime(), clock.t + HOUSE_HAND_PAUSE_MS);
  assert.equal(await untilDealt(first), HOUSE_HAND_PAUSE_MS / 1000);
  // The real agent sits down mid-hand; from then on each hand follows the last after HAND_PAUSE_MS.
  await playOut(true);
  for (let i = 0; i < 6; i++) {
    const n = (await row()).state.hand!.number;
    assert.equal(await untilDealt(n), HAND_PAUSE_MS / 1000, `the hand after hand ${n}`);
    assert.ok((await casino.state(r.id)).table?.hand?.players.some((p) => p.name.startsWith(r.name)), "the real agent is dealt in");
    await playOut();
  }
  // It leaves: after the hand already coming, the house agents go back to their own pause.
  await casino.leave(r.id);
  await untilDealt((await row()).state.hand!.number);
  await playOut();
  assert.equal(await untilDealt((await row()).state.hand!.number), HOUSE_HAND_PAUSE_MS / 1000);
});

test("a seated agent that stopped answering holds up one hand by its clock, not every hand", async (t) => {
  const { casino, clock } = room(await testDb(t), true);
  const [gone, here, h] = [await real(casino, 1), await real(casino, 2), await house(casino, 1)];
  const { table } = await casino.join(gone.id, {});
  await casino.join(here.id, {});
  await casino.join(h.id, {});
  const row = async () => (await casino.db.query<{ state: { hand: { number: number; street: string } | null } }>("SELECT state FROM tables WHERE id = $1", [table])).rows[0]!.state;
  const started = clock.t;
  // Everyone but the absent agent answers at once; it's woken and never acts.
  for (let s = 0; s < 400 && (await row()).hand?.number === 1 && (await row()).hand?.street !== "complete"; s++) {
    let acted = false;
    for (const id of [here.id, h.id]) {
      const st = await casino.state(id);
      if (st.table?.hand?.yourTurn) {
        await casino.act(id, { type: st.table.legal!.choices.includes("check") ? "check" : "call" }, st.table.hand.turn!);
        acted = true;
      }
    }
    if (!acted) {
      clock.t += 1000;
      await casino.tick();
    }
  }
  assert.equal((await row()).hand?.street, "complete");
  assert.ok(clock.t - started <= 61_000, "its clock ran out once");
  assert.equal((await casino.state(gone.id)).table, null, "it was taken off the table");
  const ended = clock.t;
  while ((await row()).hand?.number === 1) {
    clock.t += 1000;
    await casino.tick();
  }
  assert.equal(clock.t - ended, HAND_PAUSE_MS, "the next hand, without it, comes after the short pause");
});

test("a house agent asked to leave mid-hand plays it out and goes at its end, even on its turn", async (t) => {
  const { casino } = room(await testDb(t));
  const [h, r] = [await house(casino, 1), await real(casino, 1)];
  await casino.join(h.id, {});
  await casino.join(r.id, {});
  const out = await casino.leave(h.id);
  assert.deepEqual(out && { left: out.left }, { left: false });
  const s = await casino.state(h.id);
  assert.ok(s.table?.hand && !s.table.hand.result, "the hand goes on");
  const me = s.table.hand.players.find((p) => p.name.startsWith(h.name));
  assert.equal(me?.folded, false, "it didn't fold to leave");
});

test("a house agent is never out of chips", async (t) => {
  const { casino } = room(await testDb(t));
  const h = await house(casino, 1);
  await casino.db.query("INSERT INTO days (player, day, balance) VALUES ($1, '2026-10-21', 0)", [h.id]);
  const seat = await casino.join(h.id, {});
  assert.ok(seat.buyIn > 0 && seat.buyIn <= DAILY_BANKROLL);
});

test("house agents don't sit down at a table that has enough real agents", async (t) => {
  const { casino } = room(await testDb(t));
  const reals = [await real(casino, 1), await real(casino, 2), await real(casino, 3)];
  const full = (await casino.join(reals[0]!.id, {})).table;
  for (const r of reals.slice(1)) assert.equal((await casino.join(r.id, {})).table, full);
  const h = await house(casino, 1);
  assert.notEqual((await casino.join(h.id, {})).table, full);
});
