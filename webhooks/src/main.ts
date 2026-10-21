// try.wakeshift.ai: a webhook URL that wakes your agent. See README.md.
//
//   WAKESHIFT_KEY       the app's key (wst_test_… from the console, or wst_live_try_… for an env-registered app)
//   WAKESHIFT_CORE      Wakeshift's URL (default https://app.wakeshift.ai)
//   TRY_SECRET          signs links (at least 32 bytes; changing it ends every link)
//   TRY_DATABASE_URL    Postgres for what's kept per webhook (every instance shares it)
//   TRY_DB_POOL         connections per instance (default 3; one more LISTENs)
//   PUBLIC_URL          this server's public URL (default http://localhost:$PORT)
//   PORT                default 8380
//   TRY_PROXY_HOPS      proxies in front that append to X-Forwarded-For (Google's load balancer: 2)
//   TRY_PER_HOUR, TRY_PER_DAY, TRY_GLOBAL_PER_HOUR, TRY_LINKS_PER_HOUR
//                       the limits (defaults 60, 500, 20000, 30)
import { Wakeshift } from "@wakeshift/sdk";
import { Db } from "./db.js";
import { logLine, tryServer } from "./server.js";

function env(name: string, fallback?: string): string {
  const v = process.env[name]?.trim() || fallback;
  if (!v) {
    console.error(`try: set ${name}`);
    process.exit(2);
  }
  return v;
}

const log = (level: string, msg: string, attrs: Record<string, unknown> = {}) => process.stdout.write(logLine(level, msg, attrs) + "\n");

function limitsFromEnv() {
  const out: Record<string, number> = {};
  for (const [name, key] of [
    ["TRY_PER_HOUR", "perHour"],
    ["TRY_PER_DAY", "perDay"],
    ["TRY_GLOBAL_PER_HOUR", "globalPerHour"],
    ["TRY_LINKS_PER_HOUR", "linksPerHour"],
  ]) {
    const v = Number(process.env[name]);
    if (v > 0) out[key] = v;
  }
  return out;
}

const port = Number(env("PORT", "8380"));
const wake = new Wakeshift({ key: env("WAKESHIFT_KEY"), core: process.env.WAKESHIFT_CORE?.trim() || undefined });
const db = new Db(env("TRY_DATABASE_URL"), { max: Number(process.env.TRY_DB_POOL ?? 3) || 3, log });
await db.migrate();
const server = tryServer({
  wake,
  db,
  secret: env("TRY_SECRET"),
  publicUrl: env("PUBLIC_URL", `http://localhost:${port}`),
  proxyHops: Number(process.env.TRY_PROXY_HOPS ?? 0) || 0,
  limits: limitsFromEnv(),
  log,
});
server.listen(port, () => {
  log("info", "try listening", { port, app: wake.appId, mode: wake.mode, core: wake.core });
});
for (const sig of ["SIGTERM", "SIGINT"] as const) {
  process.on(sig, () => {
    server
      .stop()
      .then(() => db.close())
      .finally(() => process.exit(0));
  });
}
