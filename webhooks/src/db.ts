// The try app's Postgres: a small pool for queries, one connection that
// LISTENs for the live view, and the migrations in ../migrations.
//
// Everything the app writes is cheap to lose (a few seconds of activity or
// counts if the database crashes), so commits don't wait for the disk:
// synchronous_commit is off for its connections. That keeps the global
// limit's row, which every post updates, locked for microseconds.

import { readdirSync, readFileSync } from "node:fs";
import pg from "pg";

/** The database can't take the request right now: answer 503, not 500. */
export class Unavailable extends Error {
  constructor(cause: unknown) {
    super("database unavailable: " + String((cause as Error)?.message ?? cause));
  }
}

// Postgres errors that mean "busy or away", not "wrong query": connection
// exceptions, insufficient resources, shutdowns, statement timeouts, and a
// transaction Postgres rolled back to break a deadlock or a serialization
// conflict (40P01, 40001), which the caller can simply retry.
const unavailableCodes = /^(08|53|57|40P01$|40001$)/;

function unavailable(err: unknown): boolean {
  const code = (err as { code?: unknown })?.code;
  if (typeof code === "string") return unavailableCodes.test(code) || code === "ECONNREFUSED" || code === "ECONNRESET" || code === "ENOTFOUND";
  return /timeout|terminated|Connection|ECONN|not queryable|ended/i.test(String((err as Error)?.message ?? err));
}

export interface DbOptions {
  /** Connections in the pool (plus one for LISTEN). */
  max?: number;
  /** How long a query waits for a free connection before the request is told to come back later. */
  connectTimeoutMs?: number;
  /** Longest a statement may run. */
  statementTimeoutMs?: number;
  /** How often the LISTEN connection is pinged, and how long a ping may take before it's replaced. */
  pingMs?: number;
  pingTimeoutMs?: number;
  log?: (level: "info" | "warn" | "error", msg: string, attrs?: Record<string, unknown>) => void;
}

type Notify = (payload: string) => void;

export class Db {
  readonly pool: pg.Pool;
  private readonly url: string;
  private readonly log: NonNullable<DbOptions["log"]>;
  private listener?: pg.Client;
  private connecting?: Promise<void>;
  private readonly channels = new Map<string, Set<Notify>>();
  private closed = false;
  private retry = 500;
  private readonly pingMs: number;
  private readonly pingTimeoutMs: number;
  /** Called after the LISTEN connection comes back: notifications may have been missed. */
  onReconnect?: (channels: string[]) => void;

  constructor(url: string, o: DbOptions = {}) {
    this.url = url;
    this.log = o.log ?? (() => {});
    this.pingMs = o.pingMs ?? 30_000;
    this.pingTimeoutMs = o.pingTimeoutMs ?? 10_000;
    const statementTimeout = o.statementTimeoutMs ?? 5000;
    this.pool = new pg.Pool({
      connectionString: url,
      max: o.max ?? 4,
      connectionTimeoutMillis: o.connectTimeoutMs ?? 3000,
      idleTimeoutMillis: 60_000,
      options: `-c synchronous_commit=off -c statement_timeout=${statementTimeout}`,
    });
    // An idle connection that drops (a database restart) mustn't crash the process.
    this.pool.on("error", (err) => this.log("warn", "database connection lost", { err: String(err) }));
  }

  async query<R extends pg.QueryResultRow = any>(sql: string, params: unknown[] = []): Promise<pg.QueryResult<R>> {
    try {
      return await this.pool.query<R>(sql, params);
    } catch (err) {
      if (unavailable(err)) throw new Unavailable(err);
      throw err;
    }
  }

  /** Applies the migrations not yet applied, one instance at a time. */
  async migrate(dir = new URL("../../migrations/", import.meta.url)): Promise<void> {
    const files = readdirSync(dir)
      .filter((f) => /^\d{3}_.+\.sql$/.test(f))
      .sort();
    const c = await this.pool.connect();
    try {
      await c.query("SELECT pg_advisory_lock(7310071)");
      await c.query("CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())");
      const done = new Set((await c.query("SELECT version FROM schema_migrations")).rows.map((r) => r.version as number));
      for (const f of files) {
        const version = Number(f.slice(0, 3));
        if (done.has(version)) continue;
        await c.query("BEGIN");
        try {
          await c.query(readFileSync(new URL(f, dir), "utf8"));
          await c.query("INSERT INTO schema_migrations (version) VALUES ($1)", [version]);
          await c.query("COMMIT");
        } catch (err) {
          await c.query("ROLLBACK");
          throw new Error(`migration ${f}: ${err}`);
        }
        this.log("info", "migrated", { version });
      }
    } finally {
      await c.query("SELECT pg_advisory_unlock(7310071)").catch(() => {});
      c.release();
    }
  }

  /**
   * Calls fn with each NOTIFY on the channel until the returned function is
   * called. Resolves once the LISTEN is in place (or, if the connection is
   * down, once it's queued for when it's back).
   */
  async listen(channel: string, fn: Notify): Promise<() => void> {
    if (!/^[a-z0-9_]{1,63}$/.test(channel)) throw new Error("bad channel name");
    let set = this.channels.get(channel);
    const first = !set;
    if (!set) {
      set = new Set();
      this.channels.set(channel, set);
    }
    set.add(fn);
    if (first) {
      // Later listeners on the channel wait for this LISTEN too: until it's
      // in place, a notification on the channel would be missed.
      const ready = (async () => {
        await this.ensureListener().catch(() => {});
        await this.command(`LISTEN ${channel}`);
      })();
      this.ready.set(channel, ready);
      await ready;
    } else {
      await this.ready.get(channel);
    }
    return () => {
      const s = this.channels.get(channel);
      if (!s) return;
      s.delete(fn);
      if (s.size === 0) {
        this.channels.delete(channel);
        this.ready.delete(channel);
        void this.command(`UNLISTEN ${channel}`);
      }
    };
  }

  private readonly ready = new Map<string, Promise<void>>();

  // LISTEN and UNLISTEN, one at a time on the listening connection.
  private commands: Promise<unknown> = Promise.resolve();
  private command(sql: string): Promise<void> {
    const run = this.commands.then(() => this.listener?.query(sql)).then(
      () => {},
      () => {},
    );
    this.commands = run;
    return run;
  }

  private ensureListener(): Promise<void> {
    if (this.listener || this.closed) return Promise.resolve();
    if (this.connecting) return this.connecting;
    this.connecting = (async () => {
      // keepAlive and a periodic ping: a connection that dropped without a
      // word (a half-open TCP connection) is noticed, and replaced.
      const c = new pg.Client({ connectionString: this.url, connectionTimeoutMillis: 5000, keepAlive: true });
      c.on("notification", (n) => {
        for (const fn of this.channels.get(n.channel) ?? []) fn(n.payload ?? "");
      });
      let ping: NodeJS.Timeout | undefined;
      const lost = (err?: unknown) => {
        clearInterval(ping);
        if (this.listener !== c) return;
        this.listener = undefined;
        this.wasLost = true;
        c.end().catch(() => {});
        if (this.closed) return;
        this.log("warn", "live view connection lost", { err: String(err ?? "ended") });
        this.reconnectLater();
      };
      c.on("error", lost);
      c.on("end", () => lost());
      try {
        await c.connect();
      } catch (err) {
        this.reconnectLater();
        throw err;
      }
      if (this.closed) {
        await c.end().catch(() => {});
        return;
      }
      this.listener = c;
      ping = setInterval(() => {
        if (this.pinging) return;
        this.pinging = true;
        const timer = setTimeout(() => lost(new Error("ping timed out")), this.pingTimeoutMs);
        c.query("SELECT 1")
          .catch((err) => lost(err))
          .finally(() => {
            clearTimeout(timer);
            this.pinging = false;
          });
      }, this.pingMs);
      ping.unref();
      for (const ch of this.channels.keys()) await this.command(`LISTEN ${ch}`);
      this.retry = 500;
      // However it came back (the backoff timer, or a new listen() getting
      // there first), notifications sent while it was down were missed.
      if (this.wasLost && this.listener === c) {
        this.wasLost = false;
        if (this.channels.size > 0) this.onReconnect?.([...this.channels.keys()]);
      }
    })().finally(() => (this.connecting = undefined));
    return this.connecting;
  }

  private wasLost = false;
  private pinging = false;

  private reconnectLater() {
    if (this.closed) return;
    const wait = this.retry;
    this.retry = Math.min(this.retry * 2, 15_000);
    setTimeout(() => {
      if (this.closed || this.listener || this.channels.size === 0) return;
      this.ensureListener().catch(() => {});
    }, wait).unref();
  }

  async close(): Promise<void> {
    this.closed = true;
    const l = this.listener;
    this.listener = undefined;
    await l?.end().catch(() => {});
    await this.pool.end().catch(() => {});
  }
}
