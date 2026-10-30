// Agent Hold'em's events: what an agent subscribes to, through Wakeshift.
//
//   holdem.turn         personal: "your table", to one player: your move
//                       (with your own view of the table: your cards, never
//                       anyone else's), your removal and, at a native seat,
//                       each hand's result. Arguments: the table.
//   holdem.hand_result  public: how a hand at a table ended, for anyone
//                       watching it (spectating agents, a player's own bot).
//
// A turn goes to the player's sign-in (the guest) at their table: exactly
// the subscription join made for a bridged player's conversation, and only
// the native (webhook) subscriptions of a native seat's (house agents,
// ChatGPT). join lets each sign-in hold only one seat at a table.

import { z } from "zod";
import type { Recipient, WakeEvent, Wakeshift } from "@wakeshift/sdk";
import { type Casino, type Notifier, type TableView, type TurnTarget, SIT_OUT_RULE, resultText, turnText } from "./casino.js";

const table = z.string().min(1).max(40).describe("The table's ID (from join, or a table's link)");

const shown = z.array(z.object({ player: z.string(), cards: z.array(z.string()), hand: z.string().nullable().optional() }));

// A turn event is one of three kinds (the SDK takes one object, so the
// fields of each are optional and type says which):
//   turn         your move: the turn's ID, the hand, the street, the
//                deadline, your own view of the table (view), a note on
//                table talk and, for a bridged player, the last hand's
//                result (last_hand) if it ended since they last heard.
//   removed      the last one at a table: the clock took you off it (reason
//                "timeouts": sat out for timing out repeatedly; "unreachable":
//                your turn's wake couldn't be delivered), your hand folded
//                (cash_out back in your bankroll, text).
//   hand_result  a native seat's: how a hand at your table ended (hand,
//                number, pot, board, winners, net, shown), as
//                holdem.hand_result has it.
export const turnPayload = z.object({
  type: z.enum(["turn", "removed", "hand_result"]),
  table: z.string(),
  turn: z.string().optional(),
  hand: z.string().optional(),
  number: z.number().optional(),
  pot: z.number().optional(),
  board: z.array(z.string()).optional(),
  winners: z.array(z.string()).optional(),
  net: z.record(z.string(), z.number()).optional(),
  shown: shown.optional(),
  last_hand: z
    .object({ number: z.number(), winners: z.array(z.string()), pot: z.number(), board: z.array(z.string()), shown })
    .optional(),
  street: z.string().optional(),
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
  }).optional() as unknown as z.ZodOptional<z.ZodType<TableView>>,
  talk_note: z.string().optional(),
  reason: z.enum(["timeouts", "unreachable"]).optional(),
  cash_out: z.number().optional(),
  bankroll: z.number().optional(),
  text: z.string().optional(),
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
  shown,
});

export interface HoldemEvents {
  turn: WakeEvent;
  handResult: WakeEvent;
}

export function defineHoldemEvents(wake: Wakeshift, casino: Casino): HoldemEvents {
  const turn = wake.defineEvent("holdem.turn", {
    title: "Your table",
    // At most 500 characters (Wakeshift's limit).
    description:
      "Your Agent Hold'em table. A \"turn\" is your move: your cards, the board, every stack, what happened since your last turn (and the last hand's result, if you haven't heard), your choices and the turn's ID. " +
      `Act by the deadline (no time bank): out of time, you check if you can, else fold; time out ${SIT_OUT_RULE}, and you're sat out, as a last \"removed\" event says. ` +
      "At a native seat (a client that subscribes itself), a \"hand_result\" says how each hand ended.",
    args: z.object({ table }),
    payload: turnPayload,
    personal: true,
    // Only a sign-in with a player at that table may hear its turns (and,
    // being personal, only its own).
    authorize: async (subscriber, { table }) => !!subscriber.user && (await casino.guestSeatedAt(subscriber.user, table)),
    text: (p) => {
      if (p.type === "turn" && p.view) return turnText(p.view, p.last_hand);
      if (p.type === "hand_result" && p.number !== undefined) return resultText({ number: p.number, table: p.table, winners: p.winners ?? [], pot: p.pot ?? 0 });
      return p.text ?? "";
    },
  });
  const handResult = wake.defineEvent("holdem.hand_result", {
    title: "Hand result",
    description: "How a hand at an Agent Hold'em table ended: the winners, the pot, the board and the cards shown. Public: anyone can watch a table.",
    args: z.object({ table }),
    payload: resultPayload,
    text: (p) => resultText(p),
  });
  return { turn, handResult };
}

/** Who a turn event at a player's table goes to: the subscription join made for their conversation, else (a native seat) their native subscriptions. */
export function turnRecipient(to: TurnTarget): Recipient {
  return to.subscription ? { user: to.guest, subscription: to.subscription } : { native: to.guest };
}

/** The casino's notifier: its wakes and results as Hold'em's events (events() is read when each is sent). */
export function holdemNotifier(events: () => HoldemEvents): Notifier {
  return {
    // Delivered: Wakeshift took it for a live subscription of theirs (a
    // retry's duplicate reports what the first emit matched). Not: the
    // subscription has ended, or a native seat has none.
    turn: async (to, event) => {
      const r = await events().turn.emit({ table: to.table }, event.data, { to: turnRecipient(to), id: event.id });
      return r.matched > 0;
    },
    // The shared event, for anyone watching the table (spectators).
    result: async (table, event) => {
      await events().handResult.emit({ table }, event.data, { id: event.id });
    },
    // A native seat's own copy of a hand's result, on its turn event.
    handResult: async (to, event) => {
      await events().turn.emit({ table: to.table }, event.data, { to: { native: to.guest }, id: event.id });
    },
    // The last personal event at that table: the clock took the player off it.
    removed: async (to, event) => {
      await events().turn.emit({ table: to.table }, event.data, { to: turnRecipient(to), id: event.id });
    },
    endSubscription: async (guest, subscription) => {
      await events().turn.end(guest, subscription);
    },
  };
}
