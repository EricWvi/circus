package webapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/ledgersnaps"
	"github.com/EricWvi/circus/internal/controlplane/questions"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/ledger"
	"github.com/EricWvi/circus/internal/store"
	"github.com/google/uuid"
)

// Session API bounds.
const (
	// defaultSessionPage and maxSessionPage are ListSessions' page sizes.
	defaultSessionPage = 100
	maxSessionPage     = 1000
	// approvalGrantLifetime is how long the grant of an approval lasts.
	approvalGrantLifetime = 10 * time.Minute
	// stopReason is the reason StopSession records for a user's stop.
	stopReason = "USER"
	// stopJobsTimeout bounds the stop of a stopped session's device jobs.
	stopJobsTimeout = 5 * time.Second
)

// approvalMsgPrefix starts the client message id of the input an approval
// creates (the store's naming); SendMessage reserves it.
const approvalMsgPrefix = "approval:"

// Fixed refusals of the session API.
var (
	errSessionNotFound  = errors.New("session not found")
	errApprovalNotFound = errors.New("approval not found")
	errSessionStopped   = errors.New("the session is stopped")
)

// JobStopper stops the background device jobs of a session
// (calls.Router.StopSessionJobs). It returns once the stop is admitted to
// the devices' links; an offline device's jobs are not reached.
type JobStopper interface {
	StopSessionJobs(ctx context.Context, userID, sessionID string) error
}

// sessionIDArg checks a request's session id. An empty one is NOT_FOUND
// like any other unknown id.
func sessionIDArg(id string) error {
	if len(id) > store.MaxIDBytes {
		return invalidField("session_id", "session id is too long")
	}
	if id == "" {
		return connect.NewError(connect.CodeNotFound, errSessionNotFound)
	}
	return nil
}

// ownSession returns the session when it belongs to a project of u, else
// NOT_FOUND: the sessions of other users' projects do not exist for u. Run
// it with the same ops (store or tx) as the write it authorizes.
func ownSession(ctx context.Context, ops store.Ops, u store.User, id string, now int64) (store.Session, error) {
	sess, err := ops.GetSession(ctx, id, now)
	if err != nil {
		return store.Session{}, err
	}
	if _, err := ops.GetProject(ctx, u.ID, sess.ProjectID); err != nil {
		return store.Session{}, err
	}
	return sess, nil
}

// sessionError maps the error of a session operation: a connect error
// returned from inside a transaction passes through; store errors get fixed
// messages, never the store's text.
func (s *Service) sessionError(ctx context.Context, op string, err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce
	}
	if ctx.Err() != nil {
		return s.internal(ctx, op, err)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errSessionNotFound)
	case errors.Is(err, store.ErrStopped):
		return connect.NewError(connect.CodeFailedPrecondition, errSessionStopped)
	}
	return s.internal(ctx, op, err)
}

func protoState(st store.SessionState) circusv1.Session_State {
	switch st {
	case store.SessionIdle:
		return circusv1.Session_STATE_IDLE
	case store.SessionQueued:
		return circusv1.Session_STATE_QUEUED
	case store.SessionRunning:
		return circusv1.Session_STATE_RUNNING
	case store.SessionStopped:
		return circusv1.Session_STATE_STOPPED
	case store.SessionBroken:
		return circusv1.Session_STATE_BROKEN
	}
	return circusv1.Session_STATE_UNSPECIFIED
}

func protoOutcome(o store.TurnOutcome) circusv1.TurnOutcome {
	switch o {
	case store.TurnCompleted:
		return circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED
	case store.TurnInterrupted:
		return circusv1.TurnOutcome_TURN_OUTCOME_INTERRUPTED
	case store.TurnError:
		return circusv1.TurnOutcome_TURN_OUTCOME_ERROR
	case store.TurnStopped:
		return circusv1.TurnOutcome_TURN_OUTCOME_STOPPED
	}
	return circusv1.TurnOutcome_TURN_OUTCOME_UNSPECIFIED
}

// deviceNames resolves device ids to names, deleted devices included, once
// per id.
type deviceNames struct {
	ops   store.Ops
	names map[string]string
}

func newDeviceNames(ops store.Ops) *deviceNames {
	return &deviceNames{ops: ops, names: map[string]string{}}
}

func (d *deviceNames) name(ctx context.Context, id string) (string, error) {
	if n, ok := d.names[id]; ok {
		return n, nil
	}
	idn, err := d.ops.GetIdentity(ctx, store.KindDevice, id)
	if errors.Is(err, store.ErrNotFound) {
		err = nil
	}
	if err != nil {
		return "", err
	}
	d.names[id] = idn.Name
	return idn.Name, nil
}

// listSession is the Session of ListSessions: without config. A queued
// session carries the scheduler's queue reason; every session, whether its
// lease holder is disconnected.
func (s *Service) listSession(sess store.Session) *circusv1.Session {
	out := &circusv1.Session{
		SessionId: sess.ID, State: protoState(sess.State), LastTurnOutcome: protoOutcome(sess.LastTurnOutcome),
		TurnCount: uint64(max(sess.TurnCount, 0)), CreatedAtUnixMs: sess.CreatedAt, StoppedAtUnixMs: sess.StoppedAt,
		ViewTruncated: sess.ViewTruncated, PinnedWorkerId: sess.PinnedWorkerID, ProjectId: sess.ProjectID,
		Title: sess.Title, LastActivityAtUnixMs: sess.LastActivityAt, PendingApprovalCount: sess.PendingApprovalCount,
		ProviderId: sess.ProviderID, WorkerDisconnectedAtUnixMs: s.scheduler.WorkerDisconnectedAt(sess.ID),
	}
	if sess.State == store.SessionQueued {
		out.QueueReason = s.scheduler.QueueReason(sess.ID)
	}
	return out
}

// fullSession is the Session of GetSession and the writes: with config,
// whose working directories are the main thread's current ones.
func (s *Service) fullSession(ctx context.Context, ops store.Ops, sess store.Session) (*circusv1.Session, error) {
	out := s.listSession(sess)
	status, err := readSessionWorkspace(ctx, ops, sess)
	if err != nil {
		return nil, err
	}
	out.Workspace = status
	switch l, err := ops.GetSessionLedger(ctx, sess.ID); {
	case err == nil:
		out.Ledger = ledgersnaps.Status(l)
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	names := newDeviceNames(ops)
	device, err := names.name(ctx, sess.DefaultDeviceID)
	if err != nil {
		return nil, err
	}
	cwds, err := ops.ListSessionCwds(ctx, sess.ID)
	if err != nil {
		return nil, err
	}
	out.Config = &circusv1.SessionConfig{DefaultDevice: device, Model: sess.Model, PermissionMode: sess.PermissionMode, Local: sess.Local}
	for _, c := range cwds {
		name, err := names.name(ctx, c.DeviceID)
		if err != nil {
			return nil, err
		}
		out.Config.Cwds = append(out.Config.Cwds, &circusv1.SessionConfig_Cwd{Device: name, Cwd: c.Cwd})
	}
	return out, nil
}

// protoEvent converts a stored view event. Kinds are the proto names
// without KIND_; an unknown one is KIND_UNSPECIFIED.
func protoEvent(e store.SessionEvent) *circusv1.SessionEvent {
	payload := string(e.Payload)
	if !utf8.ValidString(payload) {
		payload = strings.ToValidUTF8(payload, "�")
	}
	return &circusv1.SessionEvent{
		Seq: uint64(e.Seq), TsUnixMs: e.At, Kind: circusv1.SessionEvent_Kind(circusv1.SessionEvent_Kind_value["KIND_"+e.Kind]),
		PayloadJson: payload, Truncated: e.Truncated,
	}
}

func kindName(k circusv1.SessionEvent_Kind) string {
	return strings.TrimPrefix(k.String(), "KIND_")
}

// encodePayload encodes a control-plane view event's payload as JSON
// without HTML escaping, so "<", ">" and "&" take one byte each.
func encodePayload(payload any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// appendEvent records one control-plane view event and returns its seq, 0
// when the view no longer stores it (a full view keeps boundaries only).
func appendEvent(ctx context.Context, tx store.Ops, sessionID string, kind circusv1.SessionEvent_Kind, payload any, at int64, boundary bool) (int64, error) {
	body, err := encodePayload(payload)
	if err != nil {
		return 0, err
	}
	res, err := tx.AppendEvents(ctx, sessionID, nil, []store.EventInput{{Kind: kindName(kind), Payload: body, At: at, Boundary: boundary}}, store.DefaultMaxViewBytes)
	if err != nil {
		return 0, err
	}
	return res.Seqs[0], nil
}

// Payloads of the view events the control plane records itself.
type (
	userEventPayload struct {
		InputID     string `json:"input_id"`
		ClientMsgID string `json:"client_msg_id"`
		Priority    string `json:"priority"`
		Text        string `json:"text"`
	}
)

func (s *Service) CreateSession(ctx context.Context, req *connect.Request[circusv1.CreateSessionRequest]) (*connect.Response[circusv1.CreateSessionResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if err := projectID(m.GetProjectId()); err != nil {
		return nil, err
	}
	cfg := m.GetConfig()
	switch {
	case m.GetClientRequestId() == "":
		return nil, invalidField("client_request_id", "client request id is required")
	case len(m.GetClientRequestId()) > store.MaxIDBytes:
		return nil, invalidField("client_request_id", "client request id is too long")
	case cfg.GetDefaultDevice() != "":
		return nil, invalidField("config.default_device", "the device comes from the project location")
	case len(cfg.GetCwds()) > 0:
		return nil, invalidField("config.cwds", "the working directory comes from the project location")
	case len(cfg.GetModel()) > store.MaxIDBytes:
		return nil, invalidField("config.model", "model is longer than 128 bytes")
	case len(cfg.GetPermissionMode()) > store.MaxIDBytes:
		return nil, invalidField("config.permission_mode", "permission mode is longer than 128 bytes")
	case cfg.GetPermissionMode() == "plan":
		// Its ExitPlanMode is disallowed until plan approval exists: the
		// session could never leave plan mode.
		return nil, invalidField("config.permission_mode", "plan mode is not supported")
	case cfg.GetLocal():
		return nil, invalidField("config.local", "local mode is decided by the server")
	case len(m.GetPinnedWorkerId()) > store.MaxIDBytes:
		return nil, invalidField("pinned_worker_id", "worker id is too long")
	case len(m.GetProviderId()) > store.MaxIDBytes:
		return nil, invalidField("provider_id", "provider id is too long")
	}
	now := s.now().UnixMilli()
	var out *circusv1.Session
	var workspaceChange store.WorkspaceChange
	// created is the session this call created; zero for a replay.
	var created store.Session
	const requestConflict = "this client request id was used for a different session"
	attempt := func(ctx context.Context, tx store.Ops) error {
		workspaceChange = store.WorkspaceChange{}
		created = store.Session{}
		p, err := tx.GetProject(ctx, u.ID, m.GetProjectId())
		if errors.Is(err, store.ErrNotFound) {
			return connect.NewError(connect.CodeNotFound, errProjectNotFound)
		}
		if err != nil {
			return err
		}
		old, err := tx.GetSessionByClientRequestID(ctx, p.ID, m.GetClientRequestId(), now)
		switch {
		case err == nil:
			if !old.MatchesCreate(m.LocationIndex, m.GetPinnedWorkerId(), cfg.GetModel(), cfg.GetPermissionMode(), m.ProviderId) {
				return fieldError(connect.CodeAlreadyExists, "client_request_id", requestConflict)
			}
			out, err = s.fullSession(ctx, tx, old)
			return err
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		if s.removingWorkspaces(p.UserID, p.ID) {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("the project is being deleted"))
		}
		if len(p.Locations) == 0 {
			return fieldError(connect.CodeFailedPrecondition, "location_index", "the project has no locations")
		}
		index := min(p.LastUsedLocation, len(p.Locations)-1)
		if m.LocationIndex != nil {
			if m.GetLocationIndex() >= uint32(len(p.Locations)) {
				return invalidField("location_index", "location index is out of range")
			}
			index = int(m.GetLocationIndex())
		}
		l := p.Locations[index]
		device, err := tx.GetIdentity(ctx, store.KindDevice, l.DeviceID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		// The location keeps the device id it was bound to; a revoked
		// (deleted) device stays deleted even when another machine takes
		// its name. The project's reads derive DeviceRevoked and DeviceName
		// from this same row.
		if err != nil || device.DeletedAt != 0 || device.UserID != u.ID {
			return fieldError(connect.CodeFailedPrecondition, "location_index", "the location's device is not one of your devices")
		}
		pin, local, derived, err := placeSession(ctx, tx, u.ID, m.GetPinnedWorkerId(), device)
		if err != nil {
			return err
		}
		provider, err := resolveProvider(ctx, tx, u, p, m.ProviderId, now)
		if err != nil {
			return err
		}
		id, err := newID()
		if err != nil {
			return err
		}
		cwd := l.Path
		var workspace store.Workspace
		var createdWorkspace bool
		if l.Path == "" {
			workspaceID, err := newID()
			if err != nil {
				return err
			}
			workspace, createdWorkspace, err = tx.EnsureWorkspace(ctx, store.Workspace{
				ID: workspaceID, UserID: p.UserID, ProjectID: p.ID, DeviceID: l.DeviceID, CreatedAt: now,
			})
			if err != nil {
				return err
			}
			cwd = defaultDir(p.ID)
			if workspace.State == store.WorkspaceReady {
				cwd = workspace.Path
			}
		}
		sess := store.Session{
			ID: id, UserID: u.ID, ProjectID: p.ID, ClaudeSessionID: uuid.NewString(), State: store.SessionIdle,
			ClientRequestID: m.GetClientRequestId(), LocationIndex: uint32(index), Workspace: l.Path == "",
			DefaultDeviceID: l.DeviceID, Model: cfg.GetModel(), PermissionMode: cfg.GetPermissionMode(),
			PinnedWorkerID: pin, PinDerived: derived, Local: local, ProviderID: provider, CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
		}
		cwds := []store.SessionCwd{{DeviceID: l.DeviceID, Cwd: cwd, UpdatedAt: now}}
		res, err := tx.CreateSession(ctx, sess, cwds, store.DefaultMaxSessionsPerUser)
		if err != nil {
			return err
		}
		if res.Created && s.snapshots != nil {
			// The session's ledger snapshot starts from the main line's
			// head; the first turn waits until it is ready or failed.
			head, err := tx.GetLedgerHead(ctx, p.ID)
			if err != nil {
				return err
			}
			if err := tx.CreateSessionLedger(ctx, ledgersnaps.NewSessionLedger(res.Session, head)); err != nil {
				return err
			}
			if res.Session, err = tx.GetSession(ctx, id, now); err != nil {
				return err
			}
		}
		if res.Created {
			created = res.Session
			if res.Session.Workspace {
				workspaceChange, err = retryWorkspace(ctx, tx, workspace, now)
				if err != nil {
					return err
				}
				if createdWorkspace {
					ev, err := tx.AppendEvents(ctx, id, nil, []store.EventInput{{
						Kind: store.EventKindWorkspace, At: workspace.UpdatedAt, Payload: workspace.EventPayload(), Boundary: true,
					}}, store.DefaultMaxViewBytes)
					if err != nil {
						return err
					}
					workspaceChange.Notices = []store.WorkspaceNotice{{SessionID: id, Seq: ev.Seqs[0]}}
				}
			}
			if _, err := tx.UpdateProject(ctx, u.ID, p.ID, store.ProjectPatch{LastUsedLocation: &index, UpdatedAt: now}); err != nil {
				return err
			}
		}
		out, err = s.fullSession(ctx, tx, res.Session)
		return err
	}
	err = s.st.Tx(ctx, attempt)
	if errors.Is(err, store.ErrExists) && ctx.Err() == nil {
		// A concurrent insert can win the unique key. Read the winner in a
		// new transaction so the lookup sees the committed session.
		err = s.st.Tx(ctx, attempt)
	}
	if err != nil {
		if ctx.Err() == nil {
			switch {
			case errors.Is(err, store.ErrExists):
				return nil, fieldError(connect.CodeAlreadyExists, "client_request_id", requestConflict)
			case errors.Is(err, store.ErrLimit):
				return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("at most %d sessions", store.DefaultMaxSessionsPerUser))
			}
		}
		return nil, s.sessionError(ctx, "create session", err)
	}
	s.workspaceCommitted(workspaceChange)
	if c := created; c.ID != "" && s.snapshots != nil {
		s.snapshots.Kick(c.ID)
	}
	if c := created; c.ID != "" {
		s.logger.LogAttrs(ctx, slog.LevelInfo, "session created", slog.String("session_id", c.ID), slog.String("user_id", c.UserID),
			slog.String("project_id", c.ProjectID), slog.String("device_id", c.DefaultDeviceID), slog.String("pinned_worker_id", c.PinnedWorkerID),
			slog.Bool("local", c.Local), slog.String("provider_id", c.ProviderID))
	}
	return connect.NewResponse(&circusv1.CreateSessionResponse{Session: out}), nil
}

// placeSession decides where a new session of user runs on device: the
// worker it is pinned to and whether it is local (that worker shares the
// device's machine). A requested worker must be a live worker of the user;
// the session is local exactly when it is on the device's machine.
// Without one, the user's live worker on that machine with an unrevoked
// credential (MachineWorker) makes the session local and pinned to it
// (derived); with none the session is neither.
func placeSession(ctx context.Context, tx store.Ops, userID, requested string, device store.Identity) (pin string, local, derived bool, err error) {
	if requested != "" {
		worker, err := tx.GetIdentity(ctx, store.KindWorker, requested)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", false, false, err
		}
		if err != nil || worker.DeletedAt != 0 || worker.UserID != userID {
			return "", false, false, invalidField("pinned_worker_id", "unknown worker")
		}
		return requested, device.MachineID != "" && worker.MachineID == device.MachineID, false, nil
	}
	if device.MachineID == "" {
		return "", false, false, nil
	}
	worker, err := tx.MachineWorker(ctx, userID, device.MachineID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", false, false, nil
	case err != nil:
		return "", false, false, err
	}
	return worker.ID, true, true, nil
}

func storePriority(p circusv1.InputPriority) (store.InputPriority, bool) {
	switch p {
	case circusv1.InputPriority_INPUT_PRIORITY_UNSPECIFIED, circusv1.InputPriority_INPUT_PRIORITY_NORMAL:
		return store.InputNormal, true
	case circusv1.InputPriority_INPUT_PRIORITY_NOW:
		return store.InputNow, true
	}
	return "", false
}

func (s *Service) SendMessage(ctx context.Context, req *connect.Request[circusv1.SendMessageRequest]) (*connect.Response[circusv1.SendMessageResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if err := sessionIDArg(m.GetSessionId()); err != nil {
		return nil, err
	}
	switch {
	case m.GetClientMsgId() == "":
		return nil, invalidField("client_msg_id", "client message id is required")
	case len(m.GetClientMsgId()) > store.MaxIDBytes:
		return nil, invalidField("client_msg_id", "client message id is too long")
	case jsTrim(m.GetText()) == "":
		return nil, invalidField("text", "enter a message")
	case len(m.GetText()) > store.MaxInputTextBytes:
		return nil, invalidField("text", "message is longer than 256 KiB")
	}
	if strings.HasPrefix(m.GetClientMsgId(), approvalMsgPrefix) {
		return nil, invalidField("client_msg_id", "client message ids starting with approval: are reserved")
	}
	priority, ok := storePriority(m.GetPriority())
	if !ok {
		return nil, invalidField("priority", "unknown priority")
	}
	inputID, err := newID()
	if err != nil {
		return nil, s.internal(ctx, "input id", err)
	}
	// The INPUT_ACCEPTED view event carries the whole text: a message is accepted only
	// when that event's payload fits the view's payload cap, so the view
	// never stores a message without its text.
	userEvent := userEventPayload{InputID: inputID, ClientMsgID: m.GetClientMsgId(), Priority: string(priority), Text: m.GetText()}
	body, err := encodePayload(userEvent)
	if err != nil {
		return nil, s.internal(ctx, "encode message", err)
	}
	if len(body) > store.MaxEventPayloadBytes {
		return nil, invalidField("text", "message is too long for the conversation view")
	}
	now := s.now().UnixMilli()
	var res store.InputSubmitted
	var seq int64
	err = s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		if _, err := ownSession(ctx, tx, u, m.GetSessionId(), now); err != nil {
			return err
		}
		var err error
		res, err = tx.SubmitInput(ctx, store.Input{
			InputID: inputID, SessionID: m.GetSessionId(), ClientMsgID: m.GetClientMsgId(), Kind: store.InputUser,
			Text: m.GetText(), Priority: priority, CreatedAt: now,
		})
		if err != nil {
			return err
		}
		if !res.Created {
			seq = res.Input.EventSeq
			return nil
		}
		// The user's message enters the view with the input, so every
		// watcher sees it at once; its seq is the reply's event_seq.
		seq, err = appendEvent(ctx, tx, m.GetSessionId(), circusv1.SessionEvent_KIND_INPUT_ACCEPTED, userEvent, now, false)
		if err != nil {
			return err
		}
		return tx.SetInputEventSeq(ctx, m.GetSessionId(), res.Input.InputID, seq)
	})
	if err != nil {
		if ctx.Err() == nil {
			switch {
			case errors.Is(err, store.ErrExists):
				return nil, fieldError(connect.CodeAlreadyExists, "client_msg_id", "this client message id was sent with other content")
			case errors.Is(err, store.ErrLimit):
				return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("at most %d messages may wait for delivery", store.MaxOpenInputs))
			}
		}
		return nil, s.sessionError(ctx, "send message", err)
	}
	if res.Created {
		s.bus.Durable(m.GetSessionId(), uint64(seq))
		reasons := []circusv1.WorkerEvent_Nudge_Reason{circusv1.WorkerEvent_Nudge_REASON_INPUT}
		if priority == store.InputNow && res.Session.State == store.SessionRunning {
			reasons = append(reasons, circusv1.WorkerEvent_Nudge_REASON_INTERRUPT)
		}
		s.waker.Wake(m.GetSessionId(), reasons...)
		if s.logger.Enabled(ctx, slog.LevelDebug) {
			s.logger.LogAttrs(ctx, slog.LevelDebug, "message accepted", slog.String("session_id", m.GetSessionId()),
				slog.String("input_id", res.Input.InputID), slog.String("priority", string(priority)), slog.Int("bytes", len(m.GetText())))
		}
	}
	return connect.NewResponse(&circusv1.SendMessageResponse{InputId: res.Input.InputID, EventSeq: uint64(seq)}), nil
}

// sessionCursor is ListSessions' page token: the keyset of the last
// session returned. Tampering only moves the caller within its own
// sessions.
func encodeCursor(c store.SessionCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.LastActivityAt, 10) + "|" + c.ID))
}

func decodeCursor(token string) (*store.SessionCursor, bool) {
	if len(token) > 2*store.MaxIDBytes {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, false
	}
	at, id, ok := strings.Cut(string(raw), "|")
	n, err := strconv.ParseInt(at, 10, 64)
	if !ok || err != nil || id == "" || len(id) > store.MaxIDBytes {
		return nil, false
	}
	return &store.SessionCursor{LastActivityAt: n, ID: id}, true
}

func (s *Service) ListSessions(ctx context.Context, req *connect.Request[circusv1.ListSessionsRequest]) (*connect.Response[circusv1.ListSessionsResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	size := int(m.GetPageSize())
	switch {
	case size == 0:
		size = defaultSessionPage
	case size > maxSessionPage:
		return nil, invalidField("page_size", "page size is larger than 1000")
	}
	filter := store.SessionFilter{Now: s.now().UnixMilli()}
	if m.GetPageToken() != "" {
		var ok bool
		if filter.After, ok = decodeCursor(m.GetPageToken()); !ok {
			return nil, invalidField("page_token", "invalid page token")
		}
	}
	if pid := m.GetProjectId(); pid != "" {
		if len(pid) > maxProjectIDBytes {
			return nil, invalidField("project_id", "project id is too long")
		}
		if _, err := s.st.GetProject(ctx, u.ID, pid); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, connect.NewError(connect.CodeNotFound, errProjectNotFound)
			}
			return nil, s.internal(ctx, "get project", err)
		}
		filter.ProjectID = pid
	} else {
		// Every project of the caller, in the store's order; never filtered
		// after paging.
		filter.WebUserID = u.ID
	}
	out := &circusv1.ListSessionsResponse{Sessions: []*circusv1.Session{}}
	statuses := make(map[[2]string]*circusv1.WorkspaceStatus)
	for len(out.Sessions) < size {
		filter.Limit = min(size-len(out.Sessions), store.MaxPageSize)
		rows, err := s.st.ListSessions(ctx, filter)
		if err != nil {
			return nil, s.internal(ctx, "list sessions", err)
		}
		ids := make([]string, 0, len(rows))
		for _, sess := range rows {
			ids = append(ids, sess.ID)
		}
		ledgers, err := s.st.ListSessionLedgerStatuses(ctx, ids)
		if err != nil {
			return nil, s.internal(ctx, "list session ledgers", err)
		}
		for _, sess := range rows {
			summary := s.listSession(sess)
			if l, ok := ledgers[sess.ID]; ok {
				summary.Ledger = ledgersnaps.Status(l)
			}
			if sess.Workspace {
				key := [2]string{sess.ProjectID, sess.DefaultDeviceID}
				status, ok := statuses[key]
				if !ok {
					status, err = readSessionWorkspace(ctx, s.st, sess)
					if err != nil {
						return nil, s.internal(ctx, "list session workspaces", err)
					}
					statuses[key] = status
				}
				summary.Workspace = status
			}
			out.Sessions = append(out.Sessions, summary)
		}
		if len(rows) < filter.Limit {
			return connect.NewResponse(out), nil
		}
		last := rows[len(rows)-1]
		filter.After = &store.SessionCursor{LastActivityAt: last.LastActivityAt, ID: last.ID}
	}
	out.NextPageToken = encodeCursor(*filter.After)
	return connect.NewResponse(out), nil
}

func (s *Service) GetSession(ctx context.Context, req *connect.Request[circusv1.GetSessionRequest]) (*connect.Response[circusv1.Session], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	if err := sessionIDArg(req.Msg.GetSessionId()); err != nil {
		return nil, err
	}
	var out *circusv1.Session
	err = s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		sess, err := ownSession(ctx, tx, u, req.Msg.GetSessionId(), s.now().UnixMilli())
		if err != nil {
			return err
		}
		out, err = s.fullSession(ctx, tx, sess)
		return err
	})
	if err != nil {
		return nil, s.sessionError(ctx, "get session", err)
	}
	return connect.NewResponse(out), nil
}

func (s *Service) UpdateSession(ctx context.Context, req *connect.Request[circusv1.UpdateSessionRequest]) (*connect.Response[circusv1.Session], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if err := sessionIDArg(m.GetSessionId()); err != nil {
		return nil, err
	}
	if m.Title == nil && m.ProviderId == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("set the title or the provider"))
	}
	var title string
	if m.Title != nil {
		var problem string
		if title, problem = checkTitle(m.GetTitle()); problem != "" {
			return nil, invalidField("title", problem)
		}
	}
	if len(m.GetProviderId()) > store.MaxIDBytes {
		return nil, invalidField("provider_id", "provider id is too long")
	}
	var out *circusv1.Session
	var set store.SessionProviderSet
	var oldProvider string
	err = s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		now := s.now().UnixMilli()
		set = store.SessionProviderSet{}
		cur, err := ownSession(ctx, tx, u, m.GetSessionId(), now)
		if err != nil {
			return err
		}
		oldProvider = cur.ProviderID
		if m.Title != nil {
			if err := tx.SetSessionTitle(ctx, m.GetSessionId(), title); err != nil {
				return err
			}
		}
		// The current provider again is no change, in any state.
		if m.ProviderId != nil && m.GetProviderId() != cur.ProviderID {
			if pid := m.GetProviderId(); pid != "" {
				_, err := tx.GetProvider(ctx, u.ID, pid, now)
				if errors.Is(err, store.ErrNotFound) {
					return invalidField("provider_id", "unknown provider")
				}
				if err != nil {
					return err
				}
			}
			set, err = tx.SetSessionProvider(ctx, m.GetSessionId(), m.GetProviderId(), now)
			switch {
			case errors.Is(err, store.ErrTurnRunning):
				return connect.NewError(connect.CodeFailedPrecondition, errors.New("a turn is running; interrupt it or wait until it ends"))
			case errors.Is(err, store.ErrUnknownProvider):
				return invalidField("provider_id", "unknown provider")
			case err != nil:
				return err
			}
		}
		sess, err := tx.GetSession(ctx, m.GetSessionId(), now)
		if err != nil {
			return err
		}
		out, err = s.fullSession(ctx, tx, sess)
		return err
	})
	if err != nil {
		return nil, s.sessionError(ctx, "update session", err)
	}
	if set.Changed {
		if l := set.Loss; l != nil && l.Interrupted && l.Session.LastEventSeq > 0 {
			s.bus.Durable(m.GetSessionId(), uint64(l.Session.LastEventSeq))
		}
		// The scheduler finds the lease released: it revokes it on the
		// holder's connection and schedules the session under its new
		// provider.
		s.waker.Wake(m.GetSessionId())
		s.logger.LogAttrs(ctx, slog.LevelInfo, "session provider changed", slog.String("session_id", m.GetSessionId()),
			slog.String("old_provider_id", oldProvider), slog.String("provider_id", set.Session.ProviderID))
		if l := set.Loss; l != nil {
			sessions.LogLeaseEnded(ctx, s.logger, *l, sessions.LeaseEndProviderChanged)
		}
	}
	return connect.NewResponse(out), nil
}

func protoApproval(a store.Approval, device string) *circusv1.SessionApproval {
	kind := circusv1.ApprovalKind_APPROVAL_KIND_TOOL
	if a.Kind == store.ApprovalQuestion {
		kind = circusv1.ApprovalKind_APPROVAL_KIND_QUESTION
	}
	return &circusv1.SessionApproval{
		ApprovalId: a.ApprovalID, SessionId: a.SessionID, ToolUseId: a.ToolUseID, AgentId: a.AgentID, Tool: a.Tool,
		Device: device, MatchKey: a.MatchKey, Display: a.Display, Reason: a.Reason,
		CreatedAtUnixMs: a.CreatedAt, ExpiresAtUnixMs: a.ExpiresAt, Kind: kind, InputJson: a.InputJSON,
	}
}

func (s *Service) ListApprovals(ctx context.Context, req *connect.Request[circusv1.ListApprovalsRequest]) (*connect.Response[circusv1.ListApprovalsResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	if err := sessionIDArg(req.Msg.GetSessionId()); err != nil {
		return nil, err
	}
	out := &circusv1.ListApprovalsResponse{Approvals: []*circusv1.SessionApproval{}}
	err = s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		now := s.now().UnixMilli()
		if _, err := ownSession(ctx, tx, u, req.Msg.GetSessionId(), now); err != nil {
			return err
		}
		// A session has at most MaxPendingApprovals pending approvals,
		// expired ones the sweep has not reached included, so one page
		// holds them all.
		list, err := tx.ListApprovals(ctx, store.ApprovalFilter{
			SessionID: req.Msg.GetSessionId(), States: []store.ApprovalState{store.ApprovalPending},
			Limit: store.MaxPendingApprovals, MaxBytes: store.MaxTranscriptPageBytes,
		})
		if err != nil {
			return err
		}
		names := newDeviceNames(tx)
		for _, a := range list {
			if a.Expired(now) {
				continue
			}
			device, err := names.name(ctx, a.DeviceID)
			if err != nil {
				return err
			}
			out.Approvals = append(out.Approvals, protoApproval(a, device))
		}
		return nil
	})
	if err != nil {
		return nil, s.sessionError(ctx, "list approvals", err)
	}
	return connect.NewResponse(out), nil
}

// approvalText is the approval input: it tells the model to retry the
// approved call.
func approvalText(a store.Approval, device string) string {
	return fmt.Sprintf("The user approved this %s call on device %q once: %s\nRetry it now.", a.Tool, device, a.Display)
}

func (s *Service) ResolveApproval(ctx context.Context, req *connect.Request[circusv1.ResolveApprovalRequest]) (*connect.Response[circusv1.ResolveApprovalResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetApprovalId()
	switch {
	case len(id) > store.MaxIDBytes:
		return nil, invalidField("approval_id", "approval id is too long")
	case id == "":
		return nil, connect.NewError(connect.CodeNotFound, errApprovalNotFound)
	case len(req.Msg.GetAnswers()) > questions.MaxQuestions:
		return nil, invalidField("answers", fmt.Sprintf("at most %d answers", questions.MaxQuestions))
	}
	for i, a := range req.Msg.GetAnswers() {
		switch {
		case len(a.GetQuestion()) > questions.MaxQuestionBytes:
			return nil, invalidField(fmt.Sprintf("answers[%d].question", i), fmt.Sprintf("the question is longer than %d bytes", questions.MaxQuestionBytes))
		case len(a.GetSelected()) > questions.MaxOptions:
			return nil, invalidField(fmt.Sprintf("answers[%d].selected", i), fmt.Sprintf("at most %d selected options", questions.MaxOptions))
		case len(a.GetOther()) > questions.MaxOtherBytes:
			return nil, invalidField(fmt.Sprintf("answers[%d].other", i), fmt.Sprintf("the answer is longer than %d bytes", questions.MaxOtherBytes))
		}
		for j, label := range a.GetSelected() {
			if len(label) > questions.MaxLabelBytes {
				return nil, invalidField(fmt.Sprintf("answers[%d].selected[%d]", i, j), fmt.Sprintf("a label is at most %d bytes", questions.MaxLabelBytes))
			}
		}
	}
	inputID, err := newID()
	if err != nil {
		return nil, s.internal(ctx, "input id", err)
	}
	now := s.now()
	at := now.UnixMilli()
	var res store.ApprovalResolved
	var seq int64
	err = s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		a, err := tx.GetApproval(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return connect.NewError(connect.CodeNotFound, errApprovalNotFound)
		}
		if err != nil {
			return err
		}
		if _, err := ownSession(ctx, tx, u, a.SessionID, at); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return connect.NewError(connect.CodeNotFound, errApprovalNotFound)
			}
			return err
		}
		r := store.ApprovalResolution{ApprovalID: id, Approve: req.Msg.GetApprove(), At: at}
		var event sessions.ApprovalResolvedPayload
		if a.Kind == store.ApprovalQuestion {
			if r, event, err = resolveQuestion(a, req.Msg, r); err != nil {
				return err
			}
			r.Input = store.Input{InputID: inputID, Text: r.Input.Text, CreatedAt: at}
		} else {
			if len(req.Msg.GetAnswers()) > 0 {
				return invalidField("answers", "only a question approval takes answers")
			}
			device, err := newDeviceNames(tx).name(ctx, a.DeviceID)
			if err != nil {
				return err
			}
			r.GrantExpiresAt = now.Add(approvalGrantLifetime).UnixMilli()
			r.Input = store.Input{InputID: inputID, Text: approvalText(a, device), CreatedAt: at}
		}
		res, err = tx.ResolveApproval(ctx, r)
		if err != nil {
			return err
		}
		if a.State != store.ApprovalPending {
			// Already resolved: idempotent, nothing recorded; but answers
			// to a question that was not answered reach nobody.
			if a.Kind == store.ApprovalQuestion && req.Msg.GetApprove() && a.State != store.ApprovalAnswered && a.State != store.ApprovalExpired {
				return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the question was %s: its answers would reach nobody", a.State))
			}
			return nil
		}
		event.ApprovalID, event.State = id, string(res.Approval.State)
		if res.Approval.State != store.ApprovalAnswered {
			event.Answers = nil
		} else if res.Input != nil {
			event.InputID = res.Input.InputID
		}
		seq, err = sessions.AppendApprovalResolvedEvent(ctx, tx, a.SessionID, event, at, store.DefaultMaxViewBytes)
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrLimit) && ctx.Err() == nil {
			return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("at most %d messages may wait for delivery", store.MaxOpenInputs))
		}
		return nil, s.sessionError(ctx, "resolve approval", err)
	}
	s.bus.Durable(res.Approval.SessionID, uint64(seq))
	if res.Changed {
		switch res.Approval.State {
		case store.ApprovalApproved:
			s.waker.Wake(res.Approval.SessionID, circusv1.WorkerEvent_Nudge_REASON_APPROVAL)
		case store.ApprovalAnswered:
			s.waker.Wake(res.Approval.SessionID, circusv1.WorkerEvent_Nudge_REASON_INPUT)
		}
		s.logger.LogAttrs(ctx, slog.LevelInfo, "approval resolved", slog.String("session_id", res.Approval.SessionID),
			slog.String("approval_id", id), slog.String("decision", string(res.Approval.State)))
	}
	// An expired approval was not resolved by the user, so the idempotent
	// answer of a resolved one does not apply: the call had no effect. The
	// expiry it may have recorded is committed.
	if res.Approval.State == store.ApprovalExpired {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the approval has expired"))
	}
	return connect.NewResponse(&circusv1.ResolveApprovalResponse{}), nil
}

// resolveQuestion checks a resolution of the question approval a: answers
// are checked against its questions while it is pending and approve is
// set (they must be empty to dismiss), and the answers input text and the
// event payload's kind and answers are filled in. A question no longer
// pending only gets the shape checks (the resolution is a no-op).
func resolveQuestion(a store.Approval, m *circusv1.ResolveApprovalRequest, r store.ApprovalResolution) (store.ApprovalResolution, sessions.ApprovalResolvedPayload, error) {
	event := sessions.ApprovalResolvedPayload{Kind: string(store.ApprovalQuestion)}
	if !m.GetApprove() {
		if len(m.GetAnswers()) > 0 {
			return r, event, invalidField("answers", "dismissing a question takes no answers")
		}
		return r, event, nil
	}
	if a.State != store.ApprovalPending {
		return r, event, nil
	}
	qs, err := questions.Parse(a.InputJSON)
	if err != nil {
		// The control plane checked the input when the worker asked.
		return r, event, fmt.Errorf("stored question approval %s: %w", a.ApprovalID, err)
	}
	if err := questions.CheckAnswers(qs, m.GetAnswers()); err != nil {
		return r, event, invalidField("answers", err.Error())
	}
	r.Input.Text = questions.AnswerText(m.GetAnswers())
	for _, ans := range m.GetAnswers() {
		event.Answers = append(event.Answers, sessions.QuestionAnswerEvent{
			Question: ans.GetQuestion(), Selected: append([]string{}, ans.GetSelected()...), Other: ans.GetOther(),
		})
	}
	return r, event, nil
}

func (s *Service) Interrupt(ctx context.Context, req *connect.Request[circusv1.InterruptRequest]) (*connect.Response[circusv1.InterruptResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	if err := sessionIDArg(req.Msg.GetSessionId()); err != nil {
		return nil, err
	}
	var changed bool
	var level int64
	err = s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		now := s.now().UnixMilli()
		if _, err := ownSession(ctx, tx, u, req.Msg.GetSessionId(), now); err != nil {
			return err
		}
		var sess store.Session
		sess, _, changed, err = tx.InterruptSession(ctx, req.Msg.GetSessionId(), now)
		level = sess.InterruptTurnNo
		return err
	})
	if err != nil {
		return nil, s.sessionError(ctx, "interrupt", err)
	}
	if changed {
		s.waker.Wake(req.Msg.GetSessionId(), circusv1.WorkerEvent_Nudge_REASON_INTERRUPT)
	}
	s.logger.LogAttrs(ctx, slog.LevelInfo, "interrupt requested", slog.String("session_id", req.Msg.GetSessionId()),
		slog.Int64("level", level), slog.Bool("changed", changed))
	return connect.NewResponse(&circusv1.InterruptResponse{}), nil
}

func (s *Service) StopSession(ctx context.Context, req *connect.Request[circusv1.StopSessionRequest]) (*connect.Response[circusv1.StopSessionResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetSessionId()
	if err := sessionIDArg(id); err != nil {
		return nil, err
	}
	var changed bool
	var sess store.Session
	var seq int64
	err = s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		now := s.now().UnixMilli()
		if _, err := ownSession(ctx, tx, u, id, now); err != nil {
			return err
		}
		sess, _, changed, err = tx.StopSession(ctx, id, now, stopReason)
		if err != nil || !changed {
			return err
		}
		seq, err = sessions.AppendStopEvent(ctx, tx, id, sessions.StopReasonUser, now, store.DefaultMaxViewBytes)
		return err
	})
	if err != nil {
		return nil, s.sessionError(ctx, "stop session", err)
	}
	if changed {
		s.bus.Durable(id, uint64(seq))
		s.waker.Wake(id, circusv1.WorkerEvent_Nudge_REASON_STOP)
		if s.snapshots != nil {
			// A turn the stop ended leaves the snapshot due for a collection.
			s.snapshots.Kick(id)
		}
	}
	// Repeated on every StopSession, so a retry reaches jobs whose device
	// was offline the first time.
	jobsCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopJobsTimeout)
	defer cancel()
	jobsErr := s.jobs.StopSessionJobs(jobsCtx, sess.UserID, id)
	if changed {
		s.logger.LogAttrs(ctx, slog.LevelInfo, "session stopped", slog.String("session_id", id), slog.String("reason", sessions.StopReasonUser))
	}
	if jobsErr != nil {
		s.logger.WarnContext(ctx, "stopping session jobs failed", "session_id", id, "error", jobsErr)
	}
	return connect.NewResponse(&circusv1.StopSessionResponse{}), nil
}

func (s *Service) DeleteSession(ctx context.Context, req *connect.Request[circusv1.DeleteSessionRequest]) (*connect.Response[circusv1.DeleteSessionResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetSessionId()
	if err := sessionIDArg(id); err != nil {
		return nil, err
	}
	// DeleteSession runs its own chunked transactions, so the check is a
	// separate read; a session never changes project, so it stays valid.
	sess, err := ownSession(ctx, s.st, u, id, s.now().UnixMilli())
	if err != nil {
		return nil, s.sessionError(ctx, "delete session", err)
	}
	del := func(ctx context.Context) error { return s.st.DeleteSession(ctx, id, s.now().UnixMilli()) }
	if s.snapshots != nil {
		check := func(ctx context.Context) error { return s.st.CheckSessionDeletable(ctx, id, s.now().UnixMilli()) }
		err = s.snapshots.DeleteSession(ctx, sess, req.Msg.GetDiscardLedgerChanges(), check, del)
	} else {
		err = del(ctx)
	}
	var refusal *ledgersnaps.Refusal
	var repair *ledger.RepairError
	switch {
	case errors.As(err, &refusal):
		return nil, refusalError(refusal)
	case errors.As(err, &repair):
		return nil, repairRefusal(repair)
	case errors.Is(err, store.ErrSessionActive) && ctx.Err() == nil:
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the session is not stopped or is still busy"))
	case err != nil:
		return nil, s.sessionError(ctx, "delete session", err)
	}
	s.bus.Deleted(id)
	s.logger.LogAttrs(ctx, slog.LevelInfo, "session deleted", slog.String("session_id", id))
	return connect.NewResponse(&circusv1.DeleteSessionResponse{}), nil
}

func (s *Service) RecoverSession(ctx context.Context, req *connect.Request[circusv1.RecoverSessionRequest]) (*connect.Response[circusv1.RecoverSessionResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetSessionId()
	if err := sessionIDArg(id); err != nil {
		return nil, err
	}
	if !req.Msg.GetAcceptTranscriptGap() {
		return nil, invalidField("accept_transcript_gap", "accept the transcript gap to recover")
	}
	var res store.SessionRecovered
	err = s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		now := s.now().UnixMilli()
		if _, err := ownSession(ctx, tx, u, id, now); err != nil {
			return err
		}
		res, err = tx.RecoverSession(ctx, id, true, now, store.DefaultMaxViewBytes)
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotBroken) && ctx.Err() == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the session is not broken"))
		}
		return nil, s.sessionError(ctx, "recover session", err)
	}
	// The session's last seq covers the gap event and a boundary the lease
	// release may have recorded.
	s.bus.Durable(id, uint64(max(res.Session.LastEventSeq, res.EventSeq)))
	s.waker.Wake(id, circusv1.WorkerEvent_Nudge_REASON_INPUT)
	s.logger.LogAttrs(ctx, slog.LevelInfo, "session recovered", slog.String("session_id", id))
	if res.Loss != nil {
		sessions.LogLeaseEnded(ctx, s.logger, *res.Loss, sessions.LeaseEndRecovered)
	}
	return connect.NewResponse(&circusv1.RecoverSessionResponse{}), nil
}
