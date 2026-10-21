// The terms and privacy policy for try.wakeshift.ai, served at /terms (and
// /tos) and /privacy. The documents are Markdown, rendered here by a small
// renderer for the little they use (headings, paragraphs, lists, bold,
// italics, code and links); everything else is escaped as text. The Go apps
// do the same with apps/legal.

import { esc, layout } from "./page.js";

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

/** Where Wakeshift's own terms and privacy policy are: the site for the Wakeshift this runs on (WAKESHIFT_CORE). */
export const WAKESHIFT = siteFor(process.env.WAKESHIFT_CORE);

export interface LegalDoc {
  title: string;
  draft: string;
  markdown: string;
}

/** The limits the documents state: the ones this server runs with (TRY_PER_HOUR and the rest). */
export interface DocLimits {
  perHour: number;
  perDay: number;
  linksPerHour: number;
  idleDays: number;
}

export const defaultDocLimits: DocLimits = { perHour: 60, perDay: 500, linksPerHour: 30, idleDays: 7 };

const days = (n: number) => (n === 1 ? "1 day" : `${n} days`);

const termsFor = (l: DocLimits) => `# Webhooks Terms of Service

*Draft for legal review · Last updated [date]*

Webhooks at try.wakeshift.ai is a tool made by Clawvisor Inc. ("we", "us"), and runs on Wakeshift. These are Webhooks' terms: they are part of [Wakeshift's Terms of Service](${WAKESHIFT}/terms), which apply to Webhooks in full, including the section on using an app without an account. Where the two conflict, these win for Webhooks.

## 1. What Webhooks is

Webhooks gives you a webhook URL that wakes your agent (an AI assistant such as Claude Code or Codex): anything posted to it wakes the agents watching it, with what was sent. It's built on Wakeshift's TypeScript SDK.

You don't need an account: your link is all you need. The page says, next to the button that makes a link, that making one means you agree to these terms and to Wakeshift's, with links to both. You must be 18 or over.

## 2. A tool for trying Wakeshift

Webhooks is for trying Wakeshift and for building with it, not for running anything that matters.

- **Limits:** ${l.perHour} posts an hour and ${l.perDay} a day to each of a webhook's URLs, bodies up to 64 KB, and ${l.linksPerHour} new links an hour from one address. Over a limit, posts are refused.
- **Links expire** after ${days(l.idleDays)} without use.
- **What's kept.** Webhooks' own copy of recent posts is deleted a day after your webhook was last used (${days(l.idleDays)} while an agent is watching it), and the rest of what it keeps for a link when the link expires. Wakeshift keeps what it delivered for longer. The [privacy notice](/privacy) lists what's kept, where, and for how long.
- We may change the limits, reset links, or stop offering Webhooks at any time.

## 3. Your link and webhook URLs

Your link and your webhook URLs work for anyone who has them:

- Anyone with your **link** can connect an agent to your webhook and open its page, which shows each post as it arrives, the conversations watching, and the webhook's GitHub and bearer secrets.
- Anyone with your **open webhook URL** can post to it and wake the agents watching. The other URLs on the page accept only posts that prove where they came from: GitHub's \`X-Hub-Signature-256\`, Stripe's \`Stripe-Signature\` or Linear's \`Linear-Signature\`, or the webhook's bearer token.
- For Stripe and Linear, you paste the signing secret they give you into the page. It's encrypted into the URL you get, which only Webhooks can read. Webhooks keeps that URL, to tell agents about it, until the link expires, so the secret is stored, but only in that encrypted form. The secret itself isn't shown again.

Keep them to yourself, and share them only with what you want to post to your agent.

## 4. What you post

- Don't post passwords, keys or other secrets, or other people's personal data. Anyone with the link can see what was posted.
- Posts reach your agent marked as coming from outside, not as your instructions, but a post can still try to get your agent to do something you didn't intend. You're responsible for what you post and for what your agent does with it.

## 5. Acceptable use

Also, don't make many links to get around the limits, and don't use a webhook to wake agents whose users didn't give you its URL.

## 6. Changes

A change to these terms applies to links made after it takes effect.

See also the [Webhooks privacy notice](/privacy).
`;

const privacyFor = (l: DocLimits) => `# Webhooks Privacy Policy

*Draft for legal review · Last updated [date]*

Webhooks at try.wakeshift.ai is a tool made by Clawvisor Inc. ("we", "us") and runs on Wakeshift. This notice is part of [Wakeshift's Privacy Policy](${WAKESHIFT}/privacy). It says what Webhooks collects, where it goes and how long it's kept. Who we are, why we use data, your rights, where data is stored and how we tell you about changes are all in Wakeshift's policy. Contact: support@clawvisor.com.

## What Webhooks collects

- **Links:** none are stored. A link is signed, and names a random webhook ID and when it was made. There's no account, and no name or email address is asked for.
- **Posts:** each request to a webhook URL: its body (up to 64 KB), method, content type, and only these headers: \`Content-Type\`, \`User-Agent\`, \`X-GitHub-Event\`, \`X-GitHub-Delivery\`, \`X-GitHub-Hook-ID\`, \`X-GitLab-Event\`, \`X-Event-Key\`, \`Linear-Event\`, \`Linear-Delivery\`, \`Idempotency-Key\` and \`X-Request-ID\`. For Stripe and Linear, also the event's type and its ID (Stripe's event ID, Linear's delivery ID), to send a retried event once. Never \`Authorization\`, cookies, or signature headers.
- **Signing secrets you paste** for Stripe or Linear: stored only encrypted. Each is encrypted into the webhook URL made for it, which only Webhooks can read, and Webhooks keeps that URL (see below).
- **Who's watching:** for each agent conversation that watches a webhook, the name Wakeshift gives it and the name the agent's program gives itself (for example "Claude Code").
- **Agent programs:** for each program connected to a webhook (for example Claude Code or Cursor), the name and version it gives itself.
- **IP addresses:** the address that asks for a new link (for an IPv6 address, its first 64 bits), to limit links to ${l.linksPerHour} an hour per address.

## Where it goes, and for how long

- **To Wakeshift.** Webhooks passes each post on as a message: a line saying what came (for GitHub, a summary such as the workflow and its result), with the parsed body and the headers above attached. Wakeshift stores it with its record of the webhook and the conversations watching it, delivers it to those conversations, and keeps it until we delete Webhooks, as Wakeshift's policy says. We may delete it sooner.
- **In Webhooks' database**, for each webhook: its last 50 requests for the live view (time, method, content type, size, the start of the body as text (up to 160 characters), the line the agent reads, which URL it came to, how many conversations it reached, and why a refused one was refused) and its last 10 posts in full for the agent's \`recent_posts\` tool. Deleted a day after the webhook was last used, or ${days(l.idleDays)} after while an agent is watching it.
- **Also in the database:** when the link was last used, which of its URLs to tell agents about (for Stripe and Linear, that URL holds your signing secret, encrypted), and who's watching, deleted when the link expires (${days(l.idleDays)} after it was last used); the name and version of each agent program connected to it, deleted after ${days(l.idleDays)} without use; and its post counts for the limits, deleted when the hour or day they count ends.
- **IP addresses** are never stored as they are: only a keyed hash, which can't be turned back into the address, with a count of the links made from it. Deleted when the hour ends.
- **Logs:** errors, never the request's path (it carries the link) or body. Our hosting provider's request logs also hold IP addresses and paths, for 30 days.
- **Cookies:** none.

## Who sees it

- **Anyone with the link**, on the webhook's page: each post as it arrives, the conversations watching, and the webhook's secret.
- **The agents watching the webhook**, and their providers (for example Anthropic or OpenAI), once a post is delivered.
- **Google Cloud**, which hosts Webhooks.

## Ending it sooner

Stop watching with the \`unwatch\` tool, or write to support@clawvisor.com with the link to have what we hold for it deleted. Otherwise the link expires after ${days(l.idleDays)} without use.

See also the [Webhooks Terms of Service](/terms).
`;

/** The documents, by path, stating the limits given. */
export function legalDocsFor(l: DocLimits): Record<string, LegalDoc> {
  const terms = termsFor(l);
  const privacy = privacyFor(l);
  return {
    "/terms": { title: "Terms of Service", draft: "These terms are being reviewed and are not yet in effect.", markdown: terms },
    "/tos": { title: "Terms of Service", draft: "These terms are being reviewed and are not yet in effect.", markdown: terms },
    "/privacy": { title: "Privacy Policy", draft: "This policy is being reviewed and is not yet in effect.", markdown: privacy },
  };
}

/** The documents with the default limits. */
export const legalDocs = legalDocsFor(defaultDocLimits);

/** Markdown to HTML, for our own documents: blocks are headings, lists and paragraphs, split by blank lines. */
export function markdown(src: string): string {
  const out: string[] = [];
  for (const block of src.trim().split(/\n\s*\n/)) {
    const lines = block.split("\n");
    const h = block.match(/^(#{1,3}) (.*)$/);
    if (h && lines.length === 1) out.push(`<h${h[1].length}>${inline(h[2])}</h${h[1].length}>`);
    else if (lines.every((l) => l.startsWith("- "))) out.push(`<ul>${lines.map((l) => `<li>${inline(l.slice(2))}</li>`).join("")}</ul>`);
    else out.push(`<p>${inline(lines.join(" "))}</p>`);
  }
  return out.join("\n");
}

/** Inline Markdown: code spans, then links (https or same-site only), bold and italics. All text is escaped. */
export function inline(s: string): string {
  return s
    .split("`")
    .map((part, i) => {
      if (i % 2 === 1) return `<code>${esc(part)}</code>`;
      return esc(part)
        .replace(/\[([^\]]+)\]\(((?:https:\/\/|\/)[^)\s]*)\)/g, '<a href="$2">$1</a>')
        .replace(/\*\*(.+?)\*\*/g, "<strong>$1</strong>")
        .replace(/\*(.+?)\*/g, "<em>$1</em>");
    })
    .join("");
}

/** A legal page, in the site's layout, with the draft banner. */
export function legalPage(nonce: string, doc: LegalDoc): string {
  return layout(
    `${doc.title} · Wakeshift webhooks`,
    `<p class="draft" role="note"><strong>Draft.</strong> ${esc(doc.draft)}</p>\n<article class="prose">${markdown(doc.markdown)}</article>`,
    nonce,
  );
}
