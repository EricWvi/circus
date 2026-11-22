import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { create } from "@bufbuild/protobuf";
import { AcquireLeaseSchema, type SessionReply_Lease } from "@circus/proto/circus/v1/worker_link_pb";
import type { Logger } from "../../src/log.js";
import type { CliProcessesPort } from "../../src/process.js";
import { Semaphore } from "../../src/runner/ops.js";
import { SessionRunner, type RunnerDeps, type RunnerEnd } from "../../src/runner/runner.js";
import { FakeLink } from "./fake-link.js";
import { Replay, type ReplayOptions, type Step } from "./replay.js";

export interface LogLine {
  level: string;
  message: string;
  fields?: Record<string, unknown>;
}

export function captureLogger(lines: LogLine[] = []): Logger & { lines: LogLine[] } {
  const write = (level: string) => (message: string, fields?: Record<string, unknown>) => { lines.push({ level, message, fields }); };
  return { lines, debug: write("debug"), info: write("info"), warn: write("warn"), error: write("error") };
}

export class FakeProcesses implements CliProcessesPort {
  confirmed = 0;
  /** The link's call count at each confirmExit, to order it against link operations. */
  confirmedAt: number[] = [];
  link?: { calls: unknown[] };
  cleaned: string[] = [];
  spawner(): () => never {
    return () => {
      throw new Error("tests replace the query; nothing is spawned");
    };
  }
  async confirmExit(): Promise<void> {
    this.confirmed++;
    this.confirmedAt.push(this.link?.calls.length ?? -1);
  }
  cleanup(sessionId: string): void {
    this.cleaned.push(sessionId);
  }
}

export async function waitFor(cond: () => boolean, what: string, ms = 5_000): Promise<void> {
  const deadline = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}

export interface HarnessOptions {
  lease?: (lease: SessionReply_Lease, link: FakeLink) => void;
  replay?: ReplayOptions;
  deps?: Partial<RunnerDeps>;
}

/** A runner on a FakeLink lease, with a replayed query. */
export class Harness {
  readonly cwd = mkdtempSync(join(tmpdir(), "circus-runner-"));
  readonly link: FakeLink;
  readonly replay: Replay;
  readonly processes = new FakeProcesses();
  readonly log = captureLogger();
  runner!: SessionRunner;
  done!: Promise<RunnerEnd>;

  constructor(scripts: Step[][], readonly opts: HarnessOptions = {}) {
    this.link = new FakeLink(this.cwd);
    this.processes.link = this.link;
    this.replay = new Replay(scripts, opts.replay);
    opts.lease?.(this.link.lease, this.link);
  }

  async start(): Promise<this> {
    const reply = await this.link.session(this.link.sessionId, 0n, {
      case: "acquireLease", value: create(AcquireLeaseSchema, { fresh: true }),
    });
    if (reply.result.case !== "lease") throw new Error("no lease");
    const deps: RunnerDeps = {
      link: this.link, ready: async () => {}, slots: new Semaphore(48), loadSlots: new Semaphore(1), log: this.log,
      query: this.replay.factory, processes: this.processes, ...this.opts.deps,
    };
    this.runner = new SessionRunner(reply.result.value, this.link.sessionId, deps);
    this.done = this.runner.start();
    return this;
  }

  get query() {
    return this.replay.instances[0];
  }
}
