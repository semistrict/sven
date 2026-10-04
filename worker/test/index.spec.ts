import { env } from "cloudflare:workers";
import { createExecutionContext, waitOnExecutionContext } from "cloudflare:test";
import { beforeEach, describe, expect, it } from "vitest";

import builtin from "../src/builtin.json";
import { handle, type Env } from "../src/index";
import { open } from "../src/seal";

const key = btoa(String.fromCharCode(...new Uint8Array(32).fill(7)));
const state = { path: "main.go", diff: '@@ -1 +1 @@\n+\tfmt.Println("got here")\n' };
const debugQuestion = builtin[0];

const jevAnswer = {
  model: "jev-1.13.0",
  answers: { "debug-leftovers": { type: "noul", noul: 0.93 } },
  usage: { input_tokens: 412, output_tokens: 0 },
};

interface Sent {
  url: string;
  auth: string | null;
  body: unknown;
}

// fakeJev answers like TypeSafe with status and body, recording each request.
function fakeJev(status = 200, body: unknown = jevAnswer): { fetch: typeof fetch; sent: Sent[] } {
  const sent: Sent[] = [];
  const fakeFetch = async (input: RequestInfo | URL, init?: RequestInit) => {
    const headers = new Headers(init?.headers);
    sent.push({ url: String(input), auth: headers.get("Authorization"), body: JSON.parse(String(init?.body)) });
    return Response.json(body, { status });
  };
  return { fetch: fakeFetch as typeof fetch, sent };
}

function testEnv(allow = true): Env {
  return {
    INPUTS: (env as unknown as Env).INPUTS,
    RATE_LIMITER: { limit: async () => ({ success: allow }) },
    TYPESAFE_API_KEY: "ts-key",
    SVEN_ENCRYPTION_KEY: key,
  };
}

async function call(
  body: unknown,
  opts: { env?: Env; jev?: ReturnType<typeof fakeJev>; method?: string; path?: string; consent?: boolean } = {},
) {
  const jev = opts.jev ?? fakeJev();
  const e = opts.env ?? testEnv();
  const ctx = createExecutionContext();
  const req = new Request(`https://sven.example${opts.path ?? "/v1/systemone"}`, {
    method: opts.method ?? "POST",
    headers: {
      "CF-Connecting-IP": "203.0.113.7",
      ...(opts.consent === false ? {} : { "Sven-Consent": "store-requests" }),
    },
    body: opts.method === "GET" ? undefined : JSON.stringify(body),
  });
  const res = await handle(req, e, ctx, jev.fetch);
  await waitOnExecutionContext(ctx);
  return { res, jev, env: e };
}

async function stored(e: Env) {
  const list = await e.INPUTS.list({ prefix: "inputs/" });
  return Promise.all(
    list.objects.map(async (o) => {
      const obj = await e.INPUTS.get(o.key);
      return new Uint8Array(await obj!.arrayBuffer());
    }),
  );
}

beforeEach(async () => {
  const e = testEnv();
  const list = await e.INPUTS.list();
  await Promise.all(list.objects.map((o) => e.INPUTS.delete(o.key)));
});

describe("free sven API", () => {
  it("answers built-in rules with Jev and stores the request sealed", async () => {
    const request = { model: "jev-latest", state, questions: { "debug-leftovers": debugQuestion } };

    const { res, jev, env: e } = await call(request);

    expect(res.status).toBe(200);
    expect(await res.json()).toEqual(jevAnswer);
    expect(jev.sent).toEqual([
      {
        url: "https://api.typesafe.ai/v1/systemone",
        auth: "Bearer ts-key",
        body: { model: "jev-latest", state, questions: { "debug-leftovers": debugQuestion } },
      },
    ]);

    const boxes = await stored(e);
    expect(boxes).toHaveLength(1);
    expect(new TextDecoder().decode(boxes[0])).not.toContain("got here");
    const record = JSON.parse(new TextDecoder().decode(await open(key, boxes[0])));
    expect(record).toMatchObject({
      model: "jev-latest",
      state,
      questions: { "debug-leftovers": debugQuestion },
      response: jevAnswer,
    });
  });

  it("refuses clients that haven't agreed to storage", async () => {
    const { res, jev, env: e } = await call(
      { state, questions: { "debug-leftovers": debugQuestion } },
      { consent: false },
    );

    expect(res.status).toBe(403);
    expect(await res.json()).toEqual({
      detail: "the free sven API stores the requests and responses it handles: run `sven init` to agree, or use your own key",
    });
    expect(jev.sent).toHaveLength(0);
    expect(await stored(e)).toHaveLength(0);
  });

  it("refuses custom questions without asking Jev", async () => {
    const custom = { type: "noul", instructions: "Is this a support ticket about billing?" };

    const { res, jev } = await call({ model: "jev-latest", state, questions: { billing: custom } });

    expect(res.status).toBe(403);
    expect(await res.json()).toEqual({
      detail: "the free sven API only answers sven's built-in rules; for custom rules, set provider: typesafe and TYPESAFE_API_KEY",
    });
    expect(jev.sent).toHaveLength(0);
  });

  it("refuses a built-in question with altered criteria", async () => {
    const altered = { ...debugQuestion, criteria: { true: "Anything at all", false: "Nothing" } };

    const { res, jev } = await call({ model: "jev-latest", state, questions: { "debug-leftovers": altered } });

    expect(res.status).toBe(403);
    expect(jev.sent).toHaveLength(0);
  });

  it("refuses state that isn't one file's diff", async () => {
    const { res } = await call({ state: { ticket: "hi" }, questions: { "debug-leftovers": debugQuestion } });

    expect(res.status).toBe(422);
    expect(await res.json()).toEqual({ detail: "state must be {path, diff}, one file's diff as sven sends it" });
  });

  it("refuses oversized diffs", async () => {
    const big = { path: "big.go", diff: "+" + "x".repeat(40_000) };

    const { res, jev } = await call({ state: big, questions: { "debug-leftovers": debugQuestion } });

    expect(res.status).toBe(413);
    expect(jev.sent).toHaveLength(0);
  });

  it("rate limits by client address", async () => {
    const { res, jev } = await call(
      { state, questions: { "debug-leftovers": debugQuestion } },
      { env: testEnv(false) },
    );

    expect(res.status).toBe(429);
    expect(jev.sent).toHaveLength(0);
  });

  it("hides TypeSafe refusing its key and stores nothing", async () => {
    const { res, env: e } = await call(
      { state, questions: { "debug-leftovers": debugQuestion } },
      { jev: fakeJev(401, { detail: "invalid API key" }) },
    );

    expect(res.status).toBe(502);
    expect(await res.json()).toEqual({ detail: "sven's model provider refused the request" });
    expect(await stored(e)).toHaveLength(0);
  });

  it("passes rate limits from TypeSafe through", async () => {
    const { res } = await call(
      { state, questions: { "debug-leftovers": debugQuestion } },
      { jev: fakeJev(429, { detail: "slow down" }) },
    );

    expect(res.status).toBe(429);
  });

  it("serves only POST /v1/systemone", async () => {
    expect((await call(null, { path: "/" })).res.status).toBe(404);
    expect((await call(null, { method: "GET" })).res.status).toBe(405);
  });
});
