import { describe, expect, it } from "vitest"

import { changedFieldIds } from "./use-project-form"

const row = (key: string, device: string, path: string) => ({ key, device, path })

describe("changedFieldIds", () => {
  const sent = { name: "web", repoUrl: "", rows: [row("a", "box", "/a"), row("b", "box", "/b")] }

  it("is empty when nothing changed", () => {
    expect(changedFieldIds(sent, { ...sent, rows: [...sent.rows] })).toEqual([])
  })

  it("names the edited fields and the form-wide error", () => {
    expect(changedFieldIds(sent, { ...sent, name: "web2" })).toEqual(["name", "form"])
    expect(changedFieldIds(sent, { ...sent, repoUrl: "git@h:x" })).toEqual(["repoUrl", "form"])
    expect(
      changedFieldIds(sent, { ...sent, rows: [row("a", "mac", "/a"), row("b", "box", "/c")] }),
    ).toEqual(["a.device", "b.path", "form"])
  })

  it("drops every row error when a row was added or removed", () => {
    expect(changedFieldIds(sent, { ...sent, rows: [sent.rows[0]] })).toEqual(["rows", "form"])
    expect(changedFieldIds(sent, { ...sent, rows: [sent.rows[0], row("c", "box", "/b")] })).toEqual([
      "rows",
      "form",
    ])
  })
})
