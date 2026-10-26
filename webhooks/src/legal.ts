// The terms and privacy policy for try.wakeshift.ai, served at /terms (and
// /tos) and /privacy. The documents are Markdown, rendered here by a small
// renderer for the little they use (headings, paragraphs, lists, bold,
// italics, code and links); everything else is escaped as text. The Go apps
// do the same with apps/legal.

import { esc, layout, siteFor, WAKESHIFT } from "./page.js";

export { siteFor, WAKESHIFT };

export interface LegalDoc {
  title: string;
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

*Last updated October 25, 2026*

These Webhooks Terms of Service ("Terms") are an agreement between Clawvisor Inc. ("we" or "us") and each person who uses Webhooks at try.wakeshift.ai ("you"). Webhooks is a tool that we make, and it runs on Wakeshift. The [Terms for Our Apps](${WAKESHIFT}/app-terms) form part of these Terms. If the two conflict, these Terms prevail for Webhooks.

## 1. The Tool

1.1 Webhooks provides a webhook URL that wakes your agent: anything posted to the URL wakes the agents watching it, with the content that was sent. Webhooks is built on Wakeshift's TypeScript SDK.

1.2 You do not need an account with Webhooks or with Wakeshift: your link is all you need. Below the button that creates a link, the page states "By getting a link, you agree to Webhooks' Terms and Privacy Policy", with links to both. By creating a link, you accept these Terms. You must be at least 18 years old to use Webhooks.

## 2. Purpose and Limits

Webhooks is intended for trying Wakeshift and for building with it, and not for operating anything critical.

- **Limits:** ${l.perHour} posts an hour and ${l.perDay} a day to each of a webhook's URLs, request bodies of up to 64 KB, and ${l.linksPerHour} new links an hour from one address. Posts over a limit are refused.
- **Expiry:** a link expires after ${days(l.idleDays)} without use.
- **Retention:** Webhooks' own copy of recent posts is deleted one day after your webhook was last used (${days(l.idleDays)} while an agent is watching it), and the rest of what it keeps for a link is deleted when the link expires. Wakeshift deletes what it delivered within 30 days, and when the link expires, Webhooks also instructs Wakeshift to delete what Wakeshift keeps for the webhook. The [Webhooks Privacy Policy](/privacy) lists what is kept, where and for how long.
- We may change the limits, reset links or discontinue Webhooks at any time.

## 3. Your Link and Webhook URLs

3.1 Your link and your webhook URLs work for anyone who has them:

- Anyone with your **link** can connect an agent to your webhook and open its page, which shows each post as it arrives, the conversations watching, and the webhook's GitHub and bearer secrets.
- Anyone with your **open webhook URL** can post to it and wake the agents watching. The other URLs on the page accept only posts that prove their origin: GitHub's \`X-Hub-Signature-256\`, Stripe's \`Stripe-Signature\` or Linear's \`Linear-Signature\`, or the webhook's bearer token.
- For Stripe and Linear, you paste the signing secret they provide into the page. The secret is encrypted into the URL you receive, which only Webhooks can read. Webhooks keeps that URL, to inform agents of it, until the link expires, so the secret is stored, but only in that encrypted form. The secret itself is not displayed again.

3.2 You must keep your link and webhook URLs confidential, and share them only with the services from which you want posts to reach your agent.

## 4. What You Post

4.1 You must not post passwords, keys or other secrets, or other people's personal data. Anyone with the link can see what was posted.

4.2 Posts reach your agent marked as coming from an outside source, and not as your instructions, but a post may still attempt to cause your agent to take actions you did not intend. You are responsible for what you post and for what your agent does with it.

## 5. Acceptable Use

In addition to Section 5 of the Terms for Our Apps, you must not create many links to circumvent the limits, and you must not use a webhook to wake agents whose users did not give you its URL.

## 6. Changes to These Terms

A change to these Terms applies to links created after the change takes effect.

See also the [Webhooks Privacy Policy](/privacy).
`;

const privacyFor = (l: DocLimits) => `# Webhooks Privacy Policy

*Last updated October 25, 2026*

This Webhooks Privacy Policy ("Policy") describes the personal data that Webhooks at try.wakeshift.ai collects, where it goes and how long it is kept. Webhooks is a tool that Clawvisor Inc. ("we" or "us") makes, and it runs on Wakeshift. This Policy forms part of our [Privacy Policy](${WAKESHIFT}/privacy), which describes who we are, why we process personal data and on what legal bases, your rights, where data is stored and how we notify you of changes. You may contact us at support@wakeshift.ai.

## 1. Data Collected

- **Links:** links are not stored. A link is signed, and contains a random webhook ID and the time it was created. There is no account, and no name or email address is requested.
- **Posts:** each request to a webhook URL, consisting of its body (up to 64 KB), method, content type and only these headers: \`Content-Type\`, \`User-Agent\`, \`X-GitHub-Event\`, \`X-GitHub-Delivery\`, \`X-GitHub-Hook-ID\`, \`X-GitLab-Event\`, \`X-Event-Key\`, \`Linear-Event\`, \`Linear-Delivery\`, \`Idempotency-Key\` and \`X-Request-ID\`. For Stripe and Linear, also the event's type and ID (Stripe's event ID or Linear's delivery ID), so that a retried event is sent only once. Webhooks never collects \`Authorization\` headers, cookies or signature headers.
- **Signing secrets you paste** for Stripe or Linear: stored only encrypted. Each secret is encrypted into the webhook URL created for it, which only Webhooks can read, and Webhooks keeps that URL (see Section 2).
- **Watching conversations:** for each agent conversation that watches a webhook, the name that Wakeshift gives it and the name that its agent program gives itself (for example, "Claude Code").
- **Agent programs:** for each agent program connected to a webhook (for example, Claude Code or Cursor), the name and version it reports.
- **IP addresses:** the address that requests a new link (for an IPv6 address, its first 64 bits), to limit links to ${l.linksPerHour} an hour per address.

## 2. Where Data Goes and Retention

- **Wakeshift.** Webhooks passes each post on as a message: a line stating what arrived (for GitHub, a summary such as the workflow and its result), with the parsed body and the headers listed in Section 1 attached. Wakeshift stores the message with its record of the webhook and the conversations watching it, and delivers it to those conversations. Wakeshift deletes each message no later than 30 days after it is delivered. When the link expires, Webhooks instructs Wakeshift to delete everything Wakeshift keeps for the webhook: the conversations that watched it, their subscriptions, and the messages and the records of their delivery. This usually occurs within one hour after the link expires; if Wakeshift cannot be reached, Webhooks retries until the deletion is complete.
- **Webhooks' database: recent requests.** For each webhook, its last 50 requests for the live view (the time, method, content type and size; the start of the body as text, up to 160 characters; the line the agent reads; the URL at which the request arrived; the number of conversations it reached; and, for a refused request, the reason) and its last 10 posts in full for the agent's \`recent_posts\` tool. These are deleted one day after the webhook was last used, or ${days(l.idleDays)} after it was last used while an agent is watching it.
- **Webhooks' database: other data.** When the link was last used, which of its URLs to inform agents of (for Stripe and Linear, that URL contains your signing secret, encrypted) and the watching conversations, deleted when the link expires (${days(l.idleDays)} after it was last used); the name and version of each agent program connected to it, deleted after ${days(l.idleDays)} without use; and its post counts for the limits, deleted when the hour or day that they count ends.
- **IP addresses** are never stored in their original form. Webhooks stores only a keyed hash, which cannot be converted back into the address, with a count of the links created from it. The hash is deleted when the hour ends.
- **Logs:** Webhooks' own logs record errors, and never contain a request's path or body. Our hosting provider, Google Cloud, keeps no log of requests to Webhooks, except a request to the home page or to create a link that our firewall blocked, or would have blocked. That entry holds the IP address, the browser or client name, the address requested and the firewall's decision, and is kept for 30 days. No log holds your link or webhook URLs.
- **Cookies:** none.

## 3. Recipients

- **Anyone with the link**, on the webhook's page: each post as it arrives, the conversations watching, and the webhook's secret.
- **The agents watching the webhook**, and their agent providers (for example, Anthropic or OpenAI), once a post is delivered.
- **Google Cloud**, which hosts Webhooks.

## 4. Ending Use Earlier

To stop watching, use the \`unwatch\` tool. To have the data we hold for a link deleted, write to support@wakeshift.ai with the link. Otherwise, the link expires after ${days(l.idleDays)} without use.

See also the [Webhooks Terms of Service](/terms).
`;

/** The documents, by path, stating the limits given. */
export function legalDocsFor(l: DocLimits): Record<string, LegalDoc> {
  const terms = termsFor(l);
  const privacy = privacyFor(l);
  return {
    "/terms": { title: "Terms of Service", markdown: terms },
    "/tos": { title: "Terms of Service", markdown: terms },
    "/privacy": { title: "Privacy Policy", markdown: privacy },
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

/** A legal page, in the site's layout. */
export function legalPage(nonce: string, doc: LegalDoc): string {
  return layout(
    `${doc.title} · Wakeshift webhooks`,
    `<article class="prose">${markdown(doc.markdown)}</article>`,
    nonce,
  );
}
