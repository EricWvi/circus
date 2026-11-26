import { isIP } from "node:net";

// The proxy of an agent download, chosen from the environment as Go's
// http.ProxyFromEnvironment (golang.org/x/net/http/httpproxy) does, so the
// worker and the circus CLI agree:
//
// - https URLs use HTTPS_PROXY, else https_proxy; http URLs use HTTP_PROXY,
//   else http_proxy (each the first nonempty of the two); none when unset;
// - localhost and loopback addresses never use a proxy;
// - NO_PROXY, else no_proxy, is a comma-separated list: "*" (no proxy at
//   all), an IP address or CIDR range, a host name that matches itself and
//   its subdomains ("example.com"), a domain suffix that matches only
//   subdomains (".example.com" or "*.example.com"); any entry may carry a
//   ":port" that must match too (an IPv6 address then in brackets; a
//   bracketed address without a port matches nothing); name entries never
//   match an IP address;
// - a proxy given without a scheme is http://.
//
// Only http:// proxies are supported (the worker tunnels https downloads
// through them with CONNECT). Proxy settings never apply to the worker's
// link to the control plane.

export interface ProxyConfig {
  httpProxy: string;
  httpsProxy: string;
  noProxy: string;
}

function firstSet(env: NodeJS.ProcessEnv, ...names: string[]): string {
  for (const name of names) {
    const value = env[name];
    if (value) return value;
  }
  return "";
}

export function proxyConfig(env: NodeJS.ProcessEnv): ProxyConfig {
  return {
    httpProxy: firstSet(env, "HTTP_PROXY", "http_proxy"),
    httpsProxy: firstSet(env, "HTTPS_PROXY", "https_proxy"),
    noProxy: firstSet(env, "NO_PROXY", "no_proxy"),
  };
}

/** A URL's host without IPv6 brackets, lowercased. */
export function bareHost(url: URL): string {
  return url.hostname.replace(/^\[(.*)\]$/, "$1").toLowerCase();
}

/** A URL's port, with the scheme's default. */
export function portOf(url: URL): string {
  return url.port || (url.protocol === "https:" ? "443" : "80");
}

function parseIPv4(host: string): number[] | undefined {
  if (isIP(host) !== 4) return undefined;
  return host.split(".").map(Number);
}

function parseIPv6(host: string): number[] | undefined {
  if (isIP(host) !== 6) return undefined;
  let text = host;
  // An embedded IPv4 tail becomes two groups.
  const v4 = /(\d+\.\d+\.\d+\.\d+)$/.exec(text);
  if (v4) {
    const [a, b, c, d] = v4[1].split(".").map(Number);
    text = text.slice(0, -v4[1].length) + `${((a << 8) | b).toString(16)}:${((c << 8) | d).toString(16)}`;
  }
  const [head, tail] = text.includes("::") ? text.split("::") : [text, undefined];
  const left = head ? head.split(":") : [];
  const right = tail ? tail.split(":") : [];
  const groups = tail === undefined ? left : [...left, ...Array(8 - left.length - right.length).fill("0"), ...right];
  const bytes: number[] = [];
  for (const group of groups) {
    const value = parseInt(group, 16);
    bytes.push(value >> 8, value & 0xff);
  }
  return bytes;
}

/** The address's bytes, IPv4 as IPv4-mapped IPv6 (as Go's net.IP compares them). */
function ipBytes(host: string): number[] | undefined {
  const v4 = parseIPv4(host);
  if (v4) return [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, ...v4];
  return parseIPv6(host);
}

function isLoopback(bytes: number[]): boolean {
  const mapped = bytes.slice(0, 12).every((b, i) => b === (i >= 10 ? 0xff : 0));
  if (mapped) return bytes[12] === 127;
  return bytes.slice(0, 15).every((b) => b === 0) && bytes[15] === 1;
}

function inCIDR(bytes: number[], cidr: string): boolean | undefined {
  const slash = cidr.indexOf("/");
  if (slash < 0) return undefined;
  const base = cidr.slice(0, slash);
  const bits = Number(cidr.slice(slash + 1));
  const network = ipBytes(base);
  if (!network || !/^\d+$/.test(cidr.slice(slash + 1))) return undefined;
  const width = isIP(base) === 4 ? 32 : 128;
  if (bits > width) return undefined;
  let remaining = bits + (width === 32 ? 96 : 0);
  for (let i = 0; i < 16 && remaining > 0; i++, remaining -= 8) {
    const mask = remaining >= 8 ? 0xff : (0xff << (8 - remaining)) & 0xff;
    if ((bytes[i] & mask) !== (network[i] & mask)) return false;
  }
  return true;
}

/** Splits "host:port" (IPv6 in brackets); undefined without a port. */
function splitHostPort(entry: string): [string, string] | undefined {
  const bracket = /^\[([^\]]+)\]:(\d+)$/.exec(entry);
  if (bracket) return [bracket[1], bracket[2]];
  const colon = entry.lastIndexOf(":");
  if (colon < 0 || entry.indexOf(":") !== colon) return undefined; // None, or a bare IPv6 address.
  return [entry.slice(0, colon), entry.slice(colon + 1)];
}

/** Whether NO_PROXY exempts host:port from the proxy. */
export function noProxyMatches(noProxy: string, host: string, port: string): boolean {
  const target = ipBytes(host);
  for (const raw of noProxy.split(",")) {
    const entry = raw.trim().toLowerCase();
    if (!entry) continue;
    if (entry === "*") return true;
    if (target) {
      const cidr = inCIDR(target, entry);
      if (cidr !== undefined) {
        if (cidr) return true;
        continue;
      }
    }
    let entryHost = entry;
    let entryPort = "";
    const split = splitHostPort(entry);
    // Brackets come off only with a port ("[::1]:443"), as Go's
    // SplitHostPort does; a bare "[::1]" stays a name that matches nothing.
    if (split) [entryHost, entryPort] = [split[0].replace(/^\[(.*)\]$/, "$1"), split[1]];
    const entryIP = ipBytes(entryHost);
    if (entryIP) {
      if (target && entryIP.every((b, i) => b === target[i]) && (!entryPort || entryPort === port)) return true;
      continue;
    }
    // Name entries never match an IP address host.
    if (!entryHost || target) continue;
    if (entryHost.startsWith("*.")) entryHost = entryHost.slice(1);
    const matchHost = !entryHost.startsWith(".");
    if (matchHost) entryHost = "." + entryHost;
    if (host.endsWith(entryHost) || (matchHost && host === entryHost.slice(1))) {
      if (!entryPort || entryPort === port) return true;
    }
  }
  return false;
}

/** The proxy for url, or undefined for a direct connection. Throws on a proxy this worker cannot use. */
export function proxyFor(url: URL, config: ProxyConfig): URL | undefined {
  const raw = url.protocol === "https:" ? config.httpsProxy : url.protocol === "http:" ? config.httpProxy : "";
  if (!raw) return undefined;
  const host = bareHost(url);
  if (host === "localhost") return undefined;
  const bytes = ipBytes(host);
  if (bytes && isLoopback(bytes)) return undefined;
  if (noProxyMatches(config.noProxy, host, portOf(url))) return undefined;
  let proxy: URL;
  try {
    proxy = new URL(raw);
    if (!/^(https?|socks5h?):$/.test(proxy.protocol)) proxy = new URL("http://" + raw);
  } catch {
    try {
      // Only a value without a scheme is retried as http://; one with a
      // scheme that does not parse (a bad port, a space) is refused.
      if (raw.includes("://")) throw new Error("unparsable");
      proxy = new URL("http://" + raw);
    } catch {
      // The value is not repeated: it may hold a password, and this error
      // reaches the control plane and the Web UI.
      const names = url.protocol === "https:" ? "HTTPS_PROXY or https_proxy" : "HTTP_PROXY or http_proxy";
      throw new Error(`invalid proxy URL in ${names}`);
    }
  }
  if (proxy.protocol !== "http:") {
    throw new Error(`unsupported proxy ${proxy.protocol}//${proxy.host}: agent downloads use http:// proxies only`);
  }
  return proxy;
}
