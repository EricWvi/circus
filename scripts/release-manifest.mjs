import { createHash } from "node:crypto";
import { readdir, readFile, writeFile } from "node:fs/promises";
import { basename, join } from "node:path";
import { pathToFileURL } from "node:url";

// Derive metadata from the finished archives so the updater verifies what was published.
// The CLI's archives (circus-<tag>-<target>.tar.gz) go in `assets`, the one
// list `circus update` reads (one entry per target). The worker's release
// packages (circus-worker-<tag>-<target>.tar.gz) go in `worker_assets`, with
// entries of the same shape, so the CLI's target lookup never sees them.
export async function generateManifest(directory, repository, version) {
  if (!/^[\w.-]+\/[\w.-]+$/.test(repository) || !/^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$/.test(version)) {
    throw new Error("Invalid release repository or version");
  }
  const names = (await readdir(directory)).sort();
  const assets = await collect(directory, names, repository, version, "circus");
  if (assets.length === 0) throw new Error("No circus release archives found");
  const workerAssets = await collect(directory, names, repository, version, "circus-worker");
  if (workerAssets.length === 0) throw new Error("No circus-worker release archives found");
  const manifest = { version, assets, worker_assets: workerAssets };
  await writeFile(join(directory, "latest.json"), `${JSON.stringify(manifest, null, 2)}\n`);
  return manifest;
}

// collect lists the <product>-<version>-<target>.tar.gz archives among names.
async function collect(directory, names, repository, version, product) {
  const prefix = `${product}-${version}-`;
  const extension = ".tar.gz";
  const assets = [];
  for (const name of names) {
    if (!name.startsWith(prefix) || !name.endsWith(extension)) continue;
    const target = name.slice(prefix.length, -extension.length);
    if (!/^(linux|darwin)-(amd64|arm64)$/.test(target)) {
      throw new Error(`Invalid release target: ${target}`);
    }
    const bytes = await readFile(join(directory, name));
    if (bytes.length === 0) throw new Error(`Empty release archive: ${name}`);
    assets.push({
      target,
      name,
      url: `https://github.com/${repository}/releases/download/${version}/${name}`,
      sha256: createHash("sha256").update(bytes).digest("hex"),
    });
  }
  return assets;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const [, script, directory, repository, version, ...extra] = process.argv;
  if (!directory || !repository || !version || extra.length) {
    throw new Error(`Usage: node ${basename(script)} <directory> <owner/repo> <tag>`);
  }
  await generateManifest(directory, repository, version);
}
