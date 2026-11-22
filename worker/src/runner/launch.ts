import type { SessionReply_LaunchEnv } from "@circus/proto/circus/v1/worker_link_pb";

// The environment of a Claude Code CLI started with a provider.
//
// GetLaunchEnv returns the provider's variables (secrets). The worker removes
// every inherited variable that selects a credential or an API endpoint and
// sets the returned ones in the CLI's process environment, together with
// CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1: with it, Claude Code ignores the
// provider, credential and model-alias variables (and apiKeyHelper) of every
// settings file, including the user's ~/.claude/settings.json, which would
// otherwise override the process environment (worker/README.md, "Providers").
// Nothing here is ever logged: values are secrets, and callers log only the
// provider id and counts.

/** Set for a provider: settings files cannot override the provider's variables. */
export const MANAGED_BY_HOST = "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST";

/**
 * The model of a provider session whose SessionConfig.model is empty: the
 * alias Claude Code 2.1.330 uses by default with an API key or auth token
 * on the Anthropic API (its default is the opus alias target, with the 1M
 * context where that applies). Passed explicitly because a `model` in user,
 * project or local settings would otherwise choose it (worker/README.md).
 */
export const PROVIDER_DEFAULT_MODEL = "opus[1m]";

/**
 * Inherited variables that select a credential, an endpoint or a model,
 * besides every ANTHROPIC_* and CLAUDE_CODE_3P_* variable. From Claude Code
 * 2.1.330: OAuth tokens and their file descriptors, the cloud-provider
 * switches and their auth skips, the gateway and host-auth hand-offs, the
 * mTLS client certificate, the runner model socket, and the subagent, auto
 * mode classifier and model catalog choices.
 */
const CLEARED = new Set([
  "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
  "CLAUDE_CODE_OAUTH_SCOPES", "CLAUDE_CODE_OAUTH_CLIENT_ID", "CLAUDE_CODE_CUSTOM_OAUTH_URL",
  "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "CLAUDE_CODE_API_BASE_URL", "CLAUDE_CODE_SESSION_ACCESS_TOKEN",
  "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_ANTHROPIC_AWS",
  "CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_GATEWAY",
  "CLAUDE_CODE_SKIP_BEDROCK_AUTH", "CLAUDE_CODE_SKIP_VERTEX_AUTH", "CLAUDE_CODE_SKIP_FOUNDRY_AUTH",
  "CLAUDE_CODE_SKIP_ANTHROPIC_AWS_AUTH", "CLAUDE_CODE_SKIP_ANTHROPIC_GOOGLE_CLOUD_AUTH", "CLAUDE_CODE_SKIP_MANTLE_AUTH",
  "AWS_BEARER_TOKEN_BEDROCK", "CLOUD_ML_REGION",
  "CLAUDE_CODE_GATEWAY_TOKEN", "CLAUDE_CODE_GATEWAY_TOKEN_FILE_DESCRIPTOR",
  "CLAUDE_CODE_HOST_AUTH_ENV_VAR", "CLAUDE_CODE_HOST_AUTH_ASK_AT_START", "CLAUDE_CODE_HOST_AUTH_REFRESH_TIMEOUT_MS",
  "CLAUDE_CODE_SDK_HAS_HOST_AUTH_REFRESH", "CLAUDE_CODE_SDK_HAS_OAUTH_REFRESH", "CLAUDE_CODE_HOST_CREDS_FILE",
  "CLAUDE_CODE_LAUNCHER_RUNS_API_KEY_HELPER", MANAGED_BY_HOST,
  "CLAUDE_CODE_CLIENT_CERT", "CLAUDE_CODE_CLIENT_KEY", "CLAUDE_CODE_CLIENT_KEY_PASSPHRASE",
  "CLAUDE_CODE_RUNNER_MODEL_SOCKET", "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL",
  // Model choice (the CLI's host-managed model variables), besides ANTHROPIC_*.
  "CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL_FORCE", "CLAUDE_CODE_WORKFLOW_SUBAGENT_MODEL",
  "CLAUDE_CODE_AUTO_MODE_MODEL", "CLAUDE_CODE_BG_CLASSIFIER_MODEL", "CLAUDE_CODE_MODEL_CATALOG", "CLAUDE_CODE_MODEL_CATALOG_URL",
]);

/** Whether an inherited variable is removed before a provider's are set. */
export function isCleared(name: string): boolean {
  const upper = name.toUpperCase();
  return upper.startsWith("ANTHROPIC_") || upper.startsWith("CLAUDE_CODE_3P_") || CLEARED.has(upper);
}

const NAME = /^[A-Z_][A-Z0-9_]*$/;
/** The worker's own: a provider never sets them (the control plane refuses them too). */
const RESERVED = new Set(["HOME", "PATH", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN", MANAGED_BY_HOST]);

export interface LaunchPlan {
  /** The CLI's environment; undefined for local login (the worker's own, unchanged). */
  env?: Record<string, string>;
  /** For logs: the provider id, empty for local login. */
  providerId: string;
  /** Returned variables not applied: reserved names, or "(invalid)" for a malformed one; never values. */
  skipped: string[];
}

/** Builds the CLI environment from the worker's environment and a GetLaunchEnv reply. */
export function launchPlan(reply: SessionReply_LaunchEnv, base: NodeJS.ProcessEnv): LaunchPlan {
  if (reply.localLogin) return { providerId: "", skipped: [] };
  const env: Record<string, string> = {};
  for (const [name, value] of Object.entries(base)) {
    if (value !== undefined && !isCleared(name)) env[name] = value;
  }
  const skipped: string[] = [];
  for (const { name, value } of reply.env) {
    if (!NAME.test(name) || RESERVED.has(name) || name.startsWith("CIRCUS_") || value.includes("\0")) {
      skipped.push(NAME.test(name) && name.length <= 128 ? name : "(invalid)");
      continue;
    }
    env[name] = value;
  }
  env[MANAGED_BY_HOST] = "1";
  return { env, providerId: reply.providerId, skipped };
}
