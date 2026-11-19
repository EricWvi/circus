/// <reference types="node" />
// The install-and-join command, run by a real POSIX shell.
import { spawnSync } from "node:child_process"
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { afterEach, beforeEach, describe, expect, it } from "vitest"

import { enrollCommand } from "./enroll"

/**
 * Runs the command in sh with stand-ins for curl, tar and circus that
 * record their calls in a log (circus join also records the link it read)
 * and fail on request.
 */
describe("enrollCommand in a POSIX shell", () => {
  let dir = ""
  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "circus-enroll-"))
    mkdirSync(join(dir, "bin"))
    mkdirSync(join(dir, "home"))
    const script = (name: string, body: string) => {
      writeFileSync(join(dir, "bin", name), `#!/bin/sh\n${body}\n`)
      chmodSync(join(dir, "bin", name), 0o755)
    }
    script("curl", `echo "curl $4" >> "$LOG"; [ "$FAIL" = curl ] && exit 22; echo archive > "$3"`)
    script("tar", `echo tar >> "$LOG"; [ "$FAIL" = tar ] && exit 2; cp "$CIRCUS" "$4/circus"`)
    script(
      "circus",
      `echo "circus $*" >> "$LOG"
if [ "$1" = join ]; then read -r link; echo "link $link" >> "$LOG"; [ "$FAIL" = join ] && exit 1; fi
exit 0`,
    )
  })
  afterEach(() => rmSync(dir, { recursive: true, force: true }))

  function run(command: string, fail = "") {
    const log = join(dir, "log")
    writeFileSync(log, "")
    const result = spawnSync("sh", ["-c", command], {
      env: {
        PATH: `${join(dir, "bin")}:/usr/bin:/bin`,
        HOME: join(dir, "home"),
        LOG: log,
        CIRCUS: join(dir, "bin", "circus"),
        FAIL: fail,
      },
      encoding: "utf8",
    })
    return { status: result.status, calls: readFileSync(log, "utf8").trim().split("\n") }
  }

  const tricky = "https://x/join#a'b$(echo no)`echo no`\\\""
  const releaseCommand = enrollCommand({ link: tricky, role: "daemon", serverVersion: "v0.2.0", platform: "linux-amd64" })
  const archive = "https://github.com/EricWvi/circus/releases/download/v0.2.0/circus-v0.2.0-linux-amd64.tar.gz"

  it("runs every step of a release in order", () => {
    expect(run(releaseCommand)).toEqual({
      status: 0,
      calls: [`curl ${archive}`, "tar", "circus join", `link ${tricky}`, "circus daemon"],
    })
  })

  it("stops when the download fails", () => {
    const { status, calls } = run(releaseCommand, "curl")
    expect(status).not.toBe(0)
    expect(calls).toEqual([`curl ${archive}`])
  })

  it("stops when unpacking fails", () => {
    const { status, calls } = run(releaseCommand, "tar")
    expect(status).not.toBe(0)
    expect(calls).toEqual([`curl ${archive}`, "tar"])
  })

  it("doesn't start the daemon when joining fails", () => {
    const link = "https://cp.example/join#d.w"
    const dev = enrollCommand({ link, role: "both", serverVersion: "dev", platform: "linux-amd64" })
    const { status, calls } = run(dev, "join")
    expect(status).not.toBe(0)
    expect(calls).toEqual(["circus join", `link ${link}`])
    expect(run(dev).calls).toEqual(["circus join", `link ${link}`, "circus daemon"])
  })
})
