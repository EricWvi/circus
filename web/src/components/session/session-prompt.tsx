import { ImageIcon } from "lucide-react"
import { nanoid } from "nanoid"

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
  const { openFileDialog } = usePromptInputAttachments()
  return (
    <PromptInputButton onClick={openFileDialog} aria-label="Attach image" tooltip="Attach image">
      <ImageIcon />
    </PromptInputButton>
  )
}

export function SessionPrompt({
  busy,
  onSend,
  onStop,
}: {
  busy: boolean
  onSend: (text: string, attachments: ImageAttachment[]) => void
  onStop: () => void
}) {
  const handleSubmit = ({ text, files }: PromptInputMessage) => {
    const trimmed = text.trim()
    if (!trimmed && files.length === 0) return
    onSend(
      trimmed,
      files.map((f) => ({
        id: nanoid(),
        filename: f.filename ?? "pasted-image",
        mediaType: f.mediaType,
        url: f.url,
      })),
    )
  }

  return (
    <PromptInput accept="image/*" multiple onSubmit={handleSubmit}>
      <PendingAttachments />
      <PromptInputBody>
        <PromptInputTextarea placeholder="Message Claude… (Shift+Enter for a new line)" />
      </PromptInputBody>
      <PromptInputFooter>
        <PromptInputTools>
          <AddImageButton />
        </PromptInputTools>
        <PromptInputSubmit status={busy ? "streaming" : "ready"} onStop={onStop} />
      </PromptInputFooter>
    </PromptInput>
  )
}
