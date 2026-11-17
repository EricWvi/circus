import { useMemo } from "react"
import { useTranslation } from "react-i18next"
import type { StreamdownTranslations } from "streamdown"

const keys = [
  "close",
  "copied",
  "copyCode",
  "copyLink",
  "copyTable",
  "copyTableAsCsv",
  "copyTableAsMarkdown",
  "copyTableAsTsv",
  "downloadDiagram",
  "downloadDiagramAsMmd",
  "downloadDiagramAsPng",
  "downloadDiagramAsSvg",
  "downloadFile",
  "downloadImage",
  "downloadTable",
  "downloadTableAsCsv",
  "downloadTableAsMarkdown",
  "errorMermaid",
  "exitFullscreen",
  "externalLinkWarning",
  "imageNotAvailable",
  "mermaidAriaLabel",
  "mermaidFormatMmd",
  "mermaidFormatPng",
  "mermaidFormatSvg",
  "openExternalLink",
  "openLink",
  "resetView",
  "tableFormatCsv",
  "tableFormatMarkdown",
  "tableFormatTsv",
  "viewFullscreen",
  "zoomIn",
  "zoomOut",
] as const satisfies readonly (keyof StreamdownTranslations)[]

// Fails to typecheck when Streamdown gains a label this list lacks.
type Missing = Exclude<keyof StreamdownTranslations, (typeof keys)[number]>
const complete: [Missing] extends [never] ? true : Missing = true
void complete

/** Streamdown's control labels in the active language. */
export function useStreamdownTranslations(): StreamdownTranslations {
  const { t } = useTranslation()
  return useMemo(() => {
    const labels = {} as StreamdownTranslations
    for (const key of keys) labels[key] = t(`markdown.${key}`)
    return labels
  }, [t])
}
