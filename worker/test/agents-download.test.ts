import { once } from "node:events";
import { existsSync, mkdirSync, mkdtempSync, readdirSync, rmSync } from "node:fs";
import { createServer, request as httpRequest, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { createServer as createTLSServer } from "node:https";
import { connect, type AddressInfo, type LookupFunction } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Readable } from "node:stream";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { DEFAULT_LIMITS, formatBytes, formatDuration, install, type InstallRequest } from "../src/agents/install.js";
import { noProxyMatches, proxyConfig, proxyFor } from "../src/agents/proxy.js";
import { extractPackage } from "../src/agents/tar.js";
import {
  TEST_CERT, TEST_KEY, gnuLongName, goodPackage, integrity, paxPath, tar, type TarEntry,
} from "./agents-fixtures.js";

const servers: Server[] = [];
/** Tunnelled sockets, which closing a server leaves open. */
const tunnels: Array<{ destroy(): void }> = [];
async function listen(server: Server): Promise<number> {
  servers.push(server);
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  return (server.address() as AddressInfo).port;
}

let root: string;
beforeEach(() => {
  root = mkdtempSync(join(tmpdir(), "circus-download-"));
});
afterEach(async () => {
  for (const socket of tunnels.splice(0)) socket.destroy();
  await Promise.all(servers.splice(0).map((server) => new Promise<void>((done) => {
    server.closeAllConnections();
    server.close(() => done());
  })));
  rmSync(root, { recursive: true, force: true });
});

// ---- The tar reader refuses every unsafe entry ----

describe("unpacking", () => {
  async function extract(entries: TarEntry[], maxBytes?: number): Promise<string> {
    const dir = join(root, "out", "package-dir");
    mkdirSync(dir, { recursive: true });
    await extractPackage(Readable.from([tar(entries)]), dir, { maxBytes });
    return dir;
  }
  const file = (name: string, data = "x"): TarEntry => ({ name, data });

  test("extracts regular files and directories under package/", async () => {
    const dir = await extract([{ name: "package/sub/", type: "5" }, file("package/sub/a", "A"), file("package/b", "B")]);
    expect(readdirSync(dir).sort()).toEqual(["b", "sub"]);
    expect(readdirSync(join(dir, "sub"))).toEqual(["a"]);
  });

  test.each<[string, TarEntry[]]>([
    ["a .. component", [file("package/../evil")]],
    ["a . component", [file("package/./x")]],
    ["an absolute path", [file("/tmp/evil")]],
    ["a backslash", [file("package/..\\evil")]],
    ["an entry outside package/", [file("other/evil")]],
    ["a pax path leaving the package", [paxPath("package/" + "a".repeat(120) + "/../../evil"), file("package/placeholder")]],
    ["a GNU long name leaving the package", [gnuLongName("package/" + "a/".repeat(60) + "../".repeat(70) + "evil"), file("package/placeholder")]],
    ["a symbolic link", [{ name: "package/claude", type: "2", linkname: "/etc/passwd" }]],
    ["a symbolic link then a file through it", [{ name: "package/d", type: "2", linkname: "/tmp" }, file("package/d/evil")]],
    ["a hard link", [{ name: "package/claude", type: "1", linkname: "/etc/passwd" }]],
    ["a FIFO", [{ name: "package/f", type: "6" }]],
    ["a duplicate entry", [file("package/claude", "a"), file("package/claude", "b")]],
    ["a file, then a path under it", [file("package/a"), file("package/a/b")]],
    ["a damaged header checksum", [{ name: "package/claude", data: "x", badChecksum: 1 }]],
  ])("refuses %s, writing nothing outside", async (_name, entries) => {
    await expect(extract(entries)).rejects.toThrow();
    // Nothing next to or above the extraction directory.
    expect(readdirSync(join(root, "out"))).toEqual(["package-dir"]);
    expect(readdirSync(root)).toEqual(["out"]);
  });

  test("a package larger than the bound unpacked fails while unpacking", async () => {
    const entries = [file("package/a", "x".repeat(600)), file("package/b", "y".repeat(600))];
    await expect(extract(entries, 1000)).rejects.toThrow("package larger than 1000 bytes unpacked");
    // The bound counts declared sizes before writing: the second file is never written.
    expect(readdirSync(join(root, "out", "package-dir"))).toEqual(["a"]);
    await expect(extract([file("package/c", "z")], 1 << 30)).resolves.toBeDefined();
  });

  test("the default bounds and how they are named", () => {
    expect(DEFAULT_LIMITS).toEqual({ maxDownloadBytes: 512 << 20, maxUnpackedBytes: 1 << 30, idleMs: 60_000, deadlineMs: 30 * 60_000, maxRedirects: 5 });
    expect(formatBytes(512 << 20)).toBe("512 MiB");
    expect(formatBytes(1 << 30)).toBe("1 GiB");
    expect(formatBytes(1000)).toBe("1000 bytes");
    expect(formatDuration(60_000 * 30)).toBe("30 min");
    expect(formatDuration(60_000)).toBe("1 min");
    expect(formatDuration(1_500)).toBe("1500 ms");
  });
});

// ---- Download bounds ----

describe("download bounds", () => {
  const data = goodPackage();
  const request = (url: string, limits: InstallRequest["limits"], extra: Partial<InstallRequest> = {}): InstallRequest => ({
    dir: join(root, "claude-code"), version: "9.9.9", binary: "claude", url, integrity: integrity(data),
    signal: new AbortController().signal, limits, env: {}, ...extra,
  });
  const leftovers = () => (existsSync(join(root, "claude-code")) ? readdirSync(join(root, "claude-code")) : []);

  async function serve(handler: (req: IncomingMessage, res: ServerResponse) => void): Promise<string> {
    const port = await listen(createServer(handler));
    return `http://127.0.0.1:${port}`;
  }

  test("a declared size past the bound is refused before any byte is written", async () => {
    let written = 0;
    const base = await serve((_req, res) => {
      res.writeHead(200, { "content-length": 4096 });
      res.write(Buffer.alloc(16), () => { written++; });
    });
    await expect(install(request(base + "/p.tgz", { maxDownloadBytes: 1024 }))).rejects.toThrow("download larger than 1 KiB");
    expect(leftovers()).toEqual([]);
    expect(written).toBeLessThanOrEqual(1);
  });

  test("a stream without a declared size is cut at the bound", async () => {
    let sent = 0;
    const base = await serve((_req, res) => {
      res.writeHead(200);
      const timer = setInterval(() => {
        sent += 4096;
        res.write(Buffer.alloc(4096));
      }, 1);
      res.on("close", () => clearInterval(timer));
    });
    await expect(install(request(base + "/p.tgz", { maxDownloadBytes: 64 << 10 }))).rejects.toThrow("download larger than 64 KiB");
    expect(leftovers()).toEqual([]);
    expect(sent).toBeLessThan(1 << 20);
  });

  test("no data for the idle time fails as a timeout, before and after the headers", async () => {
    const silent = await serve(() => {});
    await expect(install(request(silent + "/p.tgz", { idleMs: 200 }))).rejects.toThrow("download timed out (no data for 200 ms)");
    const stalled = await serve((_req, res) => {
      res.writeHead(200, { "content-length": data.length });
      res.write(data.subarray(0, 10));
    });
    await expect(install(request(stalled + "/p.tgz", { idleMs: 200 }))).rejects.toThrow("download timed out (no data for 200 ms)");
    expect(leftovers()).toEqual([]);
  });

  test("a trickle that never idles still ends at the deadline", async () => {
    const base = await serve((_req, res) => {
      res.writeHead(200, { "content-length": data.length });
      let at = 0;
      const timer = setInterval(() => res.write(data.subarray(at, ++at)), 20);
      res.on("close", () => clearInterval(timer));
    });
    await expect(install(request(base + "/p.tgz", { idleMs: 200, deadlineMs: 500 }))).rejects.toThrow("download took longer than 500 ms");
    expect(leftovers()).toEqual([]);
  });

  test("redirects are followed up to the bound", async () => {
    let hits = 0;
    const base = await serve((req, res) => {
      hits++;
      const n = Number(req.url!.slice(1));
      if (n > 0) res.writeHead(302, { location: `/${n - 1}` }).end();
      else res.writeHead(200, { "content-length": data.length }).end(data);
    });
    await expect(install(request(base + "/2", { maxRedirects: 2 }))).resolves.toBeUndefined();
    expect(hits).toBe(3);
    hits = 0;
    await expect(install(request(base + "/3", { maxRedirects: 2 }))).rejects.toThrow("too many redirects");
    expect(hits).toBe(3);
  });

  test("the caller's abort stays an abort", async () => {
    const silent = await serve(() => {});
    const abort = new AbortController();
    const done = install(request(silent + "/p.tgz", {}, { signal: abort.signal }));
    setTimeout(() => abort.abort(new Error("superseded")), 50);
    await expect(done).rejects.toThrow("superseded");
  });
});

// ---- Proxies ----

describe("proxy selection", () => {
  const url = (u: string) => new URL(u);
  const env = (vars: NodeJS.ProcessEnv) => proxyConfig(vars);

  test("the scheme's variable, uppercase first", () => {
    const vars = { HTTPS_PROXY: "http://upper:1", https_proxy: "http://lower:2", http_proxy: "http://plain:3" };
    expect(proxyFor(url("https://registry.npmjs.org/x"), env(vars))?.host).toBe("upper:1");
    expect(proxyFor(url("https://registry.npmjs.org/x"), env({ https_proxy: "http://lower:2" }))?.host).toBe("lower:2");
    expect(proxyFor(url("http://registry.npmjs.org/x"), env(vars))?.host).toBe("plain:3");
    expect(proxyFor(url("http://registry.npmjs.org/x"), env({ HTTPS_PROXY: "http://p:1" }))).toBeUndefined();
    expect(proxyFor(url("https://registry.npmjs.org/x"), env({ HTTP_PROXY: "http://p:1" }))).toBeUndefined();
    // Without a scheme: http.
    expect(proxyFor(url("https://registry.npmjs.org/x"), env({ HTTPS_PROXY: "proxy.example:3128" }))?.href).toBe("http://proxy.example:3128/");
    expect(() => proxyFor(url("https://registry.npmjs.org/x"), env({ HTTPS_PROXY: "https://proxy.example" }))).toThrow("http:// proxies only");
  });

  test("localhost and loopback never use a proxy", () => {
    const vars = env({ HTTP_PROXY: "http://p:1" });
    for (const u of ["http://localhost:8/x", "http://127.0.0.1/x", "http://127.9.9.9/x", "http://[::1]:9/x"]) {
      expect(proxyFor(url(u), vars), u).toBeUndefined();
    }
    expect(proxyFor(url("http://10.0.0.1/x"), vars)?.host).toBe("p:1");
  });

  test.each<[string, string, string, boolean]>([
    ["*", "anything.example", "443", true],
    ["example.com", "example.com", "443", true],
    ["example.com", "sub.example.com", "443", true],
    ["example.com", "badexample.com", "443", false],
    [".example.com", "example.com", "443", false],
    [".example.com", "sub.example.com", "443", true],
    ["*.example.com", "example.com", "443", false],
    ["*.example.com", "sub.example.com", "443", true],
    ["example.com:443", "example.com", "443", true],
    ["example.com:8443", "example.com", "443", false],
    [" other.org , Example.COM ", "example.com", "443", true],
    ["10.1.2.3", "10.1.2.3", "443", true],
    ["10.1.2.3:443", "10.1.2.3", "80", false],
    ["10.0.0.0/8", "10.200.0.1", "443", true],
    ["10.0.0.0/8", "11.0.0.1", "443", false],
    ["2001:db8::/32", "2001:db8::1", "443", true],
    ["[2001:db8::1]:443", "2001:db8::1", "443", true],
    ["", "example.com", "443", false],
    // Name entries never match an IP address host.
    ["1.2.3", "10.1.2.3", "443", false],
    ["2.3.4", "1.2.3.4", "80", false],
    // A bracketed address without a port matches nothing.
    ["[2001:db8::1]", "2001:db8::1", "443", false],
    // Prefixes that end inside a byte.
    ["172.16.0.0/12", "172.31.255.1", "443", true],
    ["172.16.0.0/12", "172.32.0.1", "443", false],
    ["172.16.0.0/12", "172.15.255.255", "443", false],
    ["10.0.0.0/20", "10.0.15.1", "443", true],
    ["10.0.0.0/20", "10.0.16.1", "443", false],
    ["192.168.128.0/17", "192.168.127.255", "443", false],
    ["192.168.128.0/17", "192.168.255.255", "443", true],
    ["::ffff:10.0.0.0/116", "10.0.15.1", "443", true],
    ["::ffff:10.0.0.0/116", "10.0.16.1", "443", false],
    ["2001:db8::/33", "2001:db8:7fff::1", "443", true],
    ["2001:db8::/33", "2001:db8:8000::1", "443", false],
  ])("NO_PROXY %j for %s:%s: %s", (noProxy, host, port, exempt) => {
    expect(noProxyMatches(noProxy, host, port)).toBe(exempt);
  });

  test("an unparsable proxy URL is refused without repeating it (it may hold a password)", () => {
    for (const raw of ["http://user:pw@proxy:99999", "http://user:pw@pro xy:3128"]) {
      let message = "";
      try {
        proxyFor(url("https://registry.npmjs.org/x"), env({ HTTPS_PROXY: raw }));
      } catch (error) {
        message = String(error);
      }
      expect(message, raw).toContain("invalid proxy URL in HTTPS_PROXY or https_proxy");
      expect(message, raw).not.toContain("pw");
      expect(message, raw).not.toContain("user");
    }
    let message = "";
    try {
      proxyFor(url("http://registry.npmjs.org/x"), env({ HTTP_PROXY: "http://user:pw@proxy:99999" }));
    } catch (error) {
      message = String(error);
    }
    expect(message).toBe("Error: invalid proxy URL in HTTP_PROXY or http_proxy");
  });

  test("NO_PROXY before no_proxy", () => {
    const vars = { HTTPS_PROXY: "http://p:1", NO_PROXY: "a.example", no_proxy: "b.example" };
    expect(proxyFor(url("https://a.example/"), env(vars))).toBeUndefined();
    expect(proxyFor(url("https://b.example/"), env(vars))?.host).toBe("p:1");
  });
});

describe("downloads through a proxy", () => {
  const data = goodPackage();
  /** Every name resolves to this machine (test servers listen there). */
  const lookup: LookupFunction = (_host, options, callback) => {
    if (typeof options === "object" && options.all) (callback as (e: null, a: Array<{ address: string; family: number }>) => void)(null, [{ address: "127.0.0.1", family: 4 }]);
    else callback(null, "127.0.0.1", 4);
  };

  async function registryTLS(): Promise<{ port: number; requests: string[] }> {
    const requests: string[] = [];
    const port = await listen(createTLSServer({ key: TEST_KEY, cert: TEST_CERT }, (req, res) => {
      requests.push(`${req.headers.host} ${req.url}`);
      res.writeHead(200, { "content-length": data.length }).end(data);
    }));
    return { port, requests };
  }

  /** A forward proxy: CONNECT tunnels and absolute-URI requests, every host on this machine. */
  async function proxy(): Promise<{ url: string; seen: string[]; auth: string[] }> {
    const seen: string[] = [];
    const auth: string[] = [];
    const server = createServer((req, res) => {
      seen.push(`GET ${req.url}`);
      auth.push(req.headers["proxy-authorization"] ?? "");
      const target = new URL(req.url!);
      const forward = httpRequest({ host: "127.0.0.1", port: target.port, path: target.pathname, headers: req.headers }, (upstream) => {
        res.writeHead(upstream.statusCode!, upstream.headers);
        upstream.pipe(res);
      });
      forward.end();
    });
    server.on("connect", (req: IncomingMessage, socket, head) => {
      seen.push(`CONNECT ${req.url}`);
      auth.push(req.headers["proxy-authorization"] ?? "");
      const port = Number(req.url!.split(":").at(-1));
      tunnels.push(socket);
      const upstream = connect(port, "127.0.0.1", () => {
        socket.write("HTTP/1.1 200 Connection Established\r\n\r\n");
        if (head.length) upstream.write(head);
        upstream.pipe(socket);
        socket.pipe(upstream);
      });
      tunnels.push(upstream);
      upstream.on("error", () => socket.destroy());
      socket.on("error", () => upstream.destroy());
    });
    const port = await listen(server);
    return { url: `http://user:p%40ss@127.0.0.1:${port}`, seen, auth };
  }

  const request = (url: string, env: NodeJS.ProcessEnv): InstallRequest => ({
    dir: join(root, "claude-code"), version: "9.9.9", binary: "claude", url, integrity: integrity(data),
    signal: new AbortController().signal, env, ca: TEST_CERT, lookup,
  });
  const basic = "Basic " + Buffer.from("user:p@ss").toString("base64");

  test("an https download goes through HTTPS_PROXY with CONNECT; a NO_PROXY host goes direct", async () => {
    const reg = await registryTLS();
    const p = await proxy();
    await install(request(`https://registry.test:${reg.port}/p.tgz`, { HTTPS_PROXY: p.url, NO_PROXY: "direct.test" }));
    expect(p.seen).toEqual([`CONNECT registry.test:${reg.port}`]);
    expect(p.auth).toEqual([basic]);
    expect(reg.requests).toEqual([`registry.test:${reg.port} /p.tgz`]);
    expect(readdirSync(join(root, "claude-code"))).toEqual(["9.9.9"]);

    await install(request(`https://direct.test:${reg.port}/p.tgz`, { HTTPS_PROXY: p.url, NO_PROXY: "direct.test" }));
    expect(p.seen).toHaveLength(1);
    expect(reg.requests.at(-1)).toBe(`direct.test:${reg.port} /p.tgz`);
  });

  test("an http download goes through HTTP_PROXY as an absolute-URI request; HTTPS_PROXY does not apply", async () => {
    const seenDirect: string[] = [];
    const port = await listen(createServer((req, res) => {
      seenDirect.push(`${req.headers.host} ${req.url}`);
      res.writeHead(200, { "content-length": data.length }).end(data);
    }));
    const p = await proxy();
    await install(request(`http://registry.test:${port}/p.tgz`, { http_proxy: p.url }));
    expect(p.seen).toEqual([`GET http://registry.test:${port}/p.tgz`]);
    expect(p.auth).toEqual([basic]);
    expect(seenDirect).toEqual([`registry.test:${port} /p.tgz`]);
    await install(request(`http://registry.test:${port}/p.tgz`, { HTTPS_PROXY: p.url }));
    expect(p.seen).toHaveLength(1);
    expect(seenDirect).toHaveLength(2);
  });

  test("a redirect chooses its proxy again", async () => {
    const reg = await registryTLS();
    const p = await proxy();
    const port = await listen(createServer((_req, res) => {
      res.writeHead(302, { location: `https://registry.test:${reg.port}/p.tgz` }).end();
    }));
    await install(request(`http://direct.test:${port}/start`, { HTTPS_PROXY: p.url }));
    expect(p.seen).toEqual([`CONNECT registry.test:${reg.port}`]);
  });

  test("a proxy refusing the tunnel fails the download with its status", async () => {
    const port = await listen(createServer().on("connect", (_req, socket) => socket.end("HTTP/1.1 403 Forbidden\r\n\r\n")));
    await expect(install(request("https://registry.test:1/p.tgz", { HTTPS_PROXY: `http://127.0.0.1:${port}` })))
      .rejects.toThrow("refused the tunnel to registry.test:1: HTTP 403");
  });
});
