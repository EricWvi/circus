import { createHash } from "node:crypto";
import { readdir, readFile, writeFile } from "node:fs/promises";
import { basename, join } from "node:path";
import { pathToFileURL } from "node:url";

// Derive metadata from the finished archives so the updater verifies what was published.
export async function generateManifest(directory, repository, version) {
  if (!/^[\w.-]+\/[\w.-]+$/.test(repository) || !/^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$/.test(version)) {
    throw new Error("Invalid release repository or version");
  }
  const prefix = `circus-${version}-`;
  const extension = ".tar.gz";
  const assets = [];
  for (const name of (await readdir(directory)).sort()) {
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
  if (assets.length === 0) throw new Error("No circus release archives found");
  const manifest = { version, assets };
  await writeFile(join(directory, "latest.json"), `${JSON.stringify(manifest, null, 2)}\n`);
  return manifest;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const [, script, directory, repository, version, ...extra] = process.argv;
  if (!directory || !repository || !version || extra.length) {
    throw new Error(`Usage: node ${basename(script)} <directory> <owner/repo> <tag>`);
  }
  await generateManifest(directory, repository, version);
}
