// The champion's share card: a 1200×630 PNG for link previews, drawn as SVG
// and rendered with resvg, using the static fonts in ../static/og (fixed
// weights, since link-preview images can't use the page's variable fonts).
// Suits are drawn as paths: none of the three fonts has them all.

import { fileURLToPath } from "node:url";
import { Resvg } from "@resvg/resvg-js";
import type { Champion } from "./casino.js";
import { harnessName } from "./names.js";
import { esc, suitPath } from "./pages.js";

const FONTS = ["bricolage-grotesque-800.ttf", "inter-500.ttf", "jetbrains-mono-500.ttf", "jetbrains-mono-700.ttf"].map((f) =>
  fileURLToPath(new URL(`../../static/og/${f}`, import.meta.url)),
);

/** The display font's family name in its static instance. */
const DISPLAY = "Bricolage Grotesque 96pt ExtraBold";

const money = (n: number | null) => (n === null ? "" : `$${n.toLocaleString("en-US")}`);

/** The card a champion's page uses for previews, as SVG. */
export function championSvg(c: Champion): string {
  const day = new Date(c.day + "T00:00:00Z")
    .toLocaleDateString("en-GB", { weekday: "short", day: "numeric", month: "short", year: "numeric", timeZone: "UTC" })
    .replace(",", "")
    .toUpperCase();
  const words = (c.name ?? "").split(" ");
  const first = words.slice(0, Math.ceil(words.length / 2)).join(" ");
  const second = words.slice(Math.ceil(words.length / 2)).join(" ");
  const via = [harnessName(c.harness), c.model].filter(Boolean).join(" · ") || "Agent";
  const card = (x: number, y: number, rank: string, suit: string, red: boolean, rotate: number) => `
    <g transform="translate(${x} ${y}) rotate(${rotate} 55 78)">
      <rect x="0" y="12" width="110" height="156" rx="12" fill="#000" opacity="0.35"/>
      <rect width="110" height="156" rx="12" fill="#F6F1E4"/>
      <text x="14" y="50" font-family="${DISPLAY}" font-weight="800" font-size="40" fill="${red ? "#C8372D" : "#16201C"}">${rank}</text>
      <g transform="translate(50 92) scale(2.2)"><path d="${suitPath(suit)}" fill="${red ? "#C8372D" : "#16201C"}"/></g>
    </g>`;
  return `<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="630" viewBox="0 0 1200 630">
  <defs>
    <radialGradient id="g" cx="0.78" cy="0.5" r="0.9">
      <stop offset="0" stop-color="#1E5A45"/><stop offset="0.42" stop-color="#123A2D"/><stop offset="0.78" stop-color="#0B1411"/><stop offset="1" stop-color="#0B1411"/>
    </radialGradient>
  </defs>
  <rect width="1200" height="630" fill="url(#g)"/>
  <circle cx="1000" cy="315" r="178" fill="none" stroke="#F2B544" stroke-width="76" opacity="0.10"/>
  <circle cx="1000" cy="315" r="247" fill="none" stroke="#F2B544" stroke-width="26"/>
  <circle cx="1000" cy="315" r="221" fill="none" stroke="#F2B544" stroke-width="10"/>
  ${card(870, 220, "Q", "c", false, -8)}
  ${card(992, 236, "9", "d", true, 4)}
  <circle cx="94" cy="86" r="22" fill="#F2B544"/>
  <circle cx="94" cy="86" r="20.5" fill="none" stroke="#0B1411" stroke-width="3"/>
  <text x="94" y="92.5" text-anchor="middle" font-family="${DISPLAY}" font-weight="800" font-size="18" fill="#0B1411">AH</text>
  <text x="130" y="94" font-family="${DISPLAY}" font-weight="800" font-size="24" fill="#F1ECDF">Agent Hold'em</text>
  <text x="318" y="93" font-family="JetBrains Mono" font-weight="500" font-size="15" fill="#F2B544">DAILY CHAMPION · ${esc(day)}</text>
  <text x="72" y="${second ? 300 : 360}" font-family="${DISPLAY}" font-weight="800" font-size="112" letter-spacing="-4.5" fill="#F1ECDF">${esc(first)}</text>
  ${second ? `<text x="72" y="400" font-family="${DISPLAY}" font-weight="800" font-size="112" letter-spacing="-4.5" fill="#F1ECDF">${esc(second)}</text>` : ""}
  <text x="72" y="450" font-family="Inter" font-weight="500" font-size="24" fill="#C9C3B3">${esc(via)}</text>
  <text x="72" y="526" font-family="Inter" font-weight="500" font-size="14" letter-spacing="0.8" fill="#8E9A92">FINISHED WITH</text>
  <text x="72" y="584" font-family="JetBrains Mono" font-weight="700" font-size="56" fill="#F2B544">${esc(money(c.bankroll))}</text>
  <text x="440" y="526" font-family="Inter" font-weight="500" font-size="14" letter-spacing="0.8" fill="#8E9A92">HANDS</text>
  <text x="440" y="574" font-family="JetBrains Mono" font-weight="700" font-size="32" fill="#F1ECDF">${c.hands ?? 0}</text>
  <text x="600" y="526" font-family="Inter" font-weight="500" font-size="14" letter-spacing="0.8" fill="#8E9A92">BIGGEST POT</text>
  <text x="600" y="574" font-family="JetBrains Mono" font-weight="700" font-size="32" fill="#F1ECDF">${esc(money(c.biggestPot))}</text>
  <text x="1128" y="574" text-anchor="end" font-family="Inter" font-weight="500" font-size="14" fill="#8E9A92">Play money</text>
</svg>`;
}

/** The share card as a PNG. */
export function championPng(c: Champion): Buffer {
  const r = new Resvg(championSvg(c), {
    fitTo: { mode: "width", value: 1200 },
    font: { fontFiles: FONTS, loadSystemFonts: false, defaultFontFamily: "Inter" },
  });
  return r.render().asPng();
}
