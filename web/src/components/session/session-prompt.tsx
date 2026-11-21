import {
  PromptInput,
  PromptInputBody,
  PromptInputFooter,
  PromptInputProvider,
  PromptInputSubmit,
  PromptInputTextarea,
  PromptInputTools,
  type PromptInputMessage,
} from "@/components/ai-elements/prompt-input"
import { DisabledReason } from "@/components/disabled-reason"

/** Pasting keeps only text: messages are text only for now. */
const textOnlyPaste = () => {}

export function SessionPrompt({
  busy = false,
  pending = false,
  placeholder,
  sendDisabledReason = null,
  disabledReason = null,
  onSend,
  onStop,
}: {
  /** A turn is running; the submit button stops it. */
  busy?: boolean
  /** The message is being sent; the input keeps its text until it succeeds. */
  pending?: boolean
  placeholder?: string
  /**
   * Why nothing can be sent or stopped right now (e.g. disconnected); typing
   * still works. Null when sending works.
   */
  sendDisabledReason?: string | null
  /** Why the whole composer is disabled, typing included. Null when it isn't. */
  disabledReason?: string | null
  /** A rejected promise keeps the text for another try. */
  onSend: (text: string) => void | Promise<void>
  onStop?: () => void
}) {
  const status = pending ? "submitted" : busy ? "streaming" : "ready"
  const blocked = disabledReason ?? sendDisabledReason
  // Dropped files never show (there is no attachment list) and are ignored.
  const handleSubmit = ({ text }: PromptInputMessage) => {
    const trimmed = text.trim()
    if (pending || blocked || !trimmed) return Promise.reject(new Error("nothing to send"))
    return onSend(trimmed)
  }

  // The provider keeps the text when sending fails. The tree stays the same
  // whatever is disabled, so a draft survives a reconnect.
  return (
    <PromptInputProvider>
      <PromptInput onSubmit={handleSubmit}>
        <PromptInputBody>
          <PromptInputTextarea
            placeholder={disabledReason ?? placeholder}
            title={disabledReason ?? undefined}
            disabled={disabledReason !== null}
            onPaste={textOnlyPaste}
          />
        </PromptInputBody>
        <PromptInputFooter>
          <PromptInputTools />
          <DisabledReason reason={blocked}>
            <PromptInputSubmit status={status} onStop={onStop} disabled={pending || blocked !== null} />
          </DisabledReason>
        </PromptInputFooter>
      </PromptInput>
    </PromptInputProvider>
  )
}
