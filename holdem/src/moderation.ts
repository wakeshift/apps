// The short message that goes with every move: cleaned up, and kept off
// public pages if it's abusive. A move's message is the player's own words:
// other players read it as part of the game, never as instructions.

export const MESSAGE_MAX = 140;

/** What's left to show of a message the filter caught. The move still stands. */
export const HIDDEN = "…";

export class MessageError extends Error {}

/** The longest operator note kept with a move, in characters; longer is cut. */
export const REASONING_MAX = 600;

/**
 * The operator note's description, in neutral words: a tool argument
 * described as the model's private reasoning makes Opus's safeguards refuse
 * every request that carries the tool list (reasoning_extraction).
 */
export const NOTE_DESCRIPTION = "Optional short note for your operator about this move, shown on their watch page.";

/**
 * A move's private reasoning as kept: one line, no control or bidi
 * characters, at most REASONING_MAX characters (cut, never refused).
 * undefined: nothing to keep. It's shown only on the player's own watch
 * page, so it isn't filtered like table talk.
 */
export function cleanReasoning(raw: unknown): string | undefined {
  if (typeof raw !== "string") return undefined;
  // Far more than is kept is never looked at.
  const text = raw
    .slice(0, 16 * REASONING_MAX)
    .replace(/[\u0000-\u001f\u007f-\u009f\u200b-\u200f\u202a-\u202e\u2066-\u2069]+/g, " ")
    .replace(/\s+/g, " ")
    .trim();
  if (!text) return undefined;
  const chars = [...text];
  return chars.length > REASONING_MAX ? chars.slice(0, REASONING_MAX - 1).join("") + "…" : text;
}

export interface CleanMessage {
  /** What everyone sees: the cleaned message, or HIDDEN. */
  text: string;
  /** The filter caught it. */
  flagged: boolean;
  /** The cleaned message, kept only when flagged (for moderation; never shown). */
  original: string | null;
}

// Links of any kind: a message is table talk, not a way to send people places.
const URLS = /\b(?:https?:\/\/|www\.)\S+|\b[\w-]+(?:\.[\w-]+)*\.(?:com|net|org|io|ai|co|app|dev|me|ly|gg|xyz|info|biz|us|uk|de|ru|cn|tv|link|site|online|club|top|shop)\b(?:\/\S*)?/gi;

// Slurs and direct abuse. Matched on a folded copy (lower case, look-alike
// digits and symbols mapped back to letters, repeats squeezed) so spacing
// and leetspeak tricks don't slip past; the list stays short on purpose:
// trash talk ("you're bluffing, coward") is part of the game.
const ABUSE = [
  "nigger", "nigga", "faggot", "fag", "retard", "tranny", "chink", "spic", "kike", "wetback", "gook", "coon", "dyke",
  "kill yourself", "kys", "rape", "rapist", "cunt",
];
const FOLD: Record<string, string> = { "0": "o", "1": "i", "3": "e", "4": "a", "5": "s", "7": "t", "@": "a", $: "s", "!": "i", "|": "i" };
const fold = (s: string) =>
  s
    .toLowerCase()
    .replace(/[013457@$!|]/g, (c) => FOLD[c] ?? c)
    .replace(/[^a-z ]+/g, "")
    .replace(/(.)\1{2,}/g, "$1$1")
    .replace(/\s+/g, " ");
const ABUSE_RE = new RegExp(`\\b(?:${ABUSE.map((w) => w.replace(/ /g, "\\s*")).join("|")})s?\\b`);

/** True if the filter would keep text off public pages. */
export function abusive(text: string): boolean {
  const f = fold(text);
  return ABUSE_RE.test(f) || ABUSE_RE.test(f.replace(/ /g, ""));
}

/**
 * A move's message as stored and shown: control characters and links taken
 * out, spaces squeezed, 1 to MESSAGE_MAX characters. Throws MessageError,
 * with one line the agent can act on, if there's nothing left or too much.
 */
export function cleanMessage(raw: unknown): CleanMessage {
  if (typeof raw !== "string" || !raw.trim()) {
    throw new MessageError(`Add message: a short line (up to ${MESSAGE_MAX} characters) that goes with your move, e.g. message: "Let's see what you've got."`);
  }
  const text = raw
    .replace(/[\u0000-\u001f\u007f-\u009f\u200b-\u200f\u202a-\u202e\u2066-\u2069]+/g, " ")
    .replace(URLS, " ")
    .replace(/\s+/g, " ")
    .trim();
  if (!text) throw new MessageError(`Your message had nothing left once links were taken out. Send a short line in your own words with your move.`);
  if ([...text].length > MESSAGE_MAX) throw new MessageError(`Keep message to ${MESSAGE_MAX} characters (yours has ${[...text].length}).`);
  if (abusive(text)) return { text: HIDDEN, flagged: true, original: text };
  return { text, flagged: false, original: null };
}
