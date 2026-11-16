import { describe, expect, it } from "vitest"

import { TITLE_MAX, titleFromMessage } from "./title"

describe("titleFromMessage", () => {
  it("uses the first non-empty line, whitespace collapsed", () => {
    expect(titleFromMessage("\n\n  Fix   the  build \nmore details")).toBe("Fix the build")
  })

  it("is null without text", () => {
    expect(titleFromMessage("")).toBeNull()
    expect(titleFromMessage(" \n\t ")).toBeNull()
  })

  it("keeps short lines whole", () => {
    const line = "x".repeat(TITLE_MAX)
    expect(titleFromMessage(line)).toBe(line)
  })

  it("cuts long lines at a word boundary", () => {
    const title = titleFromMessage(
      "Please migrate the thumbnail generator from ImageMagick to libvips and keep the output identical",
    )
    expect(title).toBe("Please migrate the thumbnail generator from ImageMagick to…")
    expect([...(title ?? "")].length).toBeLessThanOrEqual(TITLE_MAX)
  })

  it("cuts a long word mid-way", () => {
    const title = titleFromMessage("a".repeat(100)) ?? ""
    expect(title).toBe(`${"a".repeat(TITLE_MAX - 1)}…`)
  })
})
