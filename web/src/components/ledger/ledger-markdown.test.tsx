// @vitest-environment happy-dom
import { cleanup, screen } from "@testing-library/react"
import { MemoryRouter } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ledgerRehypePlugins, safeHref, sanitize, sanitizeSchema } from "@/lib/ledger-markdown"
import { advance, renderWithBackend } from "@/test/render"

import { LedgerProjectContext } from "./ledger-context"
import { LedgerMarkdown } from "./ledger-markdown"

beforeEach(() => {
  vi.useFakeTimers()
})
afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

/** Renders a body as a decision of `circus` (the demo ledger) shows it. */
async function renderBody(body: string, projectId = "circus") {
  const result = await renderWithBackend(
    <MemoryRouter>
      <LedgerProjectContext.Provider value={projectId}>
        <div data-testid="body">
          <LedgerMarkdown>{body}</LedgerMarkdown>
        </div>
      </LedgerProjectContext.Provider>
    </MemoryRouter>,
  )
  // The projects and the ledgers that references resolve against.
  await advance(50)
  return { ...result, body: screen.getByTestId("body") }
}

describe("rendering an untrusted body", () => {
  it("shows raw HTML as text", async () => {
    const { body } = await renderBody(
      'Hi <b>bold</b> <img src="https://x.example/a.png" onerror="alert(1)"> <script>alert(1)</script>\n\n<div onclick="x()">block</div>\n\n<iframe src="https://evil.example"></iframe>',
    )
    expect(body.querySelector("b, script, iframe, img, [onclick], [onerror]")).toBeNull()
    expect(body.textContent).toContain("<b>bold</b>")
    expect(body.textContent).toContain("<script>alert(1)</script>")
    expect(body.textContent).toContain('<div onclick="x()">block</div>')
  })

  it("makes only http, https and mailto links, opening in a new tab", async () => {
    const { body } = await renderBody(
      [
        "[web](https://example.com/a) [plain](http://example.com/b) [mail](mailto:a@example.com)",
        "[js](javascript:alert(1)) [JS](JaVaScRiPt:alert(1)) [data](data:text/html,<b>x</b>) [vb](vbscript:x) [file](file:///etc/passwd)",
        "[rel](research/foo.md) [up](../x.md) [abs](/p/x) [frag](#top) [proto](//evil.example/x)",
        "<https://auto.example/x> and www.bare.example",
      ].join("\n\n"),
    )
    const links = [...body.querySelectorAll("a")]
    expect(links.map((a) => [a.textContent, a.getAttribute("href")])).toEqual([
      ["web", "https://example.com/a"],
      ["plain", "http://example.com/b"],
      ["mail", "mailto:a@example.com"],
      ["https://auto.example/x", "https://auto.example/x"],
      ["www.bare.example", "http://www.bare.example"],
    ])
    for (const a of links) {
      expect(a.getAttribute("target")).toBe("_blank")
      expect(a.getAttribute("rel")).toBe("noopener noreferrer")
    }
    // The others are their text alone.
    for (const text of ["js", "JS", "data", "vb", "file", "rel", "up", "abs", "frag", "proto"]) {
      expect(screen.getByText(text).closest("a")).toBeNull()
    }
  })

  it("never loads images, but links to them", async () => {
    const { body } = await renderBody(
      "![a diagram](https://example.com/d.png) ![](https://example.com/e.png) ![local](img/x.png) ![js](javascript:alert(1))\n\n[![inside](https://example.com/f.png)](https://example.com/g)",
    )
    expect(body.querySelector("img")).toBeNull()
    expect(screen.getByText("a diagram").closest("a")?.getAttribute("href")).toBe("https://example.com/d.png")
    expect(screen.getByText("Image").closest("a")?.getAttribute("href")).toBe("https://example.com/e.png")
    expect(screen.getByText("local").closest("a")).toBeNull()
    expect(screen.getByText("js").closest("a")).toBeNull()
    // An image inside a link is the link's text.
    expect(screen.getByText("inside").closest("a")?.getAttribute("href")).toBe("https://example.com/g")
    // Image links open in a new tab without a referrer.
    for (const text of ["a diagram", "Image"]) {
      const a = screen.getByText(text).closest("a")
      expect(a?.getAttribute("target")).toBe("_blank")
      expect(a?.getAttribute("rel")).toBe("noopener noreferrer")
    }
  })

  it("never links an image to an email address", async () => {
    await renderBody("![write to us](mailto:a@example.com)")
    expect(screen.getByText("write to us").closest("a")).toBeNull()
  })

  it("links footnotes within the body, with translated labels", async () => {
    const { body } = await renderBody("Text[^1].\n\n[^1]: The note.")
    const ref = body.querySelector('a[href="#user-content-fn-1"]')
    expect(ref?.textContent).toBe("1")
    expect(ref?.id).toBe("user-content-fnref-1")
    expect(ref?.getAttribute("target")).toBeNull()
    const back = body.querySelector('a[href="#user-content-fnref-1"]')
    expect(back?.getAttribute("aria-label")).toBe("Back to the text")
    expect(body.querySelector("#user-content-fn-1")?.textContent).toContain("The note.")
    expect(body.textContent).toContain("Footnotes")
    cleanup()
    // Only the body's own footnote anchors are kept as links.
    await renderBody("[elsewhere](#top) [also](#user-content-fn-1%22x)")
    expect(screen.getByText("elsewhere").closest("a")).toBeNull()
    expect(screen.getByText("also").closest("a")).toBeNull()
  })

  it("keeps Markdown: lists, emphasis, code and tables", async () => {
    const { body } = await renderBody("- **one**\n- `two`\n\n| A | B |\n| --- | --- |\n| 1 | 2 |\n\n```go\nfunc main() {}\n```")
    expect(body.querySelector('li [data-streamdown="strong"]')?.textContent).toBe("one")
    expect(body.querySelector("li code")?.textContent).toBe("two")
    expect(body.querySelectorAll("td")).toHaveLength(2)
    expect(body.textContent).toContain("func main() {}")
  })
})

describe("references in a body", () => {
  it("link to decisions of this project", async () => {
    const { body } = await renderBody("See D4 and **D6**, also (D13).")
    expect([...body.querySelectorAll("a")].map((a) => [a.textContent, a.getAttribute("href"), a.getAttribute("target")])).toEqual([
      ["D4", "/p/circus/ledger/D4", null],
      ["D6", "/p/circus/ledger/D6", null],
      ["D13", "/p/circus/ledger/D13", null],
    ])
  })

  it("link to another project's decisions by its exact name", async () => {
    const { body } = await renderBody("Unlike palace#D1 and Palace#D1.")
    expect(screen.getByText("palace#D1").closest("a")?.getAttribute("href")).toBe("/p/palace/ledger/D1")
    // Names are matched exactly, case included.
    expect(body.querySelectorAll("a")).toHaveLength(1)
    expect(body.textContent).toBe("Unlike palace#D1 and Palace#D1.")
  })

  it("are muted text when the decision or the project doesn't exist", async () => {
    await renderBody("Missing D99, palace#D7 and nowhere#D1.")
    const missing = screen.getByText("D99")
    expect(missing.closest("a")).toBeNull()
    expect(missing.getAttribute("title")).toBe("D99 doesn't exist")
    expect(screen.getByText("palace#D7").closest("a")).toBeNull()
    expect(screen.getByText(/nowhere#D1/).closest("a")).toBeNull()
  })

  it("are left alone in code, links and words", async () => {
    const { body } = await renderBody("`D4` and\n\n```\nD6\n```\n\n[about D7](https://example.com) AD4 D4x")
    expect(body.querySelectorAll('a[href^="/"]')).toHaveLength(0)
    expect(body.querySelector("code")?.textContent).toBe("D4")
  })
})

describe("the sanitizer", () => {
  type Node = { type: string; tagName?: string; properties?: Record<string, unknown>; children?: Node[]; value?: string }
  const el = (tagName: string, properties: Record<string, unknown>, children: Node[] = []): Node => ({ type: "element", tagName, properties, children })
  const text = (value: string): Node => ({ type: "text", value })

  /** Runs the body's rehype plugins, in order, on a tree. */
  function run(tree: Node): Node {
    for (const [plugin, options] of ledgerRehypePlugins([])) {
      const transform = (plugin as (o: unknown) => (t: Node) => Node | undefined)(options)
      tree = transform(tree) ?? tree
    }
    return tree
  }
  const html = (node: Node): string =>
    node.type === "text"
      ? (node.value ?? "")
      : node.type === "element"
        ? `<${node.tagName}${Object.entries(node.properties ?? {}).map(([k, v]) => ` ${k}=${JSON.stringify(v)}`).join("")}>${(node.children ?? []).map(html).join("")}</${node.tagName}>`
        : (node.children ?? []).map(html).join("")

  it("comes first and strips what Markdown can't be trusted to carry", () => {
    expect(ledgerRehypePlugins([])[0]).toEqual([sanitize, sanitizeSchema])
    const out = html(
      run({
        type: "root",
        children: [
          el("p", { onClick: "x()", style: "background:url(https://evil.example)" }, [
            el("a", { href: "javascript:alert(1)" }, [text("js")]),
            el("a", { href: "data:text/html,x" }, [text("data")]),
            el("a", { href: "https://ok.example" }, [text("ok")]),
            el("script", {}, [text("alert(1)")]),
            el("iframe", { src: "https://evil.example" }, []),
          ]),
        ],
      }),
    )
    expect(out).toBe('<p><a>js</a><a>data</a><a href="https://ok.example">ok</a></p>')
  })

  it("allows only http, https and mailto links and http(s) images", () => {
    expect(sanitizeSchema.protocols.href).toEqual(["http", "https", "mailto"])
    expect(sanitizeSchema.protocols.src).toEqual(["http", "https"])
  })
})

describe("safeHref", () => {
  it("lets only absolute http, https and mailto URLs through", () => {
    expect(safeHref("https://a.example")).toBe("https://a.example")
    expect(safeHref("HTTP://A.example")).toBe("HTTP://A.example")
    expect(safeHref("mailto:x@y.example")).toBe("mailto:x@y.example")
    for (const bad of ["javascript:alert(1)", " javascript:alert(1)", "data:text/html,x", "research/foo.md", "/abs", "//host/x", "#x", "", undefined, 5]) {
      expect({ bad, href: safeHref(bad) }).toEqual({ bad, href: null })
    }
  })
})
