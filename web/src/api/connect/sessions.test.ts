import { BinaryWriter, WireType } from "@bufbuild/protobuf/wire"
import { Code, ConnectError, createRouterTransport, type ServiceImpl } from "@connectrpc/connect"
import {
  ApprovalKind,
  InputPriority,
  QueueReason,
  SessionEvent_Kind,
  SessionService,
  Session_State,
  TurnOutcome,
} from "@circus/proto/circus/v1/session_pb"
import { LedgerSnapshotError, PrepareWorkspaceResult_Action, WorkspaceError } from "@circus/proto/circus/v1/device_pb"
import { SessionLedgerStatus_Applying, SessionLedgerStatus_State } from "@circus/proto/circus/v1/ledger_pb"
import { WorkspaceStatus_State, WorkspaceStatus_Waiting } from "@circus/proto/circus/v1/workspace_pb"
import { afterEach, describe, expect, it } from "vitest"

import { ApiError } from "@/api/errors"
import { onUnauthenticated } from "@/api/unauthenticated"

import { createConnectSessions } from "./sessions"

/** A client against a fake SessionService, over JSON like the browser's. */
function sessions(impl: Partial<ServiceImpl<typeof SessionService>>) {
  return createConnectSessions(
    createRouterTransport(({ service }) => service(SessionService, impl), {
      transport: { useBinaryFormat: false },
    }),
  )
}

const protoSession = {
  sessionId: "s1",
  projectId: "p1",
  title: "Fix the build",
  state: Session_State.IDLE,
  lastTurnOutcome: TurnOutcome.INTERRUPTED,
  turnCount: 4n,
  createdAtUnixMs: 1_000n,
  lastActivityAtUnixMs: 2_000n,
  pendingApprovalCount: 0,
}

let stop = () => {}
afterEach(() => stop())

describe("the Connect session client", () => {
  it("maps sessions to what the UI shows, with the device's working directory", async () => {
    const client = sessions({
      getSession: () => ({
        ...protoSession,
        config: { defaultDevice: "box", cwds: [{ device: "other", cwd: "/o" }, { device: "box", cwd: "/src" }] },
      }),
    })
    expect(await client.getSession({ sessionId: "s1" })).toEqual({
      sessionId: "s1",
      projectId: "p1",
      title: "Fix the build",
      state: "interrupted",
      serverState: "idle",
      turnCount: 4,
      viewTruncated: false,
      lastActivityAtUnixMs: 2_000,
      pendingApprovalCount: 0,
      createdAtUnixMs: 1_000,
      device: "box",
      cwd: "/src",
      permissionMode: "auto",
      providerId: "",
      queueReason: "unspecified",
      workerDisconnectedAtUnixMs: 0,
    })
  })

  it("maps the session's ledger snapshot, and leaves it out when unset", async () => {
    const client = sessions({
      listSessions: () => ({
        sessions: [
          {
            ...protoSession,
            ledger: {
              state: SessionLedgerStatus_State.FAILED,
              path: "",
              error: LedgerSnapshotError.NO_SPACE,
              errorDetail: "disk full",
              collectedAtUnixMs: 5n,
              collectionDue: true,
              changesetId: "c1",
              changeCount: 3,
              problemCount: 1,
              updatedAtUnixMs: 9n,
              applying: SessionLedgerStatus_Applying.DISCARD,
            },
          },
          { ...protoSession, sessionId: "s2", ledger: { state: 99 as SessionLedgerStatus_State, error: 77 as LedgerSnapshotError } },
          { ...protoSession, sessionId: "s3" },
        ],
        nextPageToken: "",
      }),
    })
    const [a, b, c] = await client.listSessions({})
    expect(a.ledger).toEqual({
      state: "failed",
      path: "",
      error: "no_space",
      errorDetail: "disk full",
      collectedAtUnixMs: 5,
      collectionDue: true,
      changesetId: "c1",
      changeCount: 3,
      problemCount: 1,
      updatedAtUnixMs: 9,
      applying: "discard",
    })
    // Values from a newer server read as unspecified.
    expect(b.ledger).toMatchObject({ state: "unspecified", error: "unspecified" })
    expect(c).not.toHaveProperty("ledger")
  })

  it("deletes a session, discarding its Ledger changes only when asked, and names the refusal", async () => {
    const seen: boolean[] = []
    const failure = () => {
      const w = new BinaryWriter()
      w.tag(1, WireType.LengthDelimited).fork()
      w.tag(1, WireType.LengthDelimited).string("LEDGER_CHANGES")
      w.tag(2, WireType.LengthDelimited).string("s1")
      w.join()
      return w.finish()
    }
    const client = sessions({
      deleteSession: (req) => {
        seen.push(req.discardLedgerChanges)
        if (req.discardLedgerChanges) return {}
        const error = new ConnectError("unmerged ledger changes", Code.FailedPrecondition)
        error.details.push({ type: "google.rpc.PreconditionFailure", value: failure() })
        throw error
      },
    })
    await expect(client.deleteSession({ sessionId: "s1" })).rejects.toMatchObject({
      code: "failed_precondition",
      preconditionType: "LEDGER_CHANGES",
    })
    await client.deleteSession({ sessionId: "s1", discardLedgerChanges: true })
    expect(seen).toEqual([false, true])
  })

  it("names ledger events", async () => {
    const client = sessions({
      async *watchSession() {
        yield { tsUnixMs: 4n, kind: SessionEvent_Kind.LEDGER, payloadJson: '{"event":"collected"}', ephemeral: true }
      },
    })
    const kinds: string[] = []
    for await (const e of client.watchSession({ sessionId: "s1", afterSeq: 0, signal: new AbortController().signal })) {
      kinds.push(e.kind)
    }
    expect(kinds).toEqual(["ledger"])
  })

  it("maps when the worker holding the session disconnected, 0 while it is connected", async () => {
    const client = sessions({
      listSessions: () => ({
        sessions: [
          { ...protoSession, state: Session_State.RUNNING, workerDisconnectedAtUnixMs: 1_763_380_800_000n },
          { ...protoSession, sessionId: "s2" },
        ],
        nextPageToken: "",
      }),
    })
    const list = await client.listSessions({})
    expect(list.map((s) => s.workerDisconnectedAtUnixMs)).toEqual([1_763_380_800_000, 0])
  })

  it("names worker events", async () => {
    const client = sessions({
      async *watchSession() {
        yield {
          tsUnixMs: 4n,
          kind: SessionEvent_Kind.WORKER,
          payloadJson: '{"disconnected_at_unix_ms":"3"}',
          ephemeral: true,
        }
      },
    })
    const kinds: string[] = []
    for await (const e of client.watchSession({ sessionId: "s1", afterSeq: 0, signal: new AbortController().signal })) {
      kinds.push(e.kind)
    }
    expect(kinds).toEqual(["worker"])
  })

  it("maps the provider and why a queued session waits; a newer reason reads as none", async () => {
    const queued = { ...protoSession, state: Session_State.QUEUED, providerId: "prov-1" }
    const client = sessions({
      listSessions: () => ({
        sessions: [
          { ...queued, queueReason: QueueReason.PROVIDER_FULL },
          { ...queued, sessionId: "s2", queueReason: QueueReason.PROVIDER_SECRETS_UNREADABLE },
          { ...queued, sessionId: "s3", queueReason: 99 as QueueReason },
        ],
        nextPageToken: "",
      }),
    })
    const list = await client.listSessions({})
    expect(list.map((s) => [s.providerId, s.queueReason])).toEqual([
      ["prov-1", "provider_full"],
      ["prov-1", "provider_secrets_unreadable"],
      ["prov-1", "unspecified"],
    ])
  })

  it("sends a provider for a new session only when one is chosen, and changes it with UpdateSession", async () => {
    const created: (string | undefined)[] = []
    const updates: { title?: string; providerId?: string }[] = []
    const client = sessions({
      createSession: (req) => {
        created.push(req.providerId)
        return { session: { ...protoSession, config: { defaultDevice: "box", cwds: [] } } }
      },
      updateSession: (req) => {
        updates.push({ title: req.title, providerId: req.providerId })
        return { ...protoSession, providerId: req.providerId ?? "", config: { defaultDevice: "box", cwds: [] } }
      },
    })
    await client.createSession({ projectId: "p1", locationIndex: 0, clientRequestId: "k1" })
    await client.createSession({ projectId: "p1", locationIndex: 0, clientRequestId: "k2", providerId: "" })
    await client.createSession({ projectId: "p1", locationIndex: 0, clientRequestId: "k3", providerId: "prov-1" })
    expect(created).toEqual([undefined, "", "prov-1"])
    const session = await client.updateSession({ sessionId: "s1", providerId: "" })
    expect(updates).toEqual([{ title: undefined, providerId: "" }])
    expect(session.providerId).toBe("")
  })

  it("sends the permission mode in the config, and reads a session's mode (auto when none)", async () => {
    const modes: (string | undefined)[] = []
    const client = sessions({
      createSession: (req) => {
        modes.push(req.config?.permissionMode)
        return {
          session: { ...protoSession, config: { defaultDevice: "box", cwds: [], permissionMode: req.config?.permissionMode ?? "" } },
        }
      },
    })
    expect((await client.createSession({ projectId: "p1", locationIndex: 0, clientRequestId: "k1" })).permissionMode).toBe("auto")
    const edits = await client.createSession({ projectId: "p1", locationIndex: 0, clientRequestId: "k2", permissionMode: "acceptEdits" })
    expect(edits.permissionMode).toBe("acceptEdits")
    expect(modes).toEqual([undefined, "acceptEdits"])
  })

  it("follows list pages to the end", async () => {
    const tokens: string[] = []
    const client = sessions({
      listSessions: (req) => {
        tokens.push(req.pageToken)
        return req.pageToken === ""
          ? { sessions: [protoSession], nextPageToken: "next" }
          : { sessions: [{ ...protoSession, sessionId: "s2", pendingApprovalCount: 2 }], nextPageToken: "" }
      },
    })
    const list = await client.listSessions({ projectId: "p1" })
    expect(tokens).toEqual(["", "next"])
    expect(list.map((s) => [s.sessionId, s.state])).toEqual([
      ["s1", "interrupted"],
      ["s2", "awaiting_approval"],
    ])
  })

  it("creates a session with its client request id, and reports a reused id as already_exists", async () => {
    const seen: unknown[] = []
    const client = sessions({
      createSession: (req) => {
        seen.push({ projectId: req.projectId, locationIndex: req.locationIndex, clientRequestId: req.clientRequestId })
        if (req.locationIndex === 1) throw new ConnectError("key reused", Code.AlreadyExists)
        return { session: { ...protoSession, config: { defaultDevice: "box", cwds: [{ device: "box", cwd: "/src" }] } } }
      },
    })
    const created = await client.createSession({ projectId: "p1", locationIndex: 0, clientRequestId: "key-1" })
    expect(created).toMatchObject({ sessionId: "s1", device: "box", cwd: "/src" })
    await expect(client.createSession({ projectId: "p1", locationIndex: 1, clientRequestId: "key-1" })).rejects.toMatchObject({
      code: "already_exists",
    })
    expect(seen).toEqual([
      { projectId: "p1", locationIndex: 0, clientRequestId: "key-1" },
      { projectId: "p1", locationIndex: 1, clientRequestId: "key-1" },
    ])
  })

  it("sends a message with its id, text and priority", async () => {
    const seen: unknown[] = []
    const client = sessions({
      sendMessage: (req) => {
        seen.push({ clientMsgId: req.clientMsgId, text: req.text, priority: req.priority })
        return { inputId: "in-1", eventSeq: 7n }
      },
    })
    expect(await client.sendMessage({ sessionId: "s1", clientMsgId: "c1", text: "hi" })).toEqual({
      inputId: "in-1",
      eventSeq: 7,
    })
    await client.sendMessage({ sessionId: "s1", clientMsgId: "c2", text: "now", priority: "now" })
    expect(seen).toEqual([
      { clientMsgId: "c1", text: "hi", priority: InputPriority.NORMAL },
      { clientMsgId: "c2", text: "now", priority: InputPriority.NOW },
    ])
  })

  it("lists pending approvals", async () => {
    const client = sessions({
      listApprovals: () => ({
        approvals: [
          {
            approvalId: "a1",
            sessionId: "s1",
            toolUseId: "t1",
            tool: "bash",
            device: "box",
            display: "git push",
            reason: "needs a remote",
            expiresAtUnixMs: 9_000n,
          },
          {
            approvalId: "a2",
            sessionId: "s1",
            toolUseId: "t2",
            tool: "AskUserQuestion",
            display: "Which color?",
            kind: ApprovalKind.QUESTION,
            inputJson: '{"questions":[]}',
          },
        ],
      }),
    })
    expect(await client.listApprovals({ sessionId: "s1" })).toEqual([
      {
        approvalId: "a1",
        sessionId: "s1",
        toolUseId: "t1",
        tool: "bash",
        device: "box",
        display: "git push",
        reason: "needs a remote",
        expiresAtUnixMs: 9_000,
        // An unspecified kind is a tool approval.
        kind: "tool",
        inputJson: "",
      },
      {
        approvalId: "a2",
        sessionId: "s1",
        toolUseId: "t2",
        tool: "AskUserQuestion",
        device: "",
        display: "Which color?",
        reason: "",
        expiresAtUnixMs: 0,
        kind: "question",
        inputJson: '{"questions":[]}',
      },
    ])
  })

  it("resolves approvals, with a question's answers", async () => {
    const seen: { approvalId: string; approve: boolean; answers: { question: string; selected: string[]; other: string }[] }[] = []
    const client = sessions({
      resolveApproval: (req) => {
        seen.push({
          approvalId: req.approvalId,
          approve: req.approve,
          answers: req.answers.map((a) => ({ question: a.question, selected: [...a.selected], other: a.other })),
        })
        return {}
      },
    })
    await client.resolveApproval({ approvalId: "a1", approve: true })
    await client.resolveApproval({
      approvalId: "a2",
      approve: true,
      answers: [{ question: "Which color?", selected: ["Red"], other: "" }],
    })
    expect(seen).toEqual([
      { approvalId: "a1", approve: true, answers: [] },
      { approvalId: "a2", approve: true, answers: [{ question: "Which color?", selected: ["Red"], other: "" }] },
    ])
  })

  it("renames a session, and reads a full view", async () => {
    let title = ""
    const client = sessions({
      updateSession: (req) => {
        title = req.title ?? ""
        return { ...protoSession, title: req.title, viewTruncated: true, config: { defaultDevice: "box", cwds: [] } }
      },
    })
    expect(await client.updateSession({ sessionId: "s1", title: "Renamed" })).toMatchObject({
      title: "Renamed",
      viewTruncated: true,
      cwd: "",
    })
    expect(title).toBe("Renamed")
  })

  it("maps a session's workspace, and retries it", async () => {
    let retried = ""
    const client = sessions({
      listSessions: () => ({
        sessions: [
          {
            ...protoSession,
            workspace: {
              state: WorkspaceStatus_State.FAILED,
              device: "box",
              path: "~/circus/projects/p1",
              repoUrl: "https://h/r.git",
              error: WorkspaceError.REMOTE_MISMATCH,
              errorDetail: "git@h:other/r",
              attemptStartedAtUnixMs: 10n,
              updatedAtUnixMs: 20n,
            },
          },
          protoSession,
        ],
        nextPageToken: "",
      }),
      retryWorkspace: (req) => {
        retried = req.sessionId
        return { workspace: { state: WorkspaceStatus_State.PENDING, waiting: WorkspaceStatus_Waiting.RETRY, device: "box" } }
      },
      getSession: () => ({
        ...protoSession,
        config: { defaultDevice: "box", cwds: [{ device: "box", cwd: "/home/u/circus/projects/p1" }] },
        workspace: {
          state: WorkspaceStatus_State.READY,
          device: "box",
          path: "/home/u/circus/projects/p1",
          action: PrepareWorkspaceResult_Action.USED_REPOSITORY,
          error: 77 as WorkspaceError,
        },
      }),
    })
    const [withWorkspace, without] = await client.listSessions({})
    expect(withWorkspace.workspace).toEqual({
      state: "failed",
      waiting: "unspecified",
      device: "box",
      path: "~/circus/projects/p1",
      repoUrl: "https://h/r.git",
      background: false,
      action: "unspecified",
      error: "remote_mismatch",
      errorDetail: "git@h:other/r",
      attemptStartedAtUnixMs: 10,
      updatedAtUnixMs: 20,
    })
    expect(without.workspace).toBeUndefined()
    expect((await client.getSession({ sessionId: "s1" })).workspace).toMatchObject({
      state: "ready",
      action: "used_repository",
      error: "unspecified",
    })
    expect(await client.retryWorkspace({ sessionId: "s1" })).toMatchObject({ state: "pending", waiting: "retry" })
    expect(retried).toBe("s1")
  })

  it("treats a retry answer without a workspace as malformed", async () => {
    const client = sessions({ retryWorkspace: () => ({}) })
    await expect(client.retryWorkspace({ sessionId: "s1" })).rejects.toMatchObject({ code: "internal" })
  })

  it("names workspace events", async () => {
    const client = sessions({
      async *watchSession() {
        yield { seq: 3n, tsUnixMs: 4n, kind: SessionEvent_Kind.WORKSPACE, payloadJson: '{"state":"ready"}' }
      },
    })
    for await (const e of client.watchSession({ sessionId: "s1", afterSeq: 0, signal: new AbortController().signal })) {
      expect(e.kind).toBe("workspace")
    }
  })

  it("recovers by accepting the gap", async () => {
    let accepted: boolean | undefined
    const client = sessions({
      recoverSession: (req) => {
        accepted = req.acceptTranscriptGap
        return {}
      },
    })
    await client.recoverSession({ sessionId: "s1" })
    expect(accepted).toBe(true)
  })

  it("streams raw events after after_seq", async () => {
    let afterSeq: bigint | undefined
    const client = sessions({
      async *watchSession(req) {
        afterSeq = req.afterSeq
        yield { seq: 0n, tsUnixMs: 5n, ephemeral: true }
        yield { seq: 4n, tsUnixMs: 6n, kind: SessionEvent_Kind.INPUT_ACCEPTED, payloadJson: "{}" }
        yield { seq: 5n, tsUnixMs: 7n, kind: 99 as SessionEvent_Kind, truncated: true }
      },
    })
    const events = []
    for await (const e of client.watchSession({ sessionId: "s1", afterSeq: 3, signal: new AbortController().signal })) {
      events.push(e)
    }
    expect(afterSeq).toBe(3n)
    expect(events).toEqual([
      { seq: 0, tsUnixMs: 5, kind: "unspecified", payloadJson: "", ephemeral: true, truncated: false },
      { seq: 4, tsUnixMs: 6, kind: "input_accepted", payloadJson: "{}", ephemeral: false, truncated: false },
      { seq: 5, tsUnixMs: 7, kind: "unspecified", payloadJson: "", ephemeral: false, truncated: true },
    ])
  })

  it("ends a stream with an ApiError, and reports a signed-out user", async () => {
    let reported = 0
    stop = onUnauthenticated(() => reported++)
    const failing = (code: Code) =>
      sessions({
        // eslint-disable-next-line require-yield -- fails before the first message
        async *watchSession() {
          throw new ConnectError("no", code)
        },
      })
    const drain = async (client: ReturnType<typeof sessions>) => {
      try {
        for await (const _ of client.watchSession({ sessionId: "s1", afterSeq: 0, signal: new AbortController().signal })) {
          // nothing
        }
      } catch (error) {
        return error
      }
    }
    const notFound = await drain(failing(Code.NotFound))
    expect(notFound).toBeInstanceOf(ApiError)
    expect((notFound as ApiError).code).toBe("not_found")
    expect(reported).toBe(0)
    expect(((await drain(failing(Code.Unauthenticated))) as ApiError).code).toBe("unauthenticated")
    expect(reported).toBe(1)
  })

  it("reports a signed-out user from a unary call too", async () => {
    let reported = 0
    stop = onUnauthenticated(() => reported++)
    const client = sessions({
      interrupt: () => {
        throw new ConnectError("signed out", Code.Unauthenticated)
      },
    })
    await expect(client.interrupt({ sessionId: "s1" })).rejects.toMatchObject({ code: "unauthenticated" })
    expect(reported).toBe(1)
  })
})
