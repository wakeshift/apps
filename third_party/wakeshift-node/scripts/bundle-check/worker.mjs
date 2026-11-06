import { Wakeshift } from "@wakeshift/sdk";
import { z } from "zod";
export default {
  async fetch(_req, env) {
    const wake = new Wakeshift({ key: env.WAKESHIFT_KEY });
    const ev = wake.defineEvent("crawl.finished", { description: "A crawl finished", args: z.object({ job: z.string() }), payload: z.object({ pages: z.number() }) });
    const out = await ev.emit({ job: "j1" }, { pages: 1 });
    return new Response(JSON.stringify(out));
  },
};
