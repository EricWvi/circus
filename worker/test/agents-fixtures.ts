import { createHash } from "node:crypto";
import { gzipSync } from "node:zlib";

// Synthetic npm tarballs for the agent tests: ustar entries, pax ("x") and
// GNU long-name ("L") headers, any entry type, and a header checksum that
// can be broken on purpose.

export interface TarEntry {
  name: string;
  data?: string | Buffer;
  type?: string;
  mode?: number;
  linkname?: string;
  /** Adds this to the header checksum (a damaged header). */
  badChecksum?: number;
  /** The size field, when it must differ from the data's length. */
  size?: number;
}

export function tarHeader(entry: TarEntry, size: number): Buffer {
  const block = Buffer.alloc(512);
  block.write(entry.name, 0, 100, "utf8");
  block.write((entry.mode ?? 0o644).toString(8).padStart(7, "0") + "\0", 100, "ascii");
  block.write("0000000\0", 108, "ascii");
  block.write("0000000\0", 116, "ascii");
  block.write(size.toString(8).padStart(11, "0") + "\0", 124, "ascii");
  block.write("00000000000\0", 136, "ascii");
  block.write(entry.type ?? "0", 156, "ascii");
  block.write(entry.linkname ?? "", 157, 100, "utf8");
  block.write("ustar\0", 257, "ascii");
  block.write("00", 263, "ascii");
  block.fill(0x20, 148, 156);
  let sum = 0;
  for (const byte of block) sum += byte;
  sum += entry.badChecksum ?? 0;
  block.write(sum.toString(8).padStart(6, "0") + "\0 ", 148, "ascii");
  return block;
}

/** A pax extended header record "<len> key=value\n". */
export function paxRecord(key: string, value: string): string {
  const body = ` ${key}=${value}\n`;
  let length = body.length + 1;
  while (String(length).length + body.length !== length) length++;
  return `${length}${body}`;
}

/** The uncompressed tar of entries. */
export function tar(entries: TarEntry[]): Buffer {
  const parts: Buffer[] = [];
  for (const entry of entries) {
    const data = Buffer.from(entry.data ?? "");
    parts.push(tarHeader(entry, entry.size ?? data.length));
    parts.push(data, Buffer.alloc((512 - (data.length % 512)) % 512));
  }
  parts.push(Buffer.alloc(1024));
  return Buffer.concat(parts);
}

/** A pax header giving the next entry's path. */
export const paxPath = (path: string): TarEntry => ({ name: "PaxHeader", type: "x", data: paxRecord("path", path) });
/** A GNU long-name header giving the next entry's name. */
export const gnuLongName = (name: string): TarEntry => ({ name: "././@LongLink", type: "L", data: name + "\0" });

export const tarball = (entries: TarEntry[]): Buffer => gzipSync(tar(entries));

export const integrity = (data: Buffer) => "sha512-" + createHash("sha512").update(data).digest("base64");

export const claudeScript = "#!/bin/sh\necho '9.9.9 (Claude Code)'\n";
export const goodPackage = () => tarball([
  { name: "package/package.json", data: '{"name":"@anthropic-ai/claude-agent-sdk-test"}' },
  { name: "package/claude", data: claudeScript, mode: 0o755 },
  { name: "package/README.md", data: "readme" },
]);

/** A self-signed certificate for registry.test and direct.test, valid until 2126 (test servers only). */
export const TEST_KEY = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgubM06Kyy31m8nCVV
G6emlt1P4sa8gw/HhR4M9MWvnHmhRANCAAQ+25XcFPH+nVLpg7jNKte+6Et0NnQV
T5mo5zzclkcdEQsYYyKxtl8VpzpoSZ1dhTlpfTYaZzPW8IYILbg5NvCl
-----END PRIVATE KEY-----
`;
export const TEST_CERT = `-----BEGIN CERTIFICATE-----
MIIBrjCCAVSgAwIBAgIUUW/SwAm+/7vHonT1rBdDaPVDLJYwCgYIKoZIzj0EAwIw
GDEWMBQGA1UEAwwNcmVnaXN0cnkudGVzdDAgFw0yNjExMjYwNDQyNTZaGA8yMTI2
MTEwMjA0NDI1NlowGDEWMBQGA1UEAwwNcmVnaXN0cnkudGVzdDBZMBMGByqGSM49
AgEGCCqGSM49AwEHA0IABD7bldwU8f6dUumDuM0q177oS3Q2dBVPmajnPNyWRx0R
CxhjIrG2XxWnOmhJnV2FOWl9NhpnM9bwhggtuDk28KWjejB4MB0GA1UdDgQWBBTN
FJ2gnHERQuWOrrNmyiwRwy0M8DAfBgNVHSMEGDAWgBTNFJ2gnHERQuWOrrNmyiwR
wy0M8DAPBgNVHRMBAf8EBTADAQH/MCUGA1UdEQQeMByCDXJlZ2lzdHJ5LnRlc3SC
C2RpcmVjdC50ZXN0MAoGCCqGSM49BAMCA0gAMEUCIAc0q2h1fcIpDRn/pm5I1MDu
nKCsiSV4tqKMLfHfDfU8AiEAnj7AShKzbP+PAihPD27rlHGeiQmEC1F17nX9DF34
69Q=
-----END CERTIFICATE-----
`;
