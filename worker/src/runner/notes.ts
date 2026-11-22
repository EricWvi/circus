// Texts the worker writes for the model. Each is pushed as a
// <system-reminder> block in front of the next user input, and published as
// a USER view event with its circus_note kind.

export const previousTurnInterruptedText =
  "The previous turn was interrupted before it finished: the process running this session stopped. " +
  "Its last actions may be incomplete; check their effects before relying on them, and do not resume them unless asked.";

export const historyMissingText =
  "Part of the conversation history is missing: some earlier messages and tool results of this session could not be saved. " +
  "Do not assume you remember everything that happened before this point; ask the user or check the files when it matters.";

/** A task-notification-style failed notice for a background task lost with its worker. */
export function lostTaskText(taskId: string, toolUseId: string, description: string): string {
  const summary = `Background task ${taskId}${description ? ` (${description})` : ""} was lost: ` +
    "the process running this session stopped before the task finished, so its result will never arrive.";
  return [
    "<task-notification>",
    `<task-id>${taskId}</task-id>`,
    ...(toolUseId ? [`<tool-use-id>${toolUseId}</tool-use-id>`] : []),
    "<status>failed</status>",
    `<summary>${summary}</summary>`,
    "</task-notification>",
  ].join("\n");
}

export const lostTaskSummary = (taskId: string) =>
  `Background task ${taskId} was lost when the process running this session stopped.`;

export const systemReminder = (text: string) => `<system-reminder>\n${text}\n</system-reminder>`;

/** A background task that ended while idle, whose notice the closed CLI never delivered. */
export function undeliveredTaskText(taskId: string, toolUseId: string, status: string, summary: string): string {
  return [
    "<task-notification>",
    `<task-id>${taskId}</task-id>`,
    ...(toolUseId ? [`<tool-use-id>${toolUseId}</tool-use-id>`] : []),
    `<status>${status || "completed"}</status>`,
    `<summary>${summary || `Background task ${taskId} ended.`}</summary>`,
    "</task-notification>",
  ].join("\n");
}
