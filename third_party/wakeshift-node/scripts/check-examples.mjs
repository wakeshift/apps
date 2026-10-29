// Type-checks every TypeScript example people copy against this SDK: the
// ```ts blocks in site/docs and this README, the landing page's sample
// (site/templates/landing.html) and the console's "Try it" snippet
// (web/src/pages/app/Overview.tsx). Each example becomes a module: its
// imports at the top, the rest in an async function, so fragments (a
// `return` from a tool callback, an `await`) compile; examples/env.d.ts
// declares what fragments take from around them (extra, repo, body…).
// It also checks a mistake tsc can't see: attached.events names the tool
// it's registered as. `core` isn't required: the SDK reads WAKESHIFT_CORE
// itself, which is what an example would pass.
import { execFileSync } from "node:child_process";
import { mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync, copyFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";

const sdk = join(dirname(fileURLToPath(import.meta.url)), "..");
const repo = join(sdk, "../..");
const out = join(sdk, "build-examples");

const examples = []; // { from, code }

// ```ts blocks in Markdown (the info string may add a title or tab: "ts server.ts").
function fromMarkdown(file) {
  const src = readFileSync(file, "utf8");
  const re = /^```(ts|typescript)\b[^\n]*\n([\s\S]*?)^```/gm;
  let m;
  while ((m = re.exec(src))) {
    const line = src.slice(0, m.index).split("\n").length;
    examples.push({ from: `${relative(repo, file)}:${line}`, code: m[2] });
  }
}
// WAKESHIFT_EXAMPLES_README_ONLY=1 checks only this README: the published
// copy of the SDK (opensource/export.sh) has no site or console.
const readmeOnly = process.env.WAKESHIFT_EXAMPLES_README_ONLY === "1";
fromMarkdown(join(sdk, "README.md"));
if (!readmeOnly) {
  const docs = join(repo, "site/docs");
  for (const f of readdirSync(docs).filter((f) => f.endsWith(".md")).sort()) fromMarkdown(join(docs, f));
}

// The landing page's sample: the <pre> in the code card, without its highlighting.
if (!readmeOnly) {
  const html = readFileSync(join(repo, "site/templates/landing.html"), "utf8");
  const m = html.match(/<div class="code">[\s\S]*?<pre>([\s\S]*?)<\/pre>/);
  if (!m) throw new Error("landing.html: no code sample");
  const code = m[1]
    .replace(/<span class="ln">/g, "\n")
    .replace(/<[^>]+>/g, "")
    .replace(/&gt;/g, ">")
    .replace(/&lt;/g, "<")
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&amp;/g, "&");
  examples.push({ from: "site/templates/landing.html", code });
}

// The console's snippet: the template literal from "// server.ts", with this app's ID.
if (!readmeOnly) {
  const tsx = readFileSync(join(repo, "web/src/pages/app/Overview.tsx"), "utf8");
  const m = tsx.match(/\n\/\/ server\.ts\n([\s\S]*?)`\n\s*return \(/);
  if (!m) throw new Error("Overview.tsx: no server.ts snippet");
  const code = m[1].replace(/\$\{app\.id\}/g, "ci-bot").replace(/\\`/g, "`").replace(/\\\$\{/g, "${");
  if (/\$\{app\./.test(code)) throw new Error("Overview.tsx: the snippet uses an app field this check doesn't fill in");
  examples.push({ from: "web/src/pages/app/Overview.tsx", code });
}

// What tsc can't see: attached.events("name", …) inside registerTool("name", …)
// with another name throws at the first call. Each registration is checked
// against the first attached.events in it, before the next registerTool.
const problems = [];
for (const e of examples) {
  for (const m of e.code.matchAll(/registerTool\(\s*(["'`])([^"'`]+)\1(?:(?!registerTool\()[\s\S])*?attached\.events\(\s*(["'`])([^"'`]+)\3/g)) {
    if (m[2] !== m[4]) problems.push(`${e.from}: attached.events("${m[4]}") wraps the tool registered as ${m[2]} (it throws)`);
  }
}

rmSync(out, { recursive: true, force: true });
mkdirSync(out, { recursive: true });
copyFileSync(join(sdk, "scripts/examples/env.d.ts"), join(out, "env.d.ts"));
const files = [];
examples.forEach((e, i) => {
  const lines = e.code.split("\n");
  const imports = lines.filter((l) => /^import\s/.test(l));
  const rest = lines.filter((l) => !/^import\s/.test(l));
  const name = `${String(i).padStart(2, "0")}-${e.from.replace(/[^a-z0-9]+/gi, "-").replace(/^-|-$/g, "")}.ts`;
  writeFileSync(
    join(out, name),
    `// ${e.from}\n${imports.join("\n")}\nexport {};\n\nexport async function example() {\n${rest.join("\n")}\n}\n`,
  );
  files.push(name);
});
writeFileSync(
  join(out, "tsconfig.json"),
  JSON.stringify(
    {
      compilerOptions: {
        target: "ES2023",
        module: "NodeNext",
        moduleResolution: "NodeNext",
        lib: ["ES2023"],
        types: ["node"],
        strict: true,
        noEmit: true,
        skipLibCheck: true,
        paths: { "@wakeshift/sdk": ["../src/index.ts"] },
      },
      files: ["env.d.ts", ...files],
    },
    null,
    2,
  ),
);

let failed = false;
try {
  execFileSync(join(sdk, "node_modules/.bin/tsc"), ["-p", join(out, "tsconfig.json")], { stdio: "inherit" });
} catch {
  failed = true;
}
for (const p of problems) console.error(p);
if (failed || problems.length) {
  console.error(`examples: ${problems.length} problem(s)${failed ? ", and tsc errors above" : ""} (the files are in ${relative(process.cwd(), out)})`);
  process.exit(1);
}
rmSync(out, { recursive: true, force: true });
console.log(`examples: ${examples.length} type-check against the SDK`);
