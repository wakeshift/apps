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
//
// The port opens first, before the app's modules load (boot.ts): only types
// are imported statically here, everything else once the server listens.
import { boot } from "./boot.js";
import type { Notifier } from "./casino.js";
import type { Log } from "./db.js";
import type { HoldemEvents } from "./events.js";

const log: Log = (level, msg, attrs = {}) => process.stdout.write(JSON.stringify({ time: new Date().toISOString(), level: level.toUpperCase(), msg, ...attrs }) + "\n");
// Milliseconds since the process started.
const since = () => Math.round(performance.now());
log("info", "holdem starting", { node: process.version, ms: since() });

function env(name: string, fallback?: string): string {
  const v = process.env[name]?.trim() || fallback;
  if (!v) {
    console.error(`holdem: set ${name}`);
    process.exit(2);
  }
  return v;
}

const port = Number(env("PORT", "8390"));
const secret = env("HOLDEM_SECRET");
if (Buffer.byteLength(secret) < 32) {
  console.error("holdem: HOLDEM_SECRET must be at least 32 bytes");
  process.exit(2);
}
const key = env("WAKESHIFT_KEY");
const core = process.env.WAKESHIFT_CORE?.trim() || undefined;
const databaseUrl = env("HOLDEM_DATABASE_URL");
const houseSecret = process.env.HOUSE_SECRET?.trim() || undefined;
if (houseSecret && Buffer.byteLength(houseSecret) < 32) {
  console.error("holdem: HOUSE_SECRET must be at least 32 bytes");
  process.exit(2);
}
const operatorNote = /^(1|true)$/i.test(process.env.GAMES_OPERATOR_NOTE?.trim() ?? "");
const publicUrl = env("PUBLIC_URL", `http://localhost:${port}`);
const proxyHops = Number(process.env.HOLDEM_PROXY_HOPS ?? 0) || 0;

// Set once ready, for a shutdown: halt stops the clocks and live streams at
// once, close the database once the server has closed.
let running: { halt: () => void; close: () => Promise<void> } | undefined;

const started = boot({
  port,
  async ready() {
    const listened = since();
    const [{ Wakeshift }, { analyticsFromEnv }, { Casino }, { Db }, { defineHoldemEvents, holdemNotifier }, { SignIn }, { holdemHandler }] = await Promise.all([
      import("@wakeshift/sdk"),
      import("./analytics.js"),
      import("./casino.js"),
      import("./db.js"),
      import("./events.js"),
      import("./oauth.js"),
      import("./server.js"),
    ]);
    const loaded = since();
    const wake = new Wakeshift({ key, core });
    const db = new Db(databaseUrl, { max: Number(process.env.HOLDEM_DB_POOL ?? 4) || 4, log });
    await db.migrate();
    const migrated = since();

    // The casino is made before its events (they read it), so the notifier
    // reaches them through this holder.
    const events: { current?: HoldemEvents } = {};
    const notify: Notifier = holdemNotifier(() => events.current!);
    const casino = new Casino({ db, notify, log });
    events.current = defineHoldemEvents(wake, casino);
    // Players' sign-in (oauth.ts): sessions idle for 31 days are forgotten, by
    // every instance about once an hour.
    const signIn = new SignIn({ wake, db, secret, base: publicUrl, proxyHops, log });
    const app = holdemHandler({ wake, casino, events: events.current, secret, houseSecret, operatorNote, publicUrl, proxyHops, signIn, analytics: analyticsFromEnv(), log });
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

    running = {
      // Live streams end at once too, so the server can close (their
      // pages reconnect to another instance).
      halt: () => {
        clearInterval(clock);
        clearInterval(expiry);
        app.close();
      },
      close: () => db.close(),
    };
    // Each step's end, in milliseconds since the process started.
    log("info", "holdem ready", { port, listening: listened, loaded, migrated, ready: since() });
    wake.app().then(
      (a) => log("info", "holdem's app", { app: a.id, core: wake.core }),
      (err) => log("error", "Wakeshift didn't say which app the key is", { core: wake.core, err: String(err) }),
    );
    return app.handle;
  },
  fail(err) {
    log("error", "holdem couldn't start", { err: String(err), ms: since() });
    process.exit(1);
  },
});
started.listening.then(
  () => log("info", "holdem listening", { port: (started.server.address() as { port: number }).port, ms: since() }),
  (err) => {
    log("error", "holdem couldn't listen", { port, err: String(err) });
    process.exit(1);
  },
);

for (const sig of ["SIGTERM", "SIGINT"] as const) {
  process.on(sig, () => {
    running?.halt();
    started.server.close(() => {
      (running?.close() ?? Promise.resolve()).finally(() => process.exit(0));
    });
    setTimeout(() => process.exit(0), 8000).unref();
  });
}
