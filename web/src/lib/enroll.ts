// Adding a machine from the Web UI: the install-and-join command for an
// enrollment link, the platforms the release ships, and what to say when a
// link can't be issued.

import { errorMessage, isApiError } from "@/api/errors"
import type { EnrollRole } from "@/api/types"
import { i18n } from "@/i18n/i18n"

/** Release targets of the circus CLI (`.github/workflows/release.yml`). */
export const platforms = ["linux-amd64", "linux-arm64", "darwin-arm64", "darwin-amd64"] as const
export type Platform = (typeof platforms)[number]

export const isPlatform = (value: string): value is Platform =>
  (platforms as readonly string[]).includes(value)

export const DEFAULT_PLATFORM: Platform = "linux-amd64"

/** The subset of `navigator` used to guess the platform. */
export interface NavigatorHints {
  userAgent?: string
  userAgentData?: { platform?: string }
}

/**
 * The platform this browser runs on, as far as `navigator` tells without
 * asking: User-Agent Client Hints when available, else the User-Agent
 * string. Macs whose architecture is unknown (Safari and Firefox report
 * "Intel" on every Mac) count as Apple Silicon; anything else unknown,
 * Windows and phones included, falls back to Linux x86_64.
 */
export function detectPlatform(nav: NavigatorHints | undefined): Platform {
  const hinted = nav?.userAgentData?.platform?.toLowerCase()
  const ua = nav?.userAgent?.toLowerCase() ?? ""
  const os = hinted || ua
  const arm = /\b(aarch64|arm64|armv8)/.test(ua)
  if (/android|iphone|ipad/.test(ua)) return DEFAULT_PLATFORM
  if (os.includes("mac")) return "darwin-arm64"
  if (os.includes("linux")) return arm ? "linux-arm64" : "linux-amd64"
  return DEFAULT_PLATFORM
}

/** The subset of User-Agent Client Hints that names the CPU architecture. */
export interface HighEntropyHints {
  userAgentData?: {
    platform?: string
    getHighEntropyValues?: (hints: string[]) => Promise<{ architecture?: string; bitness?: string }>
  }
}

/**
 * Refines `detectPlatform` with the architecture from the high-entropy
 * client hints (Chromium only). Resolves to null when they say nothing more.
 */
export async function detectArchitecture(nav: HighEntropyHints | undefined): Promise<Platform | null> {
  const data = nav?.userAgentData
  if (!data?.getHighEntropyValues) return null
  const os = data.platform?.toLowerCase() ?? ""
  const prefix = os.includes("mac") ? "darwin" : os.includes("linux") ? "linux" : null
  if (!prefix) return null
  try {
    const { architecture } = await data.getHighEntropyValues(["architecture", "bitness"])
    if (architecture === "arm") return `${prefix}-arm64`
    if (architecture === "x86") return `${prefix}-amd64`
  } catch {
    // Hints can be refused; the first guess stands.
  }
  return null
}

/** Released builds have semver tags; anything else ("dev") has no download. */
export const isReleaseVersion = (version: string) =>
  /^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version)

const MINUTE = 60_000

/** How long the server's links last (`auth.DefaultEnrollTTL`). */
export const ENROLL_LINK_TTL_MS = 60 * MINUTE

/**
 * When a link received at `now` expires by this browser's clock. The
 * server's expiry is in its own clock; if that puts it in the past or
 * further away than a link lasts, the clocks disagree, and the link is
 * taken to last its usual time from now.
 */
export function localDeadline(expiresUnixMs: number, now: number): number {
  const left = expiresUnixMs - now
  return left > 0 && left <= ENROLL_LINK_TTL_MS + MINUTE ? expiresUnixMs : now + ENROLL_LINK_TTL_MS
}

const RELEASES = "https://github.com/EricWvi/circus/releases/download"

/** Quotes a string for a POSIX shell. */
const shellQuote = (s: string) => `'${s.replaceAll("'", `'\\''`)}'`

/**
 * The shell command that sets up a machine with `link`, as one chain for a
 * POSIX shell where each step runs only if the one before succeeded: for a
 * release, unless ~/.local/bin/circus already is the server's version,
 * download the matching CLI to a temporary file and unpack it there (so a
 * machine set up before skips the download, and one with another version
 * gets the server's); then join (as a worker only for the worker role);
 * then, when the link enrolls a device, install its daemon as a service of
 * the user's, and when it enrolls a worker, install the worker as one
 * (`circus worker install` downloads the worker package of the same
 * release and needs Node.js 24 or later). Both services start at boot or
 * login, again when they fail, and are updated by `circus update`. A
 * development build has no download, so the chain starts at the join and
 * runs the CLI from a circus checkout with `task cli --` (source build,
 * files under .tmp/dev); that build can't be a service, so it runs the
 * daemon in the foreground and leaves the worker to be started from the
 * checkout.
 *
 * The command starts with a space, so shells that leave such commands out
 * of their history (bash with `ignorespace`, zsh with HIST_IGNORE_SPACE)
 * don't keep the link; the other lines are indented too, for a shell that
 * records each line on its own.
 */
export function enrollCommand({
  link,
  role,
  serverVersion,
  platform,
}: {
  link: string
  role: EnrollRole
  serverVersion: string
  platform: Platform
}): string {
  const release = isReleaseVersion(serverVersion)
  const circus = release ? "~/.local/bin/circus" : "task cli --"
  const steps: string[] = []
  if (release) {
    const archive = `${RELEASES}/${serverVersion}/circus-${serverVersion}-${platform}.tar.gz`
    steps.push(
      [
        `[ "$(~/.local/bin/circus version 2>/dev/null)" = ${serverVersion} ] || {`,
        `    mkdir -p ~/.local/bin && t="$(mktemp)" &&`,
        `    curl -fsSL -o "$t" ${archive} &&`,
        `    tar -xzf "$t" -C ~/.local/bin circus && rm -f "$t"`,
        `  }`,
      ].join("\n"),
    )
  }
  steps.push(`printf '%s\\n' ${shellQuote(link)} | ${circus} join${role === "worker" ? " --role worker" : ""}`)
  if (role !== "worker") steps.push(release ? `${circus} daemon install` : `${circus} daemon`)
  if (role !== "daemon" && release) steps.push(`${circus} worker install`)
  return steps.map((step, i) => (i === 0 ? " " : "  ") + step).join(" &&\n")
}

/** A one-line message for a link that couldn't be issued. */
export function enrollLinkErrorMessage(error: unknown): string {
  if (isApiError(error, "resource_exhausted")) return i18n.t("devices.add.tooManyLinks")
  if (isApiError(error, "failed_precondition")) return i18n.t("devices.add.noPublicUrl")
  return errorMessage(error)
}
