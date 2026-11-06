#!/bin/sh
# Checks the packed SDK the way an app installs it: npm pack, then a fresh
# project per MCP SDK installs the tarball, typechecks a server with tsc
# --strict, and runs it. Needs the npm registry.
#
#   - @modelcontextprotocol/sdk 1.x (scripts/pack-check/server.ts): zod 3
#     and 4, each with the oldest 1.x the peer range allows and the latest;
#   - @modelcontextprotocol/server 2.x (server-v2.ts), with 1.x not
#     installed: zod 4 (which 2.x requires), the oldest 2.x the peer range
#     allows and the latest;
#   - a CommonJS app (scripts/pack-check/cjs.ts, with 1.x) that require()s
#     every entry, typechecked for module nodenext and module commonjs.
#
# PACK_CHECK_MCP=v1 or v2 checks only one (CI's matrix); default both.
set -eu
sdk=$(cd "$(dirname "$0")/.." && pwd)
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
which=${PACK_CHECK_MCP:-both}

cd "$sdk"
tgz="$dir/$(npm pack --silent --pack-destination "$dir" | tail -1)"

tar -xzf "$tgz" -C "$dir"

# No source maps: they carried the whole TypeScript source inline, for a
# src/ the package doesn't ship.
if ls "$dir"/package/dist/*.map >/dev/null 2>&1; then
  echo "the package ships source maps: $(ls "$dir"/package/dist/*.map | xargs -n1 basename | tr '\n' ' ')" >&2
  exit 1
fi

# The published types name neither MCP SDK, so they load with either one missing.
if grep -lE "(from |import\()[\"']@modelcontextprotocol/" "$dir"/package/dist/*.d.ts >/dev/null 2>&1; then
  echo "the published types import an MCP SDK: $(grep -lE "(from |import\()[\"']@modelcontextprotocol/" "$dir"/package/dist/*.d.ts | xargs -n1 basename | tr '\n' ' ')" >&2
  exit 1
fi

# The published types name nothing this release removed (Mastra's helpers stay, in mastra.d.ts).
removed='attached\.(events|inputs|output|jsonSchema|describe)\b|attached\.call\(|onWarning|wrappedTool|@wakeshift/sdk/xmcp'
if grep -lE "$removed" $(ls "$dir"/package/dist/*.d.ts | grep -v '/mastra\.d\.ts$') >/dev/null 2>&1; then
  echo "the published types name removed API: $(grep -nE "$removed" $(ls "$dir"/package/dist/*.d.ts | grep -v '/mastra\.d\.ts$') | head -5)" >&2
  exit 1
fi

# app installs the tarball with the given packages in a fresh project, then
# typechecks and runs the given server.
app() {
  name=$1 server=$2
  shift 2
  app="$dir/app-$name"
  mkdir -p "$app"
  cp "$sdk/scripts/pack-check/$server" "$app/server.ts"
  cd "$app"
  printf '{"name":"pack-check","private":true,"type":"module"}\n' >package.json
  npm install --silent --no-fund --no-audit "$tgz" "$@" typescript@5 @types/node@22 >/dev/null
  if [ -d node_modules/@wakeshift/sdk/node_modules ]; then
    echo "$name: the tarball brought its own dependencies: $(ls node_modules/@wakeshift/sdk/node_modules)" >&2
    exit 1
  fi
  npx tsc --strict --noEmit --module nodenext --moduleResolution nodenext --target es2023 --types node --skipLibCheck server.ts
  node --experimental-strip-types --no-warnings server.ts
}

if [ "$which" != v2 ]; then
  # The peer range takes any 1.x (#44); attach needs 1.23 or later, and warns
  # (mcp_patch_unavailable) on an older one, so 1.23 is the floor checked.
  mcp_floor=1.23.0
  for zod in 3 4; do
    for mcp in "$mcp_floor" latest; do
      app "zod$zod-mcp$mcp" server.ts "zod@^$zod" "@modelcontextprotocol/sdk@$mcp"
      echo "zod $zod ($(node -p 'require("zod/package.json").version')), MCP SDK $(node -p 'JSON.parse(require("fs").readFileSync("node_modules/@modelcontextprotocol/sdk/package.json", "utf8")).version'): ok"
      # Without require() of ES modules, attach on 1.x refuses, naming the Node floor (CR M4).
      if node --no-experimental-require-module --experimental-strip-types --no-warnings server.ts >"$dir/no-require.log" 2>&1; then
        echo "zod $zod, MCP SDK $mcp: attach ran without require(esm)" >&2
        exit 1
      fi
      grep -q "22.12" "$dir/no-require.log" || { echo "without require(esm), attach didn't name the Node floor:" >&2; cat "$dir/no-require.log" >&2; exit 1; }
      cd "$sdk"
    done
  done
fi

# A CommonJS app: require() of each entry, with types, compiled for module
# nodenext (with zod 3) and for module commonjs (node10 resolution, which
# reads typesVersions; with zod 4: under node10, zod 3's root and its zod/v3
# resolve to two copies of its types, which don't mix, so a zod 3 app there
# imports z from "zod/v3").
cjs() {
  mode=$1 zod=$2
  app="$dir/app-cjs-$mode"
  mkdir -p "$app/src"
  cp "$sdk/scripts/pack-check/cjs.ts" "$app/src/cjs.ts"
  cd "$app"
  printf '{"name":"pack-check-cjs","private":true}\n' >package.json
  npm install --silent --no-fund --no-audit "$tgz" "zod@^$zod" "@modelcontextprotocol/sdk@latest" typescript@5 @types/node@22 >/dev/null
  if [ "$mode" = commonjs ]; then
    npx tsc --strict --esModuleInterop --skipLibCheck --module commonjs --moduleResolution node10 --target es2022 --types node --outDir out src/cjs.ts
  else
    npx tsc --strict --skipLibCheck --module nodenext --moduleResolution nodenext --target es2022 --types node --outDir out src/cjs.ts
  fi
  node out/cjs.js
  echo "CommonJS (module $mode, zod $zod): ok"
  cd "$sdk"
}
if [ "$which" != v2 ]; then
  cjs nodenext 3
  cjs commonjs 4
fi
# zod 3 under node10 resolution: defineEvent typechecks, without TS2589.
if [ "$which" != v2 ]; then
  app="$dir/app-node10-zod3"
  mkdir -p "$app"
  cp "$sdk/scripts/pack-check/node10-zod3.ts" "$app/node10-zod3.ts"
  cd "$app"
  printf '{"name":"pack-check-node10","private":true}\n' >package.json
  npm install --silent --no-fund --no-audit "$tgz" "zod@^3" "@modelcontextprotocol/sdk@latest" typescript@5 @types/node@22 >/dev/null
  npx tsc --strict --noEmit --esModuleInterop --skipLibCheck --module commonjs --moduleResolution node10 --target es2022 --types node node10-zod3.ts
  echo "zod 3 under node10: ok"
  cd "$sdk"
fi

if [ "$which" != v1 ]; then
  server_floor=$(node -p 'require("./package.json").peerDependencies["@modelcontextprotocol/server"].replace(/^\^/, "")')
  for mcp in "$server_floor" latest; do
    app "server$mcp" server-v2.ts "zod@^4" "@modelcontextprotocol/server@$mcp" "@modelcontextprotocol/client@$mcp"
    if [ -d node_modules/@modelcontextprotocol/sdk ]; then
      echo "server $mcp: @modelcontextprotocol/sdk was installed too, so this doesn't show the SDK works without it" >&2
      exit 1
    fi
    echo "zod $(node -p 'require("zod/package.json").version'), @modelcontextprotocol/server $(node -p 'JSON.parse(require("fs").readFileSync("node_modules/@modelcontextprotocol/server/package.json", "utf8")).version'): ok"
    cd "$sdk"
  done
fi

# A Cloudflare Worker that only emits: what the SDK adds to its bundle, under budget.
cd "$sdk" && node scripts/bundle-check.mjs
