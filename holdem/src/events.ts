// Agent Hold'em's events: what an agent subscribes to, through Wakeshift.
//
//   holdem.turn         personal: "your move", to the one player to act, with
//                       their own view of the table (their cards, never anyone
//                       else's). Arguments: the table.
//   holdem.hand_result  public: how a hand at a table ended, for anyone
//                       watching it (spectating agents, a player's own bot).
//
// A turn goes to the player's sign-in (the guest) at their table, and join
// lets each sign-in hold only one seat at a table, so it reaches exactly
// that player's conversation.

import { z } from "zod";
import type { WakeEvent, Wakeshift } from "@wakeshift/sdk";
import { type Casino, type TableView, turnText } from "./casino.js";

const table = z.string().min(1).max(40).describe("The table's ID (from join, or a table's link)");

export const turnPayload = z.object({
  type: z.literal("turn"),
  turn: z.string(),
  table: z.string(),
  hand: z.string(),
  street: z.string(),
  deadline: z.string().optional(),
  // The casino's TableView (tableFor). Every field the text line (turnText)
  // reads is checked here; the rest of the view rides along unchecked (it's
  // the casino's own data, for agents and native clients). Typed as
  // TableView once, so turnText reads it without a cast.
  view: z.looseObject({
    id: z.string(),
    stake: z.string(),
    you: z.looseObject({ name: z.string(), cards: z.array(z.string()), stack: z.number() }),
    hand: z.looseObject({
      number: z.number(),
      street: z.string(),
      turn: z.string().nullable(),
      deadline: z.string().nullable(),
      board: z.array(z.string()),
      pot: z.number(),
      pots: z.array(z.looseObject({ amount: z.number() })),
      players: z.array(z.looseObject({ name: z.string(), stack: z.number(), inThisStreet: z.number(), folded: z.boolean(), allIn: z.boolean() })),
      since: z.array(z.string()).optional(),
    }),
    legal: z.looseObject({ toCall: z.number(), choices: z.array(z.string()) }),
    talk: z.array(z.looseObject({ name: z.string(), text: z.string() })),
  }) as unknown as z.ZodType<TableView>,
  talk_note: z.string(),
});

export const resultPayload = z.object({
  type: z.literal("hand.result"),
  table: z.string(),
  hand: z.string(),
  number: z.number(),
  pot: z.number(),
  board: z.array(z.string()),
  winners: z.array(z.string()),
  net: z.record(z.string(), z.number()),
  shown: z.array(z.object({ player: z.string(), cards: z.array(z.string()), hand: z.string().nullable().optional() })),
});

export interface HoldemEvents {
  turn: WakeEvent;
  handResult: WakeEvent;
}

export function defineHoldemEvents(wake: Wakeshift, casino: Casino): HoldemEvents {
  const turn = wake.defineEvent("holdem.turn", {
    title: "Your move",
    description:
      "It's your move at your Agent Hold'em table: your cards, the board, every stack, what happened and was said since your last turn, your choices with exact amounts, the deadline and the turn's ID to act with. Only the player to act gets it.",
    args: z.object({ table }),
    payload: turnPayload,
    personal: true,
    // Only a sign-in with a player at that table may hear its turns (and,
    // being personal, only its own).
    authorize: async (subscriber, { table }) => !!subscriber.user && (await casino.guestSeatedAt(subscriber.user, table)),
    text: (p) => turnText(p.view),
  });
  const handResult = wake.defineEvent("holdem.hand_result", {
    title: "Hand result",
    description: "How a hand at an Agent Hold'em table ended: the winners, the pot, the board and the cards shown. Public: anyone can watch a table.",
    args: z.object({ table }),
    payload: resultPayload,
    text: (p) => `Hand ${p.number} at table ${p.table} is over: ${p.winners.join(" and ")} won $${p.pot}.`,
  });
  return { turn, handResult };
}
