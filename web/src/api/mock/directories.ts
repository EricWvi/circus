// A small fake file system for the mock's listDirectory: the same tree on
// every device, with the home directory where the device's OS keeps it.
// It holds what the folder picker needs to show: project folders, git
// repositories, hidden and unreadable folders, symbolic links, an empty
// folder, a folder with more subfolders than a listing keeps, and under
// /mnt folders that misbehave (slow, timing out, failing the request).
// The git repositories have origin URLs for the mock's getGitRemote: one
// has none and one answers slowly.

import { ApiError } from "@/api/errors"
import type { DirectoryEntry, DirectoryListing, GitRemote } from "@/api/types"

/** Most entries a listing keeps, like the daemon. */
export const MAX_DIRECTORY_ENTRIES = 1000

/** How long the misbehaving folders under /mnt take, in milliseconds. */
export const SLOW_LISTING_MS = 2_000

/** How long the slow repository takes to read its origin, in milliseconds. */
export const SLOW_GIT_REMOTE_MS = 1_500

const MAX_PATH_BYTES = 4096

type Node =
  | { kind: "file" }
  /** A symbolic link to the directory at an absolute path. */
  | { kind: "link"; target: string }
  | {
      kind: "dir"
      children: Map<string, Node>
      /** A git repository; `origin` is its origin URL ("" for none). */
      git?: { origin: string; slow?: boolean }
      unreadable?: boolean
      /** Answered after SLOW_LISTING_MS, with this outcome. */
      trouble?: "slow" | "timeout" | "busy" | "unavailable" | "deadline"
    }

type Dir = Extract<Node, { kind: "dir" }>
type Spec = { [name: string]: Spec | Node }

const isNode = (v: Spec | Node): v is Node => typeof v.kind === "string"

function dir(spec: Spec = {}, extra: Omit<Dir, "kind" | "children"> = {}): Dir {
  return {
    kind: "dir",
    ...extra,
    children: new Map(Object.entries(spec).map(([name, v]) => [name, isNode(v) ? v : dir(v)])),
  }
}

const file: Node = { kind: "file" }
const link = (target: string): Node => ({ kind: "link", target })
const repo = (origin: string, spec: Spec = {}, slow = false) =>
  dir({ ".git": dir(), ...spec }, { git: { origin, ...(slow ? { slow } : {}) } })

function homeTree(home: string): Dir {
  const many: Spec = {}
  for (let i = 1; i <= 1200; i++) many[`folder-${String(i).padStart(4, "0")}`] = dir()
  return dir({
    ".cache": {},
    ".config": { git: {}, nvim: {} },
    ".ssh": dir({}, { unreadable: true }),
    circus: { projects: {} },
    Documents: { invoices: {}, "taxes-2025": {} },
    Downloads: {},
    "many-folders": many,
    notes: link(`${home}/Documents`),
    private: dir({}, { unreadable: true }),
    "README.md": file,
    src: {
      "an-extraordinarily-long-folder-name-that-does-not-fit-on-one-line-of-the-picker": {},
      circus: repo("https://github.com/eric/circus.git", {
        cmd: {},
        internal: { controlplane: { webapi: {} }, daemon: {} },
        web: {},
      }),
      dotfiles: repo("git@github.com:eric/dotfiles.git"),
      palace: repo(""),
      playground: { "scratch-1": {}, "scratch-2": {} },
      shared: link("/srv"),
      toolbox: repo("git@gitlab.com:eric/toolbox.git", {}, true),
    },
  })
}

function rootTree(os: string): { home: string; root: Dir } {
  const user = "eric"
  const homeParent = os === "darwin" ? "Users" : "home"
  const home = `/${homeParent}/${user}`
  const root = dir({
    etc: { nginx: {}, ssh: {} },
    [homeParent]: { [user]: homeTree(home) },
    mnt: {
      busy: dir({}, { trouble: "busy" }),
      hung: dir({}, { trouble: "deadline" }),
      slow: dir({ archive: {}, media: {} }, { trouble: "slow" }),
      "stale-nfs": dir({}, { trouble: "timeout" }),
      unreachable: dir({}, { trouble: "unavailable" }),
    },
    srv: { palace: repo("https://git.example.com/infra/palace.git") },
    tmp: {},
    usr: { bin: {}, lib: {}, local: { bin: {} }, share: {} },
    var: { log: {}, tmp: {} },
  })
  return { home, root }
}

const trees = new Map<string, ReturnType<typeof rootTree>>()
const treeFor = (os: string) => {
  const key = os === "darwin" ? "darwin" : "posix"
  let tree = trees.get(key)
  if (!tree) trees.set(key, (tree = rootTree(key)))
  return tree
}

/** Lexically cleans an absolute path. */
function clean(path: string): string {
  const out: string[] = []
  for (const part of path.split("/")) {
    if (part === "" || part === ".") continue
    if (part === "..") out.pop()
    else out.push(part)
  }
  return `/${out.join("/")}`
}

const parentOf = (path: string) => (path === "/" ? "" : path.slice(0, path.lastIndexOf("/")) || "/")

type Walk = { node: Node } | { error: "not_found" | "not_directory" | "permission_denied" }

/** Finds the node at a clean absolute path, following links. */
function walk(root: Dir, path: string, depth = 0): Walk {
  if (depth > 8) return { error: "not_found" }
  let node: Node = root
  for (const part of path.split("/").filter(Boolean)) {
    if (node.kind === "link") {
      const target = walk(root, node.target, depth + 1)
      if (!("node" in target)) return target
      node = target.node
    }
    if (node.kind === "file") return { error: "not_directory" }
    if (node.kind === "dir" && node.unreadable) return { error: "permission_denied" }
    const child: Node | undefined = node.kind === "dir" ? node.children.get(part) : undefined
    if (!child) return { error: "not_found" }
    node = child
  }
  if (node.kind === "link") return walk(root, node.target, depth + 1)
  return { node }
}

/**
 * A request path made absolute and clean, or null when it is invalid:
 * empty or "~" is the home directory, "~/x" is under it.
 */
function resolve(raw: string, home: string): string | null {
  if (new TextEncoder().encode(raw).length > MAX_PATH_BYTES || raw.includes("\0")) return null
  if (raw === "" || raw === "~") return home
  if (raw.startsWith("~/")) return clean(home + raw.slice(1))
  if (raw.startsWith("/")) return clean(raw)
  return null
}

export interface MockListing {
  /** How long the device takes to answer, beyond the network delay. */
  delayMs: number
  /** The answer, or the RPC error the request fails with. */
  outcome: DirectoryListing | ApiError
}

/** Lists a directory of the fake tree on a device running `os`. */
export function listMockDirectory(os: string, req: { path: string; includeHidden: boolean }): MockListing {
  const { home, root } = treeFor(os)
  const answer = (listing: Omit<DirectoryListing, "home" | "entries" | "truncated"> & Partial<DirectoryListing>) => ({
    delayMs: 0,
    outcome: { entries: [], truncated: false, home, ...listing },
  })
  const path = resolve(req.path, home)
  if (path === null) return answer({ path: "", parent: "", error: "invalid_path" })
  const parent = parentOf(path)
  const found = walk(root, path)
  if (!("node" in found)) return answer({ path, parent, error: found.error })
  const node = found.node
  if (node.kind !== "dir") return answer({ path, parent, error: "not_directory" })
  if (node.unreadable) return answer({ path, parent, error: "permission_denied" })

  const entries: DirectoryEntry[] = []
  for (const [name, child] of node.children) {
    if (!req.includeHidden && name.startsWith(".")) continue
    const target = child.kind === "link" ? walk(root, child.target) : { node: child }
    if (!("node" in target) || target.node.kind !== "dir") continue
    entries.push({ name, gitRepository: target.node.git !== undefined, symlink: child.kind === "link" })
  }
  // Byte order, like the daemon.
  entries.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))
  const listing = answer({
    path,
    parent,
    entries: entries.slice(0, MAX_DIRECTORY_ENTRIES),
    truncated: entries.length > MAX_DIRECTORY_ENTRIES,
  })

  switch (node.trouble) {
    case undefined:
      return listing
    case "slow":
      return { ...listing, delayMs: SLOW_LISTING_MS }
    case "timeout":
      return { ...answer({ path, parent, error: "timeout" }), delayMs: SLOW_LISTING_MS }
    case "busy":
      return { delayMs: 0, outcome: new ApiError("resource_exhausted", "The device is busy.") }
    case "unavailable":
      return { delayMs: 0, outcome: new ApiError("unavailable", "The device can't list directories.") }
    case "deadline":
      return { delayMs: SLOW_LISTING_MS, outcome: new ApiError("deadline_exceeded", "The device didn't answer.") }
  }
}

export interface MockGitRemote {
  /** How long the device takes to answer, beyond the network delay. */
  delayMs: number
  remote: GitRemote
}

/**
 * Reads the origin URL of the repository containing a directory of the
 * fake tree: the directory itself or its nearest ancestor that is one.
 */
export function readMockGitRemote(os: string, req: { path: string }): MockGitRemote {
  const { home, root } = treeFor(os)
  const fail = (error: NonNullable<GitRemote["error"]>) => ({ delayMs: 0, remote: { url: "", error } })
  const path = resolve(req.path, home)
  if (path === null) return fail("invalid_path")
  const found = walk(root, path)
  if (!("node" in found)) return fail(found.error === "permission_denied" ? "failed" : "not_found")
  if (found.node.kind !== "dir") return fail("not_found")
  for (let at = path; at !== ""; at = parentOf(at)) {
    const ancestor = walk(root, at)
    const git = "node" in ancestor && ancestor.node.kind === "dir" ? ancestor.node.git : undefined
    if (!git) continue
    const delayMs = git.slow ? SLOW_GIT_REMOTE_MS : 0
    return { delayMs, remote: git.origin === "" ? { url: "", error: "no_origin" } : { url: git.origin } }
  }
  return fail("not_repository")
}
