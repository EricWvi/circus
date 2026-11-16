import { describe, expect, it } from "vitest"

import { normalizeEmail, validateEmail, validatePassword } from "./validation"

describe("normalizeEmail", () => {
  it("trims and lower-cases", () => {
    expect(normalizeEmail("  Eric@Example.COM\n")).toBe("eric@example.com")
  })
})

describe("validateEmail", () => {
  it("accepts one @ between non-empty parts", () => {
    for (const email of ["a@b", "eric@example.com", " Eric@Example.com ", "a.b+c@d-e.f"]) {
      expect(validateEmail(email)).toBeNull()
    }
  })

  it("asks for an email when empty", () => {
    expect(validateEmail("   ")).toMatch(/Enter an email/)
  })

  it("rejects anything else", () => {
    for (const email of ["ab", "@b", "a@", "a@b@c", "a b@c", "a@b\tc", "a\u0000@b", "a@b\u007f", "a\u0085b@c", "a\u009f@b"]) {
      expect(validateEmail(email)).toMatch(/valid email/)
    }
  })

  it("allows at most 254 characters", () => {
    const at = (length: number) => `${"a".repeat(length - 4)}@b.c`
    expect(validateEmail(at(254))).toBeNull()
    expect(validateEmail(at(255))).not.toBeNull()
    // Counted in UTF-8 bytes: 130 characters, but 256 bytes.
    expect(validateEmail(`${"é".repeat(126)}@b.c`)).not.toBeNull()
    expect(validateEmail(`${"é".repeat(125)}@b`)).toBeNull()
  })
})

describe("validatePassword", () => {
  it("counts UTF-8 bytes, from 8 to 72", () => {
    expect(validatePassword("1234567")).toMatch(/at least 8/)
    expect(validatePassword("12345678")).toBeNull()
    expect(validatePassword("x".repeat(72))).toBeNull()
    expect(validatePassword("x".repeat(73))).toMatch(/too long/)
    // Four characters, but ten bytes.
    expect(validatePassword("日本語!")).toBeNull()
    // 24 three-byte characters are 72 bytes; one more is too many.
    expect(validatePassword("語".repeat(24))).toBeNull()
    expect(validatePassword("語".repeat(25))).toMatch(/too long/)
  })
})
