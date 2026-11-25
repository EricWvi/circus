import { InMemorySessionStore, type SessionKey, type SessionStore, type SessionStoreEntry } from "@anthropic-ai/claude-agent-sdk";
import { SessionReply_Rejected_Code as Code } from "@circus/proto/circus/v1/worker_link_pb";
import { describe, expect, test } from "vitest";
import { LinkLostError } from "../src/link/index.js";
import { BudgetError, FencedError, RejectedError, Semaphore, SessionOps, StoppedError } from "../src/runner/ops.js";
import { BATCH_BYTES, MAX_QUEUED_BYTES, RetainedBudget, TranscriptLostError, TranscriptStore, WORKER_RETAINED_BYTES } from "../src/runner/store.js";
import { FakeLink, fenced, rejected } from "./runner/fake-link.js";

const SESSION = "00000000-0000-4000-8000-0000000000aa";

function setup(budgetMs = 20_000, maxQueuedBytes?: number, slots = new Semaphore(8), retained = new RetainedBudget()) {
  const link = new FakeLink("/", "sess-1", SESSION);
  link.epoch = 1n;
  let readyCalls = 0;
  const ops = new SessionOps(link, "sess-1", 1n, new AbortController().signal, async () => { readyCalls++; }, slots);
  const seen = { fenced: 0, stopped: 0, gaps: [] as Array<[string, string[]]> };
  const store = new TranscriptStore(ops, SESSION, {
    fenced: () => { seen.fenced++; },
    stopped: () => { seen.stopped++; },
    gap: (subpath, parents) => { seen.gaps.push([subpath, parents]); },
  }, new Semaphore(1), budgetMs, maxQueuedBytes, retained);
  return { link, store, seen, readyCalls: () => readyCalls };
}

const key = (subpath?: string): SessionKey => ({ projectKey: "any-project", sessionId: SESSION, ...(subpath ? { subpath } : {}) });
let n = 0;
const entry = (extra: Record<string, unknown> = {}): SessionStoreEntry => ({ type: "user", uuid: `u-${++n}`, message: { content: `m${n}` }, ...extra });
const plain = (extra: Record<string, unknown> = {}): SessionStoreEntry => ({ type: "queue-operation", operation: "enqueue", n: ++n, ...extra });

describe("TranscriptStore keeps the SessionStore contract of InMemorySessionStore", () => {
  test("appends and loads per subpath, lists subkeys, and returns null for unknown keys", async () => {
    const { store } = setup();
    const memory = new InMemorySessionStore();
    const script: Array<[SessionKey, SessionStoreEntry[]]> = [
      [key(), [entry(), plain(), entry({ parentUuid: null })]],
      [key("subagents/agent-a1"), [entry(), entry()]],
      [key(), [plain(), entry()]],
      [key("subagents/agent-a1"), [plain()]],
      [key(), []],
    ];
    for (const [k, entries] of script) {
      await store.append(k, entries);
      await memory.append(k, entries);
    }
    for (const k of [key(), key("subagents/agent-a1"), key("subagents/none")]) {
      expect(await store.load(k)).toEqual(await memory.load(k));
    }
    expect((await store.listSubkeys(key())).sort()).toEqual((await memory.listSubkeys(key())).sort());
    // Entries with a uuid are idempotent, as the contract asks of adapters.
    const again = script[0][1];
    await store.append(key(), again);
    expect((await store.load(key()))!.filter((e) => e.uuid === again[0].uuid)).toHaveLength(1);
  });

  test("refuses another session's key", async () => {
    const { store } = setup();
    await expect(store.append({ projectKey: "p", sessionId: "other" }, [entry()])).rejects.toThrow(/bound to/);
    await expect(store.load({ projectKey: "p", sessionId: "other" })).rejects.toThrow(/bound to/);
  });
});

describe("TranscriptStore idle and drained", () => {
  test("an append counts from its call, before it reaches the link, until it is acknowledged", async () => {
    const { link, store } = setup();
    let ack!: () => void;
    const gate = new Promise<void>((resolve) => { ack = resolve; });
    link.interceptors.push(async (call) => { if (call.op.case === "appendTranscript") await gate; return undefined; });
    expect(store.idle).toBe(true);
    const first = store.append(key(), [entry()]);
    expect(store.idle).toBe(false);
    const second = store.append(key(), [entry()]);
    let drained = false;
    void store.drained().then(() => { drained = true; });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect([store.idle, drained]).toEqual([false, false]);
    ack();
    await Promise.all([first, second]);
    await store.drained();
    expect(store.idle).toBe(true);
  });
});

describe("TranscriptStore retries", () => {
  test("link loss and STORE_UNAVAILABLE are retried within the call", async () => {
    const { link, store, readyCalls } = setup();
    let calls = 0;
    link.interceptors.push((call) => {
      if (call.op.case !== "appendTranscript") return undefined;
      calls++;
      if (calls === 1) throw new LinkLostError("down");
      return calls === 2 ? rejected(Code.STORE_UNAVAILABLE) : undefined;
    });
    await store.append(key(), [entry()]);
    expect(calls).toBe(3);
    expect(readyCalls()).toBe(1);
    expect(link.ops("appendTranscript").map((a) => a.batchSeq)).toEqual([1n, 1n, 1n]);
  });

  test("a batch left unacknowledged is resent first, with its batch_seq and content", async () => {
    const { link, store } = setup(300);
    let down = true;
    link.interceptors.push((call) => call.op.case === "appendTranscript" && down ? rejected(Code.STORE_UNAVAILABLE) : undefined);
    const first = [plain(), plain()];
    await expect(store.append(key(), first)).rejects.toBeInstanceOf(BudgetError);
    down = false;
    const second = [plain()];
    await store.append(key(), second);
    const sent = link.ops("appendTranscript");
    const firstBatch = sent[0];
    const resent = sent.filter((a) => a.batchSeq === 1n).at(-1)!;
    expect(resent).toBe(firstBatch);
    expect(sent.at(-1)!.batchSeq).toBe(2n);
    expect(await store.load(key())).toEqual([...first, ...second]);
  });

  test("the SDK's retry of a call continues after its acknowledged batches", async () => {
    const { link, store } = setup(300);
    // Three entries of about 3 MiB: two wire batches under the 8 MiB limit.
    const big = () => plain({ blob: "x".repeat(3 << 20) });
    const entries = [big(), big(), big()];
    let failSecond = true;
    link.interceptors.push((call) =>
      call.op.case === "appendTranscript" && call.op.value.batchSeq === 2n && failSecond ? rejected(Code.STORE_UNAVAILABLE) : undefined);
    await expect(store.append(key(), entries)).rejects.toBeInstanceOf(BudgetError);
    failSecond = false;
    await store.append(key(), entries);
    const loaded = (await store.load(key()))!.map((e) => e.n);
    expect(loaded).toEqual(entries.map((e) => e.n));
    expect(link.ops("appendTranscript").map((a) => [a.batchSeq, a.entries.length]).filter(([seq]) => seq === 1n)).toHaveLength(1);
  });

  test("INVALID drops the batch; the next content takes its unconsumed batch_seq", async () => {
    const { link, store } = setup();
    link.interceptors.push((call) =>
      call.op.case === "appendTranscript" && call.op.value.entries.some((e) => e.json.includes("bad")) ? rejected(Code.INVALID, "over cap") : undefined);
    await expect(store.append(key(), [plain({ bad: true })])).rejects.toBeInstanceOf(RejectedError);
    await store.append(key(), [plain()]);
    const sent = link.ops("appendTranscript");
    expect(sent.map((a) => a.batchSeq)).toEqual([1n, 1n]);
    expect(sent[1].entries[0].json).not.toContain("bad");
  });

  test("FENCED and TRANSCRIPT_FULL reject the append and tell the runner", async () => {
    const a = setup();
    a.link.interceptors.push(() => fenced(2n));
    await expect(a.store.append(key(), [entry()])).rejects.toBeInstanceOf(FencedError);
    expect(a.seen.fenced).toBe(1);
    const b = setup();
    b.link.interceptors.push(() => rejected(Code.TRANSCRIPT_FULL));
    await expect(b.store.append(key(), [entry()])).rejects.toBeInstanceOf(StoppedError);
    expect(b.seen.stopped).toBe(1);
  });

  test("appends are serialized", async () => {
    const { link, store } = setup();
    let inFlight = 0;
    let most = 0;
    link.interceptors.push(async (call) => {
      if (call.op.case !== "appendTranscript") return undefined;
      most = Math.max(most, ++inFlight);
      await new Promise((resolve) => setTimeout(resolve, 5));
      inFlight--;
      return undefined;
    });
    await Promise.all([store.append(key(), [plain()]), store.append(key("s"), [plain()]), store.append(key(), [plain()])]);
    expect(most).toBe(1);
    expect(link.ops("appendTranscript").filter((a) => a.subpath === "").map((a) => a.batchSeq)).toEqual([1n, 2n]);
  });
});

describe("TranscriptStore load", () => {
  test("reports parents missing from a loaded transcript", async () => {
    const { store, seen } = setup();
    const a = entry({ parentUuid: null });
    await store.append(key(), [a, entry({ parentUuid: a.uuid }), entry({ parentUuid: "lost-1" }), plain()]);
    const loaded = await store.load(key());
    expect(loaded).toHaveLength(4);
    expect(seen.gaps).toEqual([["", ["lost-1"]]]);
  });

  test("is usable as the SDK's store type", () => {
    const { store } = setup();
    const typed: SessionStore = store;
    expect(typed.delete).toBeUndefined();
  });
});

describe("TranscriptStore repair", () => {
  /** Like the SDK: up to three attempts of one call, then the batch is given up (mirror_error). */
  async function sdkAppend(store: TranscriptStore, entries: SessionStoreEntry[]): Promise<boolean> {
    for (let attempt = 0; attempt < 3; attempt++) {
      try {
        await store.append(key(), entries);
        return true;
      } catch {
        // Retried.
      }
    }
    return false;
  }

  test("calls the SDK gave up on are kept and sent first, in order, without duplicating anything", async () => {
    const { link, store } = setup(30);
    let down = false;
    link.interceptors.push((call) => call.op.case === "appendTranscript" && down ? rejected(Code.STORE_UNAVAILABLE) : undefined);
    const turn = (i: number) => [plain({ turn: i }), entry({ turn: i }), plain({ turn: i, last: true })];
    const given: SessionStoreEntry[] = [];
    for (let i = 0; i < 10; i++) {
      // Every other turn hits an outage: the SDK gives its call up.
      down = i % 2 === 1;
      const entries = turn(i);
      given.push(...entries);
      expect(await sdkAppend(store, entries)).toBe(!down);
      down = false;
      if (i % 3 === 2) await store.repair();
    }
    await store.repair();
    expect(store.queuedBytes).toBe(0);
    // Every entry once, uuid-less ones included, in the order given.
    expect(await store.load(key())).toEqual(given);
    // A batch_seq was never reused for other content.
    const bySeq = new Map<bigint, string>();
    for (const a of link.ops("appendTranscript")) {
      const content = a.entries.map((e) => e.json).join("\n");
      expect(bySeq.get(a.batchSeq) ?? content).toBe(content);
      bySeq.set(a.batchSeq, content);
    }
  });

  test("a refused batch makes the transcript unrepairable, and the SDK's retry of it fails again unsent", async () => {
    const { link, store } = setup();
    link.interceptors.push((call) =>
      call.op.case === "appendTranscript" && call.op.value.entries.some((e) => e.json.includes("bad")) ? rejected(Code.INVALID, "over cap") : undefined);
    const bad = [plain({ bad: true })];
    expect(await sdkAppend(store, bad)).toBe(false);
    expect(link.ops("appendTranscript")).toHaveLength(1);
    await store.append(key(), [plain()]);
    await expect(store.repair()).rejects.toBeInstanceOf(TranscriptLostError);
  });

  test("the queue is bounded; beyond it the transcript is unrepairable", async () => {
    const { link, store } = setup(30, 10_000);
    link.interceptors.push((call) => call.op.case === "appendTranscript" ? rejected(Code.STORE_UNAVAILABLE) : undefined);
    await expect(store.append(key(), [plain({ blob: "x".repeat(3000) })])).rejects.toBeInstanceOf(BudgetError);
    await expect(store.append(key(), [plain({ blob: "y".repeat(3000) })])).rejects.toThrow(/more unacknowledged transcript than the session/);
    link.interceptors.length = 0;
    await expect(store.repair()).rejects.toBeInstanceOf(TranscriptLostError);
  });

  test("waiting for an operation slot counts against the call's budget", async () => {
    const slots = new Semaphore(1);
    const { store } = setup(50, undefined, slots);
    const hold = await slots.acquire(new AbortController().signal);
    await expect(store.append(key(), [plain()])).rejects.toBeInstanceOf(BudgetError);
    hold();
  });
});

describe("TranscriptStore bounds", () => {
  const down = (link: FakeLink) => link.interceptors.push((call) => call.op.case === "appendTranscript" ? rejected(Code.STORE_UNAVAILABLE) : undefined);
  const cost = (entry: SessionStoreEntry) => 2 * (Buffer.byteLength(JSON.stringify(entry)) + (typeof entry.uuid === "string" ? entry.uuid.length : 0) + 16);

  test("the defaults: 32 MiB per session, 512 MiB per worker, 1 MiB batches", () => {
    expect(MAX_QUEUED_BYTES).toBe(32 << 20);
    expect(WORKER_RETAINED_BYTES).toBe(512 << 20);
    expect(BATCH_BYTES).toBe(1 << 20);
  });

  test("retained entries count twice their UTF-8 bytes, two-byte text included", async () => {
    const { link, store } = setup(10);
    down(link);
    const ascii = entry({ text: "x".repeat(1000) });
    const wide = entry({ text: "→".repeat(1000) });
    await expect(store.append(key(), [ascii])).rejects.toBeInstanceOf(BudgetError);
    expect(store.queuedBytes).toBe(cost(ascii));
    await expect(store.append(key(), [wide])).rejects.toBeInstanceOf(BudgetError);
    expect(store.queuedBytes).toBe(cost(ascii) + cost(wide));
    expect(cost(wide)).toBeGreaterThan(2 * 3000);
  });

  test("filling a session to its bound makes it unrepairable", async () => {
    const { link, store } = setup(5, 1 << 20);
    down(link);
    let refused = "";
    for (let i = 0; i < 200 && !refused; i++) {
      try {
        await store.append(key(), [entry({ text: "→" + "y".repeat(16 << 10) + i })]);
      } catch (error) {
        if (/more unacknowledged/.test(String(error))) refused = String(error);
      }
    }
    expect(refused).toMatch(/the session may hold/);
    expect(store.queuedBytes).toBeLessThanOrEqual(1 << 20);
    link.interceptors.length = 0;
    await expect(store.repair()).rejects.toBeInstanceOf(TranscriptLostError);
  });

  test("the worker budget is shared by every session and returned when entries are acknowledged or dropped", async () => {
    const budget = new RetainedBudget(250_000);
    const a = setup(5, undefined, undefined, budget);
    const b = setup(5, undefined, undefined, budget);
    down(a.link);
    down(b.link);
    const big = () => entry({ text: "z".repeat(50_000) });
    await expect(a.store.append(key(), [big()])).rejects.toBeInstanceOf(BudgetError);
    await expect(b.store.append(key(), [big()])).rejects.toBeInstanceOf(BudgetError);
    await expect(b.store.append(key(), [big()])).rejects.toThrow(/the worker may hold/);
    expect(budget.used).toBe(a.store.queuedBytes + b.store.queuedBytes);
    a.link.interceptors.length = 0;
    await a.store.repair();
    b.store.dispose();
    expect(budget.used).toBe(0);
  });

  test("a session ending with unacknowledged entries returns its whole budget; a late append takes nothing", async () => {
    const budget = new RetainedBudget();
    const { link, store } = setup(10, undefined, undefined, budget);
    down(link);
    await expect(store.append(key(), [entry({ text: "a".repeat(5000) })])).rejects.toBeInstanceOf(BudgetError);
    await expect(store.append(key("subagents/agent-1"), [entry({ text: "b".repeat(5000) })])).rejects.toBeInstanceOf(BudgetError);
    expect(budget.used).toBeGreaterThan(20_000);
    store.dispose();
    expect(budget.used).toBe(0);
    // The SDK flushes while the query closes: refused at once, nothing queued or sent.
    link.interceptors.length = 0;
    const sent = link.ops("appendTranscript").length;
    await expect(store.append(key(), [entry({ text: "late" })])).rejects.toThrow(/session has ended/);
    expect(budget.used).toBe(0);
    expect(store.queuedBytes).toBe(0);
    expect(link.ops("appendTranscript")).toHaveLength(sent);
  });

  test("an append in flight when the session ends returns nothing twice", async () => {
    const budget = new RetainedBudget();
    const { link, store } = setup(5_000, undefined, undefined, budget);
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    link.interceptors.push(async (call) => { if (call.op.case === "appendTranscript") await gate; return undefined; });
    const pending = store.append(key(), [entry({ text: "c".repeat(5000) })]);
    await new Promise((resolve) => setTimeout(resolve, 10));
    store.dispose();
    // Another session's retained entries stay counted.
    expect(budget.take(1000)).toBe(true);
    release();
    await pending;
    expect(budget.used).toBe(1000);
  });

  test("an append drains a backlog with a budget per batch", async () => {
    const { link, store } = setup(60);
    let slow = false;
    link.interceptors.push(async (call) => {
      if (call.op.case !== "appendTranscript") return undefined;
      if (!slow) return rejected(Code.STORE_UNAVAILABLE);
      await new Promise((resolve) => setTimeout(resolve, 35));
      return undefined;
    });
    const backlog = [0, 1, 2, 3].map((i) => plain({ b: String(i).repeat(900 << 10) }));
    await expect(store.append(key(), backlog)).rejects.toBeInstanceOf(BudgetError);
    slow = true;
    const next = [plain()];
    await store.append(key(), next);
    expect((await store.load(key()))!.map((e) => e.n)).toEqual([...backlog, ...next].map((e) => e.n));
  });

  test("wire batches stay within 1 MiB; a larger entry goes alone", async () => {
    const { link, store } = setup();
    await store.append(key(), [plain({ b: "a".repeat(400 << 10) }), plain({ b: "b".repeat(400 << 10) }), plain({ b: "c".repeat(400 << 10) }), plain({ b: "d".repeat(2 << 20) }), plain()]);
    const batches = link.ops("appendTranscript").map((a) => a.entries.length);
    expect(batches).toEqual([2, 1, 1, 1]);
  });

  test("the repair gives each batch its own budget, so a slow link drains a backlog", async () => {
    const { link, store } = setup(60);
    let slow = false;
    link.interceptors.push(async (call) => {
      if (call.op.case !== "appendTranscript") return undefined;
      if (!slow) return rejected(Code.STORE_UNAVAILABLE);
      await new Promise((resolve) => setTimeout(resolve, 35));
      return undefined;
    });
    const entries = [0, 1, 2, 3].map((i) => plain({ b: String(i).repeat(900 << 10) }));
    await expect(store.append(key(), entries)).rejects.toBeInstanceOf(BudgetError);
    slow = true;
    await store.repair();
    expect((await store.load(key()))!.map((e) => e.n)).toEqual(entries.map((e) => e.n));
  });
});
