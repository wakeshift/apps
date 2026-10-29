// Wakeshift runs behind Google's load balancer on Cloud Run. For some
// minutes after a rollout, the load balancer sends a small share of requests
// to a retired revision, which answers at once with Google's own HTML "500
// Server Error" page: the request never reached a Wakeshift instance. It
// happens to every client alike (new connections included), so retrying is
// what gets the request through.

/** @internal Pauses before each retry of a refused request (so at most length + 1 attempts). */
export const RETRY_PAUSES_MS = [250, 1000];

/**
 * @internal fetchFn, retrying a request the load balancer refused before it
 * reached Wakeshift: Google's HTML 500 page, for any request (it never ran),
 * and a 502, 503 or 504 not from Wakeshift for a GET or HEAD. Wakeshift's own
 * errors (JSON) are never retried, nor is a request whose body can't be sent
 * again (a stream).
 */
export function retryRefused(fetchFn: typeof fetch, pauses: number[] = RETRY_PAUSES_MS): typeof fetch {
  return (async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const method = (init?.method ?? (input instanceof Request ? input.method : "GET")).toUpperCase();
    const resendable = !(input instanceof Request) && (init?.body == null || typeof init.body === "string" || init.body instanceof URLSearchParams);
    for (let attempt = 0; ; attempt++) {
      const resp = await fetchFn(input, init);
      if (!resendable || attempt >= pauses.length || !(await refused(method, resp))) return resp;
      await resp.body?.cancel().catch(() => {});
      await pause(pauses[attempt]!, init?.signal ?? undefined);
    }
  }) as typeof fetch;
}

/** @internal Whether resp is the load balancer's refusal, not the service's answer. */
export async function refused(method: string, resp: Response): Promise<boolean> {
  const type = (resp.headers.get("Content-Type") ?? "").split(";")[0]!.trim().toLowerCase();
  if (resp.status === 500) {
    if (type !== "text/html") return false;
    const text = await resp.clone().text().catch(() => "");
    return text.slice(0, 4096).includes("Server Error"); // within the first 4 KB, as the Go SDK reads it
  }
  if (resp.status === 502 || resp.status === 503 || resp.status === 504) {
    return (method === "GET" || method === "HEAD") && type !== "application/json";
  }
  return false;
}

function pause(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const t = setTimeout(() => {
      signal?.removeEventListener("abort", stop);
      resolve();
    }, ms);
    const stop = () => {
      clearTimeout(t);
      reject(signal!.reason);
    };
    signal?.addEventListener("abort", stop, { once: true });
  });
}
