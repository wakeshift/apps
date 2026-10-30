// Joins noted for Wakeshift's admin service (src/joins.ts), against a real
// Postgres (make db, or WAKESHIFT_TEST_DATABASE_URL).
import { test } from "node:test";
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import pg from "pg";
import { Casino, type Notifier } from "../src/casino.js";
import type { Db } from "../src/db.js";
import { grantJoins, JOINS_COLUMNS } from "../src/joins.js";
import { testDb } from "./testdb.js";

const adminUrl = process.env.WAKESHIFT_TEST_DATABASE_URL || "postgres://postgres:postgres@localhost:5439/postgres?sslmode=disable";

const quiet: Notifier = {
  turn: async () => true,
  result: async () => {},
  handResult: async () => {},
  removed: async () => {},
  endSubscription: async () => {},
};

function casinoOn(db: Db, clock = { t: Date.parse("2026-10-30T12:00:00Z") }) {
  return { casino: new Casino({ db, notify: quiet, now: () => clock.t, handPauseMs: 0 }), clock };
}

interface JoinRow {
  player: string;
  agent: string;
  name: string;
  returned: boolean;
  prev_at: Date | null;
  seated: number;
}

async function joins(db: Db): Promise<JoinRow[]> {
  return (await db.query<JoinRow>("SELECT player, agent, name, returned, prev_at, seated FROM joins ORDER BY id")).rows;
}

test("an outside player's seat is noted, new or returning, with the outside players seated; a house agent's never is", async (t) => {
  const db = await testDb(t);
  const { casino, clock } = casinoOn(db);
  const house = await casino.identify("c_house", "guest:house", { harness: "house", model: "Haiku 4.5", house: true });
  const ada = await casino.identify("c_ada", "guest:ada", { harness: "claude-code", subscription: "s1" });
  const bo = await casino.identify("c_bo", "guest:bo", { harness: "codex", subscription: "s2" });
  const { table } = await casino.join(house.id, {});
  assert.equal((await casino.join(ada.id, {})).table, table);
  await casino.join(ada.id, {}); // already seated: not a new join
  clock.t += 1000;
  await casino.join(bo.id, {});
  let got = await joins(db);
  assert.equal(got.length, 2);
  assert.deepEqual(got[0], { player: "guest:ada", agent: "claude-code", name: ada.name, returned: false, prev_at: null, seated: 1 });
  assert.deepEqual({ ...got[1], name: undefined }, { player: "guest:bo", agent: "codex", name: undefined, returned: false, prev_at: null, seated: 2 });

  // Another conversation of Ada's sign-in sits down later, at a table of its
  // own: returning, with her previous join.
  clock.t += 60_000;
  const ada2 = await casino.identify("c_ada_2", "guest:ada", { harness: "claude-code", subscription: "s3" });
  await casino.join(ada2.id, { private: true });
  got = await joins(db);
  assert.equal(got.length, 3);
  assert.equal(got[2]!.player, "guest:ada");
  assert.equal(got[2]!.returned, true);
  assert.equal(got[2]!.prev_at?.getTime(), Date.parse("2026-10-30T12:00:00Z"));
  assert.equal(got[2]!.seated, 1);
  assert.ok(!got.some((j) => j.player === "guest:house"), "a house agent's seat was noted");
});

test("a join whose note fails still seats the player", async (t) => {
  const db = await testDb(t);
  const { casino } = casinoOn(db);
  await db.query("DROP TABLE joins");
  const ada = await casino.identify("c_ada", "guest:ada", { harness: "claude-code", subscription: "s1" });
  const { table } = await casino.join(ada.id, {});
  assert.equal((await db.query("SELECT 1 FROM seats WHERE player = $1 AND table_id = $2", [ada.id, table])).rowCount, 1);
});

test("grantJoins lets the admin service's login read joins but not who joined, or anything else", async (t) => {
  const db = await testDb(t);
  const { casino } = casinoOn(db);
  const logs: string[] = [];
  const log = (_level: string, msg: string) => void logs.push(msg);
  await grantJoins(db, "no_such_admin_login", log);
  assert.match(logs.join("\n"), /doesn't exist yet/);
  const ada = await casino.identify("c_ada", "guest:ada", { harness: "claude-code", subscription: "s1" });
  await casino.join(ada.id, {});
  const login = "admin_joins_" + randomBytes(4).toString("hex");
  await db.query(`CREATE ROLE ${login} LOGIN PASSWORD 'pw'`);
  // After testDb's own cleanup has dropped the database, and its grants with it.
  t.after(async () => {
    const c = new pg.Client({ connectionString: adminUrl });
    await c.connect();
    await c.query(`DROP ROLE IF EXISTS ${login}`).catch(() => {});
    await c.end();
  });
  await grantJoins(db, login, log);
  await grantJoins(db, login, log); // again at the next start: the same
  const name = (await db.query<{ db: string }>("SELECT current_database() AS db")).rows[0]!.db;
  const u = new URL(adminUrl);
  u.username = login;
  u.password = "pw";
  u.pathname = "/" + name;
  const c = new pg.Client({ connectionString: u.toString() });
  await c.connect();
  try {
    const r = await c.query(`SELECT ${JOINS_COLUMNS} FROM joins`);
    assert.equal(r.rowCount, 1);
    assert.equal(r.rows[0].agent, "claude-code");
    for (const q of ["SELECT player FROM joins", "SELECT count(*) FROM players", "SELECT count(*) FROM player_sessions", "DELETE FROM joins"]) {
      await assert.rejects(c.query(q), /permission denied/, q);
    }
  } finally {
    await c.end();
  }
});
