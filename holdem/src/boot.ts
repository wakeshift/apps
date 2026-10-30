// Agent Hold'em's start: open the port first, then get ready.
//
// Cloud Run's startup probe asks a new instance for /health and stops it if
// nothing answers in time. On staging (2026-10-27 to 29) the slow part of a
// slow start came before the app's first database connection: the container
// starting and Node loading the app's modules (the MCP SDK, zod, ajv, pg,
// resvg), sometimes 15 s and more. Migrating and asking Wakeshift took under
// a second. So the server listens with nothing but node:http loaded, answers
// /health from then on, and holds every other request until the app is
// ready. If the app fails to get ready, or isn't ready by the deadline, fail
// is called (main.ts ends the process, so Cloud Run starts another instance).
// This file imports only Node's built-ins: keep it that way.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";

export type Handler = (req: IncomingMessage, res: ServerResponse) => void;

export interface BootOptions {
  port: number;
  host?: string;
  /** Gets the app ready once the port is open: its request handler. */
  ready: () => Promise<Handler>;
  /** Called once if ready throws or takes longer than deadlineMs. */
  fail: (err: unknown) => void;
  /** Default 2 minutes. */
  deadlineMs?: number;
}

export interface Boot {
  server: Server;
  /** Resolves once the port is open. */
  listening: Promise<void>;
  /** The app's handler, once ready. */
  ready: Promise<Handler>;
}

const unavailable = (res: ServerResponse) => {
  res.writeHead(503, { "Content-Type": "text/plain", "Cache-Control": "no-store", "Retry-After": "5" });
  res.end("Starting up. Try again in a moment.");
};

export function boot(o: BootOptions): Boot {
  let handler: Handler | undefined;
  let failed = false;
  let deadline: NodeJS.Timeout | undefined;
  let start!: () => void;
  const begun = new Promise<void>((r) => (start = r));
  const ready = begun.then(() => {
    deadline = setTimeout(() => fail(new Error(`not ready after ${o.deadlineMs ?? 120_000} ms`)), o.deadlineMs ?? 120_000);
    return o.ready();
  });
  const fail = (err: unknown) => {
    if (failed) return;
    failed = true;
    clearTimeout(deadline);
    o.fail(err);
  };
  ready.then(
    (h) => {
      clearTimeout(deadline);
      if (!failed) handler = h;
    },
    fail,
  );

  const server = createServer((req, res) => {
    if (handler) return handler(req, res);
    if ((req.url ?? "").split("?")[0] === "/health") {
      res.writeHead(200, { "Content-Type": "text/plain", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff" });
      return res.end("ok");
    }
    if (failed) return unavailable(res);
    ready.then(
      (h) => (failed ? unavailable(res) : h(req, res)),
      () => unavailable(res),
    );
  });
  const listening = new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(o.port, o.host, () => {
      server.off("error", reject);
      resolve();
      // The app's modules load only now, so the probe is answered while they do.
      start();
    });
  });
  return { server, listening, ready };
}
