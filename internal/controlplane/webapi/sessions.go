package webapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
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

// listSession is the Session of ListSessions: without config.
func listSession(sess store.Session) *circusv1.Session {
	return &circusv1.Session{
		SessionId: sess.ID, State: protoState(sess.State), LastTurnOutcome: protoOutcome(sess.LastTurnOutcome),
		TurnCount: uint64(max(sess.TurnCount, 0)), CreatedAtUnixMs: sess.CreatedAt, StoppedAtUnixMs: sess.StoppedAt,
		ViewTruncated: sess.ViewTruncated, PinnedWorkerId: sess.PinnedWorkerID, ProjectId: sess.ProjectID,
		Title: sess.Title, LastActivityAtUnixMs: sess.LastActivityAt, PendingApprovalCount: sess.PendingApprovalCount,
	}
}

// fullSession is the Session of GetSession and the writes: with config,
// whose working directories are the main thread's current ones.
func fullSession(ctx context.Context, ops store.Ops, sess store.Session) (*circusv1.Session, error) {
	out := listSession(sess)
	names := newDeviceNames(ops)
	device, err := names.name(ctx, sess.DefaultDeviceID)
	if err != nil {
		return nil, err
	}
	cwds, err := ops.ListSessionCwds(ctx, sess.ID)
	if err != nil {
		return nil, err
	}
	out.Config = &circusv1.SessionConfig{DefaultDevice: device, Model: sess.Model, PermissionMode: sess.PermissionMode}
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
	approvalEventPayload struct {
		ApprovalID string `json:"approval_id"`
		State      string `json:"state"`
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
	case len(m.GetPinnedWorkerId()) > store.MaxIDBytes:
		return nil, invalidField("pinned_worker_id", "worker id is too long")
	}
	now := s.now().UnixMilli()
	var out *circusv1.Session
	const requestConflict = "this client request id was used for a different session"
	attempt := func(ctx context.Context, tx store.Ops) error {
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
			if !old.MatchesCreate(m.LocationIndex, m.GetPinnedWorkerId(), cfg.GetModel(), cfg.GetPermissionMode()) {
				return fieldError(connect.CodeAlreadyExists, "client_request_id", requestConflict)
			}
			out, err = fullSession(ctx, tx, old)
			return err
		case !errors.Is(err, store.ErrNotFound):
			return err
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
		if w := m.GetPinnedWorkerId(); w != "" {
			worker, err := tx.GetIdentity(ctx, store.KindWorker, w)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if err != nil || worker.DeletedAt != 0 || worker.UserID != u.ID {
				return invalidField("pinned_worker_id", "unknown worker")
			}
		}
		// A location without a path starts in the project's default
		// directory on the device. This step does not create or clone it;
		// the worker's tools resolve "~".
		cwd := l.Path
		if cwd == "" {
			cwd = defaultDir(p.ID)
		}
		id, err := newID()
		if err != nil {
			return err
		}
		sess := store.Session{
			ID: id, UserID: u.ID, ProjectID: p.ID, ClaudeSessionID: uuid.NewString(), State: store.SessionIdle,
			ClientRequestID: m.GetClientRequestId(), LocationIndex: uint32(index),
			DefaultDeviceID: l.DeviceID, Model: cfg.GetModel(), PermissionMode: cfg.GetPermissionMode(),
			PinnedWorkerID: m.GetPinnedWorkerId(), CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
		}
		cwds := []store.SessionCwd{{DeviceID: l.DeviceID, Cwd: cwd, UpdatedAt: now}}
		res, err := tx.CreateSession(ctx, sess, cwds, store.DefaultMaxSessionsPerUser)
		if err != nil {
			return err
		}
		if res.Created {
			if _, err := tx.UpdateProject(ctx, u.ID, p.ID, store.ProjectPatch{LastUsedLocation: &index, UpdatedAt: now}); err != nil {
				return err
			}
		}
		out, err = fullSession(ctx, tx, res.Session)
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
	return connect.NewResponse(&circusv1.CreateSessionResponse{Session: out}), nil
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
	for len(out.Sessions) < size {
		filter.Limit = min(size-len(out.Sessions), store.MaxPageSize)
		rows, err := s.st.ListSessions(ctx, filter)
		if err != nil {
			return nil, s.internal(ctx, "list sessions", err)
		}
		for _, sess := range rows {
			out.Sessions = append(out.Sessions, listSession(sess))
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
		out, err = fullSession(ctx, tx, sess)
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
	if err := sessionIDArg(req.Msg.GetSessionId()); err != nil {
		return nil, err
	}
	title, problem := checkTitle(req.Msg.GetTitle())
	if problem != "" {
		return nil, invalidField("title", problem)
	}
	var out *circusv1.Session
	err = s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		now := s.now().UnixMilli()
		if _, err := ownSession(ctx, tx, u, req.Msg.GetSessionId(), now); err != nil {
			return err
		}
		if err := tx.SetSessionTitle(ctx, req.Msg.GetSessionId(), title); err != nil {
			return err
		}
		sess, err := tx.GetSession(ctx, req.Msg.GetSessionId(), now)
		if err != nil {
			return err
		}
		out, err = fullSession(ctx, tx, sess)
		return err
	})
	if err != nil {
		return nil, s.sessionError(ctx, "update session", err)
	}
	return connect.NewResponse(out), nil
}

func protoApproval(a store.Approval, device string) *circusv1.SessionApproval {
	return &circusv1.SessionApproval{
		ApprovalId: a.ApprovalID, SessionId: a.SessionID, ToolUseId: a.ToolUseID, AgentId: a.AgentID, Tool: a.Tool,
		Device: device, MatchKey: a.MatchKey, Display: a.Display, Reason: a.Reason,
		CreatedAtUnixMs: a.CreatedAt, ExpiresAtUnixMs: a.ExpiresAt,
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
			if a.ExpiresAt <= now {
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
		device, err := newDeviceNames(tx).name(ctx, a.DeviceID)
		if err != nil {
			return err
		}
		res, err = tx.ResolveApproval(ctx, store.ApprovalResolution{
			ApprovalID: id, Approve: req.Msg.GetApprove(), At: at,
			GrantExpiresAt: now.Add(approvalGrantLifetime).UnixMilli(),
			Input:          store.Input{InputID: inputID, Text: approvalText(a, device), CreatedAt: at},
		})
		if err != nil {
			return err
		}
		if a.State != store.ApprovalPending {
			return nil // already resolved: idempotent, nothing recorded
		}
		seq, err = appendEvent(ctx, tx, a.SessionID, circusv1.SessionEvent_KIND_APPROVAL_RESOLVED,
			approvalEventPayload{ApprovalID: id, State: string(res.Approval.State)}, at, true)
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrLimit) && ctx.Err() == nil {
			return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("at most %d messages may wait for delivery", store.MaxOpenInputs))
		}
		return nil, s.sessionError(ctx, "resolve approval", err)
	}
	s.bus.Durable(res.Approval.SessionID, uint64(seq))
	if res.Changed && res.Approval.State == store.ApprovalApproved {
		s.waker.Wake(res.Approval.SessionID, circusv1.WorkerEvent_Nudge_REASON_APPROVAL)
	}
	// An expired approval was not resolved by the user, so the idempotent
	// answer of a resolved one does not apply: the call had no effect. The
	// expiry it may have recorded is committed.
	if res.Approval.State == store.ApprovalExpired {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the approval has expired"))
	}
	return connect.NewResponse(&circusv1.ResolveApprovalResponse{}), nil
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
	err = s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		now := s.now().UnixMilli()
		if _, err := ownSession(ctx, tx, u, req.Msg.GetSessionId(), now); err != nil {
			return err
		}
		_, _, changed, err = tx.InterruptSession(ctx, req.Msg.GetSessionId(), now)
		return err
	})
	if err != nil {
		return nil, s.sessionError(ctx, "interrupt", err)
	}
	if changed {
		s.waker.Wake(req.Msg.GetSessionId(), circusv1.WorkerEvent_Nudge_REASON_INTERRUPT)
	}
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
	}
	// Repeated on every StopSession, so a retry reaches jobs whose device
	// was offline the first time.
	jobsCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopJobsTimeout)
	defer cancel()
	if err := s.jobs.StopSessionJobs(jobsCtx, sess.UserID, id); err != nil {
		s.logger.WarnContext(ctx, "stopping session jobs failed", "session_id", id, "error", err)
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
	if _, err := ownSession(ctx, s.st, u, id, s.now().UnixMilli()); err != nil {
		return nil, s.sessionError(ctx, "delete session", err)
	}
	err = s.st.DeleteSession(ctx, id, s.now().UnixMilli())
	if errors.Is(err, store.ErrSessionActive) && ctx.Err() == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the session is not stopped or is still busy"))
	}
	if err != nil {
		return nil, s.sessionError(ctx, "delete session", err)
	}
	s.bus.Deleted(id)
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
	return connect.NewResponse(&circusv1.RecoverSessionResponse{}), nil
}
