import { describe, expect, it } from "vitest"

import { formatRelative } from "./time"

describe("formatRelative", () => {
  const now = Date.parse("2026-11-15T12:00:00Z")
  const ago = (ms: number) => now - ms

  it("formats compact units", () => {
    expect(formatRelative(ago(10_000), now)).toBe("now")
    expect(formatRelative(ago(5 * 60_000), now)).toBe("5m")
    expect(formatRelative(ago(3 * 3_600_000), now)).toBe("3h")
    expect(formatRelative(ago(2 * 86_400_000), now)).toBe("2d")
  })

  it("formats in the given language", () => {
    expect(formatRelative(ago(10_000), now, "zh-CN")).toBe("刚刚")
    expect(formatRelative(ago(5 * 60_000), now, "zh-CN")).toBe("5分钟前")
    expect(formatRelative(ago(3 * 3_600_000), now, "zh-CN")).toBe("3小时前")
    expect(formatRelative(ago(86_400_000), now, "zh-CN")).toBe("1天前")
    expect(formatRelative(Date.parse("2026-11-01T12:00:00Z"), now, "zh-CN")).toBe("11月1日")
    expect(formatRelative(Date.parse("2026-11-01T12:00:00Z"), now, "en")).toBe("Nov 1")
  })

  it("clamps future timestamps to now", () => {
    expect(formatRelative(ago(-60_000), now)).toBe("now")
  })
})
