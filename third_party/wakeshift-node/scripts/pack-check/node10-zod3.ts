// A zod 3 app compiled with moduleResolution node10 (pack-check.sh): its
// "zod" and the "zod/v3" the SDK names resolve to two copies of zod 3's
// declarations, which must not send the compiler too deep (TS2589), and
// defineEvent still infers the payload.
import { z } from "zod";
import { Wakeshift } from "@wakeshift/sdk";
const wake = new Wakeshift();
const ev = wake.defineEvent("crawl.finished", {
  description: "A crawl finished",
  args: z.object({ job: z.string() }),
  payload: z.object({ pages: z.number(), status: z.enum(["ok", "failed"]) }),
  text: (p) => `${p.pages}`,
});
ev.emit({ job: "j" }, { pages: 1, status: "ok" });
const bad = wake.defineEvent("x.y", { description: "d", args: z.object({ a: z.string() }), payload: z.object({ n: z.number() }), text: (p) => p.n.toFixed() });
// @ts-expect-error: payload n is a number
bad.emit({ a: "x" }, { n: "1" });
