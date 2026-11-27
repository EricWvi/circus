// @vitest-environment happy-dom
// The model providers, a section of the devices page.
import { act, cleanup, fireEvent, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, onTestFinished, vi } from "vitest"

import { ApiError } from "@/api/errors"
import { seedState, type MockState } from "@/api/mock/state"
import { queryKeys } from "@/api/query-client"
import type { CircusClient, ModelProviderService, ProjectService } from "@/api/types"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { addMenuTrigger, chooseAdd } from "@/test/add-menu"
import { advance, renderWithBackend } from "@/test/render"

import { DevicesPage } from "./devices-page"

const NOW = Date.parse("2026-11-22T12:00:00Z")

async function renderProviders(
  state: MockState = seedState(NOW),
  wrap?: (real: ModelProviderService) => Partial<ModelProviderService>,
  entry = "/devices",
  wrapProjects?: (real: ProjectService) => Partial<ProjectService>,
) {
  const router = createMemoryRouter(
    [
      {
        path: "/devices",
        element: (
          <SidebarProvider>
            <DevicesPage />
          </SidebarProvider>
        ),
      },
    ],
    { initialEntries: [entry] },
  )
  const wrapClient = (client: CircusClient): CircusClient => ({
    ...client,
    modelProviders: { ...client.modelProviders, ...wrap?.(client.modelProviders) },
    projects: { ...client.projects, ...wrapProjects?.(client.projects) },
  })
  const result = await renderWithBackend(<RouterProvider router={router} />, { state, wrapClient })
  await advance(50)
  return result
}

/** The providers section (the devices and workers above have rows too). */
const section = () => {
  const element = document.getElementById("providers")
  if (!element) throw new Error("no providers section")
  return within(element)
}

/** The list item of the provider with that name. */
const row = (name: string) => {
  const item = section().getByText(name, { selector: "span.font-medium" }).closest("li")
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

describe("the providers section", () => {
  it("sits below the workers, adding providers only through the header's menu", async () => {
    await renderProviders()
    expect(screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent)).toEqual(["Devices", "Workers", "Providers"])
    expect(section().getByRole("heading", { level: 2, name: "Providers" })).toBeTruthy()
    expect(screen.queryByRole("button", { name: "Add provider" })).toBeNull()
    expect(screen.queryByRole("heading", { level: 1, name: "Providers" })).toBeNull()
  })

  it("shows placeholders while the list loads", async () => {
    await renderProviders(seedState(NOW), () => ({ listProviders: () => new Promise(() => {}) }))
    expect(section().getByRole("list", { name: "Loading providers" }).getAttribute("aria-busy")).toBe("true")
    expect(section().queryByRole("button", { name: "Add provider" })).toBeNull()
  })

  it("offers a retry when the list can't load", async () => {
    let failing = true
    await renderProviders(seedState(NOW), (real) => ({
      listProviders: () => (failing ? Promise.reject(new Error("boom")) : real.listProviders()),
    }))
    expect(section().getByText("Couldn't load providers.")).toBeTruthy()
    expect(section().queryByRole("button", { name: "Add provider" })).toBeNull()
    failing = false
    fireEvent.click(section().getByRole("button", { name: "Try again" }))
    await advance(50)
    expect(row("Anthropic").getByText("default")).toBeTruthy()
  })

  it("is scrolled to and focused from /devices#providers once the lists have loaded", async () => {
    const scrolled = vi.spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {})
    onTestFinished(() => scrolled.mockRestore())
    let release = () => {}
    await renderProviders(
      seedState(NOW),
      (real) => ({
        // The list arrives late: until then the section isn't where it ends up.
        listProviders: () => new Promise((resolve) => (release = () => resolve(real.listProviders()))),
      }),
      "/devices#providers",
    )
    expect(scrolled).not.toHaveBeenCalled()
    release()
    await advance(50)
    const target = document.getElementById("providers")
    expect(scrolled).toHaveBeenCalledTimes(1)
    expect(scrolled.mock.contexts[0]).toBe(target)
    expect(scrolled.mock.calls[0]).toEqual([{ block: "start" }])
    expect(document.activeElement).toBe(target)
    // Later polls leave the user's scrolling alone.
    await advance(60_000)
    expect(scrolled).toHaveBeenCalledTimes(1)
  })

  it("waits for the projects too, which add a line to each device", async () => {
    const scrolled = vi.spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {})
    onTestFinished(() => scrolled.mockRestore())
    let release = () => {}
    await renderProviders(seedState(NOW), undefined, "/devices#providers", (real) => ({
      listProjects: () => new Promise((resolve) => (release = () => resolve(real.listProjects()))),
    }))
    expect(scrolled).not.toHaveBeenCalled()
    release()
    await advance(50)
    expect(scrolled.mock.contexts).toEqual([document.getElementById("providers")])
  })

  it("leaves focus where the user put it while the lists loaded", async () => {
    const scrolled = vi.spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {})
    onTestFinished(() => scrolled.mockRestore())
    let release = () => {}
    await renderProviders(
      seedState(NOW),
      (real) => ({ listProviders: () => new Promise((resolve) => (release = () => resolve(real.listProviders()))) }),
      "/devices#providers",
    )
    const menu = screen.getAllByRole("button", { name: "Actions for devbox" })[0]
    menu.focus()
    release()
    await advance(50)
    expect(scrolled).toHaveBeenCalledTimes(1)
    expect(document.activeElement).toBe(menu)
  })

  it("ignores a hash that isn't a valid escape", async () => {
    const scrolled = vi.spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {})
    onTestFinished(() => scrolled.mockRestore())
    await renderProviders(seedState(NOW), undefined, "/devices#%E0")
    expect(screen.getByRole("heading", { level: 1, name: "Devices" })).toBeTruthy()
    expect(row("Anthropic").getByText("default")).toBeTruthy()
    expect(scrolled).not.toHaveBeenCalled()
  })

  it("returns focus to the button that opened a provider dialog, or to Add provider once it is gone", async () => {
    await renderProviders()
    const edit = row("Team proxy").getByRole("button", { name: "Edit Team proxy" })
    fireEvent.click(edit)
    await advance(50)
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" })
    await advance(300)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(document.activeElement).toBe(edit)

    const remove = row("Anthropic").getByRole("button", { name: "Delete Anthropic" })
    fireEvent.click(remove)
    await advance(0)
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Cancel" }))
    await advance(300)
    expect(document.activeElement).toBe(remove)

    fireEvent.click(row("Team proxy").getByRole("button", { name: "Delete Team proxy" }))
    await advance(0)
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Delete" }))
    await advance(300)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(section().queryByText("Team proxy")).toBeNull()
    // The deleted row's button is gone, and the list isn't empty: the header's menu.
    expect(document.activeElement).toBe(addMenuTrigger())
  })

  it("adds a worker from the form when there are none, with the worker role chosen", async () => {
    const state = seedState(NOW)
    state.workers = []
    await renderProviders(state)
    const add = addMenuTrigger()
    await chooseAdd("Add model provider")
    expect(dialog().getByText(/No workers yet/).textContent).toBe("No workers yet. Add a worker first.")
    fireEvent.click(dialog().getByRole("button", { name: "Add a worker" }))
    await advance(300)
    // The form closed for the add dialog.
    expect(screen.getAllByRole("dialog")).toHaveLength(1)
    const addWorker = within(screen.getByRole("dialog", { name: "Add worker" }))
    expect(addWorker.getByRole("radio", { name: /^Worker/ })).toHaveProperty("checked", true)
    expect(document.activeElement).not.toBe(add)
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" })
    await advance(300)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(document.activeElement).toBe(add)
  })

  it("says so in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    const state = seedState(NOW)
    state.workers = []
    await renderProviders(state)
    await chooseAdd("添加模型提供方", "添加")
    expect(dialog().getByText(/还没有工作节点/).textContent).toBe("还没有工作节点。请先添加工作节点。")
    fireEvent.click(dialog().getByRole("button", { name: "添加工作节点" }))
    await advance(300)
    expect(screen.getByRole("dialog", { name: "添加工作节点" })).toBeTruthy()
  })

  it("is not scrolled to without the hash", async () => {
    const scrolled = vi.spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {})
    onTestFinished(() => scrolled.mockRestore())
    await renderProviders()
    expect(scrolled).not.toHaveBeenCalled()
    expect(document.activeElement).toBe(document.body)
  })
})

describe("the provider list", () => {
  it("shows an empty state when there are none", async () => {
    const state = seedState(NOW)
    state.providers = []
    await renderProviders(state)
    expect(screen.getByText("No providers yet")).toBeTruthy()
    expect(screen.queryByText(/Without one, sessions use the worker machine's own Claude login/)).toBeNull()
    // The empty state keeps its own button.
    const add = section().getByRole("button", { name: "Add provider" })
    fireEvent.click(add)
    await advance(50)
    expect(screen.getByRole("dialog", { name: "Add provider" })).toBeTruthy()
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" })
    await advance(300)
    expect(document.activeElement).toBe(add)
  })

  it("returns focus to the empty state's Add provider once the last one is deleted", async () => {
    const state = seedState(NOW)
    state.providers = state.providers.filter((p) => p.name === "Team proxy")
    await renderProviders(state)
    fireEvent.click(row("Team proxy").getByRole("button", { name: "Delete Team proxy" }))
    await advance(0)
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Delete" }))
    await advance(300)
    expect(document.activeElement).toBe(section().getByRole("button", { name: "Add provider" }))
  })

  it("shows each provider's endpoint, masked key, workers, sessions and default", async () => {
    const state = seedState(NOW)
    state.sessions[0].providerId = "provider-proxy"
    state.sessions[0].leased = true
    await renderProviders(state)
    const names = section().getAllByText(/./, { selector: "li > div > span.font-medium" }).map((e) => e.textContent)
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
    expect(section().getByRole("heading", { level: 2, name: "模型提供方" })).toBeTruthy()
    expect(row("Anthropic").getByText("会话 0 / 不限")).toBeTruthy()
    expect(row("Team proxy").getByText("2 个变量")).toBeTruthy()
    expect(row("Anthropic").getByText("密钥 ••••7Qa2")).toBeTruthy()
    // Lists join the Chinese way.
    const state = seedState(NOW)
    cleanup()
    state.workers.find((w) => w.name === "devbox")!.localDevices = ["devbox", "eric-mbp"]
    await renderProviders(state)
    await chooseAdd("添加模型提供方", "添加")
    await advance(50)
    expect(dialog().getByText("与 devbox、eric-mbp 在同一台机器上")).toBeTruthy()
  })
})

describe("adding a provider", () => {
  it("checks the form before sending, then adds it", async () => {
    const { state } = await renderProviders()
    await chooseAdd("Add model provider")
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
    await chooseAdd("Add model provider")
    type("Name", "Staging")
    type("Key", "sk-staging-0000")
    fireEvent.click(dialog().getByRole("checkbox", { name: /Use as my default/ }))
    await save("Create")
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(section().getAllByText("default").map((e) => e.closest("li")?.querySelector("span.font-medium")?.textContent)).toEqual([
      "Staging",
    ])
  })

  it("shows each worker's state and the devices on its machine", async () => {
    await renderProviders()
    await chooseAdd("Add model provider")
    await advance(50)
    const win = dialog().getByRole("checkbox", { name: /eric-win/ }).closest("label")!
    expect(within(win).getByText("Offline")).toBeTruthy()
    expect(within(win).getByText("same machine as eric-win")).toBeTruthy()
    const cloud = dialog().getByRole("checkbox", { name: /cloud-runner/ }).closest("label")!
    expect(within(cloud).getByText("Online")).toBeTruthy()
    expect(within(cloud).queryByText(/same machine/)).toBeNull()
  })
})

describe("the agent of a provider", () => {
  const agents = () => dialog().getByRole("radiogroup", { name: "Agent" })
  const agent = (name: string) => within(agents()).getByRole("radio", { name })
  const checked = () => within(agents()).getByRole("radio", { checked: true })
  /** The form's fields show: a textbox finds no hidden element. */
  const fieldsShown = () => dialog().queryByRole("textbox", { name: "Name" }) !== null
  const createButton = () => dialog().getByRole("button", { name: "Create" }) as HTMLButtonElement
  /** What an element's aria-describedby points at, as text. */
  const description = (element: HTMLElement) =>
    (element.getAttribute("aria-describedby") ?? "")
      .split(" ")
      .map((id) => document.getElementById(id)?.textContent ?? "")
      .join(" ")

  it("starts at Claude Code; Codex and opencode say they aren't available yet", async () => {
    const { state } = await renderProviders()
    const before = state.providers.length
    await chooseAdd("Add model provider")
    expect(within(agents()).getAllByRole("radio").map((r) => r.textContent)).toEqual(["Claude Code", "Codex", "opencode"])
    expect(checked().textContent).toBe("Claude Code")
    // The marks are decorative: each option is named by its text alone.
    for (const radio of within(agents()).getAllByRole("radio")) {
      expect(radio.querySelector("[data-agent-mark]")?.getAttribute("aria-hidden")).toBe("true")
    }
    expect(fieldsShown()).toBe(true)
    expect(agent("Claude Code").getAttribute("aria-describedby")).toBeNull()
    // A complete Claude Code provider, which could be created as it is.
    type("Name", "Staging")
    type("Key", "sk-staging-0000")

    for (const name of ["Codex", "opencode"]) {
      fireEvent.click(agent(name))
      expect(checked().textContent).toBe(name)
      expect(fieldsShown()).toBe(false)
      expect(dialog().queryByRole("checkbox")).toBeNull()
      // The body says why, and the chosen option and the button point at it.
      const message = `Providers for ${name} aren't available yet.`
      expect(dialog().getByText(message)).toBeTruthy()
      expect(description(agent(name))).toBe(message)
      expect(description(createButton())).toBe(message)
      expect(createButton().disabled).toBe(true)
      // Neither the button nor a submit (as Enter in the form makes) creates it.
      fireEvent.click(createButton())
      fireEvent.submit(createButton().closest("form")!)
      await advance(50)
      expect(state.providers.length).toBe(before)
      expect(screen.getByRole("dialog")).toBeTruthy()
    }

    // Back at Claude Code, the fields are as they were left.
    fireEvent.click(agent("Claude Code"))
    expect(fieldsShown()).toBe(true)
    expect(input("Name").value).toBe("Staging")
    expect(dialog().queryByText(/aren't available yet/)).toBeNull()
    expect(createButton().disabled).toBe(false)
    await save("Create")
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(stored(state, "Staging")).toBeTruthy()
  })

  it("is one radio group for the keyboard", async () => {
    /** A key pressed and released, with the focus moves it makes in between. */
    const press = async (target: HTMLElement, key: string) => {
      fireEvent.keyDown(target, { key })
      await advance(0)
      fireEvent.keyUp(target, { key })
    }
    await renderProviders()
    await chooseAdd("Add model provider")
    // The form starts at the name, below the choice.
    expect(document.activeElement).toBe(input("Name"))
    // Leaving the empty name for the choice isn't an error yet. Tabbing
    // into the group lands on the chosen option.
    fireEvent.blur(input("Name"))
    agents().focus()
    await advance(0)
    expect(document.activeElement).toBe(agent("Claude Code"))
    expect(dialog().queryByText("Enter a provider name.")).toBeNull()
    await press(agent("Claude Code"), "ArrowRight")
    expect(document.activeElement).toBe(agent("Codex"))
    expect(checked().textContent).toBe("Codex")
    expect(fieldsShown()).toBe(false)

    await press(agent("Codex"), "ArrowRight")
    expect(checked().textContent).toBe("opencode")
    // It wraps around.
    await press(agent("opencode"), "ArrowRight")
    expect(checked().textContent).toBe("Claude Code")
    await press(agent("Claude Code"), "ArrowLeft")
    expect(document.activeElement).toBe(agent("opencode"))
    expect(checked().textContent).toBe("opencode")
    // Up and down move too.
    await press(agent("opencode"), "ArrowUp")
    expect(checked().textContent).toBe("Codex")
    await press(agent("Codex"), "ArrowDown")
    expect(checked().textContent).toBe("opencode")
    // Only the chosen option is in the tab order.
    expect(within(agents()).getAllByRole("radio").map((r) => r.tabIndex)).toEqual([-1, -1, 0])

    // Enter selects the focused option, and doesn't submit the form.
    agent("Claude Code").focus()
    await press(agent("Claude Code"), "Enter")
    await advance(50)
    expect(checked().textContent).toBe("Claude Code")
    expect(fieldsShown()).toBe(true)
    expect(dialog().queryByText("Enter a provider name.")).toBeNull()
    // Space is each option's own activation as a button, which happy-dom
    // doesn't emulate.
  })

  it("can't change while the provider is being created", async () => {
    let release = () => {}
    const { state } = await renderProviders(seedState(NOW), (real) => ({
      createProvider: (req) => new Promise((resolve) => (release = () => resolve(real.createProvider(req)))),
    }))
    await chooseAdd("Add model provider")
    type("Name", "Staging")
    type("Key", "sk-staging-0000")
    await save("Create")
    for (const radio of within(agents()).getAllByRole("radio")) expect((radio as HTMLButtonElement).disabled).toBe(true)
    fireEvent.click(agent("Codex"))
    fireEvent.keyDown(agent("Codex"), { key: "Enter" })
    expect(checked().textContent).toBe("Claude Code")
    expect(fieldsShown()).toBe(true)
    release()
    await advance(50)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(stored(state, "Staging")).toBeTruthy()
  })

  it("keeps a saved provider on Claude Code", async () => {
    await renderProviders()
    fireEvent.click(row("Team proxy").getByRole("button", { name: "Edit Team proxy" }))
    expect(dialog().queryByRole("radiogroup")).toBeNull()
    expect(dialog().queryByRole("radio")).toBeNull()
    const shown = dialog().getByRole("group", { name: "Agent" })
    expect(shown.textContent).toBe("AgentClaude Code")
    expect(fieldsShown()).toBe(true)
    expect((dialog().getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false)
  })

  it("reads in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderProviders()
    await chooseAdd("添加模型提供方", "添加")
    expect(checked().textContent).toBe("Claude Code")
    fireEvent.click(agent("Codex"))
    expect(dialog().queryByRole("textbox", { name: "名称" })).toBeNull()
    expect((dialog().getByRole("button", { name: "创建" }) as HTMLButtonElement).disabled).toBe(true)
    expect(dialog().getByText("暂不支持为 Codex 添加提供方。")).toBeTruthy()
    expect(description(agent("Codex"))).toBe("暂不支持为 Codex 添加提供方。")
    fireEvent.click(agent("opencode"))
    expect(dialog().getByText("暂不支持为 opencode 添加提供方。")).toBeTruthy()
    await i18n.changeLanguage("en")
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
    await chooseAdd("Add model provider")
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
    await chooseAdd("Add model provider")
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
    expect(section().queryByText("Team proxy")).toBeNull()
  })
})
