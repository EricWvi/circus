import { MessageSquareDashedIcon } from "lucide-react"

import {
  Conversation,
  ConversationContent,
  ConversationEmptyState,
  ConversationScrollButton,
} from "@/components/ai-elements/conversation"
import { Shimmer } from "@/components/ai-elements/shimmer"
import { StatusBadge } from "@/components/status-dot"
import { Separator } from "@/components/ui/separator"
import { SidebarTrigger } from "@/components/ui/sidebar"
import type { Project, SessionSummary } from "@/api/types"
import { useConversations } from "@/state/conversations-context"

import { EventItem } from "./event-list"
import { SessionPrompt } from "./session-prompt"

export function SessionView({
  project,
  session,
}: {
  project: Project
  session: SessionSummary
}) {
  const { events: eventsOf, send, stop, decideApproval } = useConversations()
  const events = eventsOf(session.sessionId)
  const last = events.at(-1)
  const streamingText = last?.kind === "assistant_text" && last.streaming && last.text !== ""
  const busy = session.state === "running"
  const showWorking = busy && !streamingText && last?.kind !== "tool_call"

  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="flex h-12 shrink-0 items-center gap-2 border-b px-3">
        <SidebarTrigger />
        <Separator orientation="vertical" className="mr-1 data-vertical:h-4 data-vertical:self-center" />
        <div className="flex min-w-0 flex-1 items-center gap-2 text-sm">
          <span className="hidden shrink-0 text-muted-foreground sm:inline">{project.name}</span>
          <span className="hidden text-muted-foreground sm:inline" aria-hidden>
            /
          </span>
          <h1 className="min-w-0 truncate font-medium">{session.title}</h1>
          <StatusBadge state={session.state} />
        </div>
        <span className="hidden shrink-0 text-xs text-muted-foreground md:inline">
          on {session.device}
        </span>
      </header>

      <Conversation className="min-h-0">
        <ConversationContent className="mx-auto w-full max-w-3xl gap-6 px-4 py-6">
          {events.length === 0 ? (
            <ConversationEmptyState
              className="min-h-[50vh]"
              icon={<MessageSquareDashedIcon className="size-8" />}
              title="No messages yet"
              description={`Send a prompt to start working on ${session.device}.`}
            />
          ) : (
            events.map((event) => (
              <EventItem
                key={event.id}
                event={event}
                deviceName={session.device}
                onDecide={(eventId, decision) =>
                  decideApproval(session.sessionId, eventId, decision)
                }
              />
            ))
          )}
          {showWorking && (
            <div className="text-sm">
              <Shimmer>Working…</Shimmer>
            </div>
          )}
        </ConversationContent>
        <ConversationScrollButton />
      </Conversation>

      <div className="mx-auto w-full max-w-3xl shrink-0 px-4 pb-4">
        <SessionPrompt
          busy={busy}
          onSend={(text, attachments) => send(session.sessionId, text, attachments)}
          onStop={() => stop(session.sessionId)}
        />
      </div>
    </div>
  )
}
