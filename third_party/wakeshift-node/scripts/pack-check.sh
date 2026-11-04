#!/bin/sh
# Checks the packed SDK the way an app installs it: npm pack, then a fresh
# project per MCP SDK installs the tarball, typechecks a server with tsc
# --strict, and runs it. Needs the npm registry.
#
#   - @modelcontextprotocol/sdk 1.x (scripts/pack-check/server.ts): zod 3
#     and 4, each with the oldest 1.x the peer range allows and the latest;
#   - @modelcontextprotocol/server 2.x (server-v2.ts), with 1.x not
#     installed: zod 4 (which 2.x requires), the oldest 2.x the peer range
#     allows and the latest.
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
  mcp_floor=$(node -p 'require("./package.json").peerDependencies["@modelcontextprotocol/sdk"].replace(/^\^/, "")')
  for zod in 3 4; do
    for mcp in "$mcp_floor" latest; do
      app "zod$zod-mcp$mcp" server.ts "zod@^$zod" "@modelcontextprotocol/sdk@$mcp"
      echo "zod $zod ($(node -p 'require("zod/package.json").version')), MCP SDK $(node -p 'JSON.parse(require("fs").readFileSync("node_modules/@modelcontextprotocol/sdk/package.json", "utf8")).version'): ok"
      cd "$sdk"
    done
  done
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
