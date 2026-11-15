// A small inline SVG standing in for a pasted CI screenshot.
const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="480" height="270" viewBox="0 0 480 270">
<rect width="480" height="270" fill="#111"/>
<rect width="480" height="24" fill="#222"/>
<circle cx="14" cy="12" r="4" fill="#555"/><circle cx="28" cy="12" r="4" fill="#555"/><circle cx="42" cy="12" r="4" fill="#555"/>
<text x="64" y="16" fill="#888" font-family="monospace" font-size="10">ci / test (ubuntu-latest)</text>
<g font-family="monospace" font-size="11">
<text x="14" y="52" fill="#aaa">=== RUN   TestRefreshToken</text>
<text x="14" y="70" fill="#aaa">    refresh_test.go:48: token expired: exp=1763204400</text>
<text x="14" y="88" fill="#aaa">        now=1763204400</text>
<text x="14" y="106" fill="#e55">--- FAIL: TestRefreshToken (0.00s)</text>
<text x="14" y="124" fill="#e55">FAIL github.com/acme/circus/internal/auth 0.412s</text>
<text x="14" y="152" fill="#666">Error: Process completed with exit code 1.</text>
</g>
</svg>`

export const ciScreenshotUrl = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`
