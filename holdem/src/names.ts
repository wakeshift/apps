// Players' names: an adjective and an animal, given when a conversation
// first joins. Agents don't choose them, which keeps the leaderboard clean.

import { randomInt } from "node:crypto";

export const ADJECTIVES = [
  "Brave", "Quiet", "Clever", "Lucky", "Bold", "Calm", "Swift", "Sly", "Steady", "Wild",
  "Patient", "Fierce", "Gentle", "Sharp", "Witty", "Daring", "Silent", "Merry", "Stern", "Crafty",
  "Nimble", "Mellow", "Plucky", "Wary", "Zesty", "Breezy", "Cosmic", "Dapper", "Eager", "Frosty",
  "Golden", "Hidden", "Icy", "Jolly", "Keen", "Lofty", "Misty", "Noble", "Odd", "Proud",
  "Quick", "Rusty", "Sunny", "Tidy", "Urbane", "Vivid", "Wise", "Young", "Zany", "Amber",
  "Bright", "Cheeky", "Dusty", "Electric", "Fuzzy", "Grumpy", "Hasty", "Inky", "Jumpy", "Lunar",
  "Mighty", "Nifty", "Polite", "Royal",
] as const;

export const ANIMALS = [
  "Otter", "Heron", "Badger", "Fox", "Falcon", "Lynx", "Owl", "Panda", "Raven", "Tiger",
  "Walrus", "Yak", "Zebra", "Bison", "Cobra", "Dingo", "Eagle", "Ferret", "Gecko", "Hare",
  "Ibis", "Jackal", "Koala", "Lemur", "Moose", "Newt", "Ocelot", "Pelican", "Quail", "Rhino",
  "Salmon", "Toucan", "Urchin", "Viper", "Wombat", "Beaver", "Crane", "Dolphin", "Elk", "Finch",
  "Gopher", "Hawk", "Iguana", "Jaguar", "Kestrel", "Llama", "Magpie", "Narwhal", "Orca", "Puffin",
  "Robin", "Seal", "Turtle", "Vulture", "Weasel", "Albatross", "Bobcat", "Coyote", "Duck", "Egret",
  "Flamingo", "Gazelle", "Hedgehog", "Marmot",
] as const;

/** A random name, such as "Brave Otter". */
export function randomName(rand: (n: number) => number = randomInt): string {
  return `${ADJECTIVES[rand(ADJECTIVES.length)]} ${ANIMALS[rand(ANIMALS.length)]}`;
}

/**
 * A name not taken: random ones first, then, if those keep colliding, a
 * random one with a number ("Brave Otter 7").
 */
export async function freshName(taken: (name: string) => Promise<boolean>, rand: (n: number) => number = randomInt): Promise<string> {
  for (let i = 0; i < 12; i++) {
    const n = randomName(rand);
    if (!(await taken(n))) return n;
  }
  for (let i = 0; ; i++) {
    const n = `${randomName(rand)} ${2 + rand(98 + i * 100)}`;
    if (!(await taken(n))) return n;
  }
}

const TREES = [
  "Juniper", "Aspen", "Birch", "Cedar", "Cypress", "Elm", "Fir", "Hazel", "Hemlock", "Holly",
  "Larch", "Laurel", "Linden", "Magnolia", "Maple", "Myrtle", "Oak", "Olive", "Pine", "Poplar",
  "Redwood", "Rowan", "Sequoia", "Spruce", "Sycamore", "Tamarack", "Willow", "Yew", "Alder", "Banyan",
  "Baobab", "Chestnut", "Ebony", "Ginkgo", "Hickory", "Ironwood", "Jacaranda", "Kauri", "Mahogany", "Teak",
];

/** A table's name, as people say it: "Table Juniper". Fixed for its ID (two tables may share one). */
export function tableName(id: string): string {
  let h = 0;
  for (const ch of id) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  return `Table ${TREES[h % TREES.length]}`;
}

const HARNESS_NAMES: Record<string, string> = {
  "claude-code": "Claude Code",
  codex: "Codex",
  cursor: "Cursor",
  "cursor-cloud": "Cursor",
  opencode: "opencode",
  antigravity: "Antigravity",
  copilot: "Copilot CLI",
  "grok-bot": "Grok Bot",
  chatgpt: "ChatGPT",
  hermes: "Hermes",
  openclaw: "OpenClaw",
  gemini: "Gemini CLI",
  goose: "Goose",
  cline: "Cline",
  amp: "Amp",
  devin: "Devin",
  vscode: "VS Code",
  house: "House",
};

/** The program an agent runs in, as people know it ("Claude Code"), from Wakeshift's ID for it. */
export function harnessName(id: string | null | undefined): string | null {
  if (!id) return null;
  return HARNESS_NAMES[id] ?? id;
}

/** A player as shown: "Brave Otter · Claude Code · Opus 5.5". */
export function displayName(p: { name: string; harness?: string | null; model?: string | null }): string {
  return [p.name, harnessName(p.harness), p.model].filter(Boolean).join(" · ");
}
