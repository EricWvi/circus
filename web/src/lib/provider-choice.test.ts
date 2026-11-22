import { describe, expect, it } from "vitest"

import type { ModelProvider } from "@/api/types"

import {
  choiceIsLocalLogin,
  DEFAULT_CHOICE,
  defaultChoiceLabel,
  initialChoice,
  LOCAL_LOGIN,
  requestedProvider,
  sessionProviderName,
} from "./provider-choice"

const provider = (providerId: string, name: string, isDefault = false) => ({ providerId, name, isDefault }) as ModelProvider
const providers = [provider("a", "Anthropic", true), provider("p", "Proxy")]

describe("the provider of a new session", () => {
  it("labels the default after the project's provider, else the user's", () => {
    expect(defaultChoiceLabel({ defaultProviderId: "p" }, providers)).toBe("Project default (Proxy)")
    expect(defaultChoiceLabel({ defaultProviderId: "" }, providers)).toBe("My default (Anthropic)")
    // A project default naming a deleted provider counts as unset.
    expect(defaultChoiceLabel({ defaultProviderId: "gone" }, providers)).toBe("My default (Anthropic)")
    expect(defaultChoiceLabel({ defaultProviderId: "" }, [provider("p", "Proxy")])).toBeNull()
  })

  it("starts at the default, which is not the explicit local login choice", () => {
    expect(initialChoice).toBe(DEFAULT_CHOICE)
    expect(initialChoice).not.toBe(LOCAL_LOGIN)
  })

  it("names no provider until one is picked, and an empty one for local login", () => {
    expect(requestedProvider(null)).toBeUndefined()
    expect(requestedProvider(DEFAULT_CHOICE)).toBeUndefined()
    expect(requestedProvider(LOCAL_LOGIN)).toBe("")
    expect(requestedProvider("p")).toBe("p")
  })

  it("knows when the session will use local login", () => {
    expect(choiceIsLocalLogin(null, { defaultProviderId: "" }, providers)).toBe(false)
    expect(choiceIsLocalLogin(null, { defaultProviderId: "" }, [])).toBe(true)
    expect(choiceIsLocalLogin(LOCAL_LOGIN, { defaultProviderId: "p" }, providers)).toBe(true)
    expect(choiceIsLocalLogin("p", { defaultProviderId: "" }, [])).toBe(false)
  })
})

describe("a session's provider", () => {
  it("reads as its name, local login, or a deleted provider once the list is known", () => {
    expect(sessionProviderName("p", providers)).toEqual({ label: "Proxy", deleted: false })
    expect(sessionProviderName("", providers)).toEqual({ label: "Local login", deleted: false })
    expect(sessionProviderName("gone", providers)).toEqual({ label: "Deleted provider", deleted: true })
    expect(sessionProviderName("gone", undefined)).toEqual({ label: "", deleted: false })
  })
})
