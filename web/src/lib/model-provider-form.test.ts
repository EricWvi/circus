import { describe, expect, it } from "vitest"

import { ApiError } from "@/api/errors"
import type { ModelProvider } from "@/api/types"

import {
  mapProviderServerError,
  parseMaxSessions,
  providerFormChanged,
  providerFormValues,
  toProviderInput,
  validateProviderForm,
  type ProviderFormValues,
} from "./model-provider-form"

const stored: ModelProvider = {
  providerId: "p1",
  name: "Proxy",
  baseUrl: "https://proxy.example.com/v1",
  scheme: "bearer",
  keyLast4: "k9Qz",
  fableModel: "huge",
  opusModel: "big",
  sonnetModel: "",
  haikuModel: "",
  env: [
    { name: "TEAM", valueLast4: "-web" },
    { name: "EMPTY", valueLast4: "" },
  ],
  workerIds: ["w1"],
  maxSessions: 2,
  isDefault: false,
  leasedSessions: 0,
  secretsUnreadable: false,
  createdAtUnixMs: 0,
  updatedAtUnixMs: "5",
}

const valid = (over: Partial<ProviderFormValues> = {}): ProviderFormValues => ({
  ...providerFormValues(),
  name: "Anthropic",
  key: "sk-test",
  ...over,
})

const row = (name: string, value = "v", key = name || "blank") => ({ key, name, value })

const check = (values: ProviderFormValues, editing?: ModelProvider, others: { name: string }[] = []) =>
  validateProviderForm(values, { others, editing })

describe("validating the provider form", () => {
  it("accepts a minimal provider", () => {
    const errors = check(valid())
    expect(Object.values(errors.fields).filter(Boolean)).toEqual([])
    expect(errors.env).toEqual({})
  })

  it("checks the name: required, at most 64 characters, no control characters, unused ignoring case", () => {
    expect(check(valid({ name: "  " })).fields.name).toBe("Enter a provider name.")
    expect(check(valid({ name: "x".repeat(65) })).fields.name).toBe("Provider names are at most 64 characters.")
    expect(check(valid({ name: "a".repeat(64) })).fields.name).toBeUndefined()
    expect(check(valid({ name: "a\u0007b" })).fields.name).toBe("Provider names can't contain control characters.")
    expect(check(valid({ name: " proxy " }), undefined, [{ name: "Proxy" }]).fields.name).toBe(
      'A provider named "proxy" already exists.',
    )
  })

  it("checks the base URL: empty, or http(s) without user info, query or fragment", () => {
    const problem = (baseUrl: string) => check(valid({ baseUrl })).fields.baseUrl
    expect(problem("")).toBeUndefined()
    expect(problem(" https://api.example.com/anthropic ")).toBeUndefined()
    expect(problem("ftp://example.com")).toBe("Enter an http or https URL, like https://api.example.com.")
    expect(problem("api.example.com")).toBe("Enter an http or https URL, like https://api.example.com.")
    expect(problem("https://user:pw@example.com")).toBe(
      "Leave the user name and password out of the URL; the key goes in Key.",
    )
    expect(problem("https://example.com/?a=1")).toBe("The URL can't have a query (?) or a fragment (#).")
    expect(problem("https://example.com/#top")).toBe("The URL can't have a query (?) or a fragment (#).")
    expect(problem("https://user@example.com/v1")).toBe(
      "Leave the user name and password out of the URL; the key goes in Key.",
    )
    // Spellings a browser repairs, but the server parses as sent.
    for (const repaired of ["http:/example.com", "https:example.com", "https://", "https://:443/x"]) {
      expect({ repaired, problem: problem(repaired) }).toEqual({
        repaired,
        problem: "Enter an http or https URL, like https://api.example.com.",
      })
    }
    for (const outside of ["https:\\\\example.com", "https://h/a b", "https://h/é"]) {
      expect({ outside, problem: problem(outside) }).toEqual({
        outside,
        problem: "URLs hold only printable ASCII without spaces or backslashes; percent-encode anything else.",
      })
    }
    // An international domain name goes in its punycode form.
    expect(problem("https://hé/")).toBe(
      "Type an international domain name in its punycode form (xn--…); the host must be ASCII.",
    )
    expect(problem("https://xn--h-9fa/")).toBeUndefined()
    expect(problem("HTTPS://api.example.com:8443/v1/")).toBeUndefined()
    expect(problem(`https://example.com/${"a".repeat(2048)}`)).toBe("Base URLs are at most 2048 bytes.")
  })

  it("checks the base URL's host labels and path characters", () => {
    const problem = (baseUrl: string) => check(valid({ baseUrl })).fields.baseUrl
    for (const empty of ["https://.", "https://a..b", "https://api.example.com."]) {
      expect({ empty, problem: problem(empty) }).toEqual({
        empty,
        problem: "The host can't have two dots in a row or a dot at either end.",
      })
    }
    expect(problem(`https://${"l".repeat(64)}.example`)).toBe(
      "Each part of the host between dots is at most 63 characters.",
    )
    expect(problem(`https://${"l".repeat(63)}.example`)).toBeUndefined()
    for (const hyphen of ["https://-", "https://-a.example", "https://a-.example"]) {
      expect({ hyphen, problem: problem(hyphen) }).toEqual({
        hyphen,
        problem: "No part of the host between dots can start or end with a hyphen (-).",
      })
    }
    for (const numeric of ["https://999.999.999.999", "https://1", "https://a.1", "https://01.2.3.4"]) {
      expect({ numeric, problem: problem(numeric) }).toEqual({
        numeric,
        problem: "A host ending in a number must be an IPv4 address, like 192.0.2.1.",
      })
    }
    expect(problem("https://192.0.2.1:8443/v1")).toBeUndefined()
    for (const ch of ['"', "<", ">", "[", "]", "^", "`", "{", "|", "}"]) {
      expect({ ch, problem: problem(`https://h/a${ch}b`) }).toEqual({
        ch,
        problem: 'Percent-encode " < > [ ] ^ ` { | and } in the URL\'s path.',
      })
    }
    expect(problem("https://h/%7Bid%7D%5B0%5D")).toBeUndefined()
    expect(problem("https://h~x.localhost")).toBe("Enter an http or https URL, like https://api.example.com.")
    const label = "l".repeat(63)
    expect(problem(`https://${label}.${label}.${label}.${"l".repeat(61)}`)).toBeUndefined()
    expect(problem(`https://${label}.${label}.${label}.${"l".repeat(62)}`)).toBe("The host is at most 253 characters.")
  })

  it("takes only printable ASCII in the key and trims spaces around it", () => {
    const problem = (key: string) => check(valid({ key })).fields.key
    for (const key of ["sk\u200bx", "sk\u2028x", "sk\u00adx", "sk\nx", "sk x", "sk-é", "sk-汉"]) {
      expect({ key, problem: problem(key) }).toEqual({
        key,
        problem: "Keys hold only printable ASCII characters, without spaces.",
      })
    }
    expect(problem("sk-ant-api03-AZaz09_-./+=~!")).toBeUndefined()
    // The form sends the key trimmed, so spaces around it are no error.
    expect(problem("  sk-key\n")).toBeUndefined()
    expect(toProviderInput(valid({ key: "  sk-key\n" })).key).toBe("sk-key")
  })

  it("requires the key for a new provider and once the stored one can't be read, not otherwise", () => {
    expect(check(valid({ key: "" })).fields.key).toBe("Enter the key.")
    expect(check(valid({ key: "" }), stored).fields.key).toBeUndefined()
    expect(check(valid({ key: "" }), { ...stored, secretsUnreadable: true }).fields.key).toBe("Enter the key.")
    expect(check(valid({ key: "x".repeat(4097) })).fields.key).toBe("Keys are at most 4096 bytes.")
  })

  it("counts the name in code points and the variables in UTF-8 bytes", () => {
    // 64 two-byte and 64 four-byte (two UTF-16 units) characters are 64 code points.
    expect(check(valid({ name: "é".repeat(64) })).fields.name).toBeUndefined()
    expect(check(valid({ name: "😀".repeat(64) })).fields.name).toBeUndefined()
    expect(check(valid({ name: "😀".repeat(65) })).fields.name).toBe("Provider names are at most 64 characters.")
    // 22,000 three-byte characters: 66,000 bytes but 22,000 UTF-16 units.
    expect(check(valid({ env: [row("BIG", "€".repeat(22_000))] })).fields.env).toBe(
      "Names and values are at most 64 KiB in all.",
    )
    expect(check(valid({ env: [row("BIG", "€".repeat(21_000))] })).fields.env).toBeUndefined()
  })

  it("refuses every variable the endpoint sets", () => {
    for (const name of [
      "ANTHROPIC_BASE_URL",
      "ANTHROPIC_API_KEY",
      "ANTHROPIC_AUTH_TOKEN",
      "ANTHROPIC_DEFAULT_FABLE_MODEL",
      "ANTHROPIC_DEFAULT_OPUS_MODEL",
      "ANTHROPIC_DEFAULT_SONNET_MODEL",
      "ANTHROPIC_DEFAULT_HAIKU_MODEL",
    ]) {
      expect(check(valid({ env: [row(name)] })).env).toEqual({ [name]: { name: `${name} is set from the endpoint above.` } })
    }
  })

  it("asks to choose a scheme this client doesn't know", () => {
    expect(check(valid({ scheme: "unspecified" })).fields.scheme).toBe("Choose how the key is sent.")
    expect(providerFormChanged(stored, { ...providerFormValues(stored), scheme: "unspecified" })).toBe(true)
  })

  it("checks the model ids", () => {
    expect(check(valid({ opusModel: "a b" })).fields.opusModel).toBe("Model IDs can't contain spaces or control characters.")
    expect(check(valid({ haikuModel: "m".repeat(257) })).fields.haikuModel).toBe("Model IDs are at most 256 bytes.")
    expect(check(valid({ fableModel: "a\tb" })).fields.fableModel).toBe("Model IDs can't contain spaces or control characters.")
  })

  it("checks variable names: the pattern, reserved and endpoint names, duplicates", () => {
    const values = valid({
      env: [
        row("lower"),
        row("9LIVES"),
        row("CIRCUS_TOKEN"),
        row("HOME"),
        row("PATH"),
        row("CLAUDE_CONFIG_DIR"),
        row("CLAUDE_CODE_OAUTH_TOKEN"),
        row("CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST"),
        row("ANTHROPIC_API_KEY"),
        row("OK_1"),
        row("OK_1", "x", "again"),
        row("", "value only"),
        row("", ""),
      ],
    })
    const errors = check(values).env
    expect(errors).toEqual({
      lower: { name: "Use upper-case letters, digits and _, not starting with a digit." },
      "9LIVES": { name: "Use upper-case letters, digits and _, not starting with a digit." },
      CIRCUS_TOKEN: { name: "CIRCUS_TOKEN is reserved for Circus and the worker." },
      HOME: { name: "HOME is reserved for Circus and the worker." },
      PATH: { name: "PATH is reserved for Circus and the worker." },
      CLAUDE_CONFIG_DIR: { name: "CLAUDE_CONFIG_DIR is reserved for Circus and the worker." },
      CLAUDE_CODE_OAUTH_TOKEN: { name: "CLAUDE_CODE_OAUTH_TOKEN is reserved for Circus and the worker." },
      CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST: {
        name: "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST is reserved for Circus and the worker.",
      },
      ANTHROPIC_API_KEY: { name: "ANTHROPIC_API_KEY is set from the endpoint above." },
      again: { name: "This name is already listed." },
      // A value without a name; a row left blank is dropped instead.
      blank: { name: "Enter a name." },
    })
  })

  it("caps variables at 64 and 64 KiB of names and values", () => {
    const many = valid({ env: Array.from({ length: 65 }, (_, i) => row(`V${i}`, "1")) })
    expect(check(many).fields.env).toBe("A provider has at most 64 variables.")
    expect(check(valid({ env: Array.from({ length: 64 }, (_, i) => row(`V${i}`, "1")) })).fields.env).toBeUndefined()
    const large = valid({ env: [row("BIG", "x".repeat(64 * 1024))] })
    expect(check(large).fields.env).toBe("Names and values are at most 64 KiB in all.")
  })

  it("reads the session limit: empty is unlimited, a whole number up to 1024", () => {
    expect(parseMaxSessions("")).toBe(0)
    expect(parseMaxSessions(" 3 ")).toBe(3)
    expect(parseMaxSessions("1.5")).toBeNaN()
    expect(check(valid({ maxSessions: "1025" })).fields.maxSessions).toBe("Enter a whole number from 0 to 1024.")
    expect(check(valid({ maxSessions: "-1" })).fields.maxSessions).toBe("Enter a whole number from 0 to 1024.")
    expect(check(valid({ maxSessions: "1024" })).fields.maxSessions).toBeUndefined()
  })
})

describe("write-only secrets", () => {
  it("start empty when editing, and an untouched form changes nothing", () => {
    const values = providerFormValues(stored)
    expect(values.key).toBe("")
    expect(values.env.map((r) => [r.name, r.value])).toEqual([
      ["TEAM", ""],
      ["EMPTY", ""],
    ])
    expect(values.maxSessions).toBe("2")
    expect(providerFormChanged(stored, values)).toBe(false)
  })

  it("keep what is left empty: the key, and stored values by name; new names send their value", () => {
    const values = { ...providerFormValues(stored), env: [row("TEAM", ""), row("EMPTY", "now set"), row("NEW", "x")] }
    expect(check(values, stored).env).toEqual({})
    const input = toProviderInput(values, stored)
    expect(input.key).toBe("")
    expect(input.env).toEqual([{ name: "TEAM" }, { name: "EMPTY", value: "now set" }, { name: "NEW", value: "x" }])
    expect(providerFormChanged(stored, values)).toBe(true)
  })

  it("need a value for a name without a stored one: new, or renamed", () => {
    const values = { ...providerFormValues(stored), env: [row("TEAM_ID", "", "renamed"), row("NEW", "", "new")] }
    expect(check(values, stored).env).toEqual({
      renamed: { value: "Enter a value, or mark it Empty." },
      new: { value: "Enter a value, or mark it Empty." },
    })
    expect(check(valid({ env: [row("A", "")] })).env).toEqual({ A: { value: "Enter a value, or mark it Empty." } })
  })

  it("send an empty value chosen on purpose, for any name", () => {
    const empty = (name: string, key = name) => ({ key, name, value: "", empty: true })
    // New provider.
    const created = valid({ env: [empty("FOO")] })
    expect(check(created).env).toEqual({})
    expect(toProviderInput(created).env).toEqual([{ name: "FOO", value: "" }])
    // Renamed, new and stored names alike: "" replaces what is stored.
    const edited = { ...providerFormValues(stored), env: [empty("TEAM_ID", "renamed"), empty("TEAM", "stored")] }
    expect(check(edited, stored).env).toEqual({})
    expect(toProviderInput(edited, stored).env).toEqual([
      { name: "TEAM_ID", value: "" },
      { name: "TEAM", value: "" },
    ])
    // Unreadable secrets too.
    const unreadable = { ...stored, secretsUnreadable: true }
    const reentered = { ...providerFormValues(unreadable), key: "k", env: [empty("TEAM")] }
    expect(check(reentered, unreadable).env).toEqual({})
    // A row with only the choice is not blank: it needs its name.
    expect(check(valid({ env: [empty("", "nameless")] })).env).toEqual({ nameless: { name: "Enter a name." } })
  })

  it("need the key and every value again once the stored secrets can't be read", () => {
    const unreadable = { ...stored, secretsUnreadable: true }
    const values = providerFormValues(unreadable)
    const errors = check(values, unreadable)
    expect(errors.fields.key).toBe("Enter the key.")
    expect(errors.env).toEqual(
      Object.fromEntries(values.env.map((r) => [r.key, { value: "The stored value can't be read. Enter it again, or mark it Empty." }])),
    )
    // Re-entered, or removed: nothing is kept.
    const fixed = { ...values, key: " sk-new ", env: [{ ...values.env[0], value: "platform" }] }
    expect(check(fixed, unreadable).env).toEqual({})
    const input = toProviderInput(fixed, unreadable)
    expect(input.key).toBe("sk-new")
    expect(input.env).toEqual([{ name: "TEAM", value: "platform" }])
  })

  it("send every value of a new provider, and drop rows left blank", () => {
    const input = toProviderInput(valid({ env: [row("A", "1"), row("", ""), row("B", "2")], maxSessions: "" }))
    expect(input.env).toEqual([
      { name: "A", value: "1" },
      { name: "B", value: "2" },
    ])
    expect(input.maxSessions).toBe(0)
  })
})

describe("mapping server errors to the form", () => {
  const context = { others: [], editing: stored }
  const invalid = (field: string) => new ApiError("invalid_argument", "server text", field)

  it("puts field violations on their fields", () => {
    const values = providerFormValues(stored)
    expect(mapProviderServerError(invalid("api.base_url"), values, context).fields).toEqual({
      baseUrl: "This URL can't be used.",
    })
    expect(mapProviderServerError(invalid("api.sonnet_model"), values, context).fields).toEqual({
      sonnetModel: "This model ID can't be used.",
    })
    expect(mapProviderServerError(invalid("api.fable_model"), values, context).fields).toEqual({
      fableModel: "This model ID can't be used.",
    })
    expect(mapProviderServerError(invalid("max_sessions"), values, context).fields).toEqual({
      maxSessions: "Enter a whole number from 0 to 1024.",
    })
    expect(mapProviderServerError(invalid("worker_ids[0]"), values, context).fields).toEqual({
      workerIds: "A chosen worker no longer exists. Choose the workers again.",
    })
    expect(
      mapProviderServerError(new ApiError("already_exists", "x", "name"), { ...values, name: " Proxy " }, context).fields,
    ).toEqual({ name: 'A provider named "Proxy" already exists.' })
  })

  it("says the key is needed when the server couldn't keep an empty one", () => {
    expect(mapProviderServerError(invalid("api.key"), providerFormValues(stored), context).fields).toEqual({
      key: "Enter the key.",
    })
  })

  it("finds a variable's row by its index among the rows sent", () => {
    const values = { ...providerFormValues(), env: [row("", "", "blank"), row("A", "1", "a"), row("B", "", "b")] }
    expect(mapProviderServerError(invalid("env[1].value"), values, { others: [] }).env).toEqual({
      b: { value: "Enter this variable's value again." },
    })
    expect(mapProviderServerError(invalid("env[0].name"), values, { others: [] }).env).toEqual({
      a: { name: "This name can't be used." },
    })
  })

  it("puts what no field shows on the form", () => {
    const values = providerFormValues(stored)
    expect(mapProviderServerError(new ApiError("not_found", "x", "provider_id"), values, context).form).toBe(
      "This provider no longer exists.",
    )
    expect(mapProviderServerError(new ApiError("resource_exhausted", "x"), values, context).form).toBe(
      "You have the most providers allowed (32).",
    )
    expect(mapProviderServerError(new ApiError("unavailable", "x"), values, context).form).toBe(
      "Can't reach the server. Try again.",
    )
  })
})

describe("base URLs, as the server parses them", () => {
  const problem = (baseUrl: string) => validateProviderForm(valid({ baseUrl }), { others: [] }).fields.baseUrl
  it.each([
    ["https://host/%zz", false],
    ["https://host/%4", false],
    ["https://h%41st", false],
    ["https://host:99999", false],
    ["https://host:0", false],
    ["https://host:", false],
    ["https://host:x", false],
    ["https://[::1", false],
    ["https://host/%41%2F", true],
    ["https://host:1", true],
    ["https://host:65535/v1", true],
    ["https://127.0.0.1:8443", true],
    ["http://[::1]:8080/anthropic", true],
  ])("%s accepted: %s", (url, accepted) => {
    expect(problem(url) === undefined).toBe(accepted)
  })
})
