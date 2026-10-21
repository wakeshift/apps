// A fresh, migrated database per test, on the Postgres of make db (or
// WAKESHIFT_TEST_DATABASE_URL), dropped afterwards.
import { randomBytes } from "node:crypto";
import pg from "pg";
import { Db } from "../src/db.js";

const admin = process.env.WAKESHIFT_TEST_DATABASE_URL || "postgres://postgres:postgres@localhost:5439/postgres?sslmode=disable";

interface After {
  after: (fn: () => Promise<void>) => void;
}

/** A Db on a new database with Agent Hold'em's migrations applied, dropped after the test. */
export async function testDb(t: After): Promise<Db> {
  const name = "holdem_test_" + randomBytes(6).toString("hex");
  const c = new pg.Client({ connectionString: admin });
  await c.connect();
  await c.query(`CREATE DATABASE ${name}`);
  await c.end();
  const u = new URL(admin);
  u.pathname = "/" + name;
  const db = new Db(u.toString());
  await db.migrate();
  t.after(async () => {
    await db.close();
    const c = new pg.Client({ connectionString: admin });
    await c.connect();
    await c.query(`DROP DATABASE IF EXISTS ${name} WITH (FORCE)`);
    await c.end();
  });
  return db;
}
