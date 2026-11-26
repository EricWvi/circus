// The groups the nightly race runs of the e2e suite are split into.
//
// The whole suite under the race detector outgrows go test's 10-minute
// timeout, so the nightly workflow runs `task e2e-race` once per group with
// E2E_RUN set to the group's -run pattern. Every top-level test of
// tests/e2e, as `go test -race -tags e2e -list` finds them on Linux,
// belongs to exactly one group; `--check` fails otherwise, and when the
// workflow's matrix does not give every group its three runs, so a new
// test cannot silently drop out of the nightly runs. It needs Go and runs
// on Linux.
//
// Usage (from the repository root):
//
//   node scripts/e2e-race-groups.mjs <group>   print the group's -run pattern
//   node scripts/e2e-race-groups.mjs --check   check the groups
import { execFile } from "node:child_process";
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { promisify } from "node:util";

const run = promisify(execFile);

// Each group's tests take under 4 minutes under the race detector on a
// 12-CPU developer machine (sessions 229 s, providers 227 s, calls 212 s of
// 667 s in all, measured 2026-11-27), so with a runner twice as slow every
// group's run stays well under go test's 10-minute timeout. A new test goes
// into the group with the most room.
export const groups = {
  sessions: [
    "TestClaudeApproval",
    "TestClaudeBackgroundTask",
    "TestClaudeControlPlaneRestart",
    "TestClaudeIdleRelease",
    "TestClaudeInterruptAndStop",
    "TestClaudeMergeAndNow",
    "TestClaudeMissingDirectory",
    "TestClaudeTextAndTools",
    "TestClaudeWorkerKilled",
    "TestClaudeWorkerKilledDuringBash",
    "TestFakeWorkerSessions",
    "TestSessionLedgerSnapshots",
  ],
  providers: [
    "TestClaudeAskModes",
    "TestClaudeAskTwoQuestions",
    "TestClaudeAskUserQuestion",
    "TestClaudeProviderConflictingConfig",
    "TestClaudeProviderFullAfterRestart",
    "TestClaudeProviderResume",
    "TestClaudeProviderSchemes",
    "TestClaudeProviderUnassigned",
    "TestProviderChangesAndLeases",
    "TestProviderFullReleasesIdleLease",
    "TestProviderScheduling",
  ],
  calls: [
    "TestCLIExitCodes",
    "TestCLITransportFailureIsUnknown",
    "TestCallValidationAndNotFound",
    "TestCancelErrorDoesNotBlockLaterCancel",
    "TestCancelReplies",
    "TestClientCancelWatchedCall",
    "TestDaemonCrashLeavesCallUnknown",
    "TestGracefulShutdown",
    "TestHappyPath",
    "TestLedgerChangesets",
    "TestListDirectoryOnDaemon",
    "TestLocalMode",
    "TestRevocationEndsDaemon",
    "TestRunDirectorySweepUsesFlock",
    "TestServerCrashAfterDispatchCommitLosesCall",
    "TestServerCrashLosesAcceptedCallThenResubmits",
    "TestServerCrashWithRunningToolRecoversResult",
    "TestSessionTurn",
    "TestTypeScriptWorkerLink",
    "TestUnwatchFreesSlotImmediately",
    "TestWorkerCloseSlotsAndCancelOrdering",
    "TestWorkerDisconnected",
    "TestWorkerPackage",
    "TestWorkerRestartWhileHalfOpen",
    "TestWorkspacePreparationWithFakeDaemon",
    "TestWorkspaceReattachAfterControlPlaneRestart",
    "TestWorkspaceRetryAfterDaemonRestart",
  ],
};

/** The -run pattern of a group: its tests' names, anchored. */
export function runPattern(names) {
  return `^(${names.join("|")})$`;
}

/**
 * The top-level tests of tests/e2e in the build the nightly runs them from
 * (Linux, -race, -tags e2e), as go test itself lists them: build
 * constraints, file names and signatures are Go's to decide.
 */
export async function listTests(root, pkg = "./tests/e2e/") {
  if (process.platform !== "linux") {
    throw new Error("the nightly runs the e2e suite on Linux; list its tests on Linux");
  }
  const { stdout } = await run("go", ["test", "-race", "-tags", "e2e", "-list", ".", pkg], {
    cwd: root, maxBuffer: 16 << 20, env: { ...process.env, GOOS: "linux", GOFLAGS: "" },
  });
  // Fuzz targets run their seed corpus as tests, and examples with an
  // output comment run too.
  return stdout.split("\n").filter((line) => /^(Test|Fuzz|Example)/u.test(line));
}

/**
 * How many race runs each group gets from the nightly workflow's e2e-race
 * job: its matrix expanded. Only the shape the workflow uses is understood
 * (`matrix:` with `group:` and `run:` as flow lists, besides `fail-fast`
 * and `max-parallel`); anything else, include and exclude among them, is a
 * problem to extend this check for, never a guess.
 */
export function matrixRuns(workflow) {
  const all = workflow.split("\n");
  // A line that is neither blank nor only a comment.
  const content = (line) => !/^\s*(#.*)?$/.test(line);
  const head = all.findIndex((line) => /^ {2}e2e-race:\s*$/.test(line));
  if (head < 0) return { problems: ["the nightly workflow has no e2e-race job"] };
  // The job ends at the next key indented two spaces or less; a comment
  // ends nothing in YAML, so it does not end the job either.
  let end = all.findIndex((line, i) => i > head && content(line) && !/^ {3}/.test(line));
  if (end < 0) end = all.length;
  const job = all.slice(head + 1, end);
  const start = job.findIndex((line) => /^ {4}strategy:\s*$/.test(line));
  if (start < 0) return { problems: ["the e2e-race job has no strategy"] };
  const found = [];
  const lists = {};
  let inMatrix = false;
  for (const line of job.slice(start + 1)) {
    if (/^\s*$/.test(line)) continue;
    if (content(line) && !/^ {5}/.test(line)) break;
    let m;
    if (!content(line)) {
      found.push(`the e2e-race strategy has a comment, which this check does not read: ${JSON.stringify(line.trim())}`);
    } else if (/^ {6}(fail-fast|max-parallel):\s*[\w.-]+\s*$/.test(line)) {
      inMatrix = false;
    } else if (/^ {6}matrix:\s*$/.test(line)) {
      inMatrix = true;
    } else if (inMatrix && (m = line.match(/^ {8}(group|run):\s*\[([\w.\s,-]*)\]\s*$/)) && !Object.hasOwn(lists, m[1])) {
      lists[m[1]] = m[2].split(",").map((value) => value.trim());
      if (lists[m[1]].some((value) => value === "")) found.push(`the e2e-race matrix's ${m[1]} list has an empty entry`);
    } else {
      found.push(`the e2e-race strategy has a line this check does not understand: ${JSON.stringify(line.trim())}`);
    }
  }
  const keys = Object.keys(lists).sort();
  if (keys.join(",") !== "group,run") {
    found.push(`the e2e-race matrix has the keys [${keys.join(", ")}], want exactly group and run`);
  }
  if (found.length > 0) return { problems: found };
  const runs = new Map();
  for (const group of lists.group) runs.set(group, (runs.get(group) ?? 0) + new Set(lists.run).size);
  for (const group of new Set(lists.group)) {
    if (lists.group.filter((g) => g === group).length > 1) found.push(`the e2e-race matrix lists group ${group} more than once`);
  }
  if (new Set(lists.run).size !== lists.run.length) found.push("the e2e-race matrix lists a run more than once");
  return { runs, problems: found };
}

/** The race runs every group gets each night. */
export const RUNS = 3;

/**
 * The problems with groups for tests and the workflow's matrix: a test that
 * no group's pattern or more than one matches, a group name that names no
 * test, a group without tests, and a matrix that does not give every group,
 * and nothing else, RUNS runs. The patterns are matched as go test -run
 * matches a top-level test: an unanchored search, which is the same in
 * JavaScript for these patterns.
 */
export function problems(tests, groups, matrix) {
  const found = [];
  const patterns = Object.entries(groups).map(([group, names]) => [group, new RegExp(runPattern(names), "u")]);
  for (const test of tests) {
    const matching = patterns.filter(([, pattern]) => pattern.test(test)).map(([group]) => group);
    if (matching.length === 0) found.push(`${test} is in no group: add it to one in scripts/e2e-race-groups.mjs`);
    if (matching.length > 1) found.push(`${test} is in more than one group: ${matching.join(", ")}`);
  }
  for (const [group, names] of Object.entries(groups)) {
    if (names.length === 0) found.push(`group ${group} has no tests`);
    for (const name of names) {
      if (!/^(Test|Fuzz|Example)[\p{L}\p{N}_]*$/u.test(name)) found.push(`group ${group} lists ${JSON.stringify(name)}, which is not a test name`);
      else if (!tests.includes(name)) found.push(`group ${group} lists ${name}, which is not a top-level test of tests/e2e`);
    }
  }
  found.push(...matrix.problems);
  if (matrix.runs) {
    for (const group of Object.keys(groups)) {
      const n = matrix.runs.get(group) ?? 0;
      if (n !== RUNS) found.push(`the e2e-race matrix runs group ${group} ${n} times, want ${RUNS}`);
    }
    for (const group of matrix.runs.keys()) {
      if (!Object.hasOwn(groups, group)) found.push(`the e2e-race matrix runs ${group}, which is no group`);
    }
  }
  return found;
}

/** Checks the repository at root and returns its problems. */
export async function check(root) {
  const tests = await listTests(root);
  const workflow = await readFile(join(root, ".github", "workflows", "nightly.yml"), "utf8");
  return problems(tests, groups, matrixRuns(workflow));
}

async function main(args) {
  if (args.length !== 1) {
    console.error("usage: node scripts/e2e-race-groups.mjs <group> | --check");
    return 2;
  }
  if (args[0] === "--check") {
    const found = await check(process.cwd());
    for (const problem of found) console.error(problem);
    return found.length === 0 ? 0 : 1;
  }
  if (!Object.hasOwn(groups, args[0])) {
    console.error(`unknown group ${JSON.stringify(args[0])}; the groups are ${Object.keys(groups).join(", ")}`);
    return 2;
  }
  console.log(runPattern(groups[args[0]]));
  return 0;
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) process.exitCode = await main(process.argv.slice(2));
