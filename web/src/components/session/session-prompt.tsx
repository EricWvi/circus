import { ImageIcon } from "lucide-react"
import { nanoid } from "nanoid"
import { useTranslation } from "react-i18next"

import {
  Attachment,
  AttachmentPreview,
  AttachmentRemove,
  Attachments,
} from "@/components/ai-elements/attachments"
import {
  PromptInput,
  PromptInputBody,
  PromptInputButton,
  PromptInputFooter,
  PromptInputHeader,
  PromptInputProvider,
  PromptInputSubmit,
  PromptInputTextarea,
  PromptInputTools,
  usePromptInputAttachments,
  type PromptInputMessage,
} from "@/components/ai-elements/prompt-input"
import type { ImageAttachment } from "@/lib/types"

function PendingAttachments() {
  const { files, remove } = usePromptInputAttachments()
  if (files.length === 0) return null
  return (
    <PromptInputHeader className="pt-2">
      <Attachments variant="grid" className="ml-0">
        {files.map((file) => (
          <Attachment key={file.id} data={file} onRemove={() => remove(file.id)} className="size-16 border">
            <AttachmentPreview />
            <AttachmentRemove />
          </Attachment>
        ))}
      </Attachments>
    </PromptInputHeader>
  )
}

function AddImageButton() {
  const { t } = useTranslation()
  const { openFileDialog } = usePromptInputAttachments()
  return (
    <PromptInputButton
      onClick={openFileDialog}
      aria-label={t("session.attachImage")}
      tooltip={t("session.attachImage")}
    >
      <ImageIcon />
    </PromptInputButton>
  )
}

export function SessionPrompt({
  busy = false,
  pending = false,
  placeholder,
  onSend,
  onStop,
}: {
  /** A turn is running; the submit button stops it. */
  busy?: boolean
  /** The message is being sent; the input keeps its text until it succeeds. */
  pending?: boolean
  placeholder?: string
  /** A rejected promise keeps the text and images for another try. */
  onSend: (text: string, attachments: ImageAttachment[]) => void | Promise<void>
  onStop?: () => void
}) {
  const status = pending ? "submitted" : busy ? "streaming" : "ready"
  const handleSubmit = ({ text, files }: PromptInputMessage) => {
    const trimmed = text.trim()
    if (pending || (!trimmed && files.length === 0)) return Promise.reject(new Error("nothing to send"))
    return onSend(
      trimmed,
      files.map((f) => ({
        id: nanoid(),
        filename: f.filename ?? "pasted-image",
        mediaType: f.mediaType,
        url: f.url,
      })),
    )
  }

  // The provider keeps text and images when sending fails.
  return (
    <PromptInputProvider>
      <PromptInput accept="image/*" multiple onSubmit={handleSubmit}>
        <PendingAttachments />
        <PromptInputBody>
          <PromptInputTextarea placeholder={placeholder} />
        </PromptInputBody>
        <PromptInputFooter>
          <PromptInputTools>
            <AddImageButton />
          </PromptInputTools>
          <PromptInputSubmit
            status={status}
            onStop={onStop}
            disabled={pending}
          />
        </PromptInputFooter>
      </PromptInput>
    </PromptInputProvider>
  )
}
