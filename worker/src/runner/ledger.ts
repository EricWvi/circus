// The system prompt section of a session with a ledger snapshot: the
// directory on the session's device holding a copy of the project's
// decision ledger. The control plane collects the directory's changes after
// each turn and validates them (proto/circus/v1/ledger.proto, "Files").

/**
 * The text appended to the system prompt when the lease has a ledger path;
 * sent on every turn, so it stays compact.
 */
export function ledgerPrompt(ledgerPath: string): string {
  return [
    "# Decision ledger",
    "",
    `This project's decision ledger is the directory \`${ledgerPath}\`. Read and edit it with your usual tools. ` +
      "It is not a git repository and not part of the working directory.",
    "",
    "- One decision per file: `<dir>/D<n>-<title>.md`; the directory is its category. " +
      "The front matter holds exactly `number` (e.g. `D12`) and `title`.",
    "- Only decision files belong there. The generated `INDEX.md` lists them all: read it first, do not edit it.",
    "- Decisions are never deleted: revise, overturn or supersede them inside the body. " +
      "Moving a file changes its category. Never change an existing number.",
    "- A new decision takes a temporary number `D?a`, `D?b`, … (one to four lowercase letters) " +
      "in its front matter, its file name and references to it; numbers are assigned when the changes are merged.",
    "- Changes are collected after each turn; problems are reported back to you as a note.",
  ].join("\n");
}
