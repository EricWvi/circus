// Package updates updates users' machines to the latest circus release from
// the Web, as `circus update` run on the machine does.
//
// The Manager knows the latest release (it reads the release manifest at
// start and then every CheckInterval), offers an update on the card of a
// device or worker that is online as the installed service (it takes
// UpdateMachine) and whose version is an older release, and keeps at most
// one update per machine (store.MachineUpdate). Start records the update
// with a deadline timer and sends UpdateMachine to an executor: an online
// role of the machine that takes it, one below the target when there is
// one, the daemon first. The update completes, and its record goes, when
// a role of the machine connects with a version at least the target
// (Connected), or is found at it when the deadline passes; it fails when
// the executor reports an error (Failed), the command cannot be sent, or
// the deadline passes first (HandleTimer). Outcomes name their attempt, so
// a late one of an older attempt changes nothing. The record goes with the
// machine's last identity (auth.ForgetMachine).
package updates

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/timers"
	"github.com/EricWvi/circus/internal/selfupdate"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/textutil"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/google/uuid"
	"golang.org/x/mod/semver"
)

const (
	// Deadline is how long an update may run before it fails as timed
	// out. The executor stops `circus update` 30 seconds before.
	Deadline = 5 * time.Minute
	// CheckInterval is how often the latest release is read again.
	CheckInterval = 6 * time.Hour
	// MaxErrorBytes bounds an executor's error as stored: a longer one
	// keeps its last MaxErrorBytes bytes.
	MaxErrorBytes = store.MaxMachineUpdateErrorBytes
)

var (
	// ErrNotFound: no such live identity of the user.
	ErrNotFound = errors.New("updates: not found")
	// ErrUnavailable: no update can be offered; errors matching it say why.
	ErrUnavailable = errors.New("updates: no update can be offered")
)

// unavailable is an ErrUnavailable whose message is only the reason.
type unavailable string

func (u unavailable) Error() string        { return string(u) }
func (u unavailable) Is(target error) bool { return target == ErrUnavailable }

// Daemons reaches daemons over their live links (gateway.Handler).
type Daemons interface {
	// Supports reports whether the device is online and its daemon listed
	// feature.
	Supports(deviceID, feature string) (online, supported bool)
	UpdateMachine(ctx context.Context, deviceID string, u *circusv1.UpdateMachine) error
}

// Workers reaches workers over their live links (sessions.Service).
type Workers interface {
	// Updatable reports whether the worker has a live connection that
	// takes UpdateMachine.
	Updatable(workerID string) bool
	// UpdateMachine queues u on such a connection and reports whether it did.
	UpdateMachine(workerID string, u *circusv1.UpdateMachine) bool
}

// Config configures a Manager.
type Config struct {
	Store store.Store
	// Latest reads the latest release's version (selfupdate.Release.Latest).
	// Nil never knows one, so no update is offered.
	Latest func(context.Context) (string, error)
	// CheckInterval is how often Run reads Latest; zero means CheckInterval.
	CheckInterval time.Duration
	Daemons       Daemons
	Workers       Workers
	// Now supplies time; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Manager runs machine updates. Its methods are safe for concurrent use.
type Manager struct {
	cfg    Config
	mu     sync.Mutex
	latest string
}

// New returns a Manager. Store, Daemons and Workers are required.
func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil || cfg.Daemons == nil || cfg.Workers == nil {
		return nil, errors.New("updates: store, daemons and workers are required")
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = CheckInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Manager{cfg: cfg}, nil
}

// Run reads the latest release now and then every CheckInterval until ctx
// ends. A failed read keeps the version read before and logs a warning.
func (m *Manager) Run(ctx context.Context) {
	if m.cfg.Latest == nil {
		return
	}
	t := time.NewTicker(m.cfg.CheckInterval)
	defer t.Stop()
	for {
		m.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check reads the latest release once (Run does it periodically).
func (m *Manager) Check(ctx context.Context) {
	if m.cfg.Latest == nil {
		return
	}
	v, err := m.cfg.Latest(ctx)
	switch {
	case err != nil:
	case !selfupdate.IsRelease(v):
		err = fmt.Errorf("%q is not a release version", v)
	case len(v) > store.MaxMachineUpdateVersionBytes:
		err = fmt.Errorf("release version %.16q… is longer than %d bytes", v, store.MaxMachineUpdateVersionBytes)
	}
	if err != nil {
		if ctx.Err() == nil {
			m.cfg.Logger.WarnContext(ctx, "reading the latest circus release failed; keeping the last one", "error", err, "latest", m.Latest())
		}
		return
	}
	m.mu.Lock()
	changed := m.latest != v
	m.latest = v
	m.mu.Unlock()
	if changed {
		m.cfg.Logger.InfoContext(ctx, "latest circus release", "version", v)
	}
}

// Latest is the latest release read, or "" before the first good read.
func (m *Manager) Latest() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.latest
}

// Machine is one machine of a user as the Web shows it.
type Machine struct {
	// Update is the machine's update, nil without one.
	Update *store.MachineUpdate
}

// Machines returns the updates of the user's machines, by machine id; a
// machine without one is absent.
func (m *Manager) Machines(ctx context.Context, userID string) (map[string]Machine, error) {
	updates, err := m.cfg.Store.ListMachineUpdates(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Machine, len(updates))
	for i := range updates {
		out[updates[i].MachineID] = Machine{Update: &updates[i]}
	}
	return out, nil
}

// Offer is the release an update pressed on id's card would install, or
// "" when none is offered (see unavailable).
func (m *Manager) Offer(id store.Identity, mach Machine) string {
	latest := m.Latest()
	if m.unavailable(id, mach.Update, latest) != nil {
		return ""
	}
	return latest
}

// unavailable says why no update to latest (one read of Latest) is
// offered on id's card, or nil: latest is known, id's version is a release
// older than it, id is online and takes UpdateMachine, and no update of
// its machine runs.
func (m *Manager) unavailable(id store.Identity, u *store.MachineUpdate, latest string) error {
	switch {
	case u != nil && u.State == store.MachineUpdateRunning:
		return unavailable("an update of the machine is running")
	case latest == "":
		return unavailable("the latest release is not known yet")
	case !selfupdate.IsRelease(id.ClientVersion):
		return unavailable(fmt.Sprintf("%q is not a release (a development build)", id.ClientVersion))
	case !below(id.ClientVersion, latest):
		return unavailable("already at the latest release " + latest)
	case id.MachineID == "":
		return unavailable("the machine is not known")
	case !m.executes(id):
		return unavailable(fmt.Sprintf("the %s is not online as the installed service", noun(id.Kind)))
	}
	return nil
}

func noun(kind store.PrincipalKind) string {
	if kind == store.KindDevice {
		return "device"
	}
	return "worker"
}

// below reports whether version is a release older than target.
func below(version, target string) bool {
	return selfupdate.IsRelease(version) && semver.Compare(version, target) < 0
}

// atTarget reports whether version is a release at least target.
func atTarget(version, target string) bool {
	return selfupdate.IsRelease(version) && semver.Compare(version, target) >= 0
}

// allAtTarget reports whether a machine (its live identities ids) has
// some identity and none below target: the update to target is done even
// if no connect at the target was seen during the attempt.
func allAtTarget(ids []store.Identity, target string) bool {
	for _, id := range ids {
		if !atTarget(id.ClientVersion, target) {
			return false
		}
	}
	return len(ids) > 0
}

// executes reports whether identity id is online and takes UpdateMachine.
func (m *Manager) executes(id store.Identity) bool {
	if id.Kind == store.KindDevice {
		online, ok := m.cfg.Daemons.Supports(id.ID, wire.FeatureMachineUpdate)
		return online && ok
	}
	return m.cfg.Workers.Updatable(id.ID)
}

// machineIdentities returns the user's live identities on machineID,
// devices first.
func machineIdentities(ctx context.Context, ops store.Ops, userID, machineID string) ([]store.Identity, error) {
	var out []store.Identity
	for _, kind := range []store.PrincipalKind{store.KindDevice, store.KindWorker} {
		ids, err := ops.ListIdentities(ctx, kind, userID)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if id.MachineID == machineID {
				out = append(out, id)
			}
		}
	}
	return out, nil
}

// Start updates the machine of the user's identity (kind, id) and returns
// its update. While one runs it returns that one unchanged; otherwise
// (none, or a failed one) it records a new attempt to the latest release,
// with its deadline timer, and sends UpdateMachine to the executor (see
// executor). A command that cannot be sent fails the attempt at once.
// The transaction that records it reads id and the executor again: when
// either was deleted meanwhile, or id reached the target (its reconnect
// raced the press), nothing is recorded or sent. ErrNotFound for an
// identity that is not the user's live one; an ErrUnavailable when no
// update is offered for it.
func (m *Manager) Start(ctx context.Context, userID string, kind store.PrincipalKind, id string) (store.MachineUpdate, error) {
	live := func(ctx context.Context, ops store.Ops) (store.Identity, error) {
		ident, err := ops.GetIdentity(ctx, kind, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && (ident.UserID != userID || ident.DeletedAt != 0)) {
			return ident, ErrNotFound
		}
		return ident, err
	}
	ident, err := live(ctx, m.cfg.Store)
	if err != nil {
		return store.MachineUpdate{}, err
	}
	var current *store.MachineUpdate
	if ident.MachineID != "" {
		u, err := m.cfg.Store.GetMachineUpdate(ctx, userID, ident.MachineID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return store.MachineUpdate{}, err
		}
		if err == nil {
			if u.State == store.MachineUpdateRunning {
				return u, nil
			}
			current = &u
		}
	}
	latest := m.Latest()
	if err := m.unavailable(ident, current, latest); err != nil {
		return store.MachineUpdate{}, err
	}
	var exec store.Identity
	updateID, err := uuid.NewV7()
	if err != nil {
		return store.MachineUpdate{}, err
	}
	now := m.cfg.Now()
	u := store.MachineUpdate{UserID: userID, MachineID: ident.MachineID, UpdateID: updateID.String(), TargetVersion: latest,
		State: store.MachineUpdateRunning, StartedAt: now.UnixMilli(), DeadlineAt: now.Add(Deadline).UnixMilli()}
	started := true
	err = m.cfg.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		// Another Start may have won meanwhile: its update stands.
		before, err := tx.GetMachineUpdate(ctx, userID, ident.MachineID)
		if err == nil && before.State == store.MachineUpdateRunning {
			u, started = before, false
			return nil
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		// The identity and the machine's roles with their versions as of
		// now: a reconnect at the target or a deletion may have committed
		// since the press was read. The executor is chosen from them.
		fresh, err := live(ctx, tx)
		if err != nil {
			return err
		}
		if !below(fresh.ClientVersion, latest) {
			return unavailable("already at the latest release " + latest)
		}
		ids, err := machineIdentities(ctx, tx, userID, ident.MachineID)
		if err != nil {
			return err
		}
		var ok bool
		if exec, ok = m.executor(ids, latest); !ok {
			return unavailable(fmt.Sprintf("the %s is not online as the installed service", noun(kind)))
		}
		if before.UpdateID != "" {
			if err := tx.CancelTimer(ctx, timers.MachineUpdate, before.UpdateID); err != nil {
				return err
			}
		}
		if err := tx.PutMachineUpdate(ctx, u); err != nil {
			return err
		}
		return tx.SetTimer(ctx, timers.MachineUpdate, u.UpdateID, u.DeadlineAt)
	})
	if err != nil || !started {
		return u, err
	}
	cmd := &circusv1.UpdateMachine{UpdateId: u.UpdateID, Version: latest}
	var sendErr error
	if exec.Kind == store.KindDevice {
		sendErr = m.cfg.Daemons.UpdateMachine(ctx, exec.ID, cmd)
	} else if !m.cfg.Workers.UpdateMachine(exec.ID, cmd) {
		sendErr = errors.New("the worker went offline")
	}
	if sendErr != nil {
		m.cfg.Logger.WarnContext(ctx, "machine update could not be sent", "user_id", userID, "machine_id", u.MachineID,
			"update_id", u.UpdateID, "executor_kind", exec.Kind, "executor_id", exec.ID, "error", sendErr)
		failed, ok, err := m.fail(ctx, u.UpdateID, store.UpdateUnreachable, CleanError(sendErr.Error()))
		if err != nil || !ok {
			return u, err
		}
		return failed, nil
	}
	m.cfg.Logger.InfoContext(ctx, "machine update started", "user_id", userID, "machine_id", u.MachineID, "update_id", u.UpdateID,
		"version", latest, "executor_kind", exec.Kind, "executor_id", exec.ID, "pressed", id, "from", ident.ClientVersion)
	return u, nil
}

// executor picks the role of a machine (its live identities ids, devices
// first) that runs an update to target: among those online that take
// UpdateMachine, one whose version is below target when there is one,
// the daemon first; otherwise the daemon first.
func (m *Manager) executor(ids []store.Identity, target string) (store.Identity, bool) {
	var first, firstBelow *store.Identity
	for i := range ids {
		if !m.executes(ids[i]) {
			continue
		}
		if first == nil {
			first = &ids[i]
		}
		if firstBelow == nil && below(ids[i].ClientVersion, target) {
			firstBelow = &ids[i]
		}
	}
	switch {
	case firstBelow != nil:
		return *firstBelow, true
	case first != nil:
		return *first, true
	}
	return store.Identity{}, false
}

// fail marks the running update updateID failed and cancels its timer.
func (m *Manager) fail(ctx context.Context, updateID string, failure store.MachineUpdateFailure, errText string) (store.MachineUpdate, bool, error) {
	var u store.MachineUpdate
	var ok bool
	err := m.cfg.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		if u, ok, err = tx.FailMachineUpdate(ctx, updateID, failure, errText); err != nil || !ok {
			return err
		}
		return tx.CancelTimer(ctx, timers.MachineUpdate, updateID)
	})
	return u, ok, err
}

// Failed is the executor's report that update updateID failed with
// errText (cleaned, its last MaxErrorBytes kept): it fails the update when
// it is the running attempt of the reporter's (kind, id) machine, or
// removes it as done when every role of the machine already reports the
// target.
// Otherwise (another machine's or an older attempt, an unknown reporter)
// it changes nothing and returns nil; an error means the report could not
// be recorded.
func (m *Manager) Failed(ctx context.Context, kind store.PrincipalKind, id, updateID, errText string) error {
	if store.CheckID("update id", updateID) != nil {
		return nil
	}
	errText = CleanError(errText)
	var failed bool
	err := m.cfg.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		ident, err := tx.GetIdentity(ctx, kind, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && ident.MachineID == "") {
			return nil
		}
		if err != nil {
			return err
		}
		u, err := tx.GetMachineUpdate(ctx, ident.UserID, ident.MachineID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && (u.UpdateID != updateID || u.State != store.MachineUpdateRunning)) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := tx.CancelTimer(ctx, timers.MachineUpdate, updateID); err != nil {
			return err
		}
		// Every role already reports the target (a completion was lost):
		// the update is done, whatever the command said.
		ids, err := machineIdentities(ctx, tx, u.UserID, u.MachineID)
		if err != nil {
			return err
		}
		if allAtTarget(ids, u.TargetVersion) {
			_, err := tx.DeleteMachineUpdate(ctx, u.UserID, u.MachineID, u.UpdateID)
			return err
		}
		_, failed, err = tx.FailMachineUpdate(ctx, updateID, store.UpdateFailedCommand, errText)
		return err
	})
	if err == nil {
		m.cfg.Logger.InfoContext(ctx, "machine update failure reported", "kind", kind, "identity_id", id, "update_id", updateID,
			"applied", failed, "error", errText)
	}
	return err
}

// Connected is a role's connect at version: an update of its machine (running
// or failed) to a release at most version is complete and removed.
func (m *Manager) Connected(ctx context.Context, kind store.PrincipalKind, id, version string) error {
	if !selfupdate.IsRelease(version) {
		return nil
	}
	ident, err := m.cfg.Store.GetIdentity(ctx, kind, id)
	if err != nil || ident.MachineID == "" {
		return err
	}
	var done store.MachineUpdate
	err = m.cfg.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		u, err := tx.GetMachineUpdate(ctx, ident.UserID, ident.MachineID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && !atTarget(version, u.TargetVersion)) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.DeleteMachineUpdate(ctx, u.UserID, u.MachineID, u.UpdateID); err != nil {
			return err
		}
		done = u
		return tx.CancelTimer(ctx, timers.MachineUpdate, u.UpdateID)
	})
	if err == nil && done.UpdateID != "" {
		m.cfg.Logger.InfoContext(ctx, "machine update completed", "user_id", done.UserID, "machine_id", done.MachineID,
			"update_id", done.UpdateID, "version", version, "kind", kind, "identity_id", id, "state", done.State)
	}
	return err
}

// HandleTimer handles the deadline of the update claim names (a
// timers.Handler for timers.MachineUpdate). When it still runs and every
// role of its machine reports the target, it is done (a reconnect's
// completion may have been lost); otherwise it fails as timed out. A role
// that was at the target before the attempt does not count alone: the
// role the command was for may never have got it.
func (m *Manager) HandleTimer(ctx context.Context, claim store.Timer) error {
	var u store.MachineUpdate
	var outcome string
	err := m.cfg.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		ok, err := tx.CompleteTimer(ctx, claim)
		if err != nil || !ok {
			return err
		}
		u, err = tx.GetMachineUpdateByID(ctx, claim.SubjectID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && u.State != store.MachineUpdateRunning) {
			return nil
		}
		if err != nil {
			return err
		}
		ids, err := machineIdentities(ctx, tx, u.UserID, u.MachineID)
		if err != nil {
			return err
		}
		if allAtTarget(ids, u.TargetVersion) {
			outcome = "completed"
			_, err := tx.DeleteMachineUpdate(ctx, u.UserID, u.MachineID, u.UpdateID)
			return err
		}
		outcome = "timed out"
		_, _, err = tx.FailMachineUpdate(ctx, u.UpdateID, store.UpdateTimedOut, "")
		return err
	})
	if err == nil && outcome != "" {
		m.cfg.Logger.InfoContext(ctx, "machine update deadline", "outcome", outcome, "user_id", u.UserID, "machine_id", u.MachineID,
			"update_id", u.UpdateID, "version", u.TargetVersion)
	}
	return err
}

// CleanError makes an executor's error safe to store and show (see
// textutil.Tail), at most MaxErrorBytes; the tail is kept, as it says why.
func CleanError(s string) string { return textutil.Tail(s, MaxErrorBytes) }
