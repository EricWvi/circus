import type { MockDecisionState, MockLedgerCommit, MockLedgers } from "@/api/mock/ledger"

/**
 * Demo ledgers of the mock backend: `circus` has a few directories (one
 * nested) and sixteen decisions with some history, `palace` two decisions
 * (for references across projects), and `toolbox` none.
 */

const HOUR = 60 * 60_000
const DAY = 24 * HOUR

/** A stable 40-digit hex id, so links to old versions survive a reload. */
export function fakeCommitId(seed: string): string {
  let out = ""
  for (let round = 0; out.length < 40; round++) {
    let h = 0x811c9dc5 ^ round
    for (const ch of `${round}:${seed}`) {
      h ^= ch.codePointAt(0) ?? 0
      h = Math.imul(h, 0x01000193)
    }
    out += (h >>> 0).toString(16).padStart(8, "0")
  }
  return out.slice(0, 40)
}

const d = (number: number, dir: string, title: string, body: string): MockDecisionState => ({
  number,
  dir,
  title,
  body: body.replace(/^\n/, ""),
})

const conventions = (revision: number) =>
  d(
    0,
    "",
    "Ledger conventions",
    `
Every decision is one file, \`<dir>/D<n>-<name>.md\`, and **the directory is its category**.

- Numbers are never reused; refer to a decision as D1 or, across projects, as \`palace#D1\`.
- Status, revisions and overrides are prose in the body, not fields.
- Write \`D12\` inside code when you mean the text, not the decision.

See D1 for the overall architecture and D4 for how the ledger is stored.
${revision > 0 ? `\n_Revision ${revision}: wording._\n` : ""}`,
  )

const architecture = d(
  1,
  "architecture",
  "Go control plane, React Web UI",
  `
The control plane is a single Go binary; the Web UI is a React app embedded into it.

## Why

1. One binary is easy to deploy: \`circus server --config server.yaml\`.
2. The UI talks to the server over Connect (D5), so both share the protocol in \`proto/\`.
3. Devices connect out to the control plane (D2), never the other way round.

| Part | Language | Ships as |
| --- | --- | --- |
| Control plane | Go | \`bin/circus\` |
| Web UI | TypeScript | embedded in \`bin/circus\` |
| Daemon | Go | \`circus daemon\` |

Background reading: [Connect protocol](https://connectrpc.com/docs/protocol) and the notes in \`research/connect-vs-grpc-web.md\`.
`,
)

const daemonLink = d(
  2,
  "architecture",
  "设备守护进程通过 WebSocket 连接控制面",
  `
守护进程主动连接控制面的 \`/v1/daemon\`，**不开放任何入站端口**。

- 断线后按退避重连，见 D7。
- 消息使用 JSON，每条带单调递增的 \`seq\`。
- 旧版本守护进程以 \`daemon_outdated\` 区分。

背景资料：[RFC 6455](https://www.rfc-editor.org/rfc/rfc6455) 和 [内部笔记](research/websocket-keepalive.md)。
`,
)

const sqliteBody = `
The control plane keeps everything in one SQLite database.

| Option | Pros | Cons |
| --- | --- | --- |
| **SQLite** | one file, no service to run | one writer at a time |
| PostgreSQL | concurrent writers | another service to operate |
| BoltDB | pure Go | no SQL, manual indexes |

Writes go through a single connection; reads use a pool. Backups copy the file with \`VACUUM INTO\`.
`
const sqlite = (dir: string) => d(3, dir, "SQLite as the only database", sqliteBody)

const ledgerStorageBody = (bounds: boolean) => `
每个项目的 Ledger 是控制面上的一个裸 Git 仓库，主线只由编排器合并的变更集推进。

\`\`\`text
ledgers/<project id>.git
  INDEX.md
  architecture/D1-Go-control-plane,-React-Web-UI.md
  session/D6-M3-worker-设计的七项取舍.md
\`\`\`

- 文件头只有 \`number\` 和 \`title\` 两个键。
- 数据库仍用 SQLite（D3），Ledger 不进数据库。
- 与 palace#D1 的对象存储方案不同，Ledger 不需要外部服务。
${bounds ? "\n上限见 D13。\n" : ""}`
const ledgerStorage = (dir: string, bounds: boolean) =>
  d(4, dir, "Ledger 以 Git 仓库存储", ledgerStorageBody(bounds))

const connect = d(
  5,
  "architecture",
  "Connect over JSON for the browser",
  `
Browsers call the control plane with the **Connect protocol over JSON**, not gRPC-Web.

- Plain \`fetch\`, readable in the network panel.
- Server streams work over HTTP/1.1.
- Errors carry \`google.rpc.BadRequest\` details naming the field.

Unknown numbers like D404 stay plain text.
`,
)

const workerTradeoffs = (title: string, revision: number) =>
  d(
    6,
    "session",
    title,
    `
M3 worker 在七个方面做了取舍：

1. **进程模型**：每个会话一个进程，崩溃只影响自己。
2. **转录**：只追加（D9），不改写历史。
3. **审批**：默认一小时过期（D8）。
4. **重连**：worker 断线后会话保留（D7）${revision > 1 ? "，最多等待 10 分钟" : ""}。
5. **工具执行**：在设备上，经守护进程（D2）。
6. **流式输出**：文本流可丢，持久事件为准。
7. **资源**：单个视图最多 64 MiB，之后只保留轮次边界。
${revision > 0 ? "\n> 修订：第 4 条补充了等待时间上限。\n" : ""}`,
  )

const sessions = [
  d(
    7,
    "session",
    "Sessions survive worker loss",
    `
When a worker disappears mid-turn, the session is **not** stopped: the turn is recorded as interrupted and the next message starts a new turn on another worker.

- The daemon keeps running tools until told otherwise.
- The UI shows the interruption inline.

See D6 (item 4).
`,
  ),
  d(
    8,
    "session",
    "Approvals expire after an hour",
    `
A pending approval expires after **60 minutes**. Expired approvals count as denied.

\`\`\`go
const approvalTTL = time.Hour
\`\`\`
`,
  ),
  d(
    9,
    "session",
    "会话转录只追加",
    `
转录是只追加的事件序列；任何修正都作为新事件追加。

- 丢失的部分以 \`transcript_gap_detected\` 标出，用户确认后继续。
- 视图满 64 MiB 后只保存轮次边界。
`,
  ),
]

const untrustedBody = (images: boolean) => `
Agent-written Markdown is rendered as **untrusted input**:

- Raw HTML is shown as text: <b>this is not bold</b>.
- Only \`http:\`, \`https:\` and \`mailto:\` links are links, and they open in a new tab.
- Relative links such as [the research notes](research/markdown-sanitizing.md) render as plain text.
${images ? "- Images are not loaded: ![the rendering pipeline](https://example.com/pipeline.png)\n" : ""}
Questions go to [the Web channel](mailto:web@example.com).
`
const untrusted = (images: boolean) => d(10, "web", "Render agent Markdown as untrusted input", untrustedBody(images))

const web = [
  d(
    11,
    "web",
    "界面文本全部走 i18n",
    `
所有界面文本都来自翻译目录，英文为源语言，中文为第二语言。

- 中文与英文字母、数字之间加空格，例如 \`共 12 条决策\`。
- Markdown 渲染规则见 D10；旧编号 D99 不存在。
`,
  ),
  d(
    12,
    "web",
    "Two-step layout on narrow screens",
    `
Below 768 px, pages that show a list and a detail show them as **two steps**: the list first, the detail after a tap, with a way back.

No page scrolls horizontally at 390 px.
`,
  ),
]

const bounds = d(
  13,
  "architecture/storage",
  "Bound ledger sizes",
  `
| Bound | Limit |
| --- | --- |
| Decisions per project | 4096 |
| Decision file | 256 KiB |
| Title | 512 bytes, one line |
| Directory | 256 bytes, segments of 1–64 bytes |

Directories may not contain empty, \`.\`, \`..\` or hidden segments. Storage itself: D4.
`,
)

const ledgerPage = [
  d(
    14,
    "web",
    "Ledger 页面只读",
    `
第一版 Ledger 页面**只读**：浏览目录、阅读决策、查看历史和旧版本。

- 修改通过变更集，由编排器合并（见 D4）。
- 页面不显示“被引用”列表。
`,
  ),
  d(
    15,
    "web",
    "Hover previews for decision references",
    `
References like D6 or circus#D14 link to the decision; hovering (or focusing) one shows a short preview.

On touch screens a tap just opens the decision.
`,
  ),
]

/** Builds the main line from commits made `ago` milliseconds before now, oldest first. */
function commitsOf(
  prefix: string,
  now: number,
  list: Array<Omit<MockLedgerCommit, "commit" | "atUnixMs"> & { ago: number }>,
): MockLedgerCommit[] {
  return [...list]
    .sort((a, b) => b.ago - a.ago)
    .map(({ ago, ...c }, i) => ({ ...c, commit: fakeCommitId(`${prefix}:${i}`), atUnixMs: now - ago }))
}

/** The demo ledgers, by project id. */
export function seedLedgers(now: number = Date.now()): MockLedgers {
  const noCode = ""
  // Small wording fixes to D0, every other day, so its history pages.
  const wording = Array.from({ length: 22 }, (_, i) => ({
    ago: 56 * DAY - i * 2 * DAY - 3 * HOUR,
    author: i % 3 === 0 ? "orchestrator" : `session-${(0x3a1 + i * 37).toString(16)}`,
    summary: `Tidy the ledger conventions (${i + 1})`,
    codeCommit: noCode,
    changes: [conventions(i + 1)],
  }))

  const circus = commitsOf("circus", now, [
    {
      ago: 60 * DAY,
      author: "import",
      summary: "Import existing decisions",
      codeCommit: noCode,
      changes: [
        conventions(0),
        architecture,
        daemonLink,
        sqlite("architecture"),
        ledgerStorage("architecture", false),
        connect,
        workerTradeoffs("Worker design trade-offs", 0),
        ...sessions,
      ],
    },
    {
      ago: 57 * DAY,
      author: "session-7f3a",
      summary: "Record the Web UI decisions",
      codeCommit: fakeCommitId("code:web"),
      changes: [untrusted(false), ...web],
    },
    ...wording,
    {
      ago: 30 * DAY,
      author: "orchestrator",
      summary: "Move storage decisions into architecture/storage",
      codeCommit: noCode,
      changes: [sqlite("architecture/storage"), ledgerStorage("architecture/storage", false)],
    },
    {
      ago: 20 * DAY,
      author: "session-2b91",
      summary: "Name the worker trade-offs after M3",
      codeCommit: noCode,
      changes: [workerTradeoffs("M3 worker 设计的七项取舍", 1)],
    },
    {
      ago: 10 * DAY,
      author: "session-9c0d",
      summary: "Bound ledger sizes",
      codeCommit: fakeCommitId("code:bounds"),
      changes: [bounds, ledgerStorage("architecture/storage", true)],
    },
    {
      ago: 3 * DAY,
      author: "orchestrator",
      summary: "Decide how the ledger page works",
      codeCommit: noCode,
      changes: [...ledgerPage, untrusted(true)],
    },
    {
      ago: 5 * HOUR,
      author: "session-a1b2",
      summary: "Clarify how long a session waits for its worker",
      codeCommit: fakeCommitId("code:wait"),
      changes: [workerTradeoffs("M3 worker 设计的七项取舍", 2)],
    },
  ])

  const palace = commitsOf("palace", now, [
    {
      ago: 12 * DAY,
      author: "import",
      summary: "Import existing decisions",
      codeCommit: noCode,
      changes: [
        d(0, "media", "Thumbnails with libvips", "Thumbnails are made with **libvips**: it streams, so large images never sit in memory whole.\n"),
        d(
          1,
          "storage",
          "Store originals in object storage",
          "Originals go to S3-compatible object storage; the database keeps only keys and checksums.\n\n- Thumbnails (D0) are cached next to them.\n",
        ),
      ],
    },
  ])

  return new Map([
    ["circus", circus],
    ["palace", palace],
  ])
}

/** The size of the large stress body: just under a decision file's 256 KiB bound. */
export const STRESS_BODY_BYTES = 250 * 1024

/**
 * Bodies at the edges of what a ledger allows, for `?mock=stress`: one with
 * long unbroken words, code, headings, links and deep nesting, and one of
 * about 250 KiB holding some 25,000 references.
 */
export function stressDecisions(): MockDecisionState[] {
  const sentence = "See D0 and D1, also circus#D4. "
  return [
    d(
      2,
      "stress",
      "Layout torture",
      [
        `# ${"W".repeat(400)}`,
        `word${"x".repeat(3000)}`,
        `https://example.com/${"a".repeat(1500)}`,
        `\`${"c".repeat(1500)}\` and \`/home/eric/src/circus/internal/controlplane/webapi/ledger_service_test.go\``,
        `[${"l".repeat(1500)}](https://example.com)`,
        `| ${Array.from({ length: 30 }, (_, i) => `col${i}longheader`).join(" | ")} |\n|${" --- |".repeat(30)}\n| ${Array.from({ length: 30 }, () => "v".repeat(40)).join(" | ")} |`,
        `\`\`\`\n${"k".repeat(2000)}\n\`\`\``,
        `${">".repeat(60)} deep quote`,
        Array.from({ length: 40 }, (_, i) => `${"  ".repeat(i)}- level ${i}`).join("\n"),
        `![${"a".repeat(800)}](https://example.com/x.png)`,
        "Footnote[^1] and D0, D1, circus#D4, circus#D99 and nope#D1.",
        "[^1]: The note, with a way back.",
      ].join("\n\n"),
    ),
    d(3, "stress", "Large body", sentence.repeat(Math.floor(STRESS_BODY_BYTES / sentence.length))),
  ]
}

/** The demo ledgers with the stress decisions added to `palace`. */
export function stressLedgers(now: number = Date.now()): MockLedgers {
  const ledgers = seedLedgers(now)
  const palace = ledgers.get("palace") ?? []
  palace.push({
    commit: fakeCommitId("palace:stress"),
    atUnixMs: now - HOUR,
    author: "stress",
    summary: "Add the stress decisions",
    codeCommit: "",
    changes: stressDecisions(),
  })
  ledgers.set("palace", palace)
  return ledgers
}
