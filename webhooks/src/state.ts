// What the server keeps per webhook, in Postgres so it survives restarts and
// every instance sees the same: when the link was last used, the posts
// counted against its limits, the recent activity the page shows live (and
// the posts in full for recent_posts), who is watching, the checked URL the
// user last picked, and the MCP sessions. A post handled by one instance
// reaches pages connected to another by NOTIFY on the webhook's channel.
//
// Kept as the privacy policy says (legal.ts), by sweep(): a webhook's
// activity and posts until a day without use with no one watching (or 7
// days without use); when it was last used and its checked URL until the
// link expires (7 days without use); a watcher until it unwatches, its
// subscription ends (7 days) or the link expires; counts until their
// window ends; MCP sessions until they're idle for as long as a link lives.

import type { Check } from "./links.js";
import type { Db } from "./db.js";

const HOUR = 3600_000;
const DAY = 24 * HOUR;
const RING = 50;
const BODIES = 10;
/** How often a link's last use is written: it only has to be right to the day. */
const TOUCH_EVERY = 5 * 60_000;

/** One line of a webhook's live view. Every string in it may come from whoever posted. */
export interface Activity {
  seq: number;
  at: string;
  kind: "post" | "refused" | "limited" | "failed" | "watch" | "unwatch";
  method?: string;
  contentType?: string;
  size?: number;
  /** The event's text, as the agent reads it (cut for the page). */
  text?: string;
  /** The start of what was posted (the text doesn't repeat it). */
  preview?: string;
  /** Which of the webhook's URLs it came to. */
  check?: Check;
  /** Conversations the publish woke (or will). */
  matched?: number;
  duplicate?: boolean;
  /** Why it was refused, or what failed. */
  reason?: string;
  /**
   * The conversation that watched or stopped, by a public label (never its
   * session key, which acts as the conversation), its client and the
   * harness Wakeshift recognised.
   */
  conversation?: string;
  client?: string;
  harness?: string;
}

/** A post as the agent can read it in full. Its text and data come from whoever posted. */
export interface Post {
  seq: number;
  at: string;
  text: string;
  data: unknown;
}

export interface Watcher {
  /** A public label (conversationLabel in server.ts), never the session key. */
  conversation: string;
  client: string;
  harness?: string;
  since: string;
}

export interface Limits {
  perHour: number;
  perDay: number;
  /** Across every webhook: protects Wakeshift from someone with many links. */
  globalPerHour: number;
}

/** A checked webhook URL the user picked on the page (or that a service posted to). */
export interface Checked {
  check: Exclude<Check, "open">;
  url: string;
}

/** An MCP session: what its initialize said, and the label of the conversation it watched as. */
export interface McpSession {
  id: string;
  hook: string;
  client: { name: string; version: string; [k: string]: unknown };
  capabilities: Record<string, unknown>;
  protocolVersion: string;
  watchedAs?: string;
}

// The live view's NOTIFY payload: "<seq> <activity JSON>", or "<seq>" alone
// when the activity is too big for a notification (it's read back instead).
const MAX_NOTIFY = 7000;

const channelOf = (hook: string) => "try_" + hook;
// Text columns can't hold NUL.
const clean = (s: string, n: number) => s.replace(/\0/g, "").slice(0, n);
const hourEnd = (t: number) => Math.floor(t / HOUR) * HOUR + HOUR;
const dayEnd = (t: number) => Math.floor(t / DAY) * DAY + DAY;
const secondsUntil = (end: number, t: number) => Math.max(1, Math.ceil((end - t) / 1000));
// A line as the page gets it: never a session key, even from a line written
// before lines named conversations by label (002_conversation_labels.sql
// removed those).
const withoutKey = (a: Activity & { session?: unknown }): Activity => {
  delete a.session;
  return a;
};

export class State {
  // When this process last wrote each link's use, so it writes at most every TOUCH_EVERY.
  private readonly touched = new Map<string, number>();

  constructor(
    private readonly db: Db,
    readonly limits: Limits,
    private readonly now: () => number = Date.now,
  ) {
    // Notifications missed while the LISTEN connection was down: send each
    // listener the recent activity again (the page drops what it has).
    db.onReconnect = (channels) => {
      for (const ch of channels) {
        const hook = ch.slice(4);
        this.recent(hook)
          .then((list) => {
            for (const a of list) this.dispatch(hook, a);
          })
          .catch(() => {});
      }
    };
  }

  /** When the link was last used (0: never, as far as anyone recorded). */
  async lastUse(id: string): Promise<number> {
    const r = await this.db.query("SELECT last_use FROM hooks WHERE hook = $1", [id]);
    const t = r.rows[0] ? (r.rows[0].last_use as Date).getTime() : 0;
    return Math.max(t, this.touched.get(id) ?? 0);
  }

  async touch(id: string): Promise<void> {
    const t = this.now();
    if (t - (this.touched.get(id) ?? 0) < TOUCH_EVERY) return;
    await this.db.query(
      "INSERT INTO hooks (hook, last_use) VALUES ($1, $2) ON CONFLICT (hook) DO UPDATE SET last_use = greatest(hooks.last_use, excluded.last_use)",
      [id, new Date(t)],
    );
    this.touched.set(id, t);
  }

  /**
   * Counts a post against the limits of one of the webhook's URLs (each
   * check has its own, so whoever has the open URL can't use up the checked
   * one's), and the global hour, in fixed windows (the hour, the UTC day).
   * Returns 0 if it's allowed, else how many seconds until the next one
   * would be. Call it only for posts that passed the URL's check; pass the
   * same time to untake.
   */
  async take(id: string, check: Check, at = this.now()): Promise<number> {
    const h = hourEnd(at);
    const d = dayEnd(at);
    const r = await this.db.query("SELECT take($1, $2, $3, $4, $5, $6, $7, $8) AS over", [
      `${id}/${check}/d`,
      `${id}/${check}/h`,
      "global/h",
      new Date(d),
      new Date(h),
      this.limits.perDay,
      this.limits.perHour,
      this.limits.globalPerHour,
    ]);
    const over = r.rows[0].over as number;
    if (over === 0) return 0;
    return secondsUntil(over === 1 ? d : h, at);
  }

  /**
   * Gives back what take counted at the time given, for a post Wakeshift
   * didn't take. One row per statement, in take's order (day, hour,
   * global): one statement over the three rows would lock them in scan
   * order, and could deadlock with a take.
   */
  async untake(id: string, check: Check, at: number): Promise<void> {
    const h = new Date(hourEnd(at));
    const d = new Date(dayEnd(at));
    const rows: [string, Date][] = [
      [`${id}/${check}/d`, d],
      [`${id}/${check}/h`, h],
      ["global/h", h],
    ];
    for (const [key, until] of rows) {
      await this.db.query("UPDATE counters SET n = greatest(n - 1, 0) WHERE key = $1 AND until = $2", [key, until]);
    }
  }

  /** Counts one against an hourly limit for key (new links per address). Returns 0, or seconds to wait. */
  async takeHourly(key: string, limit: number, at = this.now()): Promise<number> {
    const h = hourEnd(at);
    const r = await this.db.query(
      "INSERT INTO counters (key, until, n) VALUES ($1, $2, 1) ON CONFLICT (key, until) DO UPDATE SET n = counters.n + 1 WHERE counters.n < $3 RETURNING n",
      [key, new Date(h), limit],
    );
    return r.rowCount ? 0 : secondsUntil(h, at);
  }

  /** The checked URL the user last picked for the webhook, if there is one. */
  async checked(id: string): Promise<Checked | undefined> {
    const r = await this.db.query("SELECT checked_check, checked_url FROM hooks WHERE hook = $1", [id]);
    const row = r.rows[0];
    return row?.checked_url ? { check: row.checked_check, url: row.checked_url } : undefined;
  }

  async pick(id: string, c: Checked): Promise<void> {
    await this.db.query(
      `INSERT INTO hooks (hook, last_use, checked_check, checked_url) VALUES ($1, $2, $3, $4)
       ON CONFLICT (hook) DO UPDATE SET checked_check = excluded.checked_check, checked_url = excluded.checked_url
       WHERE hooks.checked_url IS DISTINCT FROM excluded.checked_url`,
      [id, new Date(this.now()), c.check, c.url],
    );
  }

  /**
   * Adds a line to the webhook's live view (keeping the last 50) and
   * notifies every instance's listeners for it.
   */
  async record(id: string, a: Omit<Activity, "seq" | "at">): Promise<Activity> {
    const at = new Date(this.now());
    const body: Omit<Activity, "seq"> = { ...a, at: at.toISOString() };
    if (body.client !== undefined) body.client = body.client.slice(0, 100);
    if (body.harness !== undefined) body.harness = body.harness.slice(0, 40);
    const json = JSON.stringify(body);
    const r = await this.db.query(
      `WITH ins AS (INSERT INTO activity (hook, at, body) VALUES ($1, $2, $3) RETURNING seq),
            del AS (DELETE FROM activity WHERE hook = $1 AND seq <= (SELECT seq FROM activity WHERE hook = $1 ORDER BY seq DESC OFFSET ${RING - 1} LIMIT 1))
       SELECT seq, pg_notify($4, CASE WHEN $5::boolean THEN seq || ' ' || $3 ELSE seq::text END) FROM ins`,
      [id, at, json, channelOf(id), Buffer.byteLength(json) <= MAX_NOTIFY],
    );
    return { ...body, seq: Number(r.rows[0].seq) };
  }

  /** Keeps a post's text and data for the recent_posts tool (the last 10). */
  async keep(id: string, p: Post): Promise<void> {
    await this.db.query(
      `WITH ins AS (INSERT INTO posts (seq, hook, at, body) VALUES ($1, $2, $3, $4)),
            del AS (DELETE FROM posts WHERE hook = $2 AND seq <= (SELECT seq FROM posts WHERE hook = $2 ORDER BY seq DESC OFFSET ${BODIES - 1} LIMIT 1))
       SELECT 1`,
      [p.seq, id, new Date(p.at), JSON.stringify({ text: p.text, data: p.data })],
    );
  }

  /** The last few posts in full, newest first. */
  async posts(id: string, limit: number): Promise<Post[]> {
    const r = await this.db.query("SELECT seq, at, body FROM posts WHERE hook = $1 ORDER BY seq DESC LIMIT $2", [id, Math.min(limit, BODIES)]);
    return r.rows.map((row) => {
      const b = JSON.parse(row.body);
      return { seq: Number(row.seq), at: (row.at as Date).toISOString(), text: b.text, data: b.data };
    });
  }

  /** The live view so far, oldest first. */
  async recent(id: string): Promise<Activity[]> {
    const r = await this.db.query("SELECT seq, body FROM activity WHERE hook = $1 ORDER BY seq DESC LIMIT $2", [id, RING]);
    return r.rows.reverse().map((row) => ({ ...withoutKey(JSON.parse(row.body)), seq: Number(row.seq) }));
  }

  private readonly local = new Map<string, Set<(a: Activity) => void>>();

  private dispatch(hook: string, a: Activity) {
    for (const fn of this.local.get(hook) ?? []) fn(a);
  }

  /**
   * Calls fn with each new line of the webhook's live view, from any
   * instance. Resolves once it's listening: read recent() after it, so
   * nothing falls between the two.
   */
  async listen(id: string, fn: (a: Activity) => void): Promise<() => void> {
    let set = this.local.get(id);
    if (!set) {
      set = new Set();
      this.local.set(id, set);
    }
    set.add(fn);
    const off = await this.db.listen(channelOf(id), (payload) => {
      const sp = payload.indexOf(" ");
      if (sp > 0) {
        try {
          fn({ ...withoutKey(JSON.parse(payload.slice(sp + 1))), seq: Number(payload.slice(0, sp)) });
        } catch {
          // not ours
        }
        return;
      }
      const seq = Number(payload);
      this.db
        .query("SELECT body FROM activity WHERE seq = $1 AND hook = $2", [seq, id])
        .then((r) => r.rows[0] && fn({ ...withoutKey(JSON.parse(r.rows[0].body)), seq }))
        .catch(() => {});
    });
    return () => {
      off();
      set.delete(fn);
      if (set.size === 0) this.local.delete(id);
    };
  }

  async watched(id: string, w: Watcher): Promise<void> {
    await this.db.query(
      "INSERT INTO watchers (hook, conversation, client, harness, since) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (hook, conversation) DO UPDATE SET client = excluded.client, harness = excluded.harness, since = excluded.since",
      [id, clean(w.conversation, 40), clean(w.client, 100), w.harness ? clean(w.harness, 40) : null, new Date(w.since)],
    );
  }

  async unwatched(id: string, conversation?: string): Promise<void> {
    if (conversation) await this.db.query("DELETE FROM watchers WHERE hook = $1 AND conversation = $2", [id, conversation]);
    else await this.db.query("DELETE FROM watchers WHERE hook = $1", [id]);
  }

  async watchers(id: string): Promise<Watcher[]> {
    const r = await this.db.query("SELECT conversation, client, harness, since FROM watchers WHERE hook = $1 ORDER BY since LIMIT 50", [id]);
    return r.rows.map((row) => ({
      conversation: row.conversation,
      client: row.client,
      ...(row.harness ? { harness: row.harness } : {}),
      since: (row.since as Date).toISOString(),
    }));
  }

  // ── MCP sessions ──────────────────────────────────────────────────────

  /** Stores a new session, keeping the webhook's newest max. */
  async saveSession(s: McpSession, max: number): Promise<void> {
    const t = new Date(this.now());
    await this.db.query(
      `WITH ins AS (INSERT INTO sessions (id, hook, client, capabilities, protocol_version, created_at, last_seen) VALUES ($1, $2, $3, $4, $5, $6, $6)),
            del AS (DELETE FROM sessions WHERE id IN (SELECT id FROM sessions WHERE hook = $2 ORDER BY last_seen DESC OFFSET $7))
       SELECT 1`,
      [s.id, s.hook, JSON.stringify(s.client), JSON.stringify(s.capabilities), clean(s.protocolVersion, 40), t, max - 1],
    );
  }

  /** A session by its ID (and marks it seen), or undefined. */
  async session(id: string): Promise<McpSession | undefined> {
    const t = new Date(this.now());
    const r = await this.db.query(
      `WITH s AS (SELECT * FROM sessions WHERE id = $1),
            u AS (UPDATE sessions SET last_seen = $2 WHERE id = $1 AND last_seen < $2::timestamptz - interval '5 minutes')
       SELECT * FROM s`,
      [id, t],
    );
    const row = r.rows[0];
    if (!row) return undefined;
    return {
      id: row.id,
      hook: row.hook,
      client: JSON.parse(row.client),
      capabilities: JSON.parse(row.capabilities),
      protocolVersion: row.protocol_version,
      watchedAs: row.watched_as ?? undefined,
    };
  }

  async sessionWatched(id: string, conversation: string | undefined): Promise<void> {
    await this.db.query("UPDATE sessions SET watched_as = $2 WHERE id = $1", [id, conversation ?? null]);
  }

  async dropSession(id: string, hook: string): Promise<boolean> {
    const r = await this.db.query("DELETE FROM sessions WHERE id = $1 AND hook = $2", [id, hook]);
    return (r.rowCount ?? 0) > 0;
  }

  // ── Retention ─────────────────────────────────────────────────────────

  /**
   * Forgets what the privacy policy says goes: activity and posts of
   * webhooks idle for over a day with no one watching, or for longer than
   * keepUse (how long a link lives unused); everything else about a webhook
   * once its link has expired; counts whose window has ended; sessions idle
   * for sessionIdle. One instance at a time does it.
   */
  async sweep(keepUse: number, sessionIdle: number): Promise<void> {
    const t = this.now();
    for (const [id, at] of this.touched) if (t - at >= TOUCH_EVERY) this.touched.delete(id);
    const c = await this.db.pool.connect();
    try {
      await c.query("BEGIN");
      const got = await c.query("SELECT pg_try_advisory_xact_lock(7310072) AS ok");
      if (!got.rows[0].ok) return void (await c.query("ROLLBACK"));
      const now = new Date(t);
      const dayAgo = new Date(t - DAY);
      const expired = new Date(t - keepUse);
      // A watch is a subscription that lasts as long as a link lives
      // (keepUse): a conversation that went away without unwatching stops
      // counting as watching once its subscription has ended.
      await c.query("DELETE FROM watchers WHERE since < $1", [expired]);
      await c.query(
        `WITH last AS (
           SELECT hook, max(t) AS t FROM (
             SELECT hook, last_use AS t FROM hooks
             UNION ALL SELECT hook, max(at) FROM activity GROUP BY hook
             UNION ALL SELECT hook, max(at) FROM posts GROUP BY hook
           ) u GROUP BY hook
         ),
         idle AS (
           SELECT hook FROM last l WHERE l.t < $1 AND NOT EXISTS (SELECT 1 FROM watchers w WHERE w.hook = l.hook)
           UNION SELECT hook FROM last WHERE t < $2
         ),
         a AS (DELETE FROM activity WHERE hook IN (SELECT hook FROM idle)),
         p AS (DELETE FROM posts WHERE hook IN (SELECT hook FROM idle))
         SELECT 1`,
        [dayAgo, expired],
      );
      await c.query("DELETE FROM watchers WHERE hook IN (SELECT hook FROM hooks WHERE last_use < $1)", [expired]);
      await c.query("DELETE FROM hooks WHERE last_use < $1", [expired]);
      await c.query("DELETE FROM counters WHERE until <= $1", [now]);
      await c.query("DELETE FROM sessions WHERE last_seen < $1", [new Date(t - sessionIdle)]);
      await c.query("COMMIT");
    } catch (err) {
      await c.query("ROLLBACK").catch(() => {});
      throw err;
    } finally {
      c.release();
    }
  }
}
