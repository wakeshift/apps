// Bundles an emit-only Cloudflare Worker (scripts/bundle-check/worker.mjs)
// against the built SDK the way wrangler does with nodejs_compat: esbuild,
// ES modules, node:* builtins left to the runtime. It fails if what the SDK
// adds to the bundle (beside the same Worker without it, baseline.mjs: the
// app's own zod) is over the size budget, or if the bundle imports a node:
// module the SDK shouldn't need at load (AsyncLocalStorage loads lazily,
// through process.getBuiltinModule). Then it bundles a zod 3 Node app
// (scripts/bundle-check/zod3.mjs) without any MCP SDK and runs it from a
// directory with no node_modules: defineEvent mustn't need an MCP SDK, and
// neither a disabled client nor an enabled one may crash. Run after npm run
// build.
import { build } from "esbuild";
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const sdk = join(dirname(fileURLToPath(import.meta.url)), "..");
const BUDGET = 200 * 1024; // bytes the SDK adds, minified
const ALLOWED_NODE = new Set(["node:crypto"]);

rmSync(join(sdk, "build-bundle"), { recursive: true, force: true });
async function bundle(entry) {
  const outfile = join(sdk, "build-bundle", entry);
  await build({
    entryPoints: [join(sdk, "scripts/bundle-check", entry)],
    bundle: true,
    format: "esm",
    platform: "neutral",
    mainFields: ["module", "main"],
    conditions: ["workerd", "worker", "import"],
    external: ["node:*"],
    alias: { "@wakeshift/sdk": join(sdk, "dist/index.js") },
    minify: true,
    outfile,
    logLevel: "error",
  });
  return readFileSync(outfile, "utf8");
}
const code = await bundle("worker.mjs");
const baseline = await bundle("baseline.mjs");
const nodeImports = [...new Set([...code.matchAll(/from\s*"(node:[^"]+)"/g)].map((m) => m[1]))];
const extra = nodeImports.filter((m) => !ALLOWED_NODE.has(m));
const added = Buffer.byteLength(code) - Buffer.byteLength(baseline);
console.log(`bundle: ${(Buffer.byteLength(code) / 1024).toFixed(1)} KB, of which the SDK adds ${(added / 1024).toFixed(1)} KB (budget ${BUDGET / 1024} KB); node imports: ${nodeImports.join(", ") || "none"}`);
if (added > BUDGET) {
  console.error(`the SDK adds more than its budget to a Worker bundle: ${added} > ${BUDGET} bytes`);
  process.exit(1);
}
if (extra.length) {
  console.error(`the Worker bundle imports node modules at load: ${extra.join(", ")}`);
  process.exit(1);
}

// zod 3, bundled, no MCP SDK: esbuild fails the build if anything imports
// one statically, and the run fails if anything loads one at run time.
const dir = mkdtempSync(join(tmpdir(), "wakeshift-zod3-"));
try {
  const outfile = join(dir, "zod3.mjs");
  await build({
    entryPoints: [join(sdk, "scripts/bundle-check/zod3.mjs")],
    bundle: true,
    format: "esm",
    platform: "node",
    external: ["node:*", "@modelcontextprotocol/*"],
    alias: { "@wakeshift/sdk": join(sdk, "dist/index.js") },
    outfile,
    logLevel: "error",
  });
  const env = { ...process.env };
  delete env.WAKESHIFT_KEY;
  delete env.WAKESHIFT_CORE;
  process.stdout.write(execFileSync(process.execPath, [outfile], { cwd: dir, env, encoding: "utf8" }));
} finally {
  rmSync(dir, { recursive: true, force: true });
}
