package store

import (
	"context"
	"encoding/json"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

type WorkspaceState string

const (
	WorkspacePending   WorkspaceState = "pending"
	WorkspacePreparing WorkspaceState = "preparing"
	WorkspaceReady     WorkspaceState = "ready"
	WorkspaceFailed    WorkspaceState = "failed"
)

type WorkspaceWaiting string

const (
	WorkspaceOffline  WorkspaceWaiting = "device_offline"
	WorkspaceOutdated WorkspaceWaiting = "daemon_outdated"
	WorkspaceBackoff  WorkspaceWaiting = "backoff"
	WorkspaceLimit    WorkspaceWaiting = "limit"
)

type Workspace struct {
	ID, UserID, ProjectID, DeviceID, DeviceName string
	CreatedAt                                   int64
	WorkspacePatch
}

// WorkspacePatch replaces the mutable fields together. A preparing row
// carries the exact request needed to recover its current call.
type WorkspacePatch struct {
	State                         WorkspaceState
	Waiting                       WorkspaceWaiting
	Path, Action, RepoURL         string
	Attempt, LostStreak           int64
	BackoffStreak                 int64 // consecutive retryable failures, saturated at the maximum delay
	ExpiredJobResumed             bool  // an attempt can recover one expired, missing job
	CallID                        string
	TimeoutMs                     int64
	ResumeJobID, JobID, JobBootID string
	JobRecordedAt                 int64 // set by the store when it records a job, zero otherwise
	ErrorCode                     circusv1.WorkspaceError
	ErrorDetail                   string
	RetryAt, AttemptStartedAt     int64
	ReadyAt, UpdatedAt            int64
}

type WorkspaceExpect struct {
	State     WorkspaceState
	Attempt   int64
	CallID    string
	UpdatedAt int64
}

func (w Workspace) Expect() WorkspaceExpect {
	return WorkspaceExpect{State: w.State, Attempt: w.Attempt, CallID: w.CallID, UpdatedAt: w.UpdatedAt}
}

type WorkspaceCursor struct {
	State     WorkspaceState
	UpdatedAt int64
	ID        string
}

type WorkspaceNotice struct {
	SessionID string
	Seq       int64 // zero for an ephemeral update
}

type WorkspaceChange struct {
	Applied   bool
	Workspace Workspace
	Notices   []WorkspaceNotice
}

type WorkspaceOps interface {
	// EnsureWorkspace creates a pending row or returns the row already in
	// this project and device. It does not change an existing row.
	EnsureWorkspace(context.Context, Workspace) (Workspace, bool, error)
	GetWorkspace(ctx context.Context, projectID, deviceID string) (Workspace, error)
	GetWorkspaceByID(ctx context.Context, id string) (Workspace, error)
	ListActiveWorkspaces(ctx context.Context, after *WorkspaceCursor, limit int) ([]Workspace, error)
	ListProjectWorkspaces(ctx context.Context, projectID, afterDeviceID string, limit int) ([]Workspace, error)
	ListDeviceWorkspaces(ctx context.Context, userID, deviceID string, limit int) ([]Workspace, error)
	// TransitionWorkspace compares the expected version, updates the row and
	// records the state change for every non-stopped session atomically.
	// Waiting-reason and background-only changes return ephemeral notices.
	// Stale or deleted rows return Applied=false with a nil error.
	TransitionWorkspace(ctx context.Context, id string, expect WorkspaceExpect, patch WorkspacePatch) (WorkspaceChange, error)
	// MarkWorkspaceReady additionally replaces the main working directory of
	// all workspace sessions. The notices identify the sessions to wake.
	MarkWorkspaceReady(ctx context.Context, id string, expect WorkspaceExpect, path, action string, now int64) (WorkspaceChange, error)
	GetWorkspaceJob(ctx context.Context, deviceID, bootID, jobID string) (Job, error)
	ListRunningWorkspaceJobs(ctx context.Context, deviceID, keepBootID string, limit int) ([]Job, error)
	// PruneWorkspaceCalls deletes a bounded chunk of terminal synthetic calls.
	// An empty session ID sweeps all retired (including orphaned) calls.
	// The current write-ahead call of a preparing workspace is retained.
	PruneWorkspaceCalls(ctx context.Context, sessionID string, limit int) (int, error)
}

func DefaultWorkspacePath(projectID string) string { return "~/circus/projects/" + projectID }

const WorkspaceSessionPrefix = "workspace-"

func (w Workspace) SessionID() string { return WorkspaceSessionPrefix + w.ID }

func CheckWorkspacePath(value string) error {
	if !path.IsAbs(value) || path.Clean(value) != value || strings.ContainsRune(value, 0) {
		return invalid("workspace path must be absolute and lexically clean")
	}
	return CheckText("workspace path", value, 4096, false)
}

func workspaceEnumName(value, prefix, unspecified string) string {
	if value == unspecified {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(value, prefix))
}

func WorkspaceActionName(action circusv1.PrepareWorkspaceResult_Action) string {
	return workspaceEnumName(action.String(), "ACTION_", "ACTION_UNSPECIFIED")
}

func (p WorkspacePatch) Background() bool {
	return p.JobID != "" || p.ResumeJobID != ""
}

func (w Workspace) Status() *circusv1.WorkspaceStatus {
	status := &circusv1.WorkspaceStatus{
		State:  circusv1.WorkspaceStatus_State(circusv1.WorkspaceStatus_State_value["STATE_"+strings.ToUpper(string(w.State))]),
		Device: w.DeviceName, Path: w.Path, RepoUrl: workspaceRepoDisplay(w.RepoURL),
		Background: w.Background(),
		Action:     circusv1.PrepareWorkspaceResult_Action(circusv1.PrepareWorkspaceResult_Action_value["ACTION_"+strings.ToUpper(w.Action)]),
		Error:      w.ErrorCode, ErrorDetail: WorkspaceErrorDetail(w.ErrorDetail, w.RepoURL),
		AttemptStartedAtUnixMs: w.AttemptStartedAt, UpdatedAtUnixMs: w.UpdatedAt,
	}
	if status.Path == "" {
		status.Path = DefaultWorkspacePath(w.ProjectID)
	}
	switch w.Waiting {
	case WorkspaceOffline:
		status.Waiting = circusv1.WorkspaceStatus_WAITING_DEVICE_OFFLINE
	case WorkspaceOutdated:
		status.Waiting = circusv1.WorkspaceStatus_WAITING_DAEMON_OUTDATED
	case WorkspaceLimit, WorkspaceBackoff:
		status.Waiting = circusv1.WorkspaceStatus_WAITING_RETRY
	}
	return status
}

func workspaceRepoDisplay(raw string) string {
	if !strings.Contains(raw, "://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Host == "" && u.Scheme != "file") {
		return ""
	}
	if u.User == nil && strings.Contains(u.Path, "@") {
		return "" // ambiguous authority
	}
	if u.User != nil {
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "ftp", "ftps":
			u.User = nil
		default:
			u.User = url.User(u.User.Username())
		}
	}
	u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = "", "", "", false
	return u.String()
}

var urlInWorkspaceDetail = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>]+`)

func WorkspaceErrorDetail(detail, repoURL string) string {
	if repoURL != "" {
		detail = strings.ReplaceAll(detail, repoURL, workspaceRepoDisplay(repoURL))
	}
	detail = urlInWorkspaceDetail.ReplaceAllStringFunc(detail, workspaceRepoDisplay)
	return workspaceText(strings.ToValidUTF8(detail, ""), 1024)
}

func workspaceText(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

// EventPayload keeps metadata and valid JSON even when escaped strings would
// exceed the wire limit. Display strings are shortened only in the event.
func (w Workspace) EventPayload() []byte {
	s := w.Status()
	payload := struct {
		State       string `json:"state"`
		Waiting     string `json:"waiting"`
		Device      string `json:"device"`
		Path        string `json:"path"`
		RepoURL     string `json:"repo_url"`
		Background  bool   `json:"background"`
		Action      string `json:"action"`
		Error       string `json:"error"`
		ErrorDetail string `json:"error_detail"`
	}{
		State: string(w.State), Waiting: workspaceEnumName(s.Waiting.String(), "WAITING_", "WAITING_UNSPECIFIED"),
		Device: s.Device, Path: s.Path, RepoURL: s.RepoUrl, Background: s.Background, Action: w.Action,
		Error: workspaceEnumName(s.Error.String(), "WORKSPACE_ERROR_", "WORKSPACE_ERROR_UNSPECIFIED"), ErrorDetail: s.ErrorDetail,
	}
	b, _ := json.Marshal(payload)
	for _, field := range []*string{&payload.ErrorDetail, &payload.RepoURL, &payload.Path, &payload.Device} {
		for len(b) > 4096 && len(*field) > 0 {
			*field = workspaceText(*field, len(*field)/2)
			b, _ = json.Marshal(payload)
		}
	}
	return b
}
