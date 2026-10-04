// The free sven API: a System One endpoint that answers sven's built-in rules
// with TypeSafe's Jev, and keeps every request, encrypted, in R2.

import builtin from "./builtin.json";
import { seal } from "./seal";

export interface Env {
  INPUTS: R2Bucket;
  RATE_LIMITER: RateLimit;
  TYPESAFE_API_KEY: string;
  SVEN_ENCRYPTION_KEY: string;
}

const upstreamURL = "https://api.typesafe.ai/v1/systemone";
const model = "jev-latest";
const maxBodyBytes = 256 * 1024;
const maxDiffChars = 40_000;
const maxPathChars = 1_000;
const maxQuestions = 64;

// canonical renders a JSON value with sorted keys, so equal questions compare
// equal however their keys are ordered.
function canonical(value: unknown): string {
  if (Array.isArray(value)) {
    return `[${value.map(canonical).join(",")}]`;
  }
  if (value !== null && typeof value === "object") {
    const entries = Object.entries(value).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
    return `{${entries.map(([k, v]) => `${JSON.stringify(k)}:${canonical(v)}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

// allowed holds the built-in rules' questions. The API answers nothing else,
// so it can't be used as a general Jev endpoint.
const allowed = new Set(builtin.map(canonical));

interface Check {
  state: { path: string; diff: string };
  questions: Record<string, unknown>;
}

export default {
  fetch: (request, env, ctx) => handle(request, env, ctx, fetch),
} satisfies ExportedHandler<Env>;

export async function handle(
  request: Request,
  env: Env,
  ctx: ExecutionContext,
  upstream: typeof fetch,
): Promise<Response> {
  if (new URL(request.url).pathname !== "/v1/systemone") {
    return problem(404, "not found");
  }
  if (request.method !== "POST") {
    return problem(405, "use POST");
  }
  if (!env.TYPESAFE_API_KEY || !env.SVEN_ENCRYPTION_KEY) {
    console.error("TYPESAFE_API_KEY and SVEN_ENCRYPTION_KEY must both be set");
    return problem(503, "the free sven API isn't set up yet");
  }
  if (request.headers.get("Sven-Consent") !== "store-requests") {
    return problem(
      403,
      "the free sven API stores the requests and responses it handles: run `sven init` to agree, or use your own key",
    );
  }
  const ip = request.headers.get("CF-Connecting-IP") ?? "unknown";
  if (!(await env.RATE_LIMITER.limit({ key: ip })).success) {
    return problem(429, "the free sven API allows 120 requests a minute");
  }
  const raw = await request.text();
  if (raw.length > maxBodyBytes) {
    return problem(413, `request is over ${maxBodyBytes} bytes`);
  }
  let body: unknown;
  try {
    body = JSON.parse(raw);
  } catch {
    return problem(400, "request is not JSON");
  }
  const invalid = validate(body);
  if (invalid) {
    return invalid;
  }
  const { state, questions } = body as Check;

  const res = await upstream(upstreamURL, {
    method: "POST",
    headers: { Authorization: `Bearer ${env.TYPESAFE_API_KEY}`, "Content-Type": "application/json" },
    body: JSON.stringify({ model, state, questions }),
  });
  const answer = await res.text();
  if (res.status === 401 || res.status === 403) {
    console.error("TypeSafe refused the API key", res.status, answer);
    return problem(502, "sven's model provider refused the request");
  }
  if (!res.ok) {
    return new Response(answer, { status: res.status, headers: { "Content-Type": "application/json" } });
  }
  ctx.waitUntil(
    store(env, { time: new Date().toISOString(), model, state, questions, response: JSON.parse(answer) }).catch(
      (err) => console.error("storing input", err),
    ),
  );
  return new Response(answer, { headers: { "Content-Type": "application/json" } });
}

function validate(body: unknown): Response | undefined {
  if (body === null || typeof body !== "object") {
    return problem(422, "request must be an object");
  }
  const { state, questions } = body as Partial<Check>;
  if (
    state === null ||
    typeof state !== "object" ||
    typeof state.path !== "string" ||
    typeof state.diff !== "string" ||
    Object.keys(state).length !== 2
  ) {
    return problem(422, "state must be {path, diff}, one file's diff as sven sends it");
  }
  if (state.diff.length > maxDiffChars || state.path.length > maxPathChars) {
    return problem(413, `diffs are limited to ${maxDiffChars} characters per request`);
  }
  if (questions === null || typeof questions !== "object") {
    return problem(422, "questions must be an object");
  }
  const qs = Object.values(questions);
  if (qs.length === 0 || qs.length > maxQuestions) {
    return problem(422, `send 1 to ${maxQuestions} questions`);
  }
  if (!qs.every((q) => allowed.has(canonical(q)))) {
    return problem(
      403,
      "the free sven API only answers sven's built-in rules; for custom rules, set provider: typesafe and TYPESAFE_API_KEY",
    );
  }
  return undefined;
}

// store keeps the request and its answers in R2, sealed with the secret key.
async function store(env: Env, record: object): Promise<void> {
  const plaintext = new TextEncoder().encode(JSON.stringify(record));
  const day = new Date().toISOString().slice(0, 10);
  await env.INPUTS.put(`inputs/${day}/${crypto.randomUUID()}`, await seal(env.SVEN_ENCRYPTION_KEY, plaintext), {
    customMetadata: { sealed: "aes-256-gcm" },
  });
}

function problem(status: number, detail: string): Response {
  return Response.json({ detail }, { status });
}
