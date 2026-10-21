// A fresh, migrated database per test, on the Postgres of make db (or
// WAKESHIFT_TEST_DATABASE_URL), dropped afterwards.
import { randomBytes } from "node:crypto";
import { createServer, request, type IncomingMessage, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import pg from "pg";
import { Db } from "../src/db.js";

const admin = process.env.WAKESHIFT_TEST_DATABASE_URL || "postgres://postgres:postgres@localhost:5439/postgres?sslmode=disable";

interface After {
  after: (fn: () => Promise<void>) => void;
}

/** The URL of a new database with the try app's migrations applied. */
export async function testDatabase(t: After): Promise<string> {
  const name = "try_test_" + randomBytes(6).toString("hex");
  const c = new pg.Client({ connectionString: admin });
  await c.connect();
  await c.query(`CREATE DATABASE ${name}`);
  await c.end();
  const u = new URL(admin);
  u.pathname = "/" + name;
  const url = u.toString();
  const db = new Db(url);
  await db.migrate();
  await db.close();
  t.after(async () => {
    const c = new pg.Client({ connectionString: admin });
    await c.connect();
    await c.query(`DROP DATABASE IF EXISTS ${name} WITH (FORCE)`);
    await c.end();
  });
  return url;
}

/** A Db on url (an instance's own pool and LISTEN connection), closed after the test. */
export function openDb(t: After, url: string, o: ConstructorParameters<typeof Db>[1] = {}): Db {
  const db = new Db(url, o);
  t.after(() => db.close());
  return db;
}

/**
 * A load balancer in miniature: each request goes to the next backend in
 * turn (backends can be swapped, as in a restart). Records which backend
 * served each request.
 */
export async function roundRobin(t: After, backends: string[]): Promise<{ url: string; backends: string[]; served: string[] }> {
  const rr = { url: "", backends, served: [] as string[] };
  let i = 0;
  const server: Server = createServer((req, res) => {
    const target = new URL(rr.backends[i++ % rr.backends.length]);
    rr.served.push(target.host);
    const up = request(
      { host: target.hostname, port: target.port, method: req.method, path: req.url, headers: req.headers },
      (r: IncomingMessage) => {
        res.writeHead(r.statusCode ?? 502, r.headers);
        r.pipe(res);
      },
    );
    up.on("error", () => {
      if (!res.headersSent) res.writeHead(502);
      res.end();
    });
    res.on("close", () => up.destroy());
    req.pipe(up);
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", () => r()));
  t.after(async () => {
    server.closeAllConnections();
    await new Promise<void>((r) => server.close(() => r()));
  });
  rr.url = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  return rr;
}
