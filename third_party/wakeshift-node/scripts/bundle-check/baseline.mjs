// The same Worker without Wakeshift: what the app's own zod costs.
import { z } from "zod";
export default {
  async fetch(_req, _env) {
    const payload = z.object({ pages: z.number() });
    return new Response(JSON.stringify(payload.parse({ pages: 1 })));
  },
};
