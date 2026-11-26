package fakeworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

type input = pb.SessionReply_Inputs_Input

type hints struct {
	inputs, jobs, stop bool
	// release: a ReleaseIdle for the held epoch arrived.
	release   bool
	interrupt uint64
}

// A session goroutine owns the turn and transcript state. The worker mutex
// protects the fields used by Attach and ResumeLeases.
type session struct {
	w      *worker
	id     string
	ctx    context.Context
	cancel context.CancelCauseFunc
	wake   chan struct{}

	epoch   uint64
	pending hints
	offered bool
	// releaseAsked: the hints just handled include a ReleaseIdle; serve
	// releases the lease if the session is idle on that pass.
	releaseAsked bool

	released                        bool
	sdkID, model, device, cwd, root string
	parent                          string
	localSeq, appendSeq, batchSeq   uint64
	interrupt, gap                  uint64
	broken                          bool

	// ledgerPath is the lease's ledger snapshot on the default device,
	// empty when the session has none.
	ledgerPath string

	totalUsage    usageTotals
	usageBatchSeq uint64
	// results counts the results published in this lease, for result_index.
	results int

	longApprovals map[string]string
	seen          map[string]bool
	// silent maps each note pushed with shouldQuery false (as the real
	// worker pushes a note followed by another input of the same
	// TakeInputs reply) to the input after it: the CLI records the note
	// without a model call, and that input runs the turn that reads it.
	silent map[string]string
	inputs []*input
	jobs   []*pb.SessionState_Job
	events []*pb.PublishEvents_Event

	recovery *pb.Recovery
	owed     map[string]*pb.SessionState_Call
	notices  []*pb.CallUpdate
}

type turn struct {
	no      uint64
	started time.Time
	marks   pb.ReportTurn_Persisted
	text    string
	tasks   []string

	abortText, abortReason                                         string
	inputTokens, usedInput, outputTokens, thinkingTokens, requests int
	denials                                                        []object

	// userMessageID is the input that started the turn, messageIDs the
	// API messages it began and firstInput its first request's input tokens.
	userMessageID string
	messageIDs    []string
	firstInput    int
	// tail ends the stream of a response the user aborted.
	tail []*pb.PublishEvents_Event
}

func (s *session) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *session) run() error {
	defer s.shutdownLease()
	r, err := s.request(wc.Acquire(s.id, true))
	if rejected(err, pb.SessionReply_Rejected_CODE_NOT_OFFERED) || rejected(err, pb.SessionReply_Rejected_CODE_PROVIDER_FULL) ||
		rejected(err, pb.SessionReply_Rejected_CODE_PROVIDER_UNAVAILABLE) || errors.Is(err, errStopped) {
		return nil
	}
	if err != nil {
		return err
	}
	lease := r.GetLease()
	if lease.GetEpoch() == 0 {
		return fmt.Errorf("acquire %s: %v", s.id, r)
	}
	if s.w.opts.Acquired != nil {
		s.w.opts.Acquired(s.id, lease.GetEpoch())
	}
	err = s.launch()
	if err == nil {
		err = s.serve(lease)
	}
	if errors.Is(err, errProvider) {
		// No CLI starts without its provider: the session queues again.
		return s.release(s.ctx, pb.ReleaseLease_REASON_PROVIDER)
	}
	if s.ctx.Err() != nil || errors.Is(err, errFenced) || errors.Is(err, errCrash) {
		return err
	}

	reason := pb.ReleaseLease_REASON_IDLE
	switch {
	case errors.Is(err, errStopped):
		reason, err = pb.ReleaseLease_REASON_STOPPED, nil
	case err != nil || s.broken:
		reason = pb.ReleaseLease_REASON_ERROR
	}
	return errors.Join(err, s.release(s.ctx, reason))
}

// launch stands in for starting the CLI: it asks for the launch
// environment, hands the reply to Options.LaunchEnv and drops it. A
// provider that is gone is errProvider.
func (s *session) launch() error {
	r, err := s.request(wc.LaunchEnv(s.id, s.epoch))
	if r != nil && s.w.opts.LaunchEnv != nil {
		s.w.opts.LaunchEnv(s.id, r)
	}
	if rejected(err, pb.SessionReply_Rejected_CODE_PROVIDER_UNAVAILABLE) {
		return errProvider
	}
	if err == nil && r.GetLaunchEnv() == nil {
		return fmt.Errorf("launch env %s: unexpected reply", s.id)
	}
	return err
}

func (s *session) release(ctx context.Context, reason pb.ReleaseLease_Reason) error {
	// The turn has stopped and any tool watch has been joined before release.
	flushErr := s.flushWith(ctx)
	_, err := s.requestWith(ctx, wc.Release(s.id, s.epoch, reason))
	if err == nil || errors.Is(err, errFenced) {
		s.released = true
		err = nil
	}
	return errors.Join(flushErr, err)
}

func (s *session) shutdownLease() {
	if s.released || context.Cause(s.ctx) != errShutdown {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
	defer cancel()
	if s.epoch == 0 {
		// An Acquire interrupted by shutdown may still have succeeded.
		r, err := s.requestWith(ctx, wc.Acquire(s.id, true))
		if err != nil || r.GetLease().GetEpoch() == 0 {
			return
		}
	}
	if err := s.release(ctx, pb.ReleaseLease_REASON_IDLE); err != nil {
		s.w.log.Warn("shutdown lease cleanup failed", "session_id", s.id, "error", err)
	}
}

func (s *session) serve(lease *pb.SessionReply_Lease) error {
	s.sdkID = lease.GetClaudeSessionId()
	s.model = lease.GetConfig().GetModel()
	if s.model == "" {
		s.model = "claude-sonnet-4-6"
	}
	s.device = lease.GetConfig().GetDefaultDevice()
	s.ledgerPath = lease.GetLedgerPath()
	for _, dir := range lease.GetConfig().GetCwds() {
		if dir.GetDevice() == s.device {
			s.cwd = dir.GetCwd()
			s.root = s.cwd
		}
	}
	s.interrupt = lease.GetState().GetInterruptTurnNo()
	s.gap = lease.GetTranscriptGapTurnNo()
	s.recovery = lease.GetRecovery()
	if lease.GetState().GetStopped() {
		return errStopped
	}
	if err := s.takeInputs(); err != nil {
		return err
	}
	s.setJobs(lease.GetState().GetJobs())
	if lease.GetHasTranscript() {
		if err := s.loadTranscript(); err != nil {
			return err
		}
	}

	idleUntil := s.w.opts.Clock.Now().Add(s.w.opts.IdleRelease)
	for {
		if err := s.serviceHints(nil); err != nil {
			return err
		}
		if len(s.owed) != 0 && !s.broken {
			if err := s.pollOwed(); err != nil {
				return err
			}
		}
		if !s.broken && (len(s.inputs) != 0 || len(s.jobs) != 0 || len(s.notices) != 0) {
			if err := s.runTurn(); err != nil {
				return err
			}
			idleUntil = s.w.opts.Clock.Now().Add(s.w.opts.IdleRelease)
			continue
		}
		// A request taken here with work to run, or during a turn (by the
		// turn's serviceHints), is overwritten by the next pass: dropped,
		// as the real worker drops one for a busy session. Owed call
		// notices and a broken transcript keep the session too.
		if s.releaseAsked {
			s.releaseAsked = false
			if len(s.owed) == 0 && !s.broken {
				s.w.log.Info("session idle; releasing", "session_id", s.id, "requested", true)
				return nil
			}
		}
		delay := idleUntil.Sub(s.w.opts.Clock.Now())
		if delay <= 0 {
			return nil
		}
		if len(s.owed) != 0 && !s.broken {
			delay = min(delay, time.Second)
		}
		select {
		case <-s.wake:
		case <-s.w.opts.Clock.After(delay):
		case <-s.ctx.Done():
			return context.Cause(s.ctx)
		}
	}
}

func (s *session) takeInputs() error {
	r, err := s.request(wc.TakeInputs(s.id, s.epoch))
	if err != nil {
		return err
	}
	var batch []*input
	for _, in := range r.GetInputs().GetInputs() {
		if !s.seen[in.GetInputId()] {
			s.seen[in.GetInputId()] = true
			batch = append(batch, in)
		}
	}
	for i, in := range batch {
		if in.GetKind() != pb.SessionReply_Inputs_Input_KIND_NOTE || i == len(batch)-1 {
			continue
		}
		s.silent[in.GetInputId()] = batch[i+1].GetInputId()
		// A note keeps its place before a NOW input: it takes its priority.
		for _, later := range batch[i+1:] {
			if later.GetPriority() == pb.InputPriority_INPUT_PRIORITY_NOW {
				in = proto.CloneOf(in)
				in.Priority = pb.InputPriority_INPUT_PRIORITY_NOW
				batch[i] = in
				break
			}
		}
	}
	s.inputs = append(s.inputs, batch...)
	return nil
}

func (s *session) serviceHints(t *turn) error {
	if s.ctx.Err() != nil {
		return context.Cause(s.ctx)
	}
	s.w.mu.Lock()
	h := s.pending
	s.pending = hints{}
	s.w.mu.Unlock()
	s.interrupt = max(s.interrupt, h.interrupt)
	if h.stop {
		return errStopped
	}
	// Read by serve's idle pass only (see there).
	s.releaseAsked = h.release
	if h.inputs {
		if err := s.takeInputs(); err != nil {
			return err
		}
	}
	if h.jobs {
		r, err := s.request(wc.State(s.id, s.epoch, pb.GetSessionState_SECTION_JOBS))
		if err != nil {
			return err
		}
		state := r.GetState().GetState()
		if state.GetStopped() {
			return errStopped
		}
		s.interrupt = max(s.interrupt, state.GetInterruptTurnNo())
		s.setJobs(state.GetJobs())
	}
	if t != nil {
		if t.no <= s.interrupt {
			return errAbort
		}
		if s.nowInput() >= 0 {
			return errNow
		}
	}
	return nil
}

func (s *session) nowInput() int {
	for i, in := range s.inputs {
		if in.GetPriority() == pb.InputPriority_INPUT_PRIORITY_NOW {
			return i
		}
	}
	return -1
}

func (s *session) popInput(i int) *input {
	in := s.inputs[i]
	copy(s.inputs[i:], s.inputs[i+1:])
	s.inputs[len(s.inputs)-1] = nil
	s.inputs = s.inputs[:len(s.inputs)-1]
	return in
}

func (s *session) setJobs(jobs []*pb.SessionState_Job) {
	s.jobs = s.jobs[:0]
	for _, job := range jobs {
		switch job.GetState() {
		case pb.JobEvent_STATE_EXITED, pb.JobEvent_STATE_KILLED, pb.JobEvent_STATE_LOST:
			if !job.GetHanded() {
				s.jobs = append(s.jobs, job)
			}
		}
	}
}

func (s *session) runTurn() error {
	origin := pb.TurnOrigin_TURN_ORIGIN_NOTIFICATION
	originText := "notification"
	var in *input
	// Every input TakeInputs returns starts an input turn, those the control
	// plane writes itself (approval retries, notes) included: like the
	// user's, it is pushed as a user message and acknowledged in
	// Persisted.input_ids.
	if len(s.inputs) != 0 {
		origin = pb.TurnOrigin_TURN_ORIGIN_INPUT
		originText = "input"
		in = s.popInput(max(s.nowInput(), 0))
	}
	s.localSeq++
	r, err := s.request(wc.Started(s.id, s.epoch, s.localSeq, origin))
	if err != nil {
		return err
	}
	t := &turn{no: r.GetTurn().GetTurnNo(), started: s.w.opts.Clock.Now()}
	if t.no == 0 {
		return fmt.Errorf("STARTED: %v", r)
	}
	// TURN_STARTED is a worker-published boundary on the current wire.
	s.queue(pb.SessionEvent_KIND_TURN_STARTED, turnStartedPayload(t.no, originText))
	if err := s.workTurn(t, in); err != nil {
		return s.finishTurn(t, err)
	}
	return s.finishTurn(t, nil)
}

func (s *session) workTurn(t *turn, in *input) error {
	if s.gap != 0 {
		if err := s.record(sdkUser(s.sdkID, uuid.NewString(), historyMissingText, noteHistoryMissing)); err != nil {
			return err
		}
		t.marks.GapNoticeTurnNo = s.gap
	}
	if err := s.recoverCalls(t); err != nil {
		return err
	}
	if err := s.deliverNotices(t); err != nil {
		return err
	}
	// Normal inputs, including approvals, join the active turn before its
	// result. A NOW input stays queued for the next turn.
	for {
		if in == nil {
			if err := s.serviceHints(nil); err != nil {
				return err
			}
			if len(s.inputs) == 0 || s.nowInput() >= 0 {
				break
			}
			in = s.popInput(0)
		}
		if err := s.deliverInput(t, in); err != nil {
			return err
		}
		// A silent note is read with the input after it, in this turn,
		// whatever that input's priority: the CLI records the note without
		// a model call and runs the turn for that input (a NOW one included,
		// which the note's own promotion to NOW kept behind it).
		if next, ok := s.silent[in.GetInputId()]; ok {
			if i := slices.IndexFunc(s.inputs, func(in *input) bool { return in.GetInputId() == next }); i >= 0 {
				in = s.popInput(i)
				continue
			}
		}
		if err := s.serviceHints(t); err != nil {
			return err
		}
		if _, silent := s.silent[in.GetInputId()]; !silent {
			if err := s.script(t, in); err != nil {
				return err
			}
		}
		in = nil
	}
	if t.text == "" && len(t.marks.Jobs) != 0 {
		t.text = "Background job completion received."
	}
	return nil
}

// inputNote is the circus_note of an input's user message and whether
// the message is synthetic: none for the user's own input; a
// control-plane note keeps its category as given, an empty one included
// (synthetic, without circus_note), as the real worker does; a ledger
// note and the user's answers to a question are not synthetic.
func inputNote(in *input) (string, bool) {
	switch in.GetKind() {
	case pb.SessionReply_Inputs_Input_KIND_APPROVAL:
		return noteApprovalRetry, true
	case pb.SessionReply_Inputs_Input_KIND_NOTE:
		return in.GetNote(), !ledgerNotes[in.GetNote()]
	case pb.SessionReply_Inputs_Input_KIND_ANSWER:
		return noteQuestionAnswer, false
	}
	return "", false
}

func (s *session) deliverInput(t *turn, in *input) error {
	note, synthetic := inputNote(in)
	message := sdkUserMessage(s.sdkID, in.GetInputId(), in.GetText(), note, synthetic)
	var only object
	if _, silent := s.silent[in.GetInputId()]; silent {
		// As the CLI records a message pushed with shouldQuery false.
		only = object{"queueTranscriptOnly": true}
	}
	if err := s.recordWith(message, only); err != nil {
		return err
	}
	if t.userMessageID == "" {
		t.userMessageID = in.GetInputId()
	}
	t.inputTokens += tokens(in.GetText())
	t.marks.InputIds = append(t.marks.InputIds, in.GetInputId())
	return s.flush()
}

var errMirror = errors.New("scripted transcript mirror failure")
var errScript = errors.New("scripted worker error")

func (s *session) finishTurn(t *turn, workErr error) error {
	outcome := pb.TurnOutcome_TURN_OUTCOME_COMPLETED
	var failure string
	persisted := &t.marks
	incomplete := errors.Is(workErr, errMirror)
	switch {
	case workErr == nil:
	case errors.Is(workErr, errStopped), errors.Is(workErr, errFenced), errors.Is(workErr, errCrash), s.ctx.Err() != nil:
		return workErr
	case errors.Is(workErr, errBroken):
		s.broken = true
		return nil
	case isAbort(workErr):
		outcome = pb.TurnOutcome_TURN_OUTCOME_STOPPED
		if errors.Is(workErr, errAbort) {
			failure = abortFailure(t.abortReason)
			if !incomplete {
				text := "[Request interrupted by user]"
				if t.abortReason == abortedTools {
					text = "[Request interrupted by user for tool use]"
				}
				annotation := []object{{"type": "text", "text": text}}
				if err := s.record(sdkUser(s.sdkID, uuid.NewString(), annotation, noteUserInterrupt)); err != nil {
					if !errors.Is(err, errMirror) {
						return err
					}
					incomplete = true
				}
			}
		}
		s.events = append(s.events, t.tail...)
	default:
		outcome = pb.TurnOutcome_TURN_OUTCOME_ERROR
		failure = workErr.Error()
		s.queue(pb.SessionEvent_KIND_ERROR, errorPayload(failure))
	}
	if incomplete {
		persisted = nil
	}
	duration := s.w.opts.Clock.Now().Sub(t.started).Milliseconds()
	result := sdkResult(s.sdkID, uuid.NewString(), t.text, failure, duration)
	if isAbort(workErr) {
		applyAbortResult(result, t.abortReason, t.abortText)
	}
	s.describeResult(result, t)
	total := s.totalUsage
	usage := resultUsage(result, s.model, t, &total, duration)
	if err := s.saveUsage(total); err != nil {
		return err
	}
	s.events = append(s.events, result.event())
	if err := s.flush(); err != nil {
		return err
	}
	if persisted != nil {
		var err error
		persisted, err = s.splitMarks(persisted)
		if err != nil {
			return err
		}
	}
	req := wc.Ended(s.id, s.epoch, s.localSeq, outcome, persisted)
	report := req.GetReportTurn()
	report.TranscriptIncomplete = incomplete
	report.UsageJson = jsonString(usage)
	r, err := s.request(req)
	if err != nil {
		return err
	}
	if r.GetTurn().GetTurnNo() != t.no {
		return fmt.Errorf("ENDED: %v", r)
	}
	if persisted != nil {
		for _, id := range persisted.GetInputIds() {
			delete(s.seen, id)
			delete(s.silent, id)
		}
		if persisted.GetGapNoticeTurnNo() == s.gap {
			s.gap = 0
		}
		for _, id := range persisted.GetLateCallIds() {
			delete(s.owed, id)
		}
		if len(t.tasks) != 0 {
			if _, err := s.request(wc.Patch(s.id, s.epoch, &pb.PatchSessionState{BgtasksEnded: t.tasks})); err != nil {
				return err
			}
		}
	}
	if incomplete {
		s.broken = true
		return nil
	}
	if outcome == pb.TurnOutcome_TURN_OUTCOME_ERROR {
		return workErr
	}
	return nil
}

func (s *session) queue(kind pb.SessionEvent_Kind, payload object) {
	s.events = append(s.events, &pb.PublishEvents_Event{Kind: kind, Json: jsonString(payload)})
}

func (s *session) record(message sdkMessage) error {
	return s.recordWith(message, nil)
}

// recordWith is record with fields for the transcript entry only.
func (s *session) recordWith(message sdkMessage, only object) error {
	message = s.stamp(message)
	entry := message.transcriptWith(s.parent, only)
	r, err := s.request(wc.Append(s.id, s.epoch, "", s.appendSeq+1, entry))
	if rejected(err, pb.SessionReply_Rejected_CODE_INVALID) || rejected(err, pb.SessionReply_Rejected_CODE_LIMIT) || errors.Is(err, wire.ErrTooLarge) {
		return fmt.Errorf("%w: %v", errMirror, err)
	}
	if err != nil {
		return err
	}
	if r.GetOk() == nil {
		return fmt.Errorf("append: %v", r)
	}
	s.appendSeq++
	s.parent = entry.GetUuid()
	s.events = append(s.events, message.event())
	return nil
}

func (s *session) flush() error {
	return s.flushWith(s.ctx)
}

func (s *session) flushWith(ctx context.Context) error {
	for len(s.events) != 0 {
		count, size := 0, 0
		for _, e := range s.events {
			size += len(e.GetJson()) + 32
			if count == 128 || (count > 0 && size > 512<<10) {
				break
			}
			count++
		}
		r, err := s.requestWith(ctx, wc.Publish(s.id, s.epoch, s.batchSeq+1, s.events[:count]...))
		if rejected(err, pb.SessionReply_Rejected_CODE_INVALID) {
			s.w.log.Warn("event batch rejected", "session_id", s.id, "error", err)
		} else if err != nil {
			return err
		} else if r.GetOk() == nil {
			return fmt.Errorf("publish: %v", r)
		}
		s.batchSeq++
		clear(s.events[:count])
		s.events = s.events[count:]
	}
	s.events = nil
	return nil
}

func (s *session) loadTranscript() error {
	r, err := s.request(wc.Op(s.id, s.epoch, &pb.ListTranscriptKeys{}))
	if err != nil {
		return err
	}
	for _, key := range r.GetTranscriptKeys().GetSubpaths() {
		if err := s.loadSubpath(key); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) loadSubpath(key string) error {
	// The fake needs only the last main-thread UUID and any missing parents.
	// Entries are read in their entirety in page order.
	var seq uint64
	ids := make(map[string]bool)
	var parents []string
	for {
		r, err := s.request(wc.Load(s.id, s.epoch, key, seq, 1<<20))
		if err != nil {
			return err
		}
		page := r.GetTranscriptPage()
		for _, e := range page.GetEntries() {
			if key == usageSubpath {
				if err := s.loadUsage(e.GetJson()); err != nil {
					return err
				}
				continue
			}
			if key == "" {
				if e.GetUuid() != "" {
					s.parent = e.GetUuid()
				}
				s.loadApprovalCommand(e.GetJson())
			}
			if s.gap == 0 {
				continue
			}
			var entry struct {
				UUID   string `json:"uuid"`
				Parent string `json:"parentUuid"`
			}
			if json.Unmarshal([]byte(e.GetJson()), &entry) == nil {
				if entry.Parent != "" && !ids[entry.Parent] {
					parents = append(parents, entry.Parent)
				}
				ids[entry.UUID] = true
			}
			ids[e.GetUuid()] = true
		}
		if page.GetDone() {
			break
		}
		if page.GetNextSeq() <= seq {
			return fmt.Errorf("transcript %q did not advance", key)
		}
		seq = page.GetNextSeq()
	}
	var unknown []string
	for _, parent := range parents {
		if !ids[parent] {
			unknown = append(unknown, parent)
			ids[parent] = true
		}
	}
	if len(unknown) != 0 {
		s.queue(pb.SessionEvent_KIND_TRANSCRIPT_GAP_DETECTED, gapDetectedPayload(key, unknown))
		return s.flush()
	}
	return nil
}

func (s *session) loadApprovalCommand(raw string) {
	if len(raw) <= 1024 || !strings.Contains(raw, "/fake approve ") {
		return
	}
	var entry struct {
		Type    string
		Message struct {
			Content string
		}
	}
	if json.Unmarshal([]byte(raw), &entry) == nil && entry.Type == "user" {
		if command, ok := strings.CutPrefix(entry.Message.Content, "/fake approve "); ok {
			s.rememberApproval(command)
		}
	}
}

func clipUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for !utf8.RuneStart(text[limit]) {
		limit--
	}
	return strings.Clone(text[:limit])
}
