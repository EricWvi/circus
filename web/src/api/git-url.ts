// Which repository URLs the control plane accepts, ported from its rules
// (checkRepoURL in internal/controlplane/webapi/rules.go). The scheme://
// forms go through Go's net/url parser there, so the parts of that parser
// the rules depend on are ported here too: how the authority, user, host,
// port and path are split, which characters and percent escapes each part
// allows, and which bracketed hosts are IPv6 addresses (net/netip). Strings
// are handled as UTF-16, which matches Go's byte-wise handling because every
// delimiter and every checked character is ASCII.
//
// The parity test (validation-parity.test.ts) runs the same cases on both
// sides.
//
// One parser setting is assumed: Go's GODEBUG urlstrictcolons=1, the
// default since Go 1.26 (the go.mod version), under which an http or https
// host with more than one ":" is refused, while other schemes take the last
// ":" as the port's. A server built with urlstrictcolons=0 would accept
// such http(s) hosts that this port refuses.

const isLetter = (c: string) => (c >= "a" && c <= "z") || (c >= "A" && c <= "Z")
const isDigit = (c: string) => c >= "0" && c <= "9"
const isHex = (c: string) => isDigit(c) || (c >= "a" && c <= "f") || (c >= "A" && c <= "F")
const hexValue = (c: string) => Number.parseInt(c, 16)

/**
 * A user or host name of [A-Za-z0-9._~-] that does not start with "-"
 * (which ssh would read as an option).
 */
function plainName(s: string): boolean {
  return s !== "" && s[0] !== "-" && /^[A-Za-z0-9._~-]+$/.test(s)
}

/** user@host:path with a non-empty path and a plain user and host. */
export function scpLike(u: string): boolean {
  const colon = u.indexOf(":")
  if (colon < 0 || colon === u.length - 1) return false
  const userHost = u.slice(0, colon)
  const at = userHost.indexOf("@")
  return at >= 0 && plainName(userHost.slice(0, at)) && plainName(userHost.slice(at + 1))
}

/** "" or ":" followed by digits. */
const validOptionalPort = (port: string) => port === "" || /^:[0-9]*$/.test(port)

// ASCII characters a host may hold unescaped (Go's shouldEscape for hosts).
const HOST_PUNCTUATION = new Set("-_.~!$&'()*+,;=:[]<>\"")
const hostCharAllowed = (c: string) =>
  c.charCodeAt(0) >= 0x80 || isLetter(c) || isDigit(c) || HOST_PUNCTUATION.has(c)

type Mode = "host" | "zone" | "other"

/**
 * Percent-decodes `s` the way Go's url.unescape does in the given mode, as
 * an array of byte values for escapes and UTF-16 code units for everything
 * else; null where Go returns an error. In a host only non-ASCII bytes may
 * be escaped (and "%25"), and only host characters may appear unescaped.
 */
function unescape(s: string, mode: Mode): number[] | null {
  const out: number[] = []
  for (let i = 0; i < s.length; ) {
    const c = s[i]
    if (c === "%") {
      if (i + 2 >= s.length || !isHex(s[i + 1]) || !isHex(s[i + 2])) return null
      const escape = s.slice(i, i + 3)
      const value = (hexValue(s[i + 1]) << 4) | hexValue(s[i + 2])
      if (mode === "host" && hexValue(s[i + 1]) < 8 && escape !== "%25") return null
      if (mode === "zone" && escape !== "%25" && value !== 0x20 && !(value < 0x80 && hostCharAllowed(String.fromCharCode(value)))) {
        return null
      }
      out.push(value)
      i += 3
      continue
    }
    if ((mode === "host" || mode === "zone") && !hostCharAllowed(c)) return null
    out.push(c.charCodeAt(0))
    i++
  }
  return out
}

const decoded = (codes: number[]) => String.fromCharCode(...codes)

/** Go's netip.ParseAddr for IPv4 fields: four decimal octets, no leading zeros. */
function ipv4Fields(s: string): boolean {
  const parts = s.split(".")
  return (
    parts.length === 4 &&
    parts.every((p) => /^[0-9]+$/.test(p) && !(p.length > 1 && p[0] === "0") && Number(p) <= 255)
  )
}

/**
 * Parses an address the way Go's netip.ParseAddr does, reporting what the
 * rules need: whether it is IPv6 (which includes IPv4-mapped addresses
 * written as IPv6) and its zone. Null when Go refuses it.
 */
export function parseAddr(s: string): { is6: boolean; zone: string } | null {
  for (const c of s) {
    if (c === ".") return ipv4Fields(s) ? { is6: false, zone: "" } : null
    if (c === ":") return parseIPv6(s)
    if (c === "%") return null
  }
  return null
}

function parseIPv6(input: string): { is6: true; zone: string } | null {
  let s = input
  let zone = ""
  const percent = s.indexOf("%")
  if (percent >= 0) {
    zone = s.slice(percent + 1)
    s = s.slice(0, percent)
    if (zone === "") return null
  }
  let ellipsis = -1
  if (s.startsWith("::")) {
    ellipsis = 0
    s = s.slice(2)
    if (s === "") return { is6: true, zone }
  }
  let i = 0
  while (i < 16) {
    let off = 0
    while (off < s.length && isHex(s[off])) {
      // Go fails on the fifth digit of a group.
      if (off > 3) return null
      off++
    }
    if (off === 0) return null
    if (off < s.length && s[off] === ".") {
      if (ellipsis < 0 && i !== 12) return null
      if (i + 4 > 16) return null
      if (!ipv4Fields(s)) return null
      s = ""
      i += 4
      break
    }
    i += 2
    s = s.slice(off)
    if (s === "") break
    if (s[0] !== ":" || s.length === 1) return null
    s = s.slice(1)
    if (s[0] === ":") {
      if (ellipsis >= 0) return null
      ellipsis = i
      s = s.slice(1)
      if (s === "") break
    }
  }
  if (s !== "") return null
  if (i < 16 ? ellipsis < 0 : ellipsis >= 0) return null
  return { is6: true, zone }
}

/**
 * Go's url.parseHost: host[:port], or a bracketed IPv6 literal with an
 * optional port. Returns the host with escapes decoded, or null.
 */
function parseHost(scheme: string, host: string): string | null {
  const open = host.lastIndexOf("[")
  if (open > 0) return null
  if (open === 0) {
    const close = host.lastIndexOf("]")
    if (close < 0) return null
    const colonPort = host.slice(close + 1)
    if (!validOptionalPort(colonPort)) return null
    const name = host.slice(1, close)
    const zoneAt = name.indexOf("%25")
    let address: string
    if (zoneAt >= 0) {
      const part = unescape(name.slice(0, zoneAt), "host")
      const zone = unescape(name.slice(zoneAt), "zone")
      if (!part || !zone) return null
      address = decoded(part) + decoded(zone)
    } else {
      const part = unescape(name, "host")
      if (!part) return null
      address = decoded(part)
    }
    const addr = parseAddr(address)
    if (!addr?.is6) return null
    return `[${address}]${colonPort}`
  }
  let colon = host.indexOf(":")
  if (colon >= 0) {
    const last = host.lastIndexOf(":")
    // Go allows several colons (taking the last as the port's) except for
    // http and https.
    if (last !== colon && scheme !== "http" && scheme !== "https") colon = last
    if (!validOptionalPort(host.slice(colon))) return null
  }
  const out = unescape(host, "host")
  return out && decoded(out)
}

/** Go's validUserinfo: unreserved, sub-delims, ":", "%" and "@". */
const validUserinfo = (s: string) => /^[A-Za-z0-9\-._:~!$&'()*+,;=%@]*$/.test(s)

/** Go's url.URL.Hostname and Port on a parsed host. */
function splitHostPort(hostPort: string): { host: string; port: string } {
  let host = hostPort
  let port = ""
  const colon = host.lastIndexOf(":")
  if (colon >= 0 && validOptionalPort(host.slice(colon))) {
    port = host.slice(colon + 1)
    host = host.slice(0, colon)
  }
  if (host.startsWith("[") && host.endsWith("]")) host = host.slice(1, -1)
  return { host, port }
}

/**
 * One of the accepted scheme://... forms, with the scheme in lower case as
 * git expects it: https://host/path, http://host/path,
 * ssh://[user@]host[:port]/path, git://host/path or file:///abs/path. A
 * password, a query or a fragment is refused.
 */
export function urlLike(raw: string): boolean {
  if (raw.includes("#") || raw.includes("?")) return false
  const match = /^([a-z][a-z0-9+.-]*):\/\//.exec(raw)
  if (!match) return false
  const scheme = match[1]
  let authority = raw.slice(match[0].length)
  let path = ""
  const slash = authority.indexOf("/")
  if (slash >= 0) {
    path = authority.slice(slash)
    authority = authority.slice(0, slash)
  }

  const at = authority.lastIndexOf("@")
  const hostPort = parseHost(scheme, authority.slice(at + 1))
  if (hostPort === null) return false
  let user: string | null = null
  if (at >= 0) {
    const userinfo = authority.slice(0, at)
    // A ":" means a password, which is refused anyway.
    if (!validUserinfo(userinfo) || userinfo.includes(":")) return false
    const name = unescape(userinfo, "other")
    if (!name) return false
    user = decoded(name)
  }
  if (!unescape(path, "other")) return false
  // Every escape decodes to one byte, so the decoded path is longer than
  // "/" exactly when the raw one is.
  const hasPath = path.length > 1
  const { host, port } = splitHostPort(hostPort)

  switch (scheme) {
    case "https":
    case "http":
    case "git":
      return user === null && host !== "" && host[0] !== "-" && hasPath
    case "ssh": {
      if (port !== "" && !(Number(port) >= 1 && Number(port) <= 65535)) return false
      const hostOk = hostPort.startsWith("[")
        ? (() => {
            const addr = parseAddr(host)
            return addr !== null && addr.is6 && addr.zone === ""
          })()
        : plainName(host)
      return hostOk && hasPath && (user === null || plainName(user))
    }
    case "file":
      return hostPort === "" && user === null && hasPath
  }
  return false
}
