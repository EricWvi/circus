import assert from "node:assert/strict";
import { create } from "@bufbuild/protobuf";
import { SessionEvent_Kind, TurnOrigin, TurnOutcome } from "@circus/proto/circus/v1/session_pb";
import {
  AcquireLeaseSchema, AppendTranscriptSchema, GetSessionStateSchema, GetSessionState_Section,
  LoadTranscriptSchema, PublishEventsSchema, ReleaseLeaseSchema,
  ReleaseLease_Reason, ReportTurnSchema, ReportTurn_Phase, TakeInputsSchema, WorkerEvent_Nudge_Reason,
} from "@circus/proto/circus/v1/worker_link_pb";
import { loadConfig } from "#worker/config";
import { LinkLostError, WorkerLink, type ResumeOutcome, type SessionOp, type SessionReply } from "#worker/link/index";
import { createLogger } from "#worker/log";

const [sessionId, inputId, bootId, proxyURL] = process.argv.slice(2);
const config = loadConfig();
const link = new WorkerLink({
  ...config, url: proxyURL, bootId, clientVersion: "e2e", log: createLogger("warn"),
  attach: { maxSessions: 1, sdkVersion: "e2e", claudeVersion: "e2e", localOnly: true },
});

const first = Promise.withResolvers<ResumeOutcome[]>();
const second = Promise.withResolvers<ResumeOutcome[]>();
let connections = 0;
link.onReady((outcomes) => {
  if (++connections === 1) first.resolve(outcomes);
  else second.resolve(outcomes);
});

const signal = AbortSignal.timeout(25_000);
const failed = new Promise<never>((_, reject) => {
  signal.addEventListener("abort", () => reject(signal.reason), { once: true });
  void link.closed.then(() => reject(new Error("worker link ended")), reject);
});
void failed.catch(() => {});
const hints = link.hints()[Symbol.asyncIterator]();
link.start();

type Result = Exclude<SessionReply["result"], { case: undefined }>;
type ReplyValues = { [Reply in Result as Reply["case"]]: Reply["value"] };

async function session<K extends keyof ReplyValues>(
  epoch: bigint, op: SessionOp, expected: K,
): Promise<ReplyValues[K]> {
  const { result } = await link.session(sessionId, epoch, op, signal);
  assert.equal(result.case, expected, op.case + ": " + result.case);
  return result.value as ReplyValues[K];
}

try {
  assert.deepEqual(await Promise.race([first.promise, failed]), []);
  const hint = await Promise.race([hints.next(), failed]);
  assert.ok(!hint.done);
  const { value } = hint;
  assert.ok(value.event.case === "offer");
  assert.equal(value.event.value.sessionId, sessionId);
  const lease = await session(0n, { case: "acquireLease", value: create(AcquireLeaseSchema, { fresh: true }) }, "lease");
  const { epoch } = lease;
  assert.ok(epoch > 0n);
  assert.ok(lease.config);
  assert.equal(lease.config.local, true);
  assert.equal(lease.config.defaultDevice, "desk");
  link.setHeld(sessionId, epoch);
  const { inputs } = await session(epoch, { case: "takeInputs", value: create(TakeInputsSchema) }, "inputs");
  assert.equal(inputs.length, 1);
  assert.equal(inputs[0].inputId, inputId);
  assert.equal(inputs[0].text, "run a local turn");
  const { turnNo } = await session(epoch, { case: "reportTurn", value: create(ReportTurnSchema, {
    localSeq: 1n, phase: ReportTurn_Phase.STARTED, origin: TurnOrigin.INPUT,
  }) }, "turn");
  assert.equal(turnNo, 1n);
  const state = {
    case: "getState", value: create(GetSessionStateSchema, { sections: [GetSessionState_Section.NONE] }),
  } as const;
  // The Go relay drops this reply after it reaches the real control plane.
  await assert.rejects(session(epoch, state, "state"), LinkLostError);

  const resumed = await Promise.race([second.promise, failed]);
  assert.equal(resumed.length, 1);
  assert.equal(resumed[0].sessionId, sessionId);
  assert.equal(resumed[0].epoch, epoch);
  assert.equal(resumed[0].ok, true);
  assert.equal(resumed[0].stopped, false);
  for (;;) {
    const hint = await Promise.race([hints.next(), failed]);
    assert.ok(!hint.done);
    const { event } = hint.value;
    if (event.case === "nudge" && event.value.sessionId === sessionId &&
        event.value.reasons.includes(WorkerEvent_Nudge_Reason.JOB)) break;
  }
  assert.equal((await session(epoch, state, "state")).state?.stopped, false);
  // Exercise negotiated fragmentation in both directions with one opaque
  // transcript entry larger than F and the requested page byte budget.
  const json = JSON.stringify({ uuid: inputId, content: inputs[0].text, padding: "x".repeat(300 << 10) });
  await session(epoch, { case: "appendTranscript", value: create(AppendTranscriptSchema, {
    batchSeq: 1n, entries: [{ uuid: inputId, json }],
  }) }, "ok");
  const page = await session(epoch, {
    case: "loadTranscript", value: create(LoadTranscriptSchema, { maxBytes: 64 << 10 }),
  }, "transcriptPage");
  assert.equal(page.entries.length, 1);
  assert.equal(page.entries[0].json, json);
  await session(epoch, { case: "publishEvents", value: create(PublishEventsSchema, {
    batchSeq: 1n, events: [{ kind: SessionEvent_Kind.RESULT, json: '{"result":"typescript worker"}' }],
  }) }, "ok");
  const ended = await session(epoch, { case: "reportTurn", value: create(ReportTurnSchema, {
    localSeq: 1n, phase: ReportTurn_Phase.ENDED, outcome: TurnOutcome.COMPLETED, persisted: { inputIds: [inputId] },
  }) }, "turn");
  assert.equal(ended.turnNo, turnNo);
  await session(epoch, {
    case: "releaseLease", value: create(ReleaseLeaseSchema, { reason: ReleaseLease_Reason.IDLE }),
  }, "ok");
  link.setHeld(sessionId, null);
  process.stdout.write("lease resumed and released\n");
} finally {
  await hints.return?.();
  await link.close();
}
