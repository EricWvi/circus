// @vitest-environment happy-dom
import { cleanup, fireEvent, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/api/errors"
import { DEVICES_LIVE_INTERVAL_MS } from "@/api/hooks"
import { SEED_OWNER_ID, seedState, type MockDevice, type MockState } from "@/api/mock/state"
import type { CircusClient, Device, EnrollLink, EnrollRole } from "@/api/types"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { DevicesPage } from "@/routes/devices-page"
import { advance, renderWithBackend } from "@/test/render"

const NOW = Date.parse("2026-11-17T12:00:00Z")
const SECOND = 1000
const MINUTE = 60 * SECOND
const HOUR = 60 * MINUTE
const LINK = "https://circus.example.com/join#tok"

type Issue = (role: EnrollRole) => Promise<EnrollLink>
type ListDevices = () => Promise<Device[]>

/**
 * The devices page on the demo data. `issue` answers createEnrollLink in
 * place of the mock (which answers as a development build); the roles it
 * was asked for are in `requested`. `listDevices` wraps the mock's list.
 */
async function renderPage({
  state = seedState(NOW),
  issue,
  listDevices,
}: { state?: MockState; issue?: Issue; listDevices?: (real: ListDevices) => ListDevices } = {}) {
  const requested: EnrollRole[] = []
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
    { initialEntries: ["/devices"] },
  )
  const wrapClient = (client: CircusClient): CircusClient => ({
    ...client,
    machines: {
      ...client.machines,
      listDevices: listDevices ? listDevices(client.machines.listDevices) : client.machines.listDevices,
      createEnrollLink: (req) => {
        requested.push(req.role)
        return issue ? issue(req.role) : client.machines.createEnrollLink(req)
      },
    },
  })
  const result = await renderWithBackend(<RouterProvider router={router} />, { state, wrapClient })
  await advance(50)
  return { ...result, requested }
}

/** When the last link was issued, by the client's clock. */
let issuedAt = 0

/** A release server whose links expire `ttl` from now by the client's clock. */
const release =
  (version = "v0.3.0", ttl = HOUR): Issue =>
  () => {
    issuedAt = Date.now()
    return Promise.resolve({ link: LINK, expiresUnixMs: issuedAt + ttl, serverVersion: version })
  }

/** Runs the clock to `ms` after the last link was issued. */
const advanceTo = (ms: number) => advance(issuedAt + ms - Date.now())

// Named "Add device" until a device joins; then after it.
const dialog = () => within(screen.getByRole("dialog"))

async function openDialog() {
  fireEvent.click(screen.getByRole("button", { name: "Add device" }))
  await advance(0)
}

async function generate() {
  fireEvent.click(dialog().getByRole("button", { name: "Generate link" }))
  await advance(10)
}

/** The command block's text, or null without one. */
const command = () => document.querySelector("[role=dialog] pre code")?.textContent ?? null
const lines = () => command()!.split("\n")

async function choosePlatform(name: string) {
  fireEvent.keyDown(dialog().getByRole("combobox", { name: "Platform" }), { key: "Enter" })
  await advance(0)
  fireEvent.click(screen.getByRole("option", { name }))
  await advance(0)
}

/** What lastInputWasKeyboard() reads: a key press, or a pointer press. */
const useKeyboard = () => fireEvent.keyDown(document.body, { key: "Tab" })
const usePointer = () => fireEvent.pointerDown(document.body)

const device = (name: string, online = true): MockDevice => ({
  name,
  online,
  lastSeenUnixMs: online ? Date.now() : 0,
  os: "linux",
  arch: "amd64",
  version: "v0.3.0",
  owner: SEED_OWNER_ID,
  deviceId: `device-${name}-${Date.now()}`,
})

const waiting = () => dialog().queryByText("Waiting for the device to join…")

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(NOW)
  usePointer()
})
afterEach(async () => {
  cleanup()
  vi.useRealTimers()
  await i18n.changeLanguage("en")
  Reflect.deleteProperty(navigator, "userAgentData")
})

describe("the add device dialog", () => {
  it("issues a link only when asked, for a device by default", async () => {
    const { requested } = await renderPage({ issue: release() })
    await openDialog()
    expect(screen.getByRole("dialog", { name: "Add device" })).toBeTruthy()
    expect(dialog().getByRole("radio", { name: /^Device/ })).toHaveProperty("checked", true)
    expect(dialog().getByRole("combobox", { name: "Platform" }).textContent).toBe("Linux x86_64")
    expect(requested).toEqual([])
    expect(command()).toBeNull()

    await generate()
    expect(requested).toEqual(["daemon"])
    expect(command()).toBe(
      [
        ` [ "$(~/.local/bin/circus version 2>/dev/null)" = v0.3.0 ] || {`,
        `    mkdir -p ~/.local/bin && t="$(mktemp)" &&`,
        `    curl -fsSL -o "$t" https://github.com/EricWvi/circus/releases/download/v0.3.0/circus-v0.3.0-linux-amd64.tar.gz &&`,
        `    tar -xzf "$t" -C ~/.local/bin circus && rm -f "$t"`,
        `  } &&`,
        `  printf '%s\\n' '${LINK}' | ~/.local/bin/circus join &&`,
        "  ~/.local/bin/circus daemon install",
      ].join("\n"),
    )
    expect(dialog().getByText("The link works once and expires in 1 hour. Don't share it.")).toBeTruthy()
    expect(dialog().queryByText(/worker process/)).toBeNull()
    // The role belongs to the link now.
    expect(dialog().getByRole("radio", { name: /^Worker/ }).closest("fieldset")?.disabled).toBe(true)
  })

  it("follows the role and the platform", async () => {
    const { requested } = await renderPage({ issue: release() })
    await openDialog()
    fireEvent.click(dialog().getByRole("radio", { name: /^Worker/ }))
    await generate()
    expect(requested).toEqual(["worker"])
    expect(lines()).toHaveLength(6)
    expect(lines()[5]).toBe(`  printf '%s\\n' '${LINK}' | ~/.local/bin/circus join --role worker`)
    expect(dialog().getByText("Start the worker process separately; it isn't released yet.")).toBeTruthy()

    // The platform only picks the download, so it changes the command in place.
    await choosePlatform("macOS Apple Silicon")
    expect(lines()[2]).toContain("/v0.3.0/circus-v0.3.0-darwin-arm64.tar.gz &&")
    await choosePlatform("Linux ARM64")
    expect(lines()[2]).toContain("-linux-arm64.tar.gz")
    await choosePlatform("macOS Intel")
    expect(lines()[2]).toContain("-darwin-amd64.tar.gz")
    expect(requested).toEqual(["worker"])
  })

  it("joins both roles with one link", async () => {
    const { requested } = await renderPage({ issue: release("v1.2.0-rc.1") })
    await openDialog()
    fireEvent.click(dialog().getByRole("radio", { name: /^Both/ }))
    await generate()
    expect(requested).toEqual(["both"])
    expect(lines().slice(5)).toEqual([
      `  printf '%s\\n' '${LINK}' | ~/.local/bin/circus join &&`,
      "  ~/.local/bin/circus daemon install",
    ])
    expect(lines()[2]).toContain("/v1.2.0-rc.1/circus-v1.2.0-rc.1-linux-amd64.tar.gz")
    expect(dialog().getByText(/worker process/)).toBeTruthy()
  })

  it("has no download for a development build", async () => {
    // The mock answers as a development build.
    await renderPage()
    await openDialog()
    await generate()
    expect(
      dialog().getByText(
        "This server is a development build (dev), so there is no release to download. Run this from the root of a circus checkout (the CLI runs from source with its files under .tmp/dev):",
      ),
    ).toBeTruthy()
    expect(dialog().getByText("Run a command on the machine to join it to your account.")).toBeTruthy()
    expect(dialog().queryByText(/to install circus/)).toBeNull()
    expect(lines()).toHaveLength(2)
    expect(lines()[0]).toMatch(/^ printf '%s\\n' 'http:\/\/localhost:\d+\/join#[\w-]{43}' \| task cli -- join &&$/)
    expect(lines()[1]).toBe("  task cli -- daemon")
    // No download, so no platform to choose.
    expect(dialog().queryByRole("combobox", { name: "Platform" })).toBeNull()
  })

  it("copies the whole command", async () => {
    const writeText = vi.fn(() => Promise.resolve())
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true })
    await renderPage({ issue: release() })
    await openDialog()
    await generate()
    fireEvent.click(dialog().getByRole("button", { name: "Copy command" }))
    await advance(0)
    expect(writeText).toHaveBeenCalledWith(command())
    expect(dialog().getByRole("button", { name: "Copied" })).toBeTruthy()
  })

  it("issues one link for a double click", async () => {
    let answer: (link: EnrollLink) => void = () => {}
    const { requested } = await renderPage({
      issue: () => new Promise((resolve) => (answer = resolve)),
    })
    await openDialog()
    const button = dialog().getByRole("button", { name: "Generate link" })
    fireEvent.click(button)
    fireEvent.click(button)
    await advance(10)
    fireEvent.click(button)
    await advance(10)
    expect(requested).toEqual(["daemon"])
    // Busy, without a second name for the button.
    expect(button.getAttribute("aria-busy")).toBe("true")
    expect(dialog().getByRole("button", { name: "Generate link" })).toBe(button)
    answer({ link: LINK, expiresUnixMs: Date.now() + HOUR, serverVersion: "v0.3.0" })
    await advance(0)
    expect(command()).toContain(LINK)
  })

  describe("expiry", () => {
    it("counts down to the expiry, then offers a new link", async () => {
      const { requested } = await renderPage({ issue: release() })
      await openDialog()
      await generate()
      expect(dialog().getByText(/expires in 1 hour\./)).toBeTruthy()
      await advance(MINUTE)
      expect(dialog().getByText(/expires in 59 minutes\./)).toBeTruthy()
      await advance(58 * MINUTE)
      expect(dialog().getByText(/expires in 1 minute\./)).toBeTruthy()

      await advance(MINUTE)
      expect(command()).toBeNull()
      expect(dialog().getByText("This link expired")).toBeTruthy()
      expect(dialog().queryByText(/expires in/)).toBeNull()
      // The role can change again for the next link.
      fireEvent.click(dialog().getByRole("radio", { name: /^Both/ }))
      fireEvent.click(dialog().getByRole("button", { name: "Generate new link" }))
      await advance(10)
      expect(requested).toEqual(["daemon", "both"])
      expect(command()).toContain("circus join")
      expect(dialog().queryByText("This link expired")).toBeNull()
    })

    it("changes the phrase when the time left crosses a whole minute", async () => {
      // 59:40 left: "1 hour" (minutes round up) until 59:00 is left.
      await renderPage({ issue: release("v0.3.0", HOUR - 20 * SECOND) })
      await openDialog()
      await generate()
      expect(dialog().getByText(/expires in 1 hour\./)).toBeTruthy()
      await advanceTo(40 * SECOND - 1)
      expect(dialog().getByText(/expires in 1 hour\./)).toBeTruthy()
      await advance(1)
      expect(dialog().getByText(/expires in 59 minutes\./)).toBeTruthy()
      await advanceTo(HOUR - 20 * SECOND - MINUTE)
      expect(dialog().getByText(/expires in 1 minute\./)).toBeTruthy()
      await advanceTo(HOUR - 20 * SECOND - 1)
      expect(command()).not.toBeNull()
      await advance(1)
      expect(dialog().getByText("This link expired")).toBeTruthy()
    })

    it.each([
      ["the server's clock is ahead", 3 * HOUR],
      ["the browser's clock is ahead", -30 * MINUTE],
    ])("lasts an hour from now when %s", async (_, ttl) => {
      await renderPage({ issue: release("v0.3.0", ttl) })
      await openDialog()
      await generate()
      expect(command()).not.toBeNull()
      expect(dialog().getByText(/expires in 1 hour\./)).toBeTruthy()
      await advanceTo(HOUR - 1)
      expect(command()).not.toBeNull()
      await advance(1)
      expect(dialog().getByText("This link expired")).toBeTruthy()
    })

    it("catches up at once after the computer slept", async () => {
      await renderPage({ issue: release() })
      await openDialog()
      await generate()
      await advance(10 * MINUTE)
      expect(dialog().getByText(/expires in 50 minutes\./)).toBeTruthy()
      // Asleep: the clock moves, timers don't.
      vi.setSystemTime(Date.now() + 20 * MINUTE + 30 * SECOND)
      document.dispatchEvent(new Event("visibilitychange"))
      await advance(0)
      expect(dialog().getByText(/expires in 30 minutes\./)).toBeTruthy()
      vi.setSystemTime(Date.now() + HOUR)
      window.dispatchEvent(new Event("focus"))
      await advance(0)
      expect(dialog().getByText("This link expired")).toBeTruthy()
    })
  })

  describe("keyboard focus", () => {
    it("moves to the copy button, then to Generate new link", async () => {
      await renderPage({ issue: release() })
      await openDialog()
      useKeyboard()
      await generate()
      expect(document.activeElement?.getAttribute("aria-label")).toBe("Copy command")
      await advance(HOUR)
      expect(document.activeElement?.textContent).toBe("Generate new link")
    })

    it("moves to Close when a device joined", async () => {
      const { state } = await renderPage({ issue: release() })
      await openDialog()
      useKeyboard()
      await generate()
      state.devices.push(device("new-box"))
      await advance(DEVICES_LIVE_INTERVAL_MS)
      const focused = document.activeElement
      expect(focused?.textContent).toBe("Close")
      expect(focused?.closest("[data-slot=dialog-footer]")).not.toBeNull()
    })

    it("stays put after pointer use", async () => {
      await renderPage({ issue: release() })
      await openDialog()
      await generate()
      expect(document.activeElement?.getAttribute("aria-label")).not.toBe("Copy command")
      await advance(HOUR)
      expect(document.activeElement?.textContent).not.toBe("Generate new link")
    })
  })

  describe("finding the device that joined", () => {
    it("names it", async () => {
      const { state, queryClient } = await renderPage({ issue: release() })
      await openDialog()
      await generate()
      expect(waiting()).toBeTruthy()

      // Devices that were there before don't count, even when they change.
      state.devices.find((d) => d.name === "eric-win")!.online = true
      await advance(DEVICES_LIVE_INTERVAL_MS)
      expect(waiting()).toBeTruthy()

      state.devices.push(device("new-box"))
      await advance(DEVICES_LIVE_INTERVAL_MS)
      expect(screen.getByRole("dialog", { name: "new-box is connected" })).toBeTruthy()
      expect(dialog().getByText("It's in your device list now.")).toBeTruthy()
      expect(command()).toBeNull()
      // The link is forgotten.
      expect(queryClient.getMutationCache().getAll()).toEqual([])
      // The page's list has it too.
      expect(screen.getByText("new-box", { selector: "li span" })).toBeTruthy()

      fireEvent.click(dialog().getAllByRole("button", { name: "Close" }).at(-1)!)
      await advance(0)
      expect(screen.queryByRole("dialog")).toBeNull()
    })

    it("says when it joined but its daemon isn't running yet", async () => {
      const { state } = await renderPage({ issue: release() })
      await openDialog()
      await generate()
      state.devices.push(device("new-box", false))
      await advance(DEVICES_LIVE_INTERVAL_MS)
      expect(screen.getByRole("dialog", { name: "new-box joined" })).toBeTruthy()
      expect(dialog().getByText("Start circus daemon on it to bring it online.")).toBeTruthy()

      state.devices.find((d) => d.name === "new-box")!.online = true
      await advance(DEVICES_LIVE_INTERVAL_MS)
      expect(screen.getByRole("dialog", { name: "new-box is connected" })).toBeTruthy()
    })

    it("recognizes a revoked device enrolled again under its old name", async () => {
      const { state } = await renderPage({ issue: release() })
      await openDialog()
      await generate()
      const old = state.devices.find((d) => d.name === "eric-mbp")!
      state.devices = state.devices.filter((d) => d !== old)
      await advance(DEVICES_LIVE_INTERVAL_MS)
      expect(waiting()).toBeTruthy()

      state.devices.push(device("eric-mbp"))
      await advance(DEVICES_LIVE_INTERVAL_MS)
      expect(screen.getByRole("dialog", { name: "eric-mbp is connected" })).toBeTruthy()
    })

    it("compares with the list as it is when the link is issued", async () => {
      const { state, requested } = await renderPage({ issue: release() })
      // Enrolled elsewhere since the page last polled.
      state.devices.push(device("other-box"))
      await openDialog()
      await generate()
      expect(requested).toEqual(["daemon"])
      await advance(DEVICES_LIVE_INTERVAL_MS)
      expect(waiting()).toBeTruthy()
    })

    it("offers no way to add a device until the list has loaded", async () => {
      let loaded: () => void = () => {}
      const gate = new Promise<void>((resolve) => (loaded = resolve))
      let calls = 0
      await renderPage({
        issue: release(),
        listDevices: (real) => () => (calls++ === 0 ? gate.then(real) : real()),
      })
      expect(screen.getByRole("list", { name: "Loading devices" })).toBeTruthy()
      expect(screen.queryByRole("button", { name: "Add device" })).toBeNull()
      loaded()
      await advance(10)
      expect(screen.getAllByRole("button", { name: "Add device" }).length).toBeGreaterThan(0)
    })

    it("takes the first list that arrives when it can't be fetched for the link", async () => {
      let failing = false
      const { state, requested } = await renderPage({
        issue: release(),
        listDevices: (real) => () => (failing ? Promise.reject(new ApiError("internal", "")) : real()),
      })
      await openDialog()
      failing = true
      await generate()
      // The link is issued anyway.
      expect(requested).toEqual(["daemon"])
      // Enrolled elsewhere before the list came back: there when it does.
      state.devices.push(device("other-box"))
      failing = false
      await advance(DEVICES_LIVE_INTERVAL_MS)
      expect(waiting()).toBeTruthy()
      state.devices.push(device("new-box"))
      await advance(DEVICES_LIVE_INTERVAL_MS)
      expect(screen.getByRole("dialog", { name: "new-box is connected" })).toBeTruthy()
    })

    it("says when the list can't be checked", async () => {
      let failing = false
      await renderPage({
        issue: release(),
        listDevices: (real) => () => (failing ? Promise.reject(new ApiError("internal", "")) : real()),
      })
      await openDialog()
      await generate()
      failing = true
      await advance(DEVICES_LIVE_INTERVAL_MS)
      expect(dialog().getByText("Can't check your devices right now. Still trying…")).toBeTruthy()
      expect(waiting()).toBeNull()
      failing = false
      await advance(DEVICES_LIVE_INTERVAL_MS)
      expect(waiting()).toBeTruthy()
    })

    it("doesn't wait when the link enrolls only a worker", async () => {
      await renderPage({ issue: release() })
      await openDialog()
      fireEvent.click(dialog().getByRole("radio", { name: /^Worker/ }))
      await generate()
      expect(waiting()).toBeNull()
    })
  })

  it("discards the link when closed", async () => {
    const { queryClient } = await renderPage({ issue: release() })
    await openDialog()
    await generate()
    expect(command()).toContain(LINK)
    expect(queryClient.getMutationCache().getAll()).toHaveLength(1)
    fireEvent.click(dialog().getAllByRole("button", { name: "Close" }).at(-1)!)
    await advance(0)
    expect(queryClient.getMutationCache().getAll()).toEqual([])
    await openDialog()
    expect(command()).toBeNull()
    expect(dialog().getByRole("button", { name: "Generate link" })).toBeTruthy()
  })

  describe("platform", () => {
    const hints = (architecture: string, delay = 0) => ({
      platform: "Linux",
      getHighEntropyValues: () =>
        new Promise((resolve) => setTimeout(() => resolve({ architecture }), delay)),
    })

    it("takes the architecture from client hints", async () => {
      Object.defineProperty(navigator, "userAgentData", { value: hints("arm"), configurable: true })
      await renderPage({ issue: release() })
      await openDialog()
      await advance(0)
      expect(dialog().getByRole("combobox", { name: "Platform" }).textContent).toBe("Linux ARM64")
    })

    it("keeps a platform chosen before the hints arrive", async () => {
      Object.defineProperty(navigator, "userAgentData", { value: hints("arm", 5000), configurable: true })
      await renderPage({ issue: release() })
      await openDialog()
      await choosePlatform("macOS Intel")
      await advance(5000)
      expect(dialog().getByRole("combobox", { name: "Platform" }).textContent).toBe("macOS Intel")
    })
  })

  it.each([
    ["resource_exhausted", "Too many unused links. Wait for some to expire, then try again."],
    [
      "failed_precondition",
      "The server has no public URL configured, so it can't issue links. Ask the server admin to set public_url.",
    ],
    ["unavailable", "Can't reach the server. Try again."],
  ] as const)("explains a %s refusal", async (code, message) => {
    await renderPage({ issue: () => Promise.reject(new ApiError(code, "server text")) })
    await openDialog()
    await generate()
    expect(dialog().getByRole("alert").textContent).toBe(message)
    expect(command()).toBeNull()
    expect(dialog().getByRole("button", { name: "Generate link" })).toBeTruthy()
  })

  it("reads in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderPage({ issue: release() })
    fireEvent.click(screen.getByRole("button", { name: "添加设备" }))
    await advance(0)
    const zh = within(screen.getByRole("dialog", { name: "添加设备" }))
    expect(zh.getByText("在要添加的机器上运行一条命令，安装 circus 并把它加入你的账户。")).toBeTruthy()
    expect(zh.getByRole("radio", { name: /^设备/ })).toBeTruthy()
    expect(zh.getByText("在该机器上为你的会话运行工具（circus daemon）。")).toBeTruthy()
    fireEvent.click(zh.getByRole("button", { name: "生成链接" }))
    await advance(10)
    expect(zh.getByText("在要添加的机器上运行：")).toBeTruthy()
    expect(zh.getByText("链接仅可使用一次，1 小时后过期。请勿分享。")).toBeTruthy()
    expect(zh.getByText("正在等待设备加入…")).toBeTruthy()
  })
})
