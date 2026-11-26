import { readFileSync } from "node:fs";
import { describe, expect, test } from "vitest";
import { proxyFor } from "../src/agents/proxy.js";

// Every case of testdata/proxy-cases.json: what Go's
// golang.org/x/net/http/httpproxy chooses for a proxy setting, a NO_PROXY
// value and a URL, written by `go run ./tools/proxycases >
// worker/test/testdata/proxy-cases.json` from the repository root. The
// worker chooses the same, except that it refuses the https:// and
// socks5:// proxies Go would use.

interface ProxyCase {
  http_proxy: string;
  https_proxy: string;
  no_proxy: string;
  url: string;
  /** "" for a direct connection, "error" when Go fails, else scheme://host. */
  proxy: string;
  /** The proxy's decoded "user:password", when it has one. */
  auth?: string;
}

const cases = JSON.parse(readFileSync(new URL("testdata/proxy-cases.json", import.meta.url), "utf8")) as ProxyCase[];

/** The worker's choice in the cases' terms. */
function choose(c: ProxyCase): { proxy: string; auth?: string } {
  let proxy: URL | undefined;
  try {
    proxy = proxyFor(new URL(c.url), { httpProxy: c.http_proxy, httpsProxy: c.https_proxy, noProxy: c.no_proxy });
  } catch (error) {
    return { proxy: /http:\/\/ proxies only/.test(String(error)) ? "refused" : "error" };
  }
  if (!proxy) return { proxy: "" };
  const auth = proxy.username || proxy.password
    ? `${decodeURIComponent(proxy.username)}:${decodeURIComponent(proxy.password)}` : undefined;
  return { proxy: `${proxy.protocol}//${proxy.host}`, ...(auth !== undefined ? { auth } : {}) };
}

/** What the worker must answer for c: Go's choice, an https or socks proxy refused. */
function expected(c: ProxyCase): { proxy: string; auth?: string } {
  if (/^(https|socks5h?):\/\//.test(c.proxy)) return { proxy: "refused" };
  return { proxy: c.proxy, ...(c.auth !== undefined ? { auth: c.auth } : {}) };
}

describe("proxy selection matches Go's httpproxy", () => {
  test("the case file covers what it should", () => {
    expect(cases.length).toBeGreaterThan(3000);
    expect(cases.some((c) => c.proxy === "")).toBe(true);
    expect(cases.some((c) => c.auth)).toBe(true);
    expect(cases.some((c) => c.no_proxy.includes("/12"))).toBe(true);
  });

  test("every case", () => {
    const wrong: string[] = [];
    for (const c of cases) {
      const got = choose(c);
      const want = expected(c);
      if (got.proxy !== want.proxy || got.auth !== want.auth) {
        wrong.push(`${c.url} http=${c.http_proxy} https=${c.https_proxy} no_proxy=${JSON.stringify(c.no_proxy)}: ` +
          `got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`);
      }
    }
    expect(wrong.slice(0, 20), `${wrong.length} of ${cases.length} cases differ`).toEqual([]);
  });
});
