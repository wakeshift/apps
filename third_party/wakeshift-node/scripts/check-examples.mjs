// Type-checks every TypeScript example people copy against this SDK: the
// ```ts blocks in site/docs, the product pages (site/pages), site/llms/agents.md (/AGENTS.md) and this README, the landing page's sample
// (site/templates/landing.html) and the console's "Try it" snippet
// (web/src/pages/app/Overview.tsx). Each example becomes a module: its
// imports at the top, the rest in an async function, so fragments (a
// `return` from a tool callback, an `await`) compile; examples/env.d.ts
// declares what fragments take from around them (extra, repo, body…).
// `core` isn't required: the SDK reads WAKESHIFT_CORE itself, which is
// what an example would pass.
import { execFileSync } from "node:child_process";
import { mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync, copyFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";

const sdk = join(dirname(fileURLToPath(import.meta.url)), "..");
const repo = join(sdk, "../..");
const out = join(sdk, "build-examples");

const examples = []; // { from, code }

// ```ts blocks in Markdown (the info string may add a title or tab: "ts server.ts").
// A title that's a file name makes the block that file, a module of its
// own: a page's blocks with the same file name are one file, in the order
// they come, and its other files import it by name ("./server.js").
function fromMarkdown(file) {
  const src = readFileSync(file, "utf8");
  const re = /^```(ts|typescript)\b([^\n]*)\n([\s\S]*?)^```/gm;
  const named = new Map();
  let m;
  while ((m = re.exec(src))) {
    const line = src.slice(0, m.index).split("\n").length;
    const from = `${relative(repo, file)}:${line}`;
    const fileName = m[2].trim().match(/^([\w.-]+\.ts)$/)?.[1];
    if (!fileName) {
      examples.push({ from, code: m[3] });
      continue;
    }
    const e = named.get(fileName);
    if (e) e.code += "\n" + m[3];
    else {
      const ex = { from, code: m[3], file: fileName, dir: relative(repo, file).replace(/[^a-z0-9]+/gi, "-") };
      named.set(fileName, ex);
      examples.push(ex);
    }
  }
}
// WAKESHIFT_EXAMPLES_README_ONLY=1 checks only this README: the published
// copy of the SDK (opensource/export.sh) has no site or console.
const readmeOnly = process.env.WAKESHIFT_EXAMPLES_README_ONLY === "1";
fromMarkdown(join(sdk, "README.md"));
if (!readmeOnly) {
  for (const dir of ["site/docs", "site/pages"]) {
    const docs = join(repo, dir);
    for (const f of readdirSync(docs).filter((f) => f.endsWith(".md")).sort()) fromMarkdown(join(docs, f));
  }
  // /AGENTS.md, the instructions for coding agents the site serves.
  fromMarkdown(join(repo, "site/llms/agents.md"));
}

// The landing page's TypeScript sample: the <pre> in its tab, without its
// highlighting. (site/landing_examples_test.go checks the Go one, and
// sdk/python/tests/test_docs.py the Python one.)
if (!readmeOnly) {
  const html = readFileSync(join(repo, "site/templates/landing.html"), "utf8");
  const m = html.match(/<div class="panel"[^>]*id="c-ts"[^>]*>\s*<pre>([\s\S]*?)<\/pre>/);
  if (!m) throw new Error("landing.html: no TypeScript sample");
  const code = m[1]
    .replace(/<span class="l">/g, "\n")
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

const problems = [];

rmSync(out, { recursive: true, force: true });
mkdirSync(out, { recursive: true });
copyFileSync(join(sdk, "scripts/examples/env.d.ts"), join(out, "env.d.ts"));
copyFileSync(join(sdk, "scripts/examples/frameworks.d.ts"), join(out, "frameworks.d.ts"));
const files = [];
examples.forEach((e, i) => {
  if (e.file) {
    // A named file, as it is (its imports first), in its page's directory.
    mkdirSync(join(out, e.dir), { recursive: true });
    const lines = e.code.split("\n");
    const imports = [...new Set(lines.filter((l) => /^import\s/.test(l)))];
    const rest = lines.filter((l) => !/^import\s/.test(l));
    writeFileSync(join(out, e.dir, e.file), `// ${e.from}\n${imports.join("\n")}\nexport {};\n${rest.join("\n")}\n`);
    files.push(join(e.dir, e.file));
    return;
  }
  const lines = e.code.split("\n");
  // A fragment's import of the page's own module ("./server.ts", the quickstart's main.ts) is dropped:
  // what it imports is declared in examples/env.d.ts.
  const imports = lines.filter((l) => /^import\s/.test(l) && !/from "\.\//.test(l));
  const rest = lines.filter((l) => !/^import\s/.test(l));
  const name = `${String(i).padStart(2, "0")}-${e.from.replace(/[^a-z0-9]+/gi, "-").replace(/^-|-$/g, "")}.ts`;
  // A whole module (a Worker's `export default` or Durable Object class, the quickstart's exports) is checked as it is.
  const whole = /^export (default|const|async function|function|class)\b/m.test(e.code);
  writeFileSync(
    join(out, name),
    whole ? `// ${e.from}\n${e.code}` : `// ${e.from}\n${imports.join("\n")}\nexport {};\n\nexport async function example() {\n${rest.join("\n")}\n}\n`,
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
        paths: { "@wakeshift/sdk": ["../src/index.ts"], "@wakeshift/sdk/mastra": ["../src/mastra.ts"], "@wakeshift/sdk/tools": ["../src/tools.ts"], "@wakeshift/sdk/testing": ["../src/testing.ts"] },
      },
      files: ["env.d.ts", "frameworks.d.ts", ...files],
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
