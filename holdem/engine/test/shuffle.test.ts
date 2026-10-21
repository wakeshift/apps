import assert from "node:assert/strict";
import { test } from "node:test";
import { ORDERED_DECK } from "../src/cards.js";
import { commitmentOf, newSeed, shuffleDeck, verifyShuffle } from "../src/shuffle.js";
import { seedOf } from "./helpers.js";

test("a seed gives the same deck every time, a full one", () => {
  const seed = seedOf("deck");
  const a = shuffleDeck(seed, "1");
  assert.deepEqual(a, shuffleDeck(seed, "1"));
  assert.deepEqual([...a].sort(), [...ORDERED_DECK].sort());
  assert.notDeepEqual(a, ORDERED_DECK);
  assert.notDeepEqual(a, shuffleDeck(seed, "2"), "the hand ID changes the deck");
  assert.notDeepEqual(a, shuffleDeck(seedOf("other"), "1"));
});

test("known answer: the algorithm can't drift", () => {
  // Pinned output for one seed, also reproduced by an independent Python
  // implementation of the algorithm in shuffle.ts: anyone verifying a hand
  // reimplements exactly this.
  const deck = shuffleDeck("00".repeat(32), "1");
  assert.deepEqual(deck.slice(0, 10), ["4h", "5h", "Ad", "2h", "Tc", "Kd", "3c", "8d", "6s", "3s"]);
  assert.equal(commitmentOf("00".repeat(32)), "66687aadf862bd776c8fc18b8e9f8e20089714856ee233b3902a591d0d5f2925");
});

test("commitment and verification", () => {
  const seed = newSeed();
  assert.match(seed, /^[0-9a-f]{64}$/);
  assert.notEqual(seed, newSeed());
  const c = commitmentOf(seed);
  assert.match(c, /^[0-9a-f]{64}$/);
  const ok = verifyShuffle(seed, c, "7");
  assert.equal(ok.ok, true);
  assert.deepEqual(ok.deck, shuffleDeck(seed, "7"));
  assert.deepEqual(verifyShuffle(newSeed(), c, "7"), { ok: false, problem: "the seed doesn't match the commitment" });
  assert.equal(verifyShuffle("xyz", c, "7").ok, false);
  assert.throws(() => shuffleDeck("ABC", "1"), /64 lowercase hex/);
});

test("the shuffle is uniform enough: each card lands in each position about equally", () => {
  // 20,800 decks: every (card, position) cell expects 400. A chi-square test
  // over the 2,704 cells (2,601 degrees of freedom) should sit near 2,601;
  // 3,000 is far into the tail (p < 1e-6) for a fair shuffle.
  const N = 20_800;
  const counts = new Map<string, number[]>(ORDERED_DECK.map((c) => [c, Array(52).fill(0)]));
  for (let i = 0; i < N; i++)
    shuffleDeck(seedOf(`u${i}`), "1").forEach((c, pos) => {
      const row = counts.get(c)!;
      row[pos] = row[pos]! + 1;
    });
  const expected = N / 52;
  let chi = 0;
  for (const row of counts.values()) for (const k of row) chi += (k - expected) ** 2 / expected;
  assert.ok(chi < 3000, `chi-square ${chi.toFixed(0)}`);
});
