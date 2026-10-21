// Agent Hold'em's Postgres: a small pool, transactions, and the migrations
// in ../migrations. Every change to a table runs in a transaction holding its
// row lock, so any number of instances can serve the same tables.

import { readdirSync, readFileSync } from "node:fs";
import pg from "pg";

export type Log = (level: "info" | "warn" | "error", msg: string, attrs?: Record<string, unknown>) => void;

export interface DbOptions {
  max?: number;
  statementTimeoutMs?: number;
  log?: Log;
}

export type Tx = pg.PoolClient;

export class Db {
  readonly pool: pg.Pool;
  private readonly log: Log;

  constructor(url: string, o: DbOptions = {}) {
    this.log = o.log ?? (() => {});
    this.pool = new pg.Pool({
      connectionString: url,
      max: o.max ?? 4,
      connectionTimeoutMillis: 5000,
      idleTimeoutMillis: 60_000,
      options: `-c statement_timeout=${o.statementTimeoutMs ?? 10_000}`,
    });
    // An idle connection that drops (a database restart) mustn't crash the process.
    this.pool.on("error", (err) => this.log("warn", "database connection lost", { err: String(err) }));
  }

  query<R extends pg.QueryResultRow = any>(sql: string, params: unknown[] = []): Promise<pg.QueryResult<R>> {
    return this.pool.query<R>(sql, params);
  }

  /** Runs fn in a transaction: committed if it returns, rolled back if it throws. */
  async tx<T>(fn: (c: Tx) => Promise<T>): Promise<T> {
    const c = await this.pool.connect();
    try {
      await c.query("BEGIN");
      const out = await fn(c);
      await c.query("COMMIT");
      return out;
    } catch (err) {
      await c.query("ROLLBACK").catch(() => {});
      throw err;
    } finally {
      c.release();
    }
  }

  /** Applies the migrations not yet applied, one instance at a time. */
  async migrate(dir = new URL("../../migrations/", import.meta.url)): Promise<void> {
    const files = readdirSync(dir)
      .filter((f) => /^\d{3}_.+\.sql$/.test(f))
      .sort();
    const c = await this.pool.connect();
    try {
      await c.query("SELECT pg_advisory_lock(4407731)");
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
          await c.query("ROLLBACK").catch(() => {});
          throw new Error(`migration ${f}: ${err}`);
        }
        this.log("info", "migrated", { version });
      }
    } finally {
      await c.query("SELECT pg_advisory_unlock(4407731)").catch(() => {});
      c.release();
    }
  }

  close(): Promise<void> {
    return this.pool.end();
  }
}
