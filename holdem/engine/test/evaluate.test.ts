import assert from "node:assert/strict";
import { test } from "node:test";
import { ORDERED_DECK } from "../src/cards.js";
import { Category, evaluate, evaluate5 } from "../src/evaluate.js";

const cat = (cards: string) => evaluate(cards.split(" ")).category;
const score = (cards: string) => evaluate(cards.split(" ")).score;
const name = (cards: string) => evaluate(cards.split(" ")).name;

test("every five-card hand: the published category counts and 7,462 distinct ranks", () => {
  // The standard table: C(52,5) = 2,598,960 hands, 7,462 equivalence classes.
  const expected: Record<Category, [hands: number, classes: number]> = {
    [Category.StraightFlush]: [40, 10],
    [Category.Quads]: [624, 156],
    [Category.FullHouse]: [3744, 156],
    [Category.Flush]: [5108, 1277],
    [Category.Straight]: [10200, 10],
    [Category.Trips]: [54912, 858],
    [Category.TwoPair]: [123552, 858],
    [Category.Pair]: [1098240, 2860],
    [Category.HighCard]: [1302540, 1277],
  };
  const counts = new Map<Category, number>();
  const classes = new Map<Category, Set<number>>();
  const all = new Set<number>();
  const d = ORDERED_DECK;
  for (let a = 0; a < 52; a++)
    for (let b = a + 1; b < 52; b++)
      for (let c = b + 1; c < 52; c++)
        for (let e = c + 1; e < 52; e++)
          for (let f = e + 1; f < 52; f++) {
            const v = evaluate5([d[a]!, d[b]!, d[c]!, d[e]!, d[f]!]);
            counts.set(v.category, (counts.get(v.category) ?? 0) + 1);
            if (!classes.has(v.category)) classes.set(v.category, new Set());
            classes.get(v.category)!.add(v.score);
            all.add(v.score);
          }
  for (const [c, [hands, cls]] of Object.entries(expected)) {
    assert.equal(counts.get(Number(c)), hands, `hands in category ${c}`);
    assert.equal(classes.get(Number(c))!.size, cls, `classes in category ${c}`);
  }
  assert.equal(all.size, 7462);
  // Categories never overlap: every score in a category beats every score in the one below.
  for (let c = 1; c <= 8; c++) assert.ok(Math.min(...classes.get(c)!) > Math.max(...classes.get(c - 1)!), `category ${c} above ${c - 1}`);
});

test("categories from seven cards", () => {
  assert.equal(cat("As Ks Qs Js Ts 2d 3c"), Category.StraightFlush);
  assert.equal(name("As Ks Qs Js Ts 2d 3c"), "a royal flush");
  assert.equal(cat("5h 4h 3h 2h Ah Kd Kc"), Category.StraightFlush); // steel wheel
  assert.equal(name("5h 4h 3h 2h Ah Kd Kc"), "a straight flush, five high");
  assert.equal(cat("9c 9d 9h 9s 2d 3c 4h"), Category.Quads);
  assert.equal(cat("Kc Kd Kh 7s 7d 2c 3h"), Category.FullHouse);
  assert.equal(name("Kc Kd Kh 7s 7d 2c 3h"), "a full house, kings full of sevens");
  assert.equal(cat("Kc Kd Kh 7s 7d 7c 3h"), Category.FullHouse); // two trips make a full house
  assert.equal(name("Kc Kd Kh 7s 7d 7c 3h"), "a full house, kings full of sevens");
  assert.equal(cat("2h 7h 9h Jh Ah Kd Qs"), Category.Flush);
  assert.equal(cat("5d 4c 3h 2s Ah Kd Qs"), Category.Straight); // wheel
  assert.equal(name("5d 4c 3h 2s Ah Kd Qs"), "a straight, five high");
  assert.equal(cat("6d 5c 4h 3s 2h Ad Ks"), Category.Straight);
  assert.equal(name("6d 5c 4h 3s 2h Ad Ks"), "a straight, six high"); // six-high beats the wheel inside it
  assert.equal(cat("Qc Qd Qh 7s 2d 3c 9h"), Category.Trips);
  assert.equal(cat("Qc Qd 7h 7s 2d 2c 9h"), Category.TwoPair); // three pair plays the best two
  assert.equal(name("Qc Qd 7h 7s 2d 2c 9h"), "two pair, queens and sevens");
  assert.equal(cat("Qc Qd 7h 8s 2d 3c 9h"), Category.Pair);
  assert.equal(name("6c 6d 7h 8s 2d 3c 9h"), "a pair of sixes");
  assert.equal(cat("Ac Jd 7h 8s 2d 3c 9h"), Category.HighCard);
});

test("ordering inside categories", () => {
  // Wheel is the lowest straight; ace-high the highest.
  assert.ok(score("5d 4c 3h 2s Ah 9c 9d") < score("6d 5c 4h 3s 2h Kc Kd"));
  assert.ok(score("Ad Kc Qh Js Th 2c 3d") > score("Kd Qc Jh Ts 9h 2c 3d"));
  // Flush on the board vs a flush straight: flush beats straight.
  assert.ok(score("2h 7h 9h Jh Ah Kd Qs") > score("Td Jc Qh Ks Ah 2c 3d"));
  // Kickers.
  assert.ok(score("Ac Ad Kh 7s 2d 3c 9h") > score("Ac Ad Qh 7s 2d 3c 9h"));
  assert.ok(score("Kc Kd 7h 7s Ad 3c 2h") > score("Kc Kd 7h 7s Qd 3c 2h")); // two pair kicker
  assert.ok(score("Kc Kd Kh 2s 2d 3c 4h") > score("Qc Qd Qh As Ad 3c 4h")); // full house by trips first
  assert.ok(score("Ah Qh 9h 7h 3h 2c 2d") > score("Ah Qh 9h 7h 2h Kc Kd")); // flush by fifth card
  // Board plays: the same hand ties.
  assert.equal(score("Ac Ad As Ah Kd 2c 3h"), score("Ac Ad As Ah Kd 4c 5h"));
  assert.equal(score("Td Jc Qh Ks Ah 2c 3d"), score("Td Jc Qh Ks Ah 4h 5s"));
});

test("five, six or seven cards, no repeats", () => {
  assert.equal(evaluate(["As", "Ks", "Qs", "Js", "Ts"]).category, Category.StraightFlush);
  assert.equal(evaluate(["As", "Ks", "Qs", "Js", "Ts", "9s"]).category, Category.StraightFlush);
  assert.throws(() => evaluate(["As", "Ks", "Qs", "Js"]));
  assert.throws(() => evaluate(["As", "As", "Qs", "Js", "Ts"]));
  assert.equal(evaluate("As Ks Qs Js Ts 2d 3c".split(" ")).cards.length, 5);
});
