import { describe, expect, it } from "vitest"

import { bodyPreview } from "./ledger-preview"

describe("bodyPreview", () => {
  it("drops Markdown marks, code blocks, tables and tags", () => {
    const body = [
      "## Why",
      "",
      "The **control plane** is `one` binary; see [the docs](https://x.example) and ![a](b.png).",
      "",
      "```go",
      "func main() {}",
      "```",
      "| A | B |",
      "| --- | --- |",
      "- first <b>item</b>",
      "1. second",
      "> quoted transcript_gap_detected",
    ].join("\n")
    expect(bodyPreview(body)).toBe(
      "Why The control plane is one binary; see the docs and a. first item second quoted transcript_gap_detected",
    )
  })

  it("cuts long text by characters", () => {
    expect(bodyPreview("账".repeat(10), 4)).toBe("账账账账…")
    expect(bodyPreview("short", 10)).toBe("short")
  })
})
