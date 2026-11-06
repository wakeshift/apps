// A zod 3 app bundled with esbuild, with no MCP SDK in the bundle or
// beside it: defineEvent renders zod 3 schemas without one, and a disabled
// client (WAKESHIFT_KEY unset) and an enabled one both construct, define
// and list their events.
import { Wakeshift } from "@wakeshift/sdk";
import { z } from "zod/v3";

for (const key of [undefined, "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV"]) {
  const wake = new Wakeshift({ key, core: "https://core.test", logLevel: "off" });
  const ev = wake.defineEvent("crawl.finished", {
    description: "A crawl finished",
    args: z.object({ job: z.string(), site: z.string().optional() }),
    payload: z.object({ pages: z.number(), site: z.string(), status: z.enum(["ok", "failed"]) }),
  });
  if (ev.name !== "crawl.finished") throw new Error("defineEvent returned the wrong event");
}
console.log("zod3 bundle: ok");
