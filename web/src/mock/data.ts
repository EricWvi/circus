import type { Project, Session, SessionEvent } from "@/lib/types"

import { ciScreenshotUrl } from "./screenshot"

const loadedAt = Date.now()
const ago = (minutes: number) => new Date(loadedAt - minutes * 60_000).toISOString()

let seq = 0
const id = () => `ev-${++seq}`

export const currentUser = { name: "eric", email: "eric@example.com" }

export const devices = { online: 3, total: 4 }

export const projects: Project[] = [
  { id: "circus", name: "circus" },
  { id: "palace", name: "palace" },
  { id: "toolbox", name: "toolbox" },
]

const refreshGo = `  1	package auth
  2
  3	import (
  4		"errors"
  5		"time"
  6	)
  7
  8	var ErrExpired = errors.New("token expired")
  9
 10	// Refresh issues a new access token if the refresh token is still valid.
 11	func (s *Service) Refresh(rt RefreshToken) (AccessToken, error) {
 12		if rt.ExpiresAt.Before(time.Now()) {
 13			return AccessToken{}, ErrExpired
 14		}
 15		return s.issue(rt.Subject, time.Now().Add(s.ttl))
 16	}`

const authEvents: SessionEvent[] = [
  {
    kind: "user_message",
    id: id(),
    at: ago(14),
    text: "The auth tests started failing intermittently on CI after yesterday's merge. Screenshot from the failing run attached. Can you find out why and fix it?",
    attachments: [
      {
        id: "att-1",
        filename: "ci-failure.png",
        mediaType: "image/svg+xml",
        url: ciScreenshotUrl,
      },
    ],
  },
  {
    kind: "reasoning",
    id: id(),
    at: ago(14),
    durationSeconds: 6,
    text: "The failure says `exp` equals `now` to the second. That smells like a boundary condition: the test probably builds a token that expires exactly at the fake clock's current time, while the service reads the wall clock. I'll run the package tests first to see whether it reproduces locally, then read `refresh.go`.",
  },
  {
    kind: "tool_call",
    id: id(),
    at: ago(13),
    tool: "Bash",
    input: { command: "go test ./internal/auth/...", description: "Run auth package tests" },
    status: "completed",
    output: `--- FAIL: TestRefreshToken (0.00s)
    refresh_test.go:48: token expired: exp=1763204400 now=1763204400
FAIL
FAIL	github.com/acme/circus/internal/auth	0.388s
ok  	github.com/acme/circus/internal/auth/oidc	0.122s
FAIL`,
  },
  {
    kind: "tool_call",
    id: id(),
    at: ago(13),
    tool: "Read",
    input: { file_path: "internal/auth/refresh.go" },
    status: "completed",
    output: refreshGo,
  },
  {
    kind: "assistant_text",
    id: id(),
    at: ago(12),
    text: `Found it. \`Refresh\` calls \`time.Now()\` directly, but the test injects a fake clock through \`Service.clock\`. When the test runs at a second boundary, the token's \`ExpiresAt\` (from the fake clock) and the wall clock disagree.

The fix is to use the injected clock everywhere and treat \`ExpiresAt == now\` as expired, matching the OIDC spec:

\`\`\`go
func (s *Service) Refresh(rt RefreshToken) (AccessToken, error) {
	now := s.clock.Now()
	if !now.Before(rt.ExpiresAt) {
		return AccessToken{}, ErrExpired
	}
	return s.issue(rt.Subject, now.Add(s.ttl))
}
\`\`\``,
  },
  {
    kind: "tool_call",
    id: id(),
    at: ago(11),
    tool: "Edit",
    input: {
      file_path: "internal/auth/refresh.go",
      old_string: `	if rt.ExpiresAt.Before(time.Now()) {
		return AccessToken{}, ErrExpired
	}
	return s.issue(rt.Subject, time.Now().Add(s.ttl))`,
      new_string: `	now := s.clock.Now()
	if !now.Before(rt.ExpiresAt) {
		return AccessToken{}, ErrExpired
	}
	return s.issue(rt.Subject, now.Add(s.ttl))`,
    },
    status: "completed",
    output: "The file internal/auth/refresh.go has been updated.",
  },
  {
    kind: "tool_call",
    id: id(),
    at: ago(10),
    tool: "Read",
    input: { file_path: "internal/auth/clock_test.go" },
    status: "error",
    error: "File does not exist. Did you mean internal/auth/clock.go?",
  },
  {
    kind: "tool_call",
    id: id(),
    at: ago(9),
    tool: "Bash",
    input: {
      command: "go test -count=50 ./internal/auth/...",
      description: "Run auth tests 50 times to check for flakes",
    },
    status: "completed",
    output: `ok  	github.com/acme/circus/internal/auth	4.871s
ok  	github.com/acme/circus/internal/auth/oidc	1.902s`,
  },
  {
    kind: "assistant_text",
    id: id(),
    at: ago(8),
    text: "50 consecutive runs pass. I committed the fix on `fix/auth-refresh-clock`; pushing it lets CI confirm the flake is gone on the same runners.",
  },
  {
    kind: "tool_call",
    id: id(),
    at: ago(8),
    tool: "Bash",
    input: {
      command: "git push -u origin fix/auth-refresh-clock",
      description: "Push the fix branch",
    },
    status: "awaiting_approval",
    approval: {
      reason: "Push the fix branch so CI can confirm the flake is gone on the same runners.",
    },
  },
]

const storeEvents: SessionEvent[] = [
  {
    kind: "user_message",
    id: id(),
    at: ago(6),
    text: "Add a fencing token check to `Store.Append` so a writer holding an expired lease cannot append to the transcript.",
  },
  {
    kind: "reasoning",
    id: id(),
    at: ago(6),
    durationSeconds: 4,
    text: "Leases already carry a monotonically increasing epoch. Append should compare the caller's epoch with the stored one inside the same transaction.",
  },
  {
    kind: "tool_call",
    id: id(),
    at: ago(5),
    tool: "Read",
    input: { file_path: "internal/store/append.go", offset: 1, limit: 40 },
    status: "completed",
    output: ` 1	package store
 2
 3	// Append writes events to the session transcript.
 4	func (s *Store) Append(ctx context.Context, sid SessionID, evs []Event) error {
 5		return s.tx(ctx, func(tx *Tx) error {
 6			return tx.insertEvents(sid, evs)
 7		})
 8	}`,
  },
  {
    kind: "tool_call",
    id: id(),
    at: ago(4),
    tool: "Edit",
    input: {
      file_path: "internal/store/append.go",
      old_string: `func (s *Store) Append(ctx context.Context, sid SessionID, evs []Event) error {
	return s.tx(ctx, func(tx *Tx) error {`,
      new_string: `func (s *Store) Append(ctx context.Context, sid SessionID, epoch uint64, evs []Event) error {
	return s.tx(ctx, func(tx *Tx) error {
		if err := tx.checkFence(sid, epoch); err != nil {
			return err
		}`,
    },
    status: "completed",
    output: "The file internal/store/append.go has been updated.",
  },
  {
    kind: "tool_call",
    id: id(),
    at: ago(1),
    tool: "Bash",
    input: { command: "go test ./internal/store/...", description: "Run store tests" },
    status: "running",
  },
]

const webEvents: SessionEvent[] = [
  {
    kind: "user_message",
    id: id(),
    at: ago(180),
    text: "Which Vite version does the web workspace use?",
  },
  {
    kind: "tool_call",
    id: id(),
    at: ago(180),
    tool: "Read",
    input: { file_path: "web/package.json" },
    status: "completed",
    output: `  1	{\n  2	  "name": "@circus/web",\n  3	  "devDependencies": {\n  4	    "vite": "8.3.8"\n  5	  }\n  6	}`,
  },
  {
    kind: "assistant_text",
    id: id(),
    at: ago(179),
    text: "It pins **Vite 8.3.8** in `web/package.json` (exact version, no range).",
  },
]

const libvipsEvents: SessionEvent[] = [
  {
    kind: "user_message",
    id: id(),
    at: ago(95),
    text: "Replace the ImageMagick thumbnailer with libvips and keep the output byte-identical for JPEGs.",
  },
  {
    kind: "assistant_text",
    id: id(),
    at: ago(94),
    text: "I'll start by checking which libvips version is available on the build device.",
  },
  {
    kind: "tool_call",
    id: id(),
    at: ago(94),
    tool: "Bash",
    input: { command: "vips --version", description: "Check libvips version" },
    status: "error",
    error: "Lost connection to device palace-builder while the command was running. The session cannot continue until the device reconnects.",
  },
]

const darkModeEvents: SessionEvent[] = [
  {
    kind: "user_message",
    id: id(),
    at: ago(2),
    text: "Add a dark mode toggle to the settings page, persisted per user.",
  },
]

const dotfilesEvents: SessionEvent[] = [
  {
    kind: "user_message",
    id: id(),
    at: ago(60 * 26),
    text: "Rewrite `install.sh` in Go so it works on Windows too.",
  },
  {
    kind: "reasoning",
    id: id(),
    at: ago(60 * 26),
    durationSeconds: 3,
    text: "The script mostly creates symlinks and copies a few files. On Windows, symlinks need developer mode, so I'll fall back to copies there.",
  },
  {
    kind: "assistant_text",
    id: id(),
    at: ago(60 * 26),
    text: "Plan:\n\n1. Port the symlink table to a Go map.\n2. Detect Windows and fall back to copying.\n3. Keep `install.sh` as a thin wrapper that calls `go run ./cmd/install`.",
  },
]

const lintEvents: SessionEvent[] = [
  {
    kind: "user_message",
    id: id(),
    at: ago(60 * 50),
    text: "Bump golangci-lint to v2 and migrate the config.",
  },
  {
    kind: "tool_call",
    id: id(),
    at: ago(60 * 50),
    tool: "Bash",
    input: { command: "golangci-lint migrate", description: "Migrate config to v2" },
    status: "completed",
    output: "Migrated .golangci.yml to version 2.",
  },
  {
    kind: "assistant_text",
    id: id(),
    at: ago(60 * 50),
    text: "Done. The config is migrated and `golangci-lint run ./...` reports no new issues.",
  },
]

export const sessions: Session[] = [
  {
    id: "fix-auth-refresh",
    projectId: "circus",
    title: "Fix flaky auth token refresh tests",
    state: "awaiting_approval",
    lastActivityAt: ago(8),
    pendingApprovalCount: 1,
    deviceName: "eric-mbp",
    events: authEvents,
  },
  {
    id: "store-fencing",
    projectId: "circus",
    title: "Fencing check for transcript appends",
    state: "running",
    lastActivityAt: ago(1),
    pendingApprovalCount: 0,
    deviceName: "eric-mbp",
    events: storeEvents,
  },
  {
    id: "web-vite",
    projectId: "circus",
    title: "Which Vite version does web use?",
    state: "idle",
    lastActivityAt: ago(179),
    pendingApprovalCount: 0,
    deviceName: "devbox",
    events: webEvents,
  },
  {
    id: "quic-spike",
    projectId: "circus",
    title: "Spike: QUIC transport for the worker link",
    state: "stopped",
    lastActivityAt: ago(60 * 24 * 6),
    pendingApprovalCount: 0,
    deviceName: "devbox",
    events: [],
  },
  {
    id: "libvips",
    projectId: "palace",
    title: "Migrate thumbnailer to libvips",
    state: "broken",
    lastActivityAt: ago(94),
    pendingApprovalCount: 0,
    deviceName: "palace-builder",
    events: libvipsEvents,
  },
  {
    id: "settings-dark-mode",
    projectId: "palace",
    title: "Dark mode toggle on the settings page",
    state: "queued",
    lastActivityAt: ago(2),
    pendingApprovalCount: 0,
    deviceName: "eric-mbp",
    events: darkModeEvents,
  },
  {
    id: "palace-new",
    projectId: "palace",
    title: "New session",
    state: "idle",
    lastActivityAt: ago(30),
    pendingApprovalCount: 0,
    deviceName: "eric-mbp",
    events: [],
  },
  {
    id: "dotfiles-go",
    projectId: "toolbox",
    title: "Rewrite the dotfiles installer in Go",
    state: "interrupted",
    lastActivityAt: ago(60 * 26),
    pendingApprovalCount: 0,
    deviceName: "eric-win",
    events: dotfilesEvents,
  },
  {
    id: "golangci-v2",
    projectId: "toolbox",
    title: "Bump golangci-lint to v2",
    state: "idle",
    lastActivityAt: ago(60 * 50),
    pendingApprovalCount: 0,
    deviceName: "devbox",
    events: lintEvents,
  },
]
