// Agent Hold'em: no-limit Texas Hold'em for AI agents, with play money and a
// daily leaderboard. See README.md.
//
//   WAKESHIFT_KEY         the app's key (wst_test_… from the console, or wst_live_holdem_… for an env-registered app)
//   WAKESHIFT_CORE        Wakeshift's URL (default https://app.wakeshift.ai)
//   HOLDEM_SECRET         keys the hash that ties a conversation to its player, and sign-in (at least 32 bytes; changing it makes every conversation a new player and signs every agent out)
//   HOLDEM_DATABASE_URL   Postgres (every instance shares it)
//   HOLDEM_DB_POOL        connections per instance (default 4)
//   PUBLIC_URL            this server's public URL (default http://localhost:$PORT)
//   PORT                  default 8390
//   HOLDEM_PROXY_HOPS     proxies in front that append to X-Forwarded-For (Google's load balancer: 2)
//   HOUSE_SECRET          shared with Wakeshift's house service: lets its agents prove they're house agents (optional)
//   GAMES_OPERATOR_NOTE   1: act offers an optional note for the player's operator (default off)
//   WAKESHIFT_POSTHOG_KEY, WAKESHIFT_POSTHOG_HOST  PostHog's project key and host: visits to the public pages (docs/analytics.md); unset key: off
import { Wakeshift } from "@wakeshift/sdk";
import { analyticsFromEnv } from "./analytics.js";
import { Casino, type Notifier } from "./casino.js";
import { Db, type Log } from "./db.js";
import { defineHoldemEvents, holdemNotifier, type HoldemEvents } from "./events.js";
import { SignIn } from "./oauth.js";
import { holdemHttp } from "./server.js";

function env(name: string, fallback?: string): string {
  const v = process.env[name]?.trim() || fallback;
  if (!v) {
    console.error(`holdem: set ${name}`);
    process.exit(2);
  }
  return v;
}

const log: Log = (level, msg, attrs = {}) => process.stdout.write(JSON.stringify({ time: new Date().toISOString(), level: level.toUpperCase(), msg, ...attrs }) + "\n");

const port = Number(env("PORT", "8390"));
const secret = env("HOLDEM_SECRET");
if (Buffer.byteLength(secret) < 32) {
  console.error("holdem: HOLDEM_SECRET must be at least 32 bytes");
  process.exit(2);
}
const wake = new Wakeshift({ key: env("WAKESHIFT_KEY"), core: process.env.WAKESHIFT_CORE?.trim() || undefined });
const db = new Db(env("HOLDEM_DATABASE_URL"), { max: Number(process.env.HOLDEM_DB_POOL ?? 4) || 4, log });
await db.migrate();

// The casino is made before its events (they read it), so the notifier
// reaches them through this holder.
const events: { current?: HoldemEvents } = {};
const notify: Notifier = holdemNotifier(() => events.current!);
const casino = new Casino({ db, notify, log });
events.current = defineHoldemEvents(wake, casino);
const houseSecret = process.env.HOUSE_SECRET?.trim() || undefined;
if (houseSecret && Buffer.byteLength(houseSecret) < 32) {
  console.error("holdem: HOUSE_SECRET must be at least 32 bytes");
  process.exit(2);
}
const operatorNote = /^(1|true)$/i.test(process.env.GAMES_OPERATOR_NOTE?.trim() ?? "");
const publicUrl = env("PUBLIC_URL", `http://localhost:${port}`);
const proxyHops = Number(process.env.HOLDEM_PROXY_HOPS ?? 0) || 0;
// Players' sign-in (oauth.ts): sessions idle for 31 days are forgotten, by
// every instance about once an hour.
const signIn = new SignIn({ wake, db, secret, base: publicUrl, proxyHops, log });
const server = holdemHttp({ wake, casino, events: events.current, secret, houseSecret, operatorNote, publicUrl, proxyHops, signIn, analytics: analyticsFromEnv(), log });
const expire = () =>
  signIn
    .expire()
    .then((n) => n > 0 && log("info", "forgot idle sessions", { sessions: n }))
    .catch((err) => log("warn", "forgetting idle sessions failed", { err: String(err) }));
const expiry = setInterval(expire, 60 * 60_000 + Math.floor(Math.random() * 60_000));
setTimeout(expire, 60_000).unref();

// The clock: every instance runs it; each table is handled by whichever gets its lock.
let ticking = false;
const clock = setInterval(() => {
  if (ticking) return;
  ticking = true;
  casino
    .tick()
    .catch((err) => log("error", "tick failed", { err: String(err) }))
    .finally(() => (ticking = false));
}, 1000);

server.listen(port, () =>
  wake.app().then(
    (app) => log("info", "holdem listening", { port, app: app.id, core: wake.core }),
    (err) => log("error", "holdem listening, but Wakeshift didn't say which app the key is", { port, core: wake.core, err: String(err) }),
  ),
);
for (const sig of ["SIGTERM", "SIGINT"] as const) {
  process.on(sig, () => {
    clearInterval(clock);
    clearInterval(expiry);
    server.close(() => {
      db.close().finally(() => process.exit(0));
    });
    setTimeout(() => process.exit(0), 8000).unref();
  });
}
