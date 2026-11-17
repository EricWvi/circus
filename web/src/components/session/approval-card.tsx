import { ShieldAlertIcon } from "lucide-react"
import { Trans, useTranslation } from "react-i18next"

import { CodeBlock } from "@/components/ai-elements/code-block"
import {
  Confirmation,
  ConfirmationAction,
  ConfirmationActions,
  ConfirmationRequest,
  ConfirmationTitle,
} from "@/components/ai-elements/confirmation"
import { DisabledReason } from "@/components/disabled-reason"
import type { ToolCallEvent } from "@/lib/types"

import { toToolPartState } from "./tool-state"

export function ApprovalCard({
  event,
  deviceName,
  disabledReason = null,
  onDecide,
}: {
  event: ToolCallEvent
  deviceName: string
  /** Why the decision can't be made right now (e.g. disconnected); null when it can. */
  disabledReason?: string | null
  onDecide: (decision: "approved" | "denied") => void
}) {
  const { t } = useTranslation()
  const command =
    typeof event.input.command === "string"
      ? event.input.command
      : JSON.stringify(event.input, null, 2)

  return (
    <Confirmation
      approval={{ id: event.id }}
      state={toToolPartState(event.status)}
      className="gap-3 border-foreground"
    >
      <ConfirmationRequest>
        <ConfirmationTitle className="flex flex-col gap-3 text-foreground">
          <span className="flex items-center gap-2 font-medium">
            <ShieldAlertIcon className="size-4 shrink-0" />
            <span>
              <Trans
                i18nKey="session.approval.title"
                values={{ tool: event.tool, device: deviceName }}
                components={{ tool: <span className="font-semibold" /> }}
              />
            </span>
          </span>
          <CodeBlock code={command} language="bash" />
          {event.approval && (
            <span className="text-pretty text-muted-foreground">{event.approval.reason}</span>
          )}
        </ConfirmationTitle>
      </ConfirmationRequest>
      <ConfirmationActions>
        <DisabledReason reason={disabledReason}>
          <ConfirmationAction
            variant="outline"
            disabled={disabledReason !== null}
            onClick={() => onDecide("denied")}
          >
            {t("session.approval.deny")}
          </ConfirmationAction>
        </DisabledReason>
        <DisabledReason reason={disabledReason}>
          <ConfirmationAction disabled={disabledReason !== null} onClick={() => onDecide("approved")}>
            {t("session.approval.approve")}
          </ConfirmationAction>
        </DisabledReason>
      </ConfirmationActions>
    </Confirmation>
  )
}
