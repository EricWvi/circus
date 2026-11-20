import { describe, expect, it } from "vitest"

import { ApiError } from "@/api/errors"

import {
  detectArchitecture,
  detectPlatform,
  ENROLL_LINK_TTL_MS,
  enrollCommand,
  enrollLinkErrorMessage,
  isReleaseVersion,
  localDeadline,
} from "./enroll"

const UA = {
  chromeLinux: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36",
  firefoxLinuxArm: "Mozilla/5.0 (X11; Linux aarch64; rv:140.0) Gecko/20100101 Firefox/140.0",
  safariMac: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/19.0 Safari/605.1.15",
  windows: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36",
  iphone: "Mozilla/5.0 (iPhone; CPU iPhone OS 19_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/19.0 Mobile/15E148 Safari/604.1",
  android: "Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36",
  androidArm: "Mozilla/5.0 (Linux; Android 9; aarch64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36",
}

describe("detectPlatform", () => {
  it.each([
    ["Linux on x86", { userAgent: UA.chromeLinux }, "linux-amd64"],
    ["Linux on ARM", { userAgent: UA.firefoxLinuxArm }, "linux-arm64"],
    // Every Mac browser says Intel; most Macs aren't.
    ["a Mac", { userAgent: UA.safariMac }, "darwin-arm64"],
    ["a Mac by client hints", { userAgent: UA.windows, userAgentData: { platform: "macOS" } }, "darwin-arm64"],
    ["Linux by client hints", { userAgent: "", userAgentData: { platform: "Linux" } }, "linux-amd64"],
    ["Windows", { userAgent: UA.windows }, "linux-amd64"],
    ["an iPhone", { userAgent: UA.iphone }, "linux-amd64"],
    ["Android", { userAgent: UA.android }, "linux-amd64"],
    // A phone isn't the machine being added, whatever its CPU.
    ["Android naming an ARM CPU", { userAgent: UA.androidArm }, "linux-amd64"],
    ["nothing", undefined, "linux-amd64"],
  ] as const)("on %s", (_, nav, platform) => {
    expect(detectPlatform(nav)).toBe(platform)
  })

  it("takes the architecture from high-entropy client hints", async () => {
    const hints = (platform: string, architecture: string) => ({
      userAgentData: { platform, getHighEntropyValues: () => Promise.resolve({ architecture }) },
    })
    expect(await detectArchitecture(hints("macOS", "x86"))).toBe("darwin-amd64")
    expect(await detectArchitecture(hints("macOS", "arm"))).toBe("darwin-arm64")
    expect(await detectArchitecture(hints("Linux", "arm"))).toBe("linux-arm64")
    expect(await detectArchitecture(hints("Windows", "x86"))).toBeNull()
    expect(await detectArchitecture(hints("Linux", ""))).toBeNull()
    expect(await detectArchitecture({ userAgentData: { platform: "Linux" } })).toBeNull()
    expect(
      await detectArchitecture({
        userAgentData: { platform: "Linux", getHighEntropyValues: () => Promise.reject(new Error("no")) },
      }),
    ).toBeNull()
  })
})

describe("isReleaseVersion", () => {
  it.each(["v0.1.0", "v12.3.45", "v1.0.0-rc.1", "v1.0.0-beta-2"])("accepts %s", (v) => {
    expect(isReleaseVersion(v)).toBe(true)
  })
  it.each(["dev", "", "0.1.0", "v0.1", "v0.1.0-", "v0.1.0+meta", "v0.1.0-rc 1", " v0.1.0"])("rejects %j", (v) => {
    expect(isReleaseVersion(v)).toBe(false)
  })
})

describe("enrollCommand", () => {
  const link = "https://cp.example/join#d.w"

  it("downloads a release unless it is installed, joins and starts the daemon, as one chain", () => {
    expect(enrollCommand({ link, role: "both", serverVersion: "v0.2.0", platform: "darwin-amd64" })).toBe(
      [
        ` [ "$(~/.local/bin/circus version 2>/dev/null)" = v0.2.0 ] || {`,
        `    mkdir -p ~/.local/bin && t="$(mktemp)" &&`,
        `    curl -fsSL -o "$t" https://github.com/EricWvi/circus/releases/download/v0.2.0/circus-v0.2.0-darwin-amd64.tar.gz &&`,
        `    tar -xzf "$t" -C ~/.local/bin circus && rm -f "$t"`,
        `  } &&`,
        `  printf '%s\\n' 'https://cp.example/join#d.w' | ~/.local/bin/circus join &&`,
        "  ~/.local/bin/circus daemon",
      ].join("\n"),
    )
  })

  it("joins a worker without starting a daemon", () => {
    const lines = enrollCommand({ link, role: "worker", serverVersion: "v0.2.0", platform: "linux-arm64" }).split("\n")
    expect(lines.at(-1)).toBe("  printf '%s\\n' 'https://cp.example/join#d.w' | ~/.local/bin/circus join --role worker")
  })

  it("leaves the download out for a development build", () => {
    expect(enrollCommand({ link, role: "daemon", serverVersion: "dev", platform: "linux-amd64" })).toBe(
      " printf '%s\\n' 'https://cp.example/join#d.w' | task cli -- join &&\n  task cli -- daemon",
    )
  })

  it("quotes the link for the shell", () => {
    expect(enrollCommand({ link: "https://x/join#a'b", role: "worker", serverVersion: "dev", platform: "linux-amd64" })).toBe(
      " printf '%s\\n' 'https://x/join#a'\\''b' | task cli -- join --role worker",
    )
  })
})

describe("localDeadline", () => {
  const now = Date.parse("2026-11-17T12:00:00Z")
  const MINUTE = 60_000

  it.each([
    ["an hour away", ENROLL_LINK_TTL_MS, now + ENROLL_LINK_TTL_MS],
    ["less than an hour away", 59 * MINUTE + 1, now + 59 * MINUTE + 1],
    ["a minute beyond an hour away", ENROLL_LINK_TTL_MS + MINUTE, now + ENROLL_LINK_TTL_MS + MINUTE],
  ])("trusts the server's expiry %s", (_, left, deadline) => {
    expect(localDeadline(now + left, now)).toBe(deadline)
  })

  it.each([
    ["further away than a link lasts", ENROLL_LINK_TTL_MS + MINUTE + 1],
    ["in the past", -30 * MINUTE],
    ["now", 0],
  ])("counts an hour from now when the server's expiry is %s", (_, left) => {
    expect(localDeadline(now + left, now)).toBe(now + ENROLL_LINK_TTL_MS)
  })
})

describe("enrollLinkErrorMessage", () => {
  it("falls back to the usual messages", () => {
    expect(enrollLinkErrorMessage(new ApiError("unavailable", ""))).toBe("Can't reach the server. Try again.")
    expect(enrollLinkErrorMessage(new Error("boom"))).toBe("Something went wrong. Try again.")
  })
})
