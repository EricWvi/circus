import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import test from "node:test";
import { generateManifest } from "./release-manifest.mjs";

test("records the archives with version-pinned URLs and SHA-256 checksums", async (t) => {
  await mkdir(".tmp", { recursive: true });
  const directory = await mkdtemp(".tmp/release-manifest-");
  t.after(() => rm(directory, { recursive: true, force: true }));
  const repository = "EricWvi/circus";
  const version = "v1.2.3";
  for (const invalid of ["v1.2", "v-x", "v1.2.3+build"]) {
    await assert.rejects(generateManifest(directory, repository, invalid), /Invalid release repository or version/);
  }
  await assert.rejects(generateManifest(directory, repository, "v1.2.3-rc.1"), /No circus release archives/);
  await assert.rejects(generateManifest(directory, repository, version), /No circus release archives/);
  const assets = [];
  for (const target of ["darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64"]) {
    const name = `circus-${version}-${target}.tar.gz`;
    const bytes = Buffer.from(`${target} archive`);
    await writeFile(join(directory, name), bytes);
    assets.push({
      target,
      name,
      url: `https://github.com/${repository}/releases/download/${version}/${name}`,
      sha256: createHash("sha256").update(bytes).digest("hex"),
    });
  }
  await writeFile(join(directory, "circus-v0.1.0-linux-amd64.tar.gz"), "old release");
  await writeFile(join(directory, "SHA256SUMS"), "ignored");
  const expected = { version, assets };
  assert.deepEqual(await generateManifest(directory, repository, version), expected);
  assert.deepEqual(JSON.parse(await readFile(join(directory, "latest.json"), "utf8")), expected);
});
