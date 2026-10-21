# Agent Hold'em: the poker engine

No-limit Texas Hold'em for 2 to 9 seats, play money, as pure TypeScript with no I/O. The app server wraps it: it stores the state, runs the action clock, and keeps bankrolls. Every function takes a `TableState` and returns a new one; a state is plain JSON, so the app can store it in Postgres and carry on after a restart.

```ts
import { createTable, sit, startHand, act, timeout, legalActions, playerView, publicView, verifyHand } from "./engine/dist/src/index.js";

let s = createTable({ smallBlind: 1, bigBlind: 2, minBuyIn: 40, maxBuyIn: 200, seats: 6 });
s = sit(s, 0, "ada", 100);
s = sit(s, 1, "bob", 100);
s = startHand(s);                      // a fresh seed from crypto.randomBytes
legalActions(s, "ada");                // { toCall, check, call, bet, raise, allIn }
s = act(s, "ada", { type: "raise", amount: 6 }); // amounts are "to": the total this street
s = timeout(s);                        // bob ran out of time: checks if he can, else folds
playerView(s, "bob");                  // the public view plus bob's own cards and choices
```

## API

- **Table:** `createTable(config)`, `sit(state, seat, player, buyIn)`, `addChips(state, player, amount)` (between hands, up to the maximum buy-in), `sitOut(state, player, out)`, `leave(state, player)` → `{ state, cashOut }` (not while live in a hand: fold first).
- **Hands:** `startHand(state, { seed?, handId? })`, `act(state, player, action)`, `timeout(state)`, `toAct(state)`, `legalActions(state, player)`, `handActive(state)`. Actions: `fold`, `check`, `call`, `bet` and `raise` (with `amount`, the total put in this street), `allin`.
- **Views:** `publicView(state)` for spectators, `playerView(state, player)`, `history(state)` (the hand's public log). They contain no hole cards except those shown at showdown, and a player's view adds only their own. The seed appears once the hand is complete.
- **Results:** a finished hand's `hand.result` has the pots with their winners and shares, the hands shown, each player's net, who busted, and the seed. The log (`hand.log`) records every post, action, returned bet, street, showdown and award, for hand histories and replays.
- **Evaluation:** `evaluate(cards)` gives the best five-card hand from five to seven cards: a score (higher wins, equal splits), the category, its tie-breaking ranks, the five cards and a name ("a full house, kings full of sevens").
- **The shuffle:** `newSeed()`, `commitmentOf(seed)`, `shuffleDeck(seed, handId)`, `verifyShuffle(seed, commitment, handId)`, and `verifyHand(log)`, which checks a finished hand's board and shown cards against its commitment from the public log alone.
- **Errors:** `EngineError` with a `code` (`illegal_action`, `not_your_turn`, `still_in_hand` …) and a message that names players, seats and amounts, never cards.
- **Checks:** `checkInvariants(state)` throws if chips were made or lost: the table always holds what was brought in (`ledger.in`) minus what was taken away (`ledger.out`).

## The shuffle

Each hand's seed is 32 bytes from `crypto.randomBytes`. Only its commitment, SHA-256 of the seed's bytes, is public while the hand is played. The deck is a Fisher–Yates shuffle of the ordered deck (`2c 2d 2h 2s 3c … As`), from the last position down. Each swap's index comes from HMAC-SHA256(seed, `"<hand id>:<block>"`) output, read as big-endian 32-bit numbers, with rejection sampling so every permutation is equally likely. The deal is two cards to each player in the start event's `dealOrder` (one each, twice), then burn, flop, burn, turn, burn, river.

When the hand ends, the seed is revealed. Anyone can recompute the deck, check it against the commitment, and check the board and shown hands. The test suite pins one seed's output, which an independent Python implementation reproduces.

The seed also reveals the folded hands and the cards that were never dealt. The engine publishes it in the finished hand's result; an app that doesn't want folded hands recoverable can publish it later.

## Rules

- **Button:** moves one eligible seat clockwise each hand, with no dead button. Heads-up, the button posts the small blind, acts first before the flop and last after it.
- **Blinds:** a player short of a blind posts what they have, all in. The bet to call before the flop is the full big blind even if the big blind is short; what nobody matches comes back.
- **New players** are dealt in from their first hand without posting.
- **Betting:** the minimum bet is the big blind. The minimum raise is the last full bet or raise on this street. An all-in short of a full raise doesn't reopen the betting: players who already acted since the last full raise may only call or fold. Nobody may bet or raise when every other live player is all in. Folding is always allowed. Uncalled bets come back.
- **Timeouts:** the player checks if they can, else folds.
- **Pots:** one side pot per all-in level, merged where the same players are eligible. Odd chips go one each to the pot's winners, starting from the first seat left of the button.
- **Showdown:** every live hand is shown, with no mucking. The last player to bet or raise on the last betting round shows first, else the first live player left of the button. Folded hands are never shown.
- **Seats:** a player with no chips after a hand loses their seat. A player who folded can leave mid-hand and is paid out at once.

## Tests

`npm test`. They cover:

- **Every five-card hand** (2,598,960): each category's count, the 7,462 distinct ranks, and categories never overlapping.
- **Scripted hands** for each rule above.
- **A uniformity check** of the shuffle.
- **Random play** over 5,000 hands on 200 tables. Players come and go, top up and time out, and the state takes JSON round trips. After every action the tests check four things: chips are conserved, someone legal is to act, no view shows an unshown hole card, and every finished hand verifies.
