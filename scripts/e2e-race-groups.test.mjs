import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import test from "node:test";
import { promisify } from "node:util";
import { check, groups, listTests, matrixRuns, problems, runPattern } from "./e2e-race-groups.mjs";

const run = promisify(execFile);
const linux = process.platform === "linux";

test("every e2e test of the nightly build is in exactly one group, and the nightly runs every group three times", { skip: !linux && "Linux only" }, async () => {
  assert.deepEqual(await check(process.cwd()), []);
});

test("lists the tests go test runs in the nightly's build: Linux, -race, -tags e2e", { skip: !linux && "Linux only" }, async (t) => {
  await mkdir(".tmp", { recursive: true });
  const dir = await mkdtemp(".tmp/e2e-race-groups-");
  t.after(() => rm(dir, { recursive: true, force: true }));
  await writeFile(join(dir, "go.mod"), "module example.com/groups\n\ngo 1.24\n");
  const file = (name, constraint, body) => writeFile(join(dir, name), `//go:build ${constraint}\n\npackage groups\n\nimport "testing"\n\n${body}\n`);
  await file("a_test.go", "e2e", [
    "func TestMain(m *testing.M) { m.Run() }",
    "func TestReviewUnnamed(*testing.T) {}",
    "func TestÉtage(t *testing.T) { t.Run(\"sub\", func(t *testing.T) {}) }",
    "func helper(t *testing.T) {}",
    "func BenchmarkX(b *testing.B) {}",
    "// func TestCommented(t *testing.T) {}",
  ].join("\n"));
  await file("b_darwin_test.go", "e2e", "func TestDarwinOnly(t *testing.T) {}");
  await file("c_test.go", "e2e && !race", "func TestWithoutRace(t *testing.T) {}");
  await file("d_test.go", "!e2e", "func TestWithoutTag(t *testing.T) {}");
  await writeFile(join(dir, "e_test.go"), '//go:build e2e\n\npackage groups\n\nimport "fmt"\n\nfunc Example() {\n\tfmt.Println("x")\n\t// Output: x\n}\n');
  assert.deepEqual((await listTests(dir, "./")).sort(), ["Example", "TestReviewUnnamed", "TestÉtage"].sort());
});

const nightly = (matrix) => [
  "jobs:",
  "  other:",
  "    strategy:",
  "      matrix:",
  "        group: [x]",
  "",
  "  e2e-race:",
  "    runs-on: ubuntu-latest",
  "    strategy:",
  "      fail-fast: false",
  "      matrix:",
  ...matrix,
  "    steps:",
  "      - run: true",
  "  later:",
  "    group: [y]",
].join("\n") + "\n";

test("expands the e2e-race job's matrix, and refuses shapes it does not understand", () => {
  const ok = matrixRuns(nightly(["        group: [one, two ,three]", "        run: [1, 2, 3]"]));
  assert.deepEqual(ok.problems, []);
  assert.deepEqual([...ok.runs], [["one", 3], ["two", 3], ["three", 3]]);
  assert.deepEqual([...matrixRuns(nightly(["        group: [one]", "        run: [1]"])).runs], [["one", 1]]);
  for (const [extra, problem] of [
    [["        exclude:", "          - group: one"], /does not understand: "exclude:"/],
    [["        include:", "          - group: four"], /does not understand: "include:"/],
    [["        group:", "          - one"], /does not understand/],
    [["        other: [a]"], /does not understand: "other: \[a\]"/],
    [["        group: [two]"], /does not understand: "group: \[two\]"/],
    [["        run: [1, {x: 2}]"], /does not understand/],
    [["        run: [1, 2] # three"], /does not understand/],
    // A comment ends nothing in YAML: an exclude after one at any indent
    // is still the matrix's.
    [["  # a comment", "        exclude: [{group: one}]"], /a comment[\s\S]*does not understand: "exclude: \[\{group: one\}\]"/],
    [["# a comment"], /has a comment/],
    [["        # a comment"], /has a comment/],
  ]) {
    const r = matrixRuns(nightly(["        group: [one]", "        run: [1, 2, 3]", ...extra]));
    assert.equal(r.runs, undefined);
    assert.match(r.problems.join("\n"), problem);
  }
  assert.match(matrixRuns(nightly(["        group: [one, one]", "        run: [1, 2, 3]"])).problems.join("\n"), /group one more than once/);
  assert.match(matrixRuns("jobs:\n  e2e-race:\n    runs-on: x\n").problems.join("\n"), /no strategy/);
  assert.match(matrixRuns("jobs:\n  other:\n    runs-on: x\n").problems.join("\n"), /no e2e-race job/);
});

test("reports a test in no group or in several, unknown names, empty groups and groups not run three times", () => {
  const tests = ["TestA", "TestB", "TestAB", "TestC"];
  const ok = { one: ["TestA", "TestAB"], two: ["TestB", "TestC"] };
  const runs = (entries) => ({ runs: new Map(entries), problems: [] });
  const three = runs([["one", 3], ["two", 3]]);
  assert.deepEqual(problems(tests, ok, three), []);
  // Anchored: TestA's pattern does not take TestAB.
  assert.deepEqual(problems(tests, { one: ["TestA"], two: ["TestB", "TestC"] }, three),
    ["TestAB is in no group: add it to one in scripts/e2e-race-groups.mjs"]);
  assert.deepEqual(problems(tests, { one: ["TestA", "TestAB"], two: ["TestB", "TestC", "TestA"] }, three),
    ["TestA is in more than one group: one, two"]);
  assert.deepEqual(problems(tests, { ...ok, three: ["TestGone"] }, runs([["one", 3], ["two", 3], ["three", 3]])),
    ["group three lists TestGone, which is not a top-level test of tests/e2e"]);
  assert.deepEqual(problems(tests, { ...ok, three: [] }, runs([["one", 3], ["two", 3], ["three", 3]])), ["group three has no tests"]);
  assert.deepEqual(problems(["TestÉtage", "Example"], { one: ["TestÉtage", "Example"] }, runs([["one", 3]])), []);
  assert.deepEqual(problems(tests, ok, runs([["one", 3]])), ["the e2e-race matrix runs group two 0 times, want 3"]);
  assert.deepEqual(problems(tests, ok, runs([["one", 1], ["two", 3]])), ["the e2e-race matrix runs group one 1 times, want 3"]);
  assert.deepEqual(problems(tests, ok, runs([["one", 3], ["two", 3], ["four", 3]])), ["the e2e-race matrix runs four, which is no group"]);
  assert.deepEqual(problems(tests, ok, { problems: ["bad matrix"] }), ["bad matrix"]);
});

test("prints a group's pattern, and refuses an unknown group", async () => {
  const [group, names] = Object.entries(groups)[0];
  const { stdout } = await run(process.execPath, ["scripts/e2e-race-groups.mjs", group]);
  assert.equal(stdout, `${runPattern(names)}\n`);
  assert.match(runPattern(["TestA", "TestB"]), /^\^\(TestA\|TestB\)\$$/);
  await assert.rejects(run(process.execPath, ["scripts/e2e-race-groups.mjs", "nope"]), (error) => {
    assert.equal(error.code, 2);
    assert.match(error.stderr, /unknown group "nope"/);
    return true;
  });
});

test("--check exits 0 on the repository", { skip: !linux && "Linux only" }, async () => {
  const { stderr } = await run(process.execPath, ["scripts/e2e-race-groups.mjs", "--check"], { maxBuffer: 16 << 20 });
  assert.equal(stderr, "");
});
