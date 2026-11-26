// Builds the worker's release packages.
//
// Usage (from anywhere, after `npm ci` in the repository root):
//
//   node worker/scripts/package.mjs <tag> <out-dir> <target>...
//
// <target> is linux-amd64, linux-arm64, darwin-amd64 or darwin-arm64. The
// worker is built once (npm -w @circus/worker run build); then, per target,
// <out-dir>/circus-worker-<tag>-<target>.tar.gz is written and its path
// printed on stdout. Each archive holds one directory, circus-worker/:
//
//   circus-worker/circus-worker   the launcher: finds Node.js 24 or later
//                                 ($CIRCUS_NODE, else node on PATH) and runs
//                                 the worker with circus from CIRCUS_BIN or PATH
//   circus-worker/package.json    the worker's, its version set to <tag>
//   circus-worker/dist/           the built worker
//   circus-worker/node_modules/   its production dependencies, copied from the
//                                 repository's node_modules as package-lock.json
//                                 lays them out, and the built @circus/proto
//
// No @anthropic-ai/claude-agent-sdk-* platform package is included: the
// worker installs Claude Code itself and starts it by path. The other
// dependencies are plain JavaScript (a package that names an os or cpu
// fails the packaging), so one machine packages every target.
import { execFileSync } from "node:child_process";
import { chmodSync, cpSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const TARGETS = new Set(["linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"]);
const PLATFORM_PACKAGE = /^@anthropic-ai\/claude-agent-sdk-/;

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");

function fail(message) {
  console.error(`package: ${message}`);
  process.exit(1);
}

const [tag, outArg, ...targets] = process.argv.slice(2);
if (!tag || !outArg || targets.length === 0) fail("usage: package.mjs <tag> <out-dir> <target>...");
if (!/^v?[0-9A-Za-z][0-9A-Za-z.+-]*$/.test(tag)) fail(`invalid tag ${JSON.stringify(tag)}`);
for (const target of targets) if (!TARGETS.has(target)) fail(`unknown target ${JSON.stringify(target)}`);
const out = resolve(outArg);

const lock = JSON.parse(readFileSync(join(root, "package-lock.json"), "utf8")).packages;

/** The lockfile location a dependency of the package at `from` resolves to, as Node resolves it. */
function locate(from, name) {
  for (let base = from; ; ) {
    const candidate = (base ? base + "/" : "") + "node_modules/" + name;
    if (lock[candidate]) return candidate;
    if (!base) return undefined;
    const cut = base.lastIndexOf("/node_modules/");
    base = cut >= 0 ? base.slice(0, cut) : base.startsWith("node_modules/") ? "" : base.includes("/") ? base.slice(0, base.lastIndexOf("/")) : "";
  }
}

/** The production closure of the worker: lockfile location → its package entry. */
function closure() {
  const found = new Map();
  const queue = [["worker", lock.worker.dependencies]];
  while (queue.length) {
    const [from, deps] = queue.shift();
    for (const name of Object.keys(deps ?? {})) {
      if (PLATFORM_PACKAGE.test(name)) continue;
      let location = locate(from, name);
      if (!location) fail(`${name} (needed by ${from}) is not in package-lock.json`);
      let entry = lock[location];
      if (entry.link) {
        location = entry.resolved;
        entry = lock[location];
      }
      if (found.has(location)) continue;
      if (entry.os || entry.cpu) fail(`${name} is platform specific (${location})`);
      found.set(location, entry);
      const required = Object.fromEntries(Object.entries(entry.peerDependencies ?? {})
        .filter(([peer]) => !entry.peerDependenciesMeta?.[peer]?.optional));
      queue.push([location, { ...entry.dependencies, ...required }]);
      // Optional dependencies only where npm installed them for every platform.
      const optional = Object.keys(entry.optionalDependencies ?? {}).filter((dep) => !PLATFORM_PACKAGE.test(dep));
      for (const dep of optional) {
        const at = locate(location, dep);
        if (at && !lock[at].os && !lock[at].cpu) queue.push([location, { [dep]: "*" }]);
      }
    }
  }
  return found;
}

execFileSync("npm", ["-w", "@circus/worker", "run", "build"], { cwd: root, stdio: ["ignore", "ignore", "inherit"] });

const packages = closure();
const worker = JSON.parse(readFileSync(join(root, "worker", "package.json"), "utf8"));
const launcher = readFileSync(join(root, "worker", "scripts", "circus-worker.sh"), "utf8");
mkdirSync(out, { recursive: true });
for (const target of targets) {
  const stage = join(root, ".tmp", "worker-package", target);
  rmSync(stage, { recursive: true, force: true });
  const top = join(stage, "circus-worker");
  mkdirSync(top, { recursive: true });
  cpSync(join(root, "worker", "dist"), join(top, "dist"), { recursive: true });
  const { devDependencies: _dev, scripts: _scripts, ...manifest } = worker;
  writeFileSync(join(top, "package.json"), JSON.stringify({ ...manifest, version: tag }, null, 2) + "\n");
  writeFileSync(join(top, "circus-worker"), launcher);
  chmodSync(join(top, "circus-worker"), 0o755);
  for (const [location, entry] of packages) {
    if (location === "gen/ts") {
      // The workspace's proto package: its manifest and build output.
      const to = join(top, "node_modules", "@circus", "proto");
      mkdirSync(to, { recursive: true });
      cpSync(join(root, "gen", "ts", "package.json"), join(to, "package.json"));
      cpSync(join(root, "gen", "ts", "dist"), join(to, "dist"), { recursive: true });
      continue;
    }
    const from = join(root, location);
    if (!existsSync(join(from, "package.json"))) fail(`${location} is not installed; run npm ci`);
    const installed = JSON.parse(readFileSync(join(from, "package.json"), "utf8")).version;
    if (installed !== entry.version) fail(`${location} is ${installed}, package-lock.json says ${entry.version}; run npm ci`);
    // Nested node_modules are entries of their own.
    cpSync(from, join(top, location), {
      recursive: true,
      filter: (path) => !path.slice(from.length).split("/").includes("node_modules"),
    });
  }
  const archive = join(out, `circus-worker-${tag}-${target}.tar.gz`);
  rmSync(archive, { force: true });
  execFileSync("tar", ["-czf", archive, "-C", stage, "circus-worker"], { stdio: ["ignore", "ignore", "inherit"] });
  rmSync(stage, { recursive: true, force: true });
  console.log(archive);
}
