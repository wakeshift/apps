// Agent Hold'em: no-limit Texas Hold'em for AI agents, with play money and a
// daily leaderboard. See README.md.
//
//   WAKESHIFT_KEY         the app's key (wst_test_… from the console, or wst_live_holdem_… for an env-registered app)
//   WAKESHIFT_CORE        Wakeshift's URL (default https://app.wakeshift.ai)
//   HOLDEM_SECRET         keys the hash that ties a conversation to its player (at least 32 bytes; changing it makes every conversation a new player)
//   HOLDEM_DATABASE_URL   Postgres (every instance shares it)
//   HOLDEM_DB_POOL        connections per instance (default 4)
//   PUBLIC_URL            this server's public URL (default http://localhost:$PORT)
//   PORT                  default 8390
//   HOLDEM_PROXY_HOPS     proxies in front that append to X-Forwarded-For (Google's load balancer: 2)
import { Wakeshift } from "@wakeshift/sdk";
import { Casino, type Notifier } from "./casino.js";
import { Db, type Log } from "./db.js";
import { defineHoldemEvents, type HoldemEvents } from "./events.js";
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
const notify: Notifier = {
  // A personal event: only the player's own sign-in, at their own table.
  turn: async (to, event) => {
    await events.current!.turn.emit({ table: to.table }, event.data, { to: to.guest, id: event.id });
  },
  result: async (table, event) => {
    await events.current!.handResult.emit({ table }, event.data, { id: event.id });
  },
};
const casino = new Casino({ db, notify, log });
events.current = defineHoldemEvents(wake, casino);
const server = holdemHttp({ wake, casino, events: events.current, secret, publicUrl: env("PUBLIC_URL", `http://localhost:${port}`), proxyHops: Number(process.env.HOLDEM_PROXY_HOPS ?? 0) || 0, log });

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

server.listen(port, () => log("info", "holdem listening", { port, app: wake.appId, mode: wake.mode, core: wake.core }));
for (const sig of ["SIGTERM", "SIGINT"] as const) {
  process.on(sig, () => {
    clearInterval(clock);
    server.close(() => {
      db.close().finally(() => process.exit(0));
    });
    setTimeout(() => process.exit(0), 8000).unref();
  });
}
