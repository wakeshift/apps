import type { Check } from "./links.js";
import type { Activity, Watcher } from "./state.js";
import { pickerHead, pickerHtml } from "./agentpicker/picker.js";

// The pages: server-rendered, in the marketing site's look (site/static),
// system fonts, no outside requests. Everything that comes from a request
// goes through esc() here, and the live view's script only ever sets
// textContent.

export function esc(s: unknown): string {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);
}

/** Wakeshift's environments: the console at app.<site> has its marketing site at <site>. */
const SITES = new Set(["wakeshift.ai", "staging.wakeshift.ai"]);

/**
 * The marketing site for a Wakeshift URL: its host without "app."
 * (https://app.staging.wakeshift.ai is https://staging.wakeshift.ai), for
 * Wakeshift's known environments only. Anything else, such as a local
 * Wakeshift, gets the production site.
 */
export function siteFor(core: string | undefined): string {
  try {
    const u = new URL((core ?? "").trim());
    const host = u.hostname.startsWith("app.") ? u.hostname.slice(4) : "";
    if (u.protocol === "https:" && !u.port && SITES.has(host)) return `https://${host}`;
  } catch {
    // not a URL: the production site
  }
  return "https://wakeshift.ai";
}

/** Wakeshift's homepage for the Wakeshift this runs on (WAKESHIFT_CORE): the pages link to it, the legal documents to its terms and privacy policy. */
export const WAKESHIFT = siteFor(process.env.WAKESHIFT_CORE);

/** Single-quotes s for a POSIX shell. */
export function shq(s: string): string {
  return "'" + s.replace(/'/g, `'\\''`) + "'";
}

export const layout = (title: string, body: string, nonce: string, script = "", head = "") => `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>${esc(title)}</title>
<meta name="theme-color" content="#F5F1E8">
<link rel="icon" href="/static/wakeshift.svg" type="image/svg+xml">
<link rel="stylesheet" href="/static/try.css">${head}
</head>
<body>
<nav class="nav wrap">
  <a class="logo" href="/"><img src="/static/wakeshift.svg" alt="" width="34" height="22">Wakeshift <span class="sub">webhooks</span></a>
  <a class="navlink" href="${esc(WAKESHIFT)}">What's Wakeshift?</a>
</nav>
<main class="wrap">
${body}
</main>
<footer class="ft wrap">A dev tool for trying <a href="${esc(WAKESHIFT)}">Wakeshift</a>, built on its TypeScript SDK. Low limits; nothing here is kept for long. <a href="/terms">Terms</a> · <a href="/privacy">Privacy</a></footer>
${script ? `<script nonce="${nonce}">${script}</script>` : ""}
</body>
</html>`;

export interface PageLimits {
  perHour: number;
  perDay: number;
  idleDays: number;
}

const limitsLine = (l: PageLimits) =>
  `${l.perHour} posts an hour and ${l.perDay} a day per webhook, bodies up to 64 KB. A link expires after ${l.idleDays} days without use.`;

// Getting a link has no sign-in, so no authorize page: this line, next to
// the button, is where people agree to Webhooks' terms.
const AGREE = `By getting a link, you agree to Webhooks' <a href="/terms">Terms</a> and <a href="/privacy">Privacy Policy</a>.`;

export function homePage(nonce: string, l: PageLimits): string {
  return layout(
    "A webhook that wakes your agent · Wakeshift",
    `<section class="hero">
  <div class="kicker">try.wakeshift.ai</div>
  <h1>A webhook that <em>wakes</em> your agent.</h1>
  <p class="lede">Get a link, add it to Claude Code, Codex, Claude Cowork, Cursor, opencode, Antigravity, ChatGPT, Hermes Agent, OpenClaw, Copilot CLI or any MCP client, and ask your agent to watch. Anything posted to your webhook URL, from curl, Postman, GitHub or your own app, wakes the agent with what was sent.</p>
  <form method="post" action="/links" class="cta"><button class="btn" type="submit">Get a link</button></form>
  <p class="note">No sign-up. ${esc(limitsLine(l))}</p>
  <p class="note">${AGREE}</p>
</section>
<ol class="steps">
  <li><span class="n">i.</span><div><b>Connect your agent</b><p>Add your link to your agent as an MCP server (for most agents, one command). Then ask it to watch for webhooks.</p></div></li>
  <li><span class="n">ii.</span><div><b>Post to your webhook URL</b><p>JSON, a form or plain text. Pointing GitHub, Stripe or Linear at it? Turn on that service's signature check.</p></div></li>
  <li><span class="n">iii.</span><div><b>Your agent wakes</b><p>It reads what was posted, as information from outside, not as your instructions. This page shows each post as it arrives.</p></div></li>
</ol>`,
    nonce,
  );
}

export function messagePage(nonce: string, title: string, text: string, action = true): string {
  return layout(
    title + " · Wakeshift webhooks",
    `<section class="hero"><h1>${esc(title)}</h1><p class="lede">${esc(text)}</p>${
      action ? `<form method="post" action="/links" class="cta"><button class="btn" type="submit">Get a new link</button></form><p class="note">${AGREE}</p>` : ""
    }</section>`,
    nonce,
  );
}

export interface LinkPageData {
  code: string;
  mcpUrl: string;
  check: Check;
  /** The URL for the check: none yet for Stripe or Linear until their secret is pasted. */
  webhookUrl?: string;
  openUrl: string;
  /** The webhook's secret for the GitHub or bearer check. */
  secret?: string;
  /** Why a pasted secret wasn't taken. */
  error?: string;
  /** A line at the top when the page couldn't show everything (the server is busy). */
  notice?: string;
  watchers: Watcher[];
  recent: Activity[];
  limits: PageLimits;
}

/** How each check is named to agents and in the GET explanation. */
export const checkNames: Record<Check, string> = {
  open: "none: anyone with the URL can post",
  github: "GitHub's X-Hub-Signature-256",
  bearer: "an Authorization: Bearer token",
  stripe: "Stripe's Stripe-Signature",
  linear: "Linear's Linear-Signature",
};

const codeBlock = (label: string, text: string, id: string) =>
  `<div class="cmd"><small>${esc(label)}</small><div class="row"><code id="${id}">${esc(text)}</code><button class="btn ghost sm copy" type="button" data-copy="${id}">Copy</button></div></div>`;

export function curlFor(check: Check, url: string, secret: string): string {
  const body = `{"hello":"from curl"}`;
  switch (check) {
    case "open":
      return `curl -X POST ${shq(url)} -H 'Content-Type: application/json' -d ${shq(body)}`;
    case "bearer":
      return `curl -X POST ${shq(url)} -H ${shq("Authorization: Bearer " + secret)} -H 'Content-Type: application/json' -d ${shq(body)}`;
    case "github":
      return (
        `body=${shq(body)}; curl -X POST ${shq(url)} -H 'Content-Type: application/json' -H 'X-GitHub-Event: ping' ` +
        `-H "X-Hub-Signature-256: sha256=$(printf %s "$body" | openssl dgst -sha256 -hmac ${shq(secret)} | sed 's/^.* //')" -d "$body"`
      );
    // The pasted secret isn't shown again: these read it from the shell.
    case "stripe": {
      const ev = `{"object":"event","type":"payment_intent.succeeded","data":{"object":{"id":"pi_test","amount":2000,"currency":"usd"}}}`;
      return (
        `body=${shq(ev)}; t=$(date +%s); curl -X POST ${shq(url)} -H 'Content-Type: application/json' ` +
        `-H "Stripe-Signature: t=$t,v1=$(printf %s "$t.$body" | openssl dgst -sha256 -hmac "$STRIPE_SECRET" | sed 's/^.* //')" -d "$body"`
      );
    }
    case "linear":
      return (
        `body='{"type":"Issue","action":"create","data":{"identifier":"ENG-1","title":"Test issue"},"webhookTimestamp":'"$(date +%s)000}"; ` +
        `curl -X POST ${shq(url)} -H 'Content-Type: application/json' -H 'Linear-Event: Issue' ` +
        `-H "Linear-Signature: $(printf %s "$body" | openssl dgst -sha256 -hmac "$LINEAR_SECRET" | sed 's/^.* //')" -d "$body"`
      );
  }
}

// What each tab says, and the line in the page's copy for it.
const checkNotes: Record<Check, string> = {
  open: "Open: anyone with this URL can post to your webhook. Turn on a check before you give it to a service.",
  github:
    "Requests must carry GitHub's X-Hub-Signature-256: an HMAC of the body with the secret. In GitHub: Settings, Webhooks, Add webhook; Payload URL is this URL, Content type application/json, Secret the secret below.",
  bearer: "Requests must carry the header Authorization: Bearer and the secret below.",
  stripe:
    "Requests must carry Stripe's Stripe-Signature, made with your endpoint's signing secret, from the last 5 minutes. In Stripe, go to Developers, Webhooks, Add endpoint, and enter the open URL. Reveal the endpoint's signing secret (whsec_…) and paste it here. Then change the endpoint's URL to the one you get.",
  linear:
    "Requests must carry Linear's Linear-Signature, made with your webhook's signing secret, and a webhookTimestamp from the last minute. In Linear, go to Settings, API, Webhooks, New webhook, and enter the open URL. Copy the webhook's signing secret and paste it here. Then change the webhook's URL to the one you get.",
};

/** What to ask the agent, whichever agent it is. */
export const askLine = "Watch for webhooks and tell me when one arrives.";

/** How to connect each agent: the shared agent picker (apps/agentpicker). */
export const agentPicker = (mcpUrl: string) => pickerHtml({ name: "webhooks", url: mcpUrl, ask: askLine, each: "each post", noAuth: true });

export function linkPage(nonce: string, d: LinkPageData): string {
  const tab = (c: Check, label: string) =>
    `<a class="tab${d.check === c ? " on" : ""}" href="/l/${esc(d.code)}${c === "open" ? "" : "?check=" + c}">${esc(label)}</a>`;
  const pasted = d.check === "stripe" || d.check === "linear";
  const service = d.check === "stripe" ? "Stripe" : "Linear";
  const secretBlock = d.secret && (d.check === "github" || d.check === "bearer") ? codeBlock("Secret", d.secret, "secret") : "";
  // Stripe and Linear make the secret: the user pastes it, and gets a URL that holds it.
  const pasteForm = pasted
    ? `<form method="post" action="/l/${esc(d.code)}?check=${d.check}" class="paste">
    <label for="pasted">${esc(service)} signing secret</label>
    <div class="row"><input id="pasted" name="secret" type="password" autocomplete="off" spellcheck="false" required maxlength="200" placeholder="${d.check === "stripe" ? "whsec_…" : "From the webhook's page in Linear"}"><button class="btn sm" type="submit">${d.webhookUrl ? "Make another URL" : "Make the URL"}</button></div>
  </form>${d.error ? `<p class="warn">${esc(d.error)}</p>` : ""}`
    : "";
  const urlBlock = d.webhookUrl
    ? codeBlock("Webhook URL", d.webhookUrl, "url") +
      secretBlock +
      codeBlock(pasted ? `Try it (set ${d.check === "stripe" ? "STRIPE_SECRET" : "LINEAR_SECRET"} to the secret first)` : "Try it", curlFor(d.check, d.webhookUrl, d.secret ?? ""), "curl")
    : "";
  const notes: string[] = [];
  if (pasted && d.webhookUrl) {
    notes.push(`This URL holds your ${service} secret, encrypted: this server can check signatures with it, but it isn't shown again. Making another URL doesn't turn this one off.`);
  }
  if (d.check !== "open" && d.webhookUrl) {
    notes.push("This URL refuses requests without the check, and has its own hourly limit. The open URL keeps working for anyone who has it; if you've shared it, get a new link.");
  }
  notes.push(
    d.check === "open" || d.check === "bearer"
      ? "A retry with the same Idempotency-Key header is sent to your agent once (it still counts against the limit), and so are GitHub's, Stripe's and Linear's retries of an event."
      : "A retry of an event (the same delivery or event ID) is sent to your agent once. It still counts against the limit.",
  );
  const also = notes.map((n) => `<p class="note">${esc(n)}</p>`).join("\n  ");
  const body = `<section class="hero">
  <div class="kicker">Your webhook</div>
  <h1>Connect your agent, then <em>post</em>.</h1>
  <p class="warn">This page's address is your link: bookmark it. Anyone with it can connect an agent to your webhook and see what's posted.</p>${d.notice ? `\n  <p class="warn" id="notice">${esc(d.notice)}</p>` : ""}
</section>

<section class="sect">
  <h2><span class="n">i.</span> Connect your agent</h2>
  ${agentPicker(d.mcpUrl)}
  <p class="status" id="watching"></p>
  <p class="status" id="connected" role="status"></p>
</section>

<section class="sect">
  <h2><span class="n">ii.</span> Post to your webhook URL</h2>
  <div class="tabs">${tab("open", "Open")}${tab("github", "GitHub")}${tab("stripe", "Stripe")}${tab("linear", "Linear")}${tab("bearer", "Bearer token")}</div>
  <p class="note">${esc(checkNotes[d.check])}</p>
  ${pasteForm}
  ${urlBlock}
  ${also}
</section>

<section class="sect">
  <h2><span class="n">iii.</span> Live</h2>
  <p class="status" id="livestatus" role="status" hidden></p>
  <ul class="live" id="live"><li class="empty">Nothing yet. Posts show up here as they arrive.</li></ul>
  <p class="note">${esc(limitsLine(d.limits))} The list keeps the last 50, for a day after the webhook was last used (longer while an agent is watching).</p>
</section>`;
  const initial = JSON.stringify({ watchers: d.watchers, recent: d.recent }).replace(/</g, "\\u003c");
  return layout("Your webhook · Wakeshift webhooks", body, nonce, liveScript(d.code, initial), pickerHead());
}

/** Who's connected now, from Wakeshift's status of the webhook's topic (GET /l/<code>/status). */
export interface Connection {
  subscriptions: number;
  connected: number;
  via: { stream: number; poll: number; hooks: number; webhook?: number };
}

/**
 * The page's line for a Connection. The live view's script runs it as is
 * (its source goes into the page), so it's plain JavaScript in effect.
 */
export function connectedText(s: Connection): string {
  if (s.subscriptions === 0) return "No agent is watching right now.";
  if (s.connected === 0) {
    return (s.subscriptions === 1 ? "An agent is" : s.subscriptions + " agents are") + " watching, not connected right now: posts wait until it connects or next uses the webhook's tools.";
  }
  const how: string[] = [];
  if (s.via.stream) how.push(s.via.stream + " listening live");
  if (s.via.hooks) how.push(s.via.hooks + " through the hooks or connector you installed");
  if (s.via.poll) how.push(s.via.poll + " waiting for posts");
  if (s.via.webhook) how.push(s.via.webhook + " in ChatGPT, which starts a run for each post");
  const rest = s.subscriptions - s.connected;
  return (
    (s.connected === 1 ? "Agent connected" : s.connected + " agents connected") +
    ": " +
    how.join(", ") +
    (rest > 0 ? "; " + rest + " more watching, not connected right now." : ".")
  );
}

// The live view: the activity so far, then each new line over SSE.
// Only textContent: everything in an activity may come from whoever posted.
function liveScript(code: string, initial: string): string {
  return `(() => {
const state = ${initial};
const live = document.getElementById("live");
const watching = document.getElementById("watching");
const labels = { post: "Received", refused: "Refused", limited: "Rate limited", failed: "Not sent", watch: "Watching", unwatch: "Stopped watching" };
const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; };
// The agent's app, as people know it (MCP clients give themselves other names).
const apps = { "claude-code": "Claude Code", "codex-mcp-client": "Codex", "Cursor": "Cursor", "cursor": "Cursor", "cursor-vscode": "Cursor", "opencode": "opencode", "antigravity-client": "Antigravity", "antigravity": "Antigravity", "copilot-cli": "Copilot CLI", "gemini-cli-mcp-client": "Gemini CLI", "Visual Studio Code": "VS Code" };
// The harness Wakeshift recognised, when it did (OpenClaw running a turn in
// Codex calls as Codex's client, but it's OpenClaw).
const harnesses = { "claude-code": "Claude Code", codex: "Codex", cursor: "Cursor", "cursor-cloud": "Cursor cloud agent", "grok-bot": "Grok Bot", copilot: "Copilot CLI", opencode: "opencode", antigravity: "Antigravity", openclaw: "OpenClaw", hermes: "Hermes Agent", chatgpt: "ChatGPT" };
const appName = (c, h) => (h && Object.hasOwn(harnesses, h) ? harnesses[h] : Object.hasOwn(apps, c) ? apps[c] : c) || "an agent app";
// A conversation: its app and its public label (never its session key).
const who = (x) => appName(x.client, x.harness) + (x.conversation ? " (" + x.conversation + ")" : "");
function showWatchers() {
  const w = state.watchers;
  watching.textContent = w.length === 0
    ? "No agent is watching yet."
    : "Watching: " + w.map(who).join(", ") + ".";
  watching.className = "status" + (w.length ? " on" : "");
}
function detail(a) {
  if (a.kind === "post") {
    if (a.duplicate) return "A retry of an earlier post: sent once.";
    if (a.matched === 0) return "Nobody is watching: ask your agent to watch your webhook.";
    return "Sent to " + a.matched + " watching conversation" + (a.matched === 1 ? "" : "s") + ".";
  }
  if (a.kind === "watch" || a.kind === "unwatch") return "A conversation in " + who(a);
  return a.reason || "";
}
function add(a) {
  if (a.kind === "watch") state.watchers = state.watchers.filter((x) => x.conversation !== a.conversation).concat([{ conversation: a.conversation, client: a.client, harness: a.harness, since: a.at }]);
  if (a.kind === "unwatch") state.watchers = state.watchers.filter((x) => x.conversation !== a.conversation);
  showWatchers();
  const empty = live.querySelector(".empty");
  if (empty) empty.remove();
  const li = el("li", "item " + a.kind);
  const head = el("div", "head");
  head.append(el("span", "badge", labels[a.kind] || a.kind), el("time", "", new Date(a.at).toLocaleTimeString()));
  if (a.method) head.append(el("span", "meta", a.method + " " + (a.contentType || "no content type") + " · " + a.size + " bytes"));
  li.append(head);
  if (a.text) li.append(el("div", "text", a.text));
  if (a.preview) li.append(el("div", "preview", a.preview));
  const d = detail(a);
  if (d) li.append(el("div", "detail", d));
  li.dataset.seq = String(a.seq);
  const first = live.firstElementChild;
  if (first && Number(first.dataset.seq) > a.seq) {
    // Arrived late (from another instance, or a replay): put it in order.
    let after = first;
    while (after.nextElementSibling && Number(after.nextElementSibling.dataset.seq) > a.seq) after = after.nextElementSibling;
    after.after(li);
  } else live.prepend(li);
  while (live.children.length > 50) live.lastChild.remove();
}
// Lines can come twice (a replay after reconnecting) or out of order (from
// another instance): each is shown once, in order of seq.
// The page shows the newest 50; it remembers the last 500 seen, so a page
// left open for days doesn't grow without end. It forgets the lowest seq;
// anything at or below that is older than 500 lines it has, so long out of
// view, and it's dropped rather than shown again.
const seen = new Set();
let forgotten = -Infinity;
const once = (a) => {
  if (seen.has(a.seq) || a.seq <= forgotten) return;
  seen.add(a.seq);
  if (seen.size > 500) {
    const lowest = Math.min(...seen);
    seen.delete(lowest);
    forgotten = lowest;
  }
  add(a);
};
for (const a of state.recent) once(a);
showWatchers();
const connected = document.getElementById("connected");
const connectedText = ${connectedText.toString()};
function showConnected(s) {
  connected.textContent = connectedText(s);
  connected.className = "status" + (s.connected ? " on" : "");
}
async function poll() {
  try {
    const r = await fetch(location.pathname.replace(/\\/$/, "") + "/status", { cache: "no-store" });
    if (r.ok) showConnected(await r.json());
  } catch {}
}
setInterval(poll, 10000);
poll();
// The live view. EventSource retries a dropped connection by itself, but
// gives up on an error status (503 when the server is busy): then try
// again later, and say so.
const liveStatus = document.getElementById("livestatus");
let wait = 5;
function connect() {
  const es = new EventSource(location.pathname.replace(/\\/$/, "") + "/live");
  es.onopen = () => { wait = 5; liveStatus.hidden = true; };
  es.onmessage = (m) => once(JSON.parse(m.data));
  es.onerror = () => {
    if (es.readyState !== EventSource.CLOSED) return;
    liveStatus.hidden = false;
    liveStatus.textContent = "The live view is busy. Trying again in " + wait + " seconds; posts still reach your agent.";
    setTimeout(connect, wait * 1000);
    wait = Math.min(wait * 2, 60);
  };
}
connect();
for (const b of document.querySelectorAll("button.copy")) {
  b.addEventListener("click", async () => {
    const t = document.getElementById(b.dataset.copy).textContent;
    try { await navigator.clipboard.writeText(t); b.textContent = "Copied"; } catch { b.textContent = "Select and copy"; }
    setTimeout(() => (b.textContent = "Copy"), 1500);
  });
}
})();`;
}

export const logoSvg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="11.0 22.5 74.0 47.5" role="img" aria-label="Wakeshift"><path d="M33 60 A15 15 0 0 1 63 60 Z" fill="#C25418"/><path d="M69.93 50.24 A24 24 0 0 0 26.07 50.24" fill="none" stroke="#C25418" stroke-width="5" stroke-linecap="round"/><path d="M78.15 46.58 A33 33 0 0 0 17.85 46.58" fill="none" stroke="#E39A6E" stroke-width="5" stroke-linecap="round"/><rect x="12.0" y="63" width="72" height="5" rx="2.5" fill="#1D1B17"/></svg>`;

// The marketing site's palette and type (site/static/site.css).
export const css = `:root{
  --paper:#F5F1E8; --paper-2:#EDE7DA; --card:#FFFDF8; --ink:#1D1B17; --ink-2:#4A463E; --ink-3:#6B665B;
  --rule:#DDD5C4; --accent:#C25418; --accent-soft:#F6E2D3; --sage:#3E6B55; --sage-soft:#DFEBE2;
  --caution:#9A5B00; --caution-bg:#FBF0D9; --caution-rule:#E9C98A;
  --serif:"Iowan Old Style","Charter","Palatino Linotype",Palatino,Georgia,serif;
  --sans:-apple-system,BlinkMacSystemFont,"Segoe UI","Helvetica Neue",Arial,sans-serif;
  --mono:ui-monospace,"SF Mono",Menlo,Consolas,monospace;
  --gutter:clamp(20px,6.67vw,96px);
}
*{box-sizing:border-box;margin:0;padding:0}
body{background:var(--paper);font-family:var(--sans);color:var(--ink);-webkit-font-smoothing:antialiased;min-height:100vh;display:flex;flex-direction:column}
body>main{flex:1}
a{color:inherit}
.wrap{max-width:960px;margin-inline:auto;padding-inline:var(--gutter);width:100%}
.nav{display:flex;align-items:center;justify-content:space-between;padding-block:24px;border-bottom:1px solid var(--rule)}
.logo{display:flex;align-items:center;gap:10px;font:600 19px var(--serif);text-decoration:none}
.logo .sub{font:600 12px var(--mono);letter-spacing:.08em;text-transform:uppercase;color:var(--accent)}
.navlink{font-size:14px;color:var(--ink-2);text-decoration:none}
.kicker{font:600 12px var(--mono);letter-spacing:.12em;text-transform:uppercase;color:var(--accent)}
.hero{padding-block:64px 32px}
.hero h1{font:400 56px/1.05 var(--serif);letter-spacing:-.02em;margin:16px 0 20px}
.hero h1 em{font-style:italic;color:var(--accent)}
.lede{font-size:19px;line-height:1.55;color:var(--ink-2);max-width:640px}
.cta{margin-top:28px}
.btn{display:inline-flex;text-decoration:none;align-items:center;justify-content:center;font:600 14px var(--sans);padding:12px 22px;border-radius:999px;border:1px solid var(--ink);background:var(--ink);color:var(--paper);cursor:pointer}
.btn:hover{background:#000}
.btn.ghost{background:transparent;color:var(--ink);border-color:var(--rule)}
.btn.ghost:hover{border-color:var(--ink-3)}
.btn.sm{padding:6px 12px;font-size:12px}
:focus-visible{outline:2px solid var(--accent);outline-offset:3px}
.note{font-size:14px;color:var(--ink-3);line-height:1.5;margin-top:12px}
.warn{font-size:14px;line-height:1.5;margin-top:8px;padding:10px 14px;border:1px solid var(--caution-rule);background:var(--caution-bg);color:var(--caution);border-radius:10px;max-width:640px}
.steps{list-style:none;border-top:1px solid var(--ink);margin-bottom:64px}
.steps li{display:grid;grid-template-columns:40px 1fr;padding:20px 0;border-bottom:1px solid var(--rule)}
.n{font:italic 400 22px var(--serif);color:var(--accent)}
.steps b{font-size:16px;display:block;margin-bottom:4px}
.steps p{font-size:14.5px;line-height:1.55;color:var(--ink-2)}
.sect{padding-block:28px;border-top:1px solid var(--rule)}
.sect h2{font:400 28px/1.2 var(--serif);margin-bottom:16px;display:flex;gap:12px;align-items:baseline}
.sect p{font-size:15px;line-height:1.55;color:var(--ink-2);margin-top:12px}
.sect p.note{font-size:13.5px;color:var(--ink-3)}
.cmd{border:1px solid var(--rule);border-radius:12px;background:var(--card);padding:12px 16px;margin-top:10px}
.cmd small{font:600 11px var(--mono);letter-spacing:.1em;text-transform:uppercase;color:var(--ink-3)}
.cmd .row{display:flex;gap:12px;align-items:flex-start;margin-top:6px}
.cmd code{flex:1;font:13.5px/1.6 var(--mono);word-break:break-all;white-space:pre-wrap}
.tabs{display:flex;flex-wrap:wrap;gap:6px;margin-bottom:4px}
.tab{font:600 13px var(--sans);padding:7px 14px;border-radius:999px;border:1px solid var(--rule);text-decoration:none;color:var(--ink-2)}
.tab.on{background:var(--ink);border-color:var(--ink);color:var(--paper)}
.sect .ap{--ap-fg:var(--ink);--ap-muted:var(--ink-3);--ap-line:var(--rule);--ap-accent:var(--ink);--ap-on-fg:var(--paper);--ap-code-bg:var(--card);--ap-code-fg:var(--ink);--ap-box-radius:12px;--ap-mono:var(--mono)}
.sect .ap-code{border:1px solid var(--rule)}
.sect .ap p.ap-note,.sect .ap-facts{font-size:13.5px;color:var(--ink-3)}
.sect .ap p.ap-label{font:600 11px var(--mono);letter-spacing:.1em;text-transform:uppercase;color:var(--ink-3)}
.status{font-size:14px;color:var(--ink-3)}
.status.on{color:var(--sage)}
.live{list-style:none;border-top:1px solid var(--ink)}
.live li{padding:12px 0;border-bottom:1px solid var(--rule);font-size:14px}
.live .empty{color:var(--ink-3)}
.live .head{display:flex;gap:10px;align-items:center;flex-wrap:wrap}
.live time,.live .meta{font:12px var(--mono);color:var(--ink-3)}
.badge{font:600 10px var(--mono);letter-spacing:.06em;text-transform:uppercase;padding:3px 8px;border-radius:999px;background:var(--paper-2);color:var(--ink-2)}
.post .badge,.watch .badge{background:var(--sage-soft);color:var(--sage)}
.refused .badge,.limited .badge,.failed .badge{background:var(--caution-bg);color:var(--caution)}
.live .text{font:13.5px/1.5 var(--mono);margin-top:6px;word-break:break-word;white-space:pre-wrap}
.live .preview{font:12.5px/1.5 var(--mono);color:var(--ink-3);margin-top:4px;word-break:break-all;white-space:pre-wrap}
.live .detail{color:var(--ink-2);margin-top:4px}
.paste{margin-top:12px}
.paste label{font:600 11px var(--mono);letter-spacing:.1em;text-transform:uppercase;color:var(--ink-3)}
.paste .row{display:flex;gap:10px;margin-top:6px}
.paste input{flex:1;font:13.5px var(--mono);padding:8px 12px;border:1px solid var(--rule);border-radius:10px;background:var(--card);color:var(--ink)}
.ft{padding-block:28px;border-top:1px solid var(--rule);font-size:13px;color:var(--ink-3)}
.prose{max-width:720px;padding-block:8px 56px;font-size:16px;line-height:1.65;color:var(--ink-2)}
.prose h1{font:400 44px/1.1 var(--serif);color:var(--ink);margin:28px 0 8px}
.prose h2{font:400 24px/1.25 var(--serif);color:var(--ink);margin:32px 0 8px}
.prose p,.prose li{margin-bottom:12px}
.prose ul{padding-left:22px;margin-bottom:12px}
.prose a{color:var(--accent)}
.prose code{font:14px var(--mono)}
.prose em{color:var(--ink-3)}
@media (max-width:640px){.hero h1{font-size:40px}}
`;
