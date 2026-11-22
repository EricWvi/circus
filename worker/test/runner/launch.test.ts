import type { ChildProcess } from "node:child_process";
import { mkdtempSync, readFileSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { create } from "@bufbuild/protobuf";
import {
  ReleaseLease_Reason, ReportTurn_Phase, SessionReply_Inputs_Input_Kind as InputKind, SessionReply_LaunchEnvSchema,
  SessionReply_Rejected_Code as Code,
  WorkerEvent_Nudge_Reason as Reason,
} from "@circus/proto/circus/v1/worker_link_pb";
import { afterEach, describe, expect, test, vi } from "vitest";
import { LinkLostError } from "../../src/link/index.js";
import { createLogger } from "../../src/log.js";
import { CliProcesses } from "../../src/process.js";
import { MANAGED_BY_HOST, isCleared, launchPlan } from "../../src/runner/launch.js";
import type { QueryFactory } from "../../src/runner/runner.js";
import { fenced, rejected, reply } from "./fake-link.js";
import { Harness, waitFor } from "./harness.js";
import { fixture, type Step } from "./replay.js";

const ID1 = "00000000-0000-4000-8000-000000000001";
const ID2 = "00000000-0000-4000-8000-000000000002";
const text = () => fixture("text").steps;
const ended = (h: Harness) => h.link.reports.filter((r) => r.phase === ReportTurn_Phase.ENDED);
const pick = (pred: (m: Record<string, unknown>) => boolean) =>
  structuredClone((text().find((s) => s.t === "msg" && pred(s.m)) as { m: Record<string, unknown> }).m);
const init = (): Step => ({ t: "msg", m: { ...pick((m) => m.type === "system" && m.subtype === "init"), uuid: crypto.randomUUID() } });
const assistant = (extra: Record<string, unknown> = {}): Step => ({ t: "msg", m: { ...pick((m) => m.type === "assistant"), uuid: crypto.randomUUID(), ...extra } });
const result = (extra: Record<string, unknown> = {}): Step => ({ t: "msg", m: { ...pick((m) => m.type === "result"), uuid: crypto.randomUUID(), ...extra } });
const started = (i: number): Step => ({ t: "msg", m: { type: "command_lifecycle", state: "started", command_uuid: `$PUSH${i}`, uuid: crypto.randomUUID() } });
const authFailed = (): Step[] => [
  { t: "push", i: 0 }, started(0), init(), assistant({ error: "authentication_failed" }),
  result({ is_error: true, terminal_reason: "api_error", user_message_uuids: ["$PUSH0"] }),
];

type LaunchInit = MessageInitShape<typeof SessionReply_LaunchEnvSchema>;
const provider = (key: string, extra: Array<{ name: string; value: string }> = []): LaunchInit => ({
  providerId: "prov-1",
  env: [
    { name: "ANTHROPIC_BASE_URL", value: "http://127.0.0.1:9/provider" },
    { name: "ANTHROPIC_API_KEY", value: key },
    { name: "ANTHROPIC_DEFAULT_OPUS_MODEL", value: "provider-opus" },
    ...extra,
  ],
});
const launchReply = (value: LaunchInit) => reply({ case: "launchEnv", value });

/** What a worker machine logged in to Claude Code, with a cloud provider configured, inherits. */
const inherited: NodeJS.ProcessEnv = {
  PATH: "/usr/bin:/bin", HOME: "/home/worker", LANG: "C.UTF-8", HTTPS_PROXY: "http://proxy:3128",
  ANTHROPIC_API_KEY: "inherited-api-key", ANTHROPIC_BASE_URL: "http://inherited.example", ANTHROPIC_AUTH_TOKEN: "inherited-token",
  ANTHROPIC_MODEL: "inherited-model", ANTHROPIC_CUSTOM_HEADERS: "x-inherited: 1",
  CLAUDE_CODE_SUBAGENT_MODEL: "inherited-subagent-model", CLAUDE_CODE_AUTO_MODE_MODEL: "inherited-classifier-model",
  CLAUDE_CODE_OAUTH_TOKEN: "inherited-oauth", CLAUDE_CODE_USE_BEDROCK: "1", CLAUDE_CODE_USE_VERTEX: "1",
  CLAUDE_CODE_USE_FOUNDRY: "1", AWS_BEARER_TOKEN_BEDROCK: "inherited-bedrock", CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR: "3",
  CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST: "0",
};

/** Records, at each GetLaunchEnv, how many queries had been created; answers with the next reply. */
function launches(h: Harness, replies: LaunchInit[]): number[] {
  const queriesAt: number[] = [];
  h.link.interceptors.push((call) => {
    if (call.op.case !== "getLaunchEnv") return undefined;
    queriesAt.push(h.replay.instances.length);
    const next = replies.shift();
    return next ? launchReply(next) : undefined;
  });
  return queriesAt;
}

describe("launch plan", () => {
  test("local login keeps the worker's environment unchanged", () => {
    const plan = launchPlan(create(SessionReply_LaunchEnvSchema, { localLogin: true }), inherited);
    expect(plan.env).toBeUndefined();
  });

  test("a provider's variables replace every inherited credential and endpoint variable", () => {
    const plan = launchPlan(create(SessionReply_LaunchEnvSchema, provider("provider-key", [{ name: "MY_FLAG", value: "on" }])), inherited);
    expect(plan.env).toEqual({
      PATH: "/usr/bin:/bin", HOME: "/home/worker", LANG: "C.UTF-8", HTTPS_PROXY: "http://proxy:3128",
      ANTHROPIC_BASE_URL: "http://127.0.0.1:9/provider", ANTHROPIC_API_KEY: "provider-key", ANTHROPIC_DEFAULT_OPUS_MODEL: "provider-opus",
      MY_FLAG: "on", [MANAGED_BY_HOST]: "1",
    });
    expect(plan.providerId).toBe("prov-1");
  });

  test("the cleared names (worker/README.md lists them)", () => {
    const cleared = [
      "ANTHROPIC_UNIX_SOCKET", "anthropic_api_key", "ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL",
      "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
      "CLAUDE_CODE_OAUTH_SCOPES", "CLAUDE_CODE_OAUTH_CLIENT_ID", "CLAUDE_CODE_CUSTOM_OAUTH_URL",
      "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "CLAUDE_CODE_API_BASE_URL", "CLAUDE_CODE_SESSION_ACCESS_TOKEN",
      "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_ANTHROPIC_AWS",
      "CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_GATEWAY",
      "CLAUDE_CODE_SKIP_BEDROCK_AUTH", "CLAUDE_CODE_SKIP_VERTEX_AUTH", "CLAUDE_CODE_SKIP_FOUNDRY_AUTH",
      "CLAUDE_CODE_SKIP_ANTHROPIC_AWS_AUTH", "CLAUDE_CODE_SKIP_ANTHROPIC_GOOGLE_CLOUD_AUTH", "CLAUDE_CODE_SKIP_MANTLE_AUTH",
      "AWS_BEARER_TOKEN_BEDROCK", "CLOUD_ML_REGION", "CLAUDE_CODE_GATEWAY_TOKEN", "CLAUDE_CODE_GATEWAY_TOKEN_FILE_DESCRIPTOR",
      "CLAUDE_CODE_HOST_AUTH_ENV_VAR", "CLAUDE_CODE_HOST_AUTH_ASK_AT_START", "CLAUDE_CODE_HOST_AUTH_REFRESH_TIMEOUT_MS",
      "CLAUDE_CODE_SDK_HAS_HOST_AUTH_REFRESH", "CLAUDE_CODE_SDK_HAS_OAUTH_REFRESH", "CLAUDE_CODE_HOST_CREDS_FILE",
      "CLAUDE_CODE_LAUNCHER_RUNS_API_KEY_HELPER", MANAGED_BY_HOST,
      "CLAUDE_CODE_CLIENT_CERT", "CLAUDE_CODE_CLIENT_KEY", "CLAUDE_CODE_CLIENT_KEY_PASSPHRASE",
      "CLAUDE_CODE_RUNNER_MODEL_SOCKET", "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL",
      "CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL_FORCE", "CLAUDE_CODE_WORKFLOW_SUBAGENT_MODEL",
      "CLAUDE_CODE_AUTO_MODE_MODEL", "CLAUDE_CODE_BG_CLASSIFIER_MODEL", "CLAUDE_CODE_MODEL_CATALOG", "CLAUDE_CODE_MODEL_CATALOG_URL",
      "CLAUDE_CODE_3P_SEEDED_OPUS_DEFAULT", "CLAUDE_CODE_3P_PROBE_WROTE_SONNET_DEFAULT",
    ];
    for (const name of cleared) expect(isCleared(name), name).toBe(true);
    for (const name of ["PATH", "HOME", "HTTPS_PROXY", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_MAX_OUTPUT_TOKENS", "AWS_PROFILE"]) {
      expect(isCleared(name), name).toBe(false);
    }
  });

  test("a returned variable wins over an inherited one that is not cleared", () => {
    const plan = launchPlan(create(SessionReply_LaunchEnvSchema, provider("k", [{ name: "HTTPS_PROXY", value: "http://provider-proxy:8080" }])), inherited);
    expect(plan.env?.HTTPS_PROXY).toBe("http://provider-proxy:8080");
  });

  test("reserved or malformed variables are not applied and are reported by name only", () => {
    const plan = launchPlan(create(SessionReply_LaunchEnvSchema, provider("k", [
      { name: "PATH", value: "/evil" }, { name: "CIRCUS_X", value: "x" }, { name: MANAGED_BY_HOST, value: "0" },
      { name: "bad name=secret-in-name", value: "v" }, { name: "NUL", value: "a\0b" },
      { name: "HOME", value: "/evil-home" }, { name: "CLAUDE_CONFIG_DIR", value: "/evil-config" }, { name: "CLAUDE_CODE_OAUTH_TOKEN", value: "t" },
    ])), inherited);
    expect(plan.env?.PATH).toBe("/usr/bin:/bin");
    expect(plan.env?.HOME).toBe("/home/worker");
    expect(plan.env).not.toHaveProperty("CLAUDE_CONFIG_DIR");
    expect(plan.env).not.toHaveProperty("CLAUDE_CODE_OAUTH_TOKEN");
    expect(plan.env?.[MANAGED_BY_HOST]).toBe("1");
    expect(plan.env).not.toHaveProperty("CIRCUS_X");
    expect(plan.env).not.toHaveProperty("NUL");
    expect(plan.skipped).toEqual(["PATH", "CIRCUS_X", MANAGED_BY_HOST, "(invalid)", "NUL", "HOME", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN"]);
  });
});

describe("launch environment", () => {
  test("local login: GetLaunchEnv precedes the CLI start, which keeps the worker's environment", async () => {
    const h = new Harness([text()], { deps: { env: inherited } });
    const queriesAt = launches(h, [{ localLogin: true }]);
    h.link.addInput({ inputId: ID1, text: "hello" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "ENDED");
    expect(queriesAt).toEqual([0]);
    expect(h.query.options.env).toBeUndefined();
    expect(h.query.options.model).toBeUndefined();
    h.runner.shutdown();
    await h.done;
  });

  test("a provider: inherited credentials and model choices are removed, the provider's set, and the default alias is named", async () => {
    const h = new Harness([text()], { deps: { env: inherited } });
    launches(h, [provider("provider-key")]);
    h.link.addInput({ inputId: ID1, text: "hello" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "ENDED");
    const env = h.query.options.env!;
    expect(env.ANTHROPIC_API_KEY).toBe("provider-key");
    expect(env.ANTHROPIC_BASE_URL).toBe("http://127.0.0.1:9/provider");
    for (const name of ["CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MODEL", "CLAUDE_CODE_USE_BEDROCK", "AWS_BEARER_TOKEN_BEDROCK",
      "CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_AUTO_MODE_MODEL"]) {
      expect(env, name).not.toHaveProperty(name);
    }
    expect(env[MANAGED_BY_HOST]).toBe("1");
    expect(env.PATH).toBe("/usr/bin:/bin");
    // A model in a settings file must not choose it: the CLI's default alias, mapped by the provider.
    expect(h.query.options.model).toBe("opus[1m]");
    h.runner.shutdown();
    await h.done;
  });

  test("a provider session with a configured model passes that model", async () => {
    const h = new Harness([text()], { deps: { env: inherited }, lease: (lease) => { lease.config!.model = "claude-test"; } });
    launches(h, [provider("k")]);
    h.link.addInput({ inputId: ID1, text: "hello" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "ENDED");
    expect(h.query.options.model).toBe("claude-test");
    h.runner.shutdown();
    await h.done;
  });

  test("a resumed session asks before its CLI starts", async () => {
    const h = new Harness([text()], { deps: { env: inherited }, lease: (lease) => { lease.hasTranscript = true; } });
    const queriesAt = launches(h, [provider("resume-key")]);
    h.link.addInput({ inputId: ID1, text: "hello" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "ENDED");
    expect(queriesAt).toEqual([0]);
    expect(h.query.options.resume).toBe(h.link.claudeSessionId);
    expect(h.query.options.env?.ANTHROPIC_API_KEY).toBe("resume-key");
    h.runner.shutdown();
    await h.done;
  });

  test("a replacement query asks again and gets the provider's current key, never the first one", async () => {
    const h = new Harness([authFailed(), text()], { deps: { env: inherited } });
    const queriesAt = launches(h, [provider("first-key"), provider("edited-key")]);
    h.link.addInput({ inputId: ID1, text: "one" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "failed turn");
    await waitFor(() => h.replay.instances[0].closed, "query closed");
    // Closing the query does not ask; only the next CLI start does.
    expect(queriesAt).toEqual([0]);
    h.link.addInput({ inputId: ID2, text: "two" });
    h.runner.nudge([Reason.INPUT], 0n);
    await waitFor(() => ended(h).length === 2, "next turn on a new query");
    expect(queriesAt).toEqual([0, 1]);
    expect(h.replay.instances[0].options.env?.ANTHROPIC_API_KEY).toBe("first-key");
    expect(h.replay.instances[1].options.env?.ANTHROPIC_API_KEY).toBe("edited-key");
    expect(h.replay.instances[1].options.resume).toBe(h.link.claudeSessionId);
    h.runner.shutdown();
    await h.done;
  });

  test("an input arriving while the replaced query's CLI is still exiting waits for that exit", async () => {
    const h = new Harness([authFailed(), text()], { deps: { env: inherited } });
    const queriesAt = launches(h, [provider("k1"), provider("k2")]);
    const exit = Promise.withResolvers<void>();
    let exiting = 0;
    h.processes.confirmExit = async () => {
      exiting++;
      if (exiting === 1) await exit.promise;
    };
    h.link.addInput({ inputId: ID1, text: "one" });
    await h.start();
    await waitFor(() => h.replay.instances[0]?.closed && exiting === 1, "the first CLI exiting");
    h.link.addInput({ inputId: ID2, text: "two" });
    h.runner.nudge([Reason.INPUT], 0n);
    await waitFor(() => h.link.ops("takeInputs").length >= 2, "the input taken");
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(queriesAt).toEqual([0]);
    expect(h.replay.instances).toHaveLength(1);
    exit.resolve();
    await waitFor(() => ended(h).length === 2, "next turn on a new query");
    expect(queriesAt).toEqual([0, 1]);
    h.runner.shutdown();
    await h.done;
  });

  test("PROVIDER_UNAVAILABLE: no CLI, release with PROVIDER", async () => {
    const h = new Harness([text()], { deps: { env: inherited } });
    h.link.interceptors.push((call) => call.op.case === "getLaunchEnv" ? rejected(Code.PROVIDER_UNAVAILABLE, "provider deleted") : undefined);
    h.link.addInput({ inputId: ID1, text: "hello" });
    await h.start();
    expect(await h.done).toBe("released");
    expect(h.replay.instances).toHaveLength(0);
    expect(h.link.released).toEqual([ReleaseLease_Reason.PROVIDER]);
    expect(h.link.persistedInputs.size).toBe(0);
    expect(h.log.lines.find((l) => l.message.includes("provider is unavailable"))?.fields).toMatchObject({ reason: "PROVIDER_UNAVAILABLE: provider deleted" });
  });

  test("PROVIDER_UNAVAILABLE for a replacement query closes the session the same way", async () => {
    const h = new Harness([authFailed(), text()], { deps: { env: inherited } });
    let calls = 0;
    h.link.interceptors.push((call) => {
      if (call.op.case !== "getLaunchEnv") return undefined;
      return ++calls === 1 ? launchReply(provider("k")) : rejected(Code.PROVIDER_UNAVAILABLE, "unassigned");
    });
    h.link.addInput({ inputId: ID1, text: "one" });
    await h.start();
    await waitFor(() => h.replay.instances[0]?.closed, "query closed");
    h.link.addInput({ inputId: ID2, text: "two" });
    h.runner.nudge([Reason.INPUT], 0n);
    expect(await h.done).toBe("released");
    expect(h.replay.instances).toHaveLength(1);
    expect(h.link.released).toEqual([ReleaseLease_Reason.PROVIDER]);
  });

  test("another rejection of GetLaunchEnv releases with ERROR, not PROVIDER", async () => {
    const h = new Harness([text()]);
    h.link.interceptors.push((call) => call.op.case === "getLaunchEnv" ? rejected(Code.INVALID, "bad") : undefined);
    h.link.addInput({ inputId: ID1, text: "hello" });
    await h.start();
    expect(await h.done).toBe("failed");
    expect(h.replay.instances).toHaveLength(0);
    expect(h.link.released).toEqual([ReleaseLease_Reason.ERROR]);
  });

  test.each(["stop", "lose"])("%s while GetLaunchEnv is in flight starts no CLI", async (how) => {
    const hold = Promise.withResolvers<void>();
    const h = new Harness([text()], { deps: { env: inherited } });
    let asked = 0;
    h.link.interceptors.push(async (call) => {
      if (call.op.case !== "getLaunchEnv") return undefined;
      asked++;
      await hold.promise;
      return launchReply(provider("late-key"));
    });
    h.link.addInput({ inputId: ID1, text: "hello" });
    await h.start();
    await waitFor(() => asked === 1, "GetLaunchEnv in flight");
    if (how === "stop") {
      h.runner.nudge([Reason.STOP], 0n);
      await waitFor(() => h.link.released.length === 1, "release");
    } else {
      h.runner.lose("revoked");
    }
    // The reply still arrives (a lost lease's call is aborted; the hold resolves anyway).
    hold.resolve();
    expect(await h.done).toBe(how === "stop" ? "released" : "lost");
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(h.replay.instances).toHaveLength(0);
    expect(h.link.released).toEqual(how === "stop" ? [ReleaseLease_Reason.STOPPED] : []);
  });

  test("a release waits for the exit of a CLI another path is closing", async () => {
    const h = new Harness([authFailed()]);
    const exit = Promise.withResolvers<void>();
    const order: string[] = [];
    let exits = 0;
    h.processes.confirmExit = async () => {
      const n = ++exits;
      if (n === 1) await exit.promise;
      order.push(`exit ${n} confirmed`);
    };
    h.link.interceptors.push((call) => {
      if (call.op.case === "releaseLease") order.push(`release ${ReleaseLease_Reason[call.op.value.reason]}`);
      return undefined;
    });
    h.link.addInput({ inputId: ID1, text: "one" });
    await h.start();
    // The failed login's query is closed; its CLI is slow to exit.
    await waitFor(() => exits === 1, "the CLI exiting");
    h.runner.nudge([Reason.STOP], 0n);
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(order).toEqual([]);
    exit.resolve();
    expect(await h.done).toBe("released");
    expect(order).toEqual(["exit 1 confirmed", "release STOPPED"]);
  });

  test("a CLI crash while an input is being taken ends the session with ERROR without another CLI", async () => {
    const crash = Promise.withResolvers<void>();
    const holdTake = Promise.withResolvers<void>();
    const h = new Harness([[{ t: "push", i: 0 }, started(0), init(), assistant(), { t: "call", fn: () => crash.promise }, { t: "throw", message: "CLI died" }], text()], {
      deps: { env: inherited },
    });
    let takes = 0;
    let launchesAfterCrash = 0;
    let crashed = false;
    h.link.interceptors.push(async (call) => {
      if (call.op.case === "takeInputs" && ++takes === 2) await holdTake.promise;
      if (call.op.case === "getLaunchEnv" && crashed) launchesAfterCrash++;
      // A network round trip for the crashed turn's ENDED.
      if (call.op.case === "reportTurn" && call.op.value.phase === ReportTurn_Phase.ENDED) await new Promise((r) => setTimeout(r, 50));
      return undefined;
    });
    launches(h, [provider("k1"), provider("k2")]);
    h.link.addInput({ inputId: ID1, text: "one" });
    await h.start();
    await waitFor(() => h.link.reports.length === 1, "turn started");
    h.link.addInput({ inputId: ID2, text: "two" });
    h.runner.nudge([Reason.INPUT], 0n);
    await waitFor(() => takes === 2, "second TakeInputs in flight");
    crashed = true;
    crash.resolve();
    await new Promise((resolve) => setTimeout(resolve, 5));
    holdTake.resolve();
    expect(await h.done).toBe("failed");
    expect(h.replay.instances).toHaveLength(1);
    expect(launchesAfterCrash).toBe(0);
    expect(h.link.released).toEqual([ReleaseLease_Reason.ERROR]);
    expect(h.link.persistedInputs.has(ID2)).toBe(false);
  });

  test("no CLI starts while a transcript repair is running; the input goes to a new CLI afterwards", async () => {
    const h = new Harness([text(), text()]);
    const stateHeld = Promise.withResolvers<void>();
    const repairHeld = Promise.withResolvers<void>();
    let stateAsked = 0;
    let repairing = false;
    let launchesDuringRepair = 0;
    h.link.interceptors.push(async (call) => {
      if (call.op.case === "getState") {
        stateAsked++;
        await stateHeld.promise;
      }
      if (call.op.case === "getLaunchEnv" && repairing) launchesDuringRepair++;
      if (call.op.case === "transcriptRepaired") {
        repairing = true;
        await repairHeld.promise;
      }
      return undefined;
    });
    h.link.addInput({ inputId: ID1, text: "one" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "first turn");
    // An approval input refreshes the grants; meanwhile the idle CLI reports a failed append.
    h.link.addInput({ inputId: ID2, text: "approved", kind: InputKind.APPROVAL });
    h.runner.nudge([Reason.APPROVAL], 0n);
    await waitFor(() => stateAsked === 1, "GetSessionState in flight");
    h.query.emit({
      type: "system", subtype: "mirror_error", error: "append failed", key: { projectKey: "p", sessionId: h.link.claudeSessionId },
      uuid: crypto.randomUUID(), session_id: h.link.claudeSessionId,
    });
    await waitFor(() => repairing && h.query.closed, "repair running, query closed");
    stateHeld.resolve();
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(h.replay.instances).toHaveLength(1);
    expect(launchesDuringRepair).toBe(0);
    repairing = false;
    repairHeld.resolve();
    await waitFor(() => ended(h).length === 2, "the input's turn on a new CLI");
    expect(h.replay.instances).toHaveLength(2);
    expect(h.replay.instances[1].pushed[0].uuid).toBe(ID2);
    expect(h.link.repaired).toBe(1);
    h.runner.shutdown();
    expect(await h.done).toBe("released");
    expect(h.link.released).toEqual([ReleaseLease_Reason.IDLE]);
  });

  test("Fenced: the lease is dropped without a CLI or a release", async () => {
    const h = new Harness([text()]);
    h.link.interceptors.push((call) => call.op.case === "getLaunchEnv" ? fenced(9n) : undefined);
    h.link.addInput({ inputId: ID1, text: "hello" });
    await h.start();
    expect(await h.done).toBe("lost");
    expect(h.replay.instances).toHaveLength(0);
    expect(h.link.released).toEqual([]);
  });

  test("STOPPED: no CLI, release with STOPPED", async () => {
    const h = new Harness([text()]);
    h.link.interceptors.push((call) => call.op.case === "getLaunchEnv" ? rejected(Code.STOPPED) : undefined);
    h.link.addInput({ inputId: ID1, text: "hello" });
    await h.start();
    expect(await h.done).toBe("released");
    expect(h.replay.instances).toHaveLength(0);
    expect(h.link.released).toEqual([ReleaseLease_Reason.STOPPED]);
  });

  test("a lost link is retried after readiness, then the CLI starts", async () => {
    let readyWaits = 0;
    const h = new Harness([text()], { deps: { env: inherited, ready: async () => { readyWaits++; } } });
    let attempts = 0;
    h.link.interceptors.push((call) => {
      if (call.op.case !== "getLaunchEnv") return undefined;
      if (++attempts === 1) throw new LinkLostError("link lost");
      return launchReply(provider("after-reconnect"));
    });
    h.link.addInput({ inputId: ID1, text: "hello" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "ENDED");
    expect(attempts).toBe(2);
    expect(readyWaits).toBe(1);
    expect(h.query.options.env?.ANTHROPIC_API_KEY).toBe("after-reconnect");
    h.runner.shutdown();
    await h.done;
  });
});

describe("launch environment secrets", () => {
  const dirs: string[] = [];
  afterEach(() => {
    vi.restoreAllMocks();
    for (const dir of dirs.splice(0)) rmSync(dir, { recursive: true, force: true });
  });

  test("no value of the launch environment reaches the debug log, the control plane or the process records", async () => {
    const secrets = ["sk-ant-api03-SECRET-PROVIDER-KEY-0001", "SECRET-CUSTOM-VALUE-0002", "SECRET-RESERVED-VALUE-0003"];
    const stderr: string[] = [];
    vi.spyOn(process.stderr, "write").mockImplementation((chunk: string | Uint8Array) => {
      stderr.push(String(chunk));
      return true;
    });
    const log = createLogger("debug");
    const state = mkdtempSync(join(tmpdir(), "circus-launch-state-"));
    const out = mkdtempSync(join(tmpdir(), "circus-launch-out-"));
    dirs.push(state, out);
    const processes = new CliProcesses(state, undefined, log, process.platform === "linux", 50);
    // The replayed query also spawns a stand-in CLI through the worker's
    // spawner with the environment the SDK would pass, and exits with an
    // error the worker logs (with its stderr tail).
    // Each replayed query goes on only once its stand-in has exited by
    // itself (and its stderr was read), so the exit grace never kills it.
    const spawned: string[] = [];
    const standins = [Promise.withResolvers<void>(), Promise.withResolvers<void>()];
    const afterStandin = (i: number, steps: Step[]): Step[] => [{ t: "call", fn: () => standins[i].promise }, ...steps];
    let factory!: QueryFactory;
    const h = new Harness([afterStandin(0, authFailed()), afterStandin(1, text())], {
      deps: { env: inherited, log, processes, query: (params) => factory(params) },
    });
    factory = (params) => {
      const query = h.replay.factory(params);
      const child = params.options.spawnClaudeCodeProcess!({
        command: process.execPath,
        args: ["-e", `require("fs").writeFileSync(${JSON.stringify(join(out, `env-${spawned.length}`))}, process.env.ANTHROPIC_API_KEY ?? ""); ` +
          "console.error('stand-in failing'); process.exit(3)"],
        cwd: params.options.cwd, env: params.options.env ?? {}, signal: new AbortController().signal,
      });
      (child as unknown as ChildProcess).once("close", standins[spawned.length].resolve);
      spawned.push(params.options.env?.ANTHROPIC_API_KEY ?? "");
      return query;
    };
    launches(h, [
      provider(secrets[0], [{ name: "MY_CUSTOM", value: secrets[1] }, { name: "PATH", value: secrets[2] }]),
      provider(secrets[0], [{ name: "MY_CUSTOM", value: secrets[1] }]),
    ]);
    h.link.addInput({ inputId: ID1, text: "one" });
    await h.start();
    await waitFor(() => h.replay.instances[0]?.closed, "query closed");
    h.link.addInput({ inputId: ID2, text: "two" });
    h.runner.nudge([Reason.INPUT], 0n);
    await waitFor(() => ended(h).length === 2, "second query");
    h.runner.shutdown();
    await h.done;
    await waitFor(() => stderr.join("").split("stand-in failing").length === 3, "both CLI exits logged with their stderr");
    // The stand-in saw the provider's key: the environment was applied.
    expect(readFileSync(join(out, "env-0"), "utf8")).toBe(secrets[0]);
    expect(spawned).toHaveLength(2);
    const logged = stderr.join("");
    expect(logged).toContain("starting Claude Code");
    expect(logged).toContain("launch environment variables not applied");
    const sent = JSON.stringify(h.link.calls.filter((c) => c.op.case !== "getLaunchEnv"), (_k, v: unknown) => typeof v === "bigint" ? String(v) : v);
    const records = readdirSync(state).map((name) => readFileSync(join(state, name), "utf8")).join("\n");
    expect(records).toContain(h.link.sessionId);
    for (const secret of secrets) {
      expect(logged).not.toContain(secret);
      expect(sent).not.toContain(secret);
      expect(records).not.toContain(secret);
    }
  });
});
