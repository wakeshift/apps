// Joins: each seat an outside player takes is noted in joins
// (migrations/013_joins.sql), in the seat's own transaction, for Wakeshift's
// alerts in Slack: the admin service reads the table and posts each join
// (docs/admin.md, "Game joins"). House agents' seats aren't noted. Noting
// one is best effort: a failure is logged and the seat stands. The Go games
// do the same (apps/gamekit/joins.go).

import type { Db, Log, Tx } from "./db.js";

/** How long a join is kept: as long as an idle sign-in is (31 days, SESSION_IDLE in oauth.ts). */
export const JOINS_KEEP_MS = 31 * 24 * 3600 * 1000;

/** The columns of joins the admin service may read: every one but player (the sign-in). */
export const JOINS_COLUMNS = "id, at, agent, name, returned, prev_at, seated";

/**
 * Notes player's seat at table, in c (the seating's transaction), unless
 * they're a house agent. It runs in a savepoint, so a failure leaves the
 * seating as it was.
 */
export async function noteJoin(c: Tx, table: string, player: string, at: Date, log: Log): Promise<void> {
  await c.query("SAVEPOINT note_join");
  try {
    await c.query(
      `INSERT INTO joins (at, player, agent, name, returned, prev_at, seated)
       SELECT $1, p.guest, coalesce(p.harness, ''), p.name, j.prev IS NOT NULL, j.prev,
         (SELECT count(*) FROM seats s JOIN players o ON o.id = s.player WHERE s.table_id = $3 AND NOT o.house)
       FROM players p, LATERAL (SELECT max(at) AS prev FROM joins WHERE player = p.guest) j
       WHERE p.id = $2 AND NOT p.house`,
      [at, player, table],
    );
    await c.query("RELEASE SAVEPOINT note_join");
  } catch (err) {
    await c.query("ROLLBACK TO SAVEPOINT note_join");
    log("warn", "noting a join for the admin service failed", { err: String(err) });
  }
}

/**
 * Lets login, the admin service's database login (on Cloud SQL, its IAM
 * user), connect to this database and read JOINS_COLUMNS of joins, and
 * nothing else. A login that doesn't exist yet is skipped (the admin
 * service reads nothing from Agent Hold'em until it does). Never throws:
 * the game starts either way.
 */
export async function grantJoins(db: Db, login: string | undefined, log: Log): Promise<void> {
  if (!login) return;
  if (login.startsWith("pg_") || login.startsWith("cloudsql")) {
    log("warn", "that can't be the admin service's database login; not granting it joins");
    return;
  }
  try {
    const exists = (await db.query("SELECT 1 FROM pg_roles WHERE rolname = $1", [login])).rowCount! > 0;
    if (!exists) {
      log("warn", "the admin service's database login doesn't exist yet; not granting it joins", { login });
      return;
    }
    const role = ident(login);
    await db.tx(async (c) => {
      const name = (await c.query<{ db: string }>("SELECT current_database() AS db")).rows[0]!.db;
      await c.query(`GRANT CONNECT ON DATABASE ${ident(name)} TO ${role}`);
      await c.query(`REVOKE ALL ON joins FROM ${role}`);
      await c.query(`GRANT SELECT (${JOINS_COLUMNS}) ON joins TO ${role}`);
    });
  } catch (err) {
    log("warn", "granting the admin service joins failed", { err: String(err) });
  }
}

/** A quoted SQL identifier. */
function ident(s: string): string {
  return '"' + s.replaceAll('"', '""') + '"';
}
