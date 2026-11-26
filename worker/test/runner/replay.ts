import { readFileSync } from "node:fs";
import type {
  HookCallback, HookJSONOutput, Options, PermissionResult, Query, SDKUserMessage, SessionStore,
} from "@anthropic-ai/claude-agent-sdk";
import type { QueryFactory } from "../../src/runner/runner.js";

// Replays SDK message sequences recorded from the real SDK and CLI (see
// test/fixtures/sdk/README.md) against a runner, behaving like the SDK where
// the runner can observe it: pushed messages are pulled from the prompt,
// SessionStore appends are serialized and retried twice (200 ms, 800 ms)
// before a mirror_error, a result waits for pending appends, PreToolUse
// hooks are called, and interrupt() / close() are honored.

export type Step =
  | { t: "msg"; m: Record<string, unknown> }
  | { t: "append"; subpath?: string; entries: Array<Record<string, unknown>> }
  // Waits until the runner has ended the prompt (the CLI's input): what
  // follows is what the CLI does as it exits.
  | { t: "exit" }
  | { t: "hook"; input: Record<string, unknown> }
  // The CLI asks canUseTool, as it does for a call the permission mode
  // asks about and for AskUserQuestion.
  | { t: "canUse"; tool: string; input: Record<string, unknown>; toolUseID: string; agentID?: string; decisionReason?: string }
  | { t: "push"; i: number; priority?: string }
  | { t: "interrupt" }
  | { t: "mirror_error_shape"; m: Record<string, unknown> }
  | { t: "call"; fn: (q: ReplayQuery) => void | Promise<void> }
  | { t: "throw"; message: string };

export interface Fixture {
  scenario: string;
  sdk: string;
  cli: string;
  steps: Step[];
}

export function fixture(name: string): Fixture {
  return JSON.parse(readFileSync(new URL(`../fixtures/sdk/${name}.json`, import.meta.url), "utf8")) as Fixture;
}

export const WAIT_MS = 5_000;

export interface ReplayOptions {
  /** Called when the script waits for the runner's i-th push (to queue the input it pushes). */
  onWaitPush?: (i: number, q: ReplayQuery) => void;
  retryDelays?: [number, number];
  /** The query ends when its script does, with appends still in flight (as the SDK may when the CLI exits). */
  endBeforeAppends?: boolean;
}

export class Replay {
  readonly instances: ReplayQuery[] = [];
  readonly scripts: Step[][];

  constructor(scripts: Step[][], readonly options: ReplayOptions = {}) {
    this.scripts = [...scripts];
  }

  readonly factory: QueryFactory = (params) => {
    const steps = this.scripts.shift();
    if (!steps) throw new Error("no replay script left for another query");
    const q = new ReplayQuery(steps, params, this.options);
    this.instances.push(q);
    return q as unknown as Query;
  };
}

export class ReplayQuery {
  readonly pushed: SDKUserMessage[] = [];
  readonly hookResults: HookJSONOutput[] = [];
  readonly canUseResults: PermissionResult[] = [];
  readonly appendErrors: unknown[] = [];
  /** The entries of every append call the SDK made (each once, not per attempt). */
  readonly appended: Array<Array<Record<string, unknown>>> = [];
  interrupts = 0;
  closed = false;
  error?: unknown;
  readonly sessionId: string;
  readonly done: Promise<void>;
  #out: unknown[] = [];
  #finished = false;
  /** The runner is waiting for its next message: it handled every earlier one. */
  #idle = false;
  #wake = new Set<() => void>();
  #chain: Promise<void> = Promise.resolve();
  #mirrorShape?: Record<string, unknown>;
  /** The runner ended the prompt: like the CLI, the replay ends once its script is done. */
  #promptEnded = false;

  constructor(steps: Step[], readonly params: { prompt: AsyncIterable<SDKUserMessage>; options: Options }, readonly opts: ReplayOptions) {
    this.sessionId = params.options.sessionId ?? params.options.resume ?? "";
    void this.#pull();
    this.done = this.#play(steps);
  }

  get options(): Options {
    return this.params.options;
  }

  get store(): SessionStore {
    return this.params.options.sessionStore!;
  }

  async #pull(): Promise<void> {
    for await (const message of this.params.prompt) {
      this.pushed.push(message);
      this.#notify();
    }
    this.#promptEnded = true;
    this.#notify();
  }

  #notify(): void {
    for (const wake of [...this.#wake]) wake();
  }

  async until(cond: () => boolean, what: string): Promise<void> {
    const deadline = Date.now() + WAIT_MS;
    while (!cond()) {
      if (this.closed) throw new Error("closed");
      if (Date.now() > deadline) throw new Error(`replay timed out waiting for ${what}`);
      await new Promise<void>((resolve) => {
        const timer = setTimeout(done, 50);
        function done() {
          clearTimeout(timer);
          resolve();
        }
        this.#wake.add(done);
      });
    }
  }

  #subst<T>(value: T): T {
    let json = JSON.stringify(value).replaceAll("$SESSION", this.sessionId);
    json = json.replace(/\$PUSH(\d+)/g, (all, i: string) => this.pushed[Number(i)]?.uuid ?? all);
    return JSON.parse(json) as T;
  }

  emit(message: Record<string, unknown>): void {
    this.#out.push(message);
    this.#notify();
  }

  async #play(steps: Step[]): Promise<void> {
    let interrupts = 0;
    try {
      for (const step of steps) {
        if (this.closed) return;
        // Lockstep: the runner has handled every message emitted so far.
        await this.until(() => this.#out.length === 0 && this.#idle, "the runner to handle the last message");
        switch (step.t) {
          case "msg": {
            const message = this.#subst(step.m);
            // The SDK yields a result only after the pending appends settle.
            if (message.type === "result") await this.#chain;
            this.emit(message);
            break;
          }
          case "append": {
            const entries = this.#subst(step.entries);
            this.#chain = this.#chain.then(() => this.#append(step.subpath, entries));
            break;
          }
          case "hook": {
            const hook = this.options.hooks?.PreToolUse?.[0]?.hooks[0] as HookCallback;
            const input = this.#subst(step.input);
            this.hookResults.push(await hook({ session_id: this.sessionId, transcript_path: "", cwd: this.options.cwd ?? "", ...input } as never,
              input.tool_use_id as string, { signal: new AbortController().signal }));
            break;
          }
          case "canUse": {
            const canUse = this.options.canUseTool!;
            this.canUseResults.push((await canUse(step.tool, this.#subst(step.input), {
              signal: new AbortController().signal, toolUseID: step.toolUseID, requestId: crypto.randomUUID(),
              ...(step.agentID ? { agentID: step.agentID } : {}), ...(step.decisionReason ? { decisionReason: step.decisionReason } : {}),
            }))!);
            break;
          }
          case "push":
            this.opts.onWaitPush?.(step.i, this);
            await this.until(() => this.pushed.length > step.i, `push ${step.i}`);
            break;
          case "interrupt": {
            const want = ++interrupts;
            await this.until(() => this.interrupts >= want, `interrupt ${want}`);
            break;
          }
          case "mirror_error_shape":
            this.#mirrorShape = step.m;
            break;
          case "call":
            await step.fn(this);
            break;
          case "exit":
            await this.until(() => this.#promptEnded, "the prompt to end");
            break;
          case "throw":
            throw new Error(step.message);
        }
      }
      if (!this.opts.endBeforeAppends) await this.#chain;
      // The CLI stays up, idle, until the runner closes the query or ends
      // its input.
      while (!this.closed && !this.#promptEnded) await new Promise<void>((resolve) => {
        const done = () => {
          this.#wake.delete(done);
          resolve();
        };
        this.#wake.add(done);
      });
    } catch (error) {
      if (!this.closed) this.error = error;
    } finally {
      this.#finished = true;
      this.#notify();
    }
  }

  async #append(subpath: string | undefined, entries: Array<Record<string, unknown>>): Promise<void> {
    const key = { projectKey: "project", sessionId: this.sessionId, ...(subpath ? { subpath } : {}) };
    const delays = this.opts.retryDelays ?? [200, 800];
    this.appended.push(entries);
    let last: unknown;
    for (let attempt = 0; attempt < 3; attempt++) {
      if (attempt) await new Promise((resolve) => setTimeout(resolve, delays[attempt - 1]));
      try {
        await this.store.append(key, entries as never);
        return;
      } catch (error) {
        last = error;
        this.appendErrors.push(error);
      }
    }
    this.emit({
      ...(this.#mirrorShape ?? {}), type: "system", subtype: "mirror_error", error: String((last as Error)?.message ?? last),
      key: { projectKey: "project", sessionId: this.sessionId, ...(subpath ? { subpath } : {}) },
      uuid: crypto.randomUUID(), session_id: this.sessionId,
    });
  }

  // ---- The Query surface the runner uses ----

  interrupt(): Promise<Record<string, never>> {
    this.interrupts++;
    this.#notify();
    return Promise.resolve({});
  }

  close(): void {
    this.closed = true;
    this.#notify();
  }

  [Symbol.asyncIterator](): AsyncIterator<unknown> {
    return {
      next: async () => {
        for (;;) {
          this.#idle = false;
          if (this.#out.length && !this.closed) return { done: false, value: this.#out.shift() };
          if (this.closed) return { done: true, value: undefined };
          if (this.#finished) {
            if (this.error) throw this.error;
            return { done: true, value: undefined };
          }
          this.#idle = true;
          this.#notify();
          await new Promise<void>((resolve) => {
            const done = () => {
              this.#wake.delete(done);
              resolve();
            };
            this.#wake.add(done);
          });
        }
      },
      return: async () => {
        this.close();
        return { done: true, value: undefined };
      },
    };
  }
}
