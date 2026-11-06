// The parts of @modelcontextprotocol/sdk 1.x the SDK uses when an app has
// it installed (zod 3's JSON Schema converter, the list of protocol
// versions its transport accepts), loaded on first use, synchronously, and
// never at import: a top-level await would stop require() of this package
// (Node's require(esm) refuses a module graph with one).
//
// A module can be loaded twice in one process, as ESM and as CommonJS (the
// 1.x SDK ships both), and an app's transport uses the copy its own import
// or require loaded. So each is loaded both ways: the ESM copy by its
// resolved file, through require(esm), which returns the instance the ESM
// loader has (Node 22.12 or later); and the CommonJS copy by require. Either
// may be missing (1.x isn't installed, or a bundler or runtime without
// import.meta.resolve or createRequire), and then it's left out. Node's
// modules are reached through process.getBuiltinModule, not imported, so
// the SDK's own module graph stays loadable where they aren't (a bundle).

type NodeModule = { createRequire(url: string): NodeJS.Require };
type NodeUrl = { fileURLToPath(url: string): string };

function builtin<T>(name: string): T | undefined {
  try {
    return (globalThis as { process?: { getBuiltinModule?(id: string): unknown } }).process?.getBuiltinModule?.(name) as T | undefined;
  } catch {
    return undefined;
  }
}

const loaded = new Map<string, unknown[]>();

/** @internal The distinct copies of an optional module (ESM, then CommonJS) this process can load: [] if none. */
export function optionalModules<T>(spec: string): T[] {
  let mods = loaded.get(spec);
  if (!mods) {
    mods = [];
    let req: NodeJS.Require | undefined;
    try {
      req = builtin<NodeModule>("node:module")?.createRequire(import.meta.url);
    } catch {
      req = undefined;
    }
    const toPath = builtin<NodeUrl>("node:url")?.fileURLToPath;
    if (req) {
      try {
        if (toPath) mods.push(req(toPath(import.meta.resolve(spec))));
      } catch {
        // not installed, or no import.meta.resolve here
      }
      try {
        const cjs = req(spec);
        if (!mods.includes(cjs)) mods.push(cjs);
      } catch {
        // not installed, or no CommonJS copy
      }
    }
    loaded.set(spec, mods);
  }
  return mods as T[];
}
