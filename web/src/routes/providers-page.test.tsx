// @vitest-environment happy-dom
import { act, cleanup, fireEvent, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/api/errors"
import { seedState, type MockState } from "@/api/mock/state"
import { queryKeys } from "@/api/query-client"
import type { CircusClient, ModelProviderService } from "@/api/types"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { advance, renderWithBackend } from "@/test/render"

import { ProvidersPage } from "./providers-page"

const NOW = Date.parse("2026-11-22T12:00:00Z")

async function renderProviders(
  state: MockState = seedState(NOW),
  wrap?: (real: ModelProviderService) => Partial<ModelProviderService>,
) {
  const router = createMemoryRouter(
    [
      {
        path: "/providers",
        element: (
          <SidebarProvider>
            <ProvidersPage />
          </SidebarProvider>
        ),
      },
      { path: "/devices", element: <h1>Devices page</h1> },
    ],
    { initialEntries: ["/providers"] },
  )
  const wrapClient = (client: CircusClient): CircusClient =>
    wrap ? { ...client, modelProviders: { ...client.modelProviders, ...wrap(client.modelProviders) } } : client
  const result = await renderWithBackend(<RouterProvider router={router} />, { state, wrapClient })
  await advance(50)
  return result
}

/** The list item of the provider with that name. */
const row = (name: string) => {
  const item = screen.getByText(name, { selector: "span.font-medium" }).closest("li")
  if (!item) throw new Error(`no row for ${name}`)
  return within(item)
}
const dialog = () => within(screen.getByRole("dialog"))
const input = (label: string | RegExp) => dialog().getByLabelText<HTMLInputElement>(label)
const type = (label: string | RegExp, value: string) => fireEvent.change(input(label), { target: { value } })
const stored = (state: MockState, name: string) => state.providers.find((p) => p.name === name)!

async function save(name: "Save" | "Create" = "Save") {
  fireEvent.click(dialog().getByRole("button", { name }))
  await advance(50)
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(NOW)
})
afterEach(async () => {
  cleanup()
  vi.useRealTimers()
  await i18n.changeLanguage("en")
})

describe("the provider list", () => {
  it("explains providers when there are none", async () => {
    const state = seedState(NOW)
    state.providers = []
    await renderProviders(state)
    expect(screen.getByText("No providers yet")).toBeTruthy()
    expect(screen.getByText(/Without one, sessions use the worker machine's own Claude login/)).toBeTruthy()
    fireEvent.click(screen.getByRole("button", { name: "Add provider" }))
    expect(screen.getByRole("dialog", { name: "Add provider" })).toBeTruthy()
  })

  it("shows each provider's endpoint, masked key, workers, sessions and default", async () => {
    const state = seedState(NOW)
    state.sessions[0].providerId = "provider-proxy"
    state.sessions[0].leased = true
    await renderProviders(state)
    const names = screen.getAllByText(/./, { selector: "li > div > span.font-medium" }).map((e) => e.textContent)
    expect(names).toEqual(["Anthropic", "Legacy key", "Team proxy"])

    const anthropic = row("Anthropic")
    expect(anthropic.getByText("default")).toBeTruthy()
    expect(anthropic.getByText("Default endpoint")).toBeTruthy()
    expect(anthropic.getByText("x-api-key")).toBeTruthy()
    expect(anthropic.getByText("Key ••••7Qa2")).toBeTruthy()
    expect(anthropic.getByText("Sessions 0 / unlimited")).toBeTruthy()
    expect(anthropic.getAllByRole("listitem").map((li) => li.textContent)).toEqual(["cloud-runner", "devbox", "eric-mbp"])

    const proxy = row("Team proxy")
    expect(proxy.queryByText("default")).toBeNull()
    expect(proxy.getByText("https://llm-proxy.example.com/anthropic")).toBeTruthy()
    expect(proxy.getByText("Bearer")).toBeTruthy()
    expect(proxy.getByText("Key ••••k9Qz")).toBeTruthy()
    expect(proxy.getByText("2 variables")).toBeTruthy()
    expect(proxy.getByText("Sessions 1 / 2")).toBeTruthy()
    // Secrets never reach the page.
    expect(document.body.textContent).not.toContain("mock-proxy-token")
    expect(document.body.textContent).not.toContain("platform-web")
  })

  it("warns about secrets that can't be read, and opens the form to enter them", async () => {
    await renderProviders()
    const legacy = row("Legacy key")
    expect(legacy.getByRole("alert").textContent).toContain("can't be read")
    expect(row("Anthropic").queryByRole("alert")).toBeNull()
    fireEvent.click(legacy.getByRole("button", { name: "Enter again" }))
    expect(input("Key").placeholder).toBe("")
    expect(dialog().getByText("The stored key can't be read. Enter it again.")).toBeTruthy()
    expect(input("Value of variable 1").placeholder).toBe("Enter the value again")
  })

  it("needs the key and every value again before saving unreadable secrets", async () => {
    const { state } = await renderProviders()
    fireEvent.click(row("Legacy key").getByRole("button", { name: "Enter again" }))
    type("Key", "sk-legacy-new-1234")
    await save()
    expect(dialog().getByText("The stored value can't be read. Enter it again, or mark it Empty.")).toBeTruthy()
    expect(stored(state, "Legacy key").secretsUnreadable).toBe(true)
    type("Value of variable 1", "eu-central")
    await save()
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(stored(state, "Legacy key")).toMatchObject({
      key: "sk-legacy-new-1234",
      env: [{ name: "LEGACY_REGION", value: "eu-central" }],
      secretsUnreadable: false,
    })
  })

  it("reads in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderProviders()
    expect(screen.getByRole("heading", { level: 1, name: "模型提供方" })).toBeTruthy()
    expect(row("Anthropic").getByText("会话 0 / 不限")).toBeTruthy()
    expect(row("Team proxy").getByText("2 个变量")).toBeTruthy()
    expect(row("Anthropic").getByText("密钥 ••••7Qa2")).toBeTruthy()
    // Lists join the Chinese way.
    const state = seedState(NOW)
    cleanup()
    state.workers.find((w) => w.name === "devbox")!.localDevices = ["devbox", "eric-mbp"]
    await renderProviders(state)
    fireEvent.click(screen.getByRole("button", { name: "添加提供方" }))
    await advance(50)
    expect(dialog().getByText("与 devbox、eric-mbp 在同一台机器上")).toBeTruthy()
  })
})

describe("adding a provider", () => {
  it("checks the form before sending, then adds it", async () => {
    const { state } = await renderProviders()
    fireEvent.click(screen.getByRole("button", { name: "Add provider" }))
    await save("Create")
    expect(dialog().getByText("Enter a provider name.")).toBeTruthy()
    expect(dialog().getByText("Enter the key.")).toBeTruthy()

    type("Name", "anthropic")
    expect(dialog().getByText('A provider named "anthropic" already exists.')).toBeTruthy()
    type("Name", "Staging")
    type("Base URL", "https://staging.example.com/anthropic")
    type("Key", "sk-staging-0000-abcd")
    type("Sonnet", "staging sonnet")
    fireEvent.click(dialog().getByRole("button", { name: "Add variable" }))
    type("Name of variable 1", "lower_case")
    type("Value of variable 1", "1")
    fireEvent.change(input("Session limit"), { target: { value: "x" } })
    await save("Create")
    expect(dialog().getByText("Model IDs can't contain spaces or control characters.")).toBeTruthy()
    expect(dialog().getByText("Use upper-case letters, digits and _, not starting with a digit.")).toBeTruthy()
    expect(dialog().getByText("Enter a whole number from 0 to 1024.")).toBeTruthy()

    type("Sonnet", "staging-sonnet")
    type("Name of variable 1", "STAGE")
    type("Session limit", "3")
    fireEvent.click(dialog().getByRole("checkbox", { name: /devbox/ }))
    fireEvent.click(dialog().getByRole("checkbox", { name: /Use as my default/ }))
    await save("Create")
    expect(screen.queryByRole("dialog")).toBeNull()

    const added = stored(state, "Staging")
    expect(added).toMatchObject({
      baseUrl: "https://staging.example.com/anthropic",
      key: "sk-staging-0000-abcd",
      sonnetModel: "staging-sonnet",
      env: [{ name: "STAGE", value: "1" }],
      workerIds: ["worker-devbox"],
      maxSessions: 3,
      isDefault: true,
    })
    // The default moves.
    expect(stored(state, "Anthropic").isDefault).toBe(false)
    expect(row("Staging").getByText("default")).toBeTruthy()
    expect(row("Anthropic").queryByText("default")).toBeNull()
    expect(row("Staging").getByText("Key ••••abcd")).toBeTruthy()
  })

  it("moves the default mark in the list at once, before the list is read again", async () => {
    let reads = 0
    await renderProviders(seedState(NOW), (real) => ({
      // Only the first read answers: what shows next comes from the cache.
      listProviders: () => (++reads > 1 ? new Promise(() => {}) : real.listProviders()),
    }))
    fireEvent.click(screen.getByRole("button", { name: "Add provider" }))
    type("Name", "Staging")
    type("Key", "sk-staging-0000")
    fireEvent.click(dialog().getByRole("checkbox", { name: /Use as my default/ }))
    await save("Create")
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(screen.getAllByText("default").map((e) => e.closest("li")?.querySelector("span.font-medium")?.textContent)).toEqual([
      "Staging",
    ])
  })

  it("shows each worker's state and the devices on its machine", async () => {
    await renderProviders()
    fireEvent.click(screen.getByRole("button", { name: "Add provider" }))
    await advance(50)
    const win = dialog().getByRole("checkbox", { name: /eric-win/ }).closest("label")!
    expect(within(win).getByText("Offline")).toBeTruthy()
    expect(within(win).getByText("same machine as eric-win")).toBeTruthy()
    const cloud = dialog().getByRole("checkbox", { name: /cloud-runner/ }).closest("label")!
    expect(within(cloud).getByText("Online")).toBeTruthy()
    expect(within(cloud).queryByText(/same machine/)).toBeNull()
  })
})

describe("editing a provider", () => {
  it("starts with empty secrets that keep the stored ones", async () => {
    const { state } = await renderProviders()
    fireEvent.click(row("Team proxy").getByRole("button", { name: "Edit Team proxy" }))
    expect(input("Name").value).toBe("Team proxy")
    expect(input("Key").value).toBe("")
    expect(input("Key").placeholder).toBe("Stored: ••••k9Qz")
    expect(dialog().getByText("Leave empty to keep the stored key.")).toBeTruthy()
    expect(input("Name of variable 1").value).toBe("DISABLE_TELEMETRY")
    expect(input("Value of variable 1").value).toBe("")
    expect(input("Value of variable 1").placeholder).toBe("Stored: ••••1")
    expect(input("Value of variable 2").placeholder).toBe("Stored: ••••-web")
    expect(dialog().getByText("Leave a stored value empty to keep it.")).toBeTruthy()
    expect(input("Session limit").value).toBe("2")

    type("Name", "Team proxy 2")
    type("Value of variable 2", "platform-api")
    // A renamed variable is a new one: it needs its value, not an empty one.
    type("Name of variable 1", "NO_TELEMETRY")
    expect(input("Value of variable 1").placeholder).toBe("Value")
    await save()
    expect(dialog().getByText("Enter a value, or mark it Empty.")).toBeTruthy()
    expect(stored(state, "Team proxy").env[0]).toEqual({ name: "DISABLE_TELEMETRY", value: "1" })
    type("Value of variable 1", "1")
    await save()
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(stored(state, "Team proxy 2")).toMatchObject({
      key: "mock-proxy-token-k9Qz",
      env: [
        { name: "NO_TELEMETRY", value: "1" },
        { name: "PROXY_TEAM", value: "platform-api" },
      ],
    })
  })

  it("saves an empty value only when chosen on purpose", async () => {
    const { state } = await renderProviders()
    fireEvent.click(screen.getByRole("button", { name: "Add provider" }))
    type("Name", "Staging")
    type("Key", "sk-staging-0000")
    fireEvent.click(dialog().getByRole("button", { name: "Add variable" }))
    type("Name of variable 1", "FOO")
    await save("Create")
    expect(dialog().getByText("Enter a value, or mark it Empty.")).toBeTruthy()
    const empty = dialog().getByRole("button", { name: "Empty value for variable 1" })
    expect(empty.getAttribute("aria-pressed")).toBe("false")
    fireEvent.click(empty)
    expect(empty.getAttribute("aria-pressed")).toBe("true")
    expect(input("Value of variable 1").placeholder).toBe("Empty value")
    expect(dialog().queryByText("Enter a value, or mark it Empty.")).toBeNull()
    await save("Create")
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(stored(state, "Staging").env).toEqual([{ name: "FOO", value: "" }])
  })

  it("masks secrets without making them a login password, and shows them on request", async () => {
    await renderProviders()
    fireEvent.click(row("Team proxy").getByRole("button", { name: "Edit Team proxy" }))
    const secrets = [input("Key"), input("Value of variable 1"), input("Value of variable 2")]
    for (const field of secrets) {
      expect(field.type).toBe("text")
      expect(field.getAttribute("autocomplete")).toBe("off")
      expect(field.hasAttribute("data-1p-ignore")).toBe(true)
      expect(field.getAttribute("data-lpignore")).toBe("true")
      expect(field.hasAttribute("data-secret-masked")).toBe(true)
    }
    expect(screen.getByRole("dialog").querySelector("input[type=password]")).toBeNull()
    const keyGroup = input("Key").closest("[data-slot=input-group]")!
    const toggle = within(keyGroup as HTMLElement).getByRole("button", { name: "Show secret" })
    fireEvent.click(toggle)
    expect(input("Key").hasAttribute("data-secret-masked")).toBe(false)
    expect(toggle.getAttribute("aria-pressed")).toBe("true")
    // One announcement of the state: the name stays.
    expect(toggle.getAttribute("aria-label")).toBe("Show secret")
    fireEvent.click(toggle)
    expect(toggle.getAttribute("aria-pressed")).toBe("false")
    expect(input("Key").hasAttribute("data-secret-masked")).toBe(true)
    for (const field of secrets) {
      expect([field.getAttribute("autocapitalize"), field.getAttribute("autocorrect"), field.getAttribute("spellcheck")]).toEqual([
        "off",
        "off",
        "false",
      ])
    }
    // Only that field.
    expect(input("Value of variable 1").hasAttribute("data-secret-masked")).toBe(true)
  })

  it("keeps no secret in the mutation cache once saved", async () => {
    const { queryClient } = await renderProviders()
    fireEvent.click(screen.getByRole("button", { name: "Add provider" }))
    type("Name", "Staging")
    type("Key", "sk-staging-secret-0000")
    await save("Create")
    expect(screen.queryByRole("dialog")).toBeNull()
    await advance(50)
    expect(JSON.stringify(queryClient.getMutationCache().getAll().map((m) => m.state.variables))).not.toContain(
      "sk-staging-secret",
    )
  })

  it("takes off assigned workers that no longer exist, saying so", async () => {
    const { state, queryClient } = await renderProviders()
    fireEvent.click(row("Team proxy").getByRole("button", { name: "Edit Team proxy" }))
    // devbox's worker is deleted while the form is open; the list is read again.
    state.workers = state.workers.filter((w) => w.workerId !== "worker-devbox")
    fireEvent.click(dialog().getByRole("checkbox", { name: /eric-mbp/ }))
    act(() => void queryClient.invalidateQueries({ queryKey: queryKeys.workers }))
    await advance(50)
    expect(dialog().getByText("1 assigned worker no longer exists. It is taken off when you save.")).toBeTruthy()
    await save()
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(stored(state, "Team proxy").workerIds).toEqual(["worker-eric-mbp"])
  })

  it("asks for a scheme it doesn't know instead of changing it", async () => {
    const { state } = await renderProviders(seedState(NOW), (real) => ({
      listProviders: async () =>
        (await real.listProviders()).map((p) => (p.name === "Team proxy" ? { ...p, scheme: "unspecified" as const } : p)),
    }))
    expect(row("Team proxy").getByText("Unknown scheme")).toBeTruthy()
    fireEvent.click(row("Team proxy").getByRole("button", { name: "Edit Team proxy" }))
    expect(dialog().getByRole("combobox", { name: "Authentication" }).textContent).toBe("Choose…")
    type("Name", "Proxy")
    await save()
    expect(dialog().getByText("Choose how the key is sent.")).toBeTruthy()
    expect(stored(state, "Team proxy")).toBeTruthy()
  })

  it("puts the server's field violations on the fields", async () => {
    await renderProviders(seedState(NOW), () => ({
      updateProvider: () => Promise.reject(new ApiError("invalid_argument", "server text", "env[1].name")),
    }))
    fireEvent.click(row("Team proxy").getByRole("button", { name: "Edit Team proxy" }))
    type("Name", "Proxy")
    await save()
    const name2 = input("Name of variable 2")
    expect(name2.getAttribute("aria-invalid")).toBe("true")
    expect(dialog().getByText("This name can't be used.")).toBeTruthy()
    expect(document.body.textContent).not.toContain("server text")
    // Editing that row takes its error away.
    type("Name of variable 2", "PROXY_GROUP")
    expect(dialog().queryByText("This name can't be used.")).toBeNull()
  })

  it("says so when the provider changed elsewhere, and reloads it", async () => {
    const { backend } = await renderProviders()
    fireEvent.click(row("Team proxy").getByRole("button", { name: "Edit Team proxy" }))
    type("Session limit", "5")

    // Another tab renames it meanwhile.
    vi.setSystemTime(NOW + 60_000)
    const elsewhere = backend.client.modelProviders.getProvider({ providerId: "provider-proxy" }).then((p) =>
      backend.client.modelProviders.updateProvider({
        ...p,
        scheme: "bearer",
        key: "",
        env: p.env.map((v) => ({ name: v.name })),
        name: "Shared proxy",
      }),
    )
    await advance(50)
    await elsewhere

    await save()
    expect(dialog().getByRole("alert").textContent).toContain("This provider was changed elsewhere.")
    expect(dialog().getByRole("button", { name: "Save" }).hasAttribute("disabled")).toBe(true)
    fireEvent.click(dialog().getByRole("button", { name: "Discard edits and reload" }))
    await advance(50)
    expect(input("Name").value).toBe("Shared proxy")
    expect(input("Session limit").value).toBe("2")
    type("Session limit", "5")
    await save()
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(row("Shared proxy").getByText("Sessions 0 / 5")).toBeTruthy()
  })
})

describe("deleting a provider", () => {
  it("asks first, and says why it can't while sessions use it", async () => {
    const state = seedState(NOW)
    const session = state.sessions.find((s) => s.state !== "stopped")!
    session.providerId = "provider-proxy"
    session.leased = true
    await renderProviders(state)
    fireEvent.click(row("Team proxy").getByRole("button", { name: "Delete Team proxy" }))
    const confirm = within(screen.getByRole("dialog", { name: "Delete Team proxy?" }))
    fireEvent.click(confirm.getByRole("button", { name: "Delete" }))
    await advance(50)
    expect(confirm.getByRole("alert").textContent).toBe(
      "Sessions using this provider are running. Stop them or switch them to another provider, then try again.",
    )
    expect(state.providers.some((p) => p.providerId === "provider-proxy")).toBe(true)

    session.leased = false
    fireEvent.click(confirm.getByRole("button", { name: "Delete" }))
    await advance(50)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(screen.queryByText("Team proxy")).toBeNull()
  })
})
