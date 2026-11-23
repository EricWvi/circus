package sessions

import (
	"context"
	"log/slog"
	"strings"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
)

// logRec is one log record collected under s.mu, written after it is
// released (unlock).
type logRec struct {
	level slog.Level
	msg   string
	attrs []slog.Attr
}

// Lease-end reasons ("lease ended"), each derived from the operation that
// committed the end.
const (
	endReleased    = "released"     // the holder's ReleaseLease
	endExpired     = "expired"      // the expiry sweep, a settle, or an acquisition that replaced an expired lease
	endTakenOver   = "taken_over"   // the live holder's own fresh acquisition replaced it
	endStopped     = "stopped"      // a fresh acquisition by the holder of a stopped session released it
	endBroken      = "broken"       // a fresh acquisition by the holder of a broken session released it
	endBootChanged = "boot_changed" // a newer boot of the worker released it
	// LeaseEndProviderChanged and LeaseEndRecovered are the reasons of the
	// web API's own lease-ending transactions (UpdateSession's provider
	// change, RecoverSession).
	LeaseEndProviderChanged = "provider_changed"
	LeaseEndRecovered       = "recovered"
)

// LogLeaseEnded writes the "lease ended" record of a committed lease loss l
// for reason, once the caller has done everything that follows the commit
// (announcements, revocations, Wakes). A repeated release ended nothing
// and writes nothing. Callers must not hold s.mu.
func LogLeaseEnded(ctx context.Context, log *slog.Logger, l store.LeaseLoss, reason string) {
	if r, ok := leaseEnd(l, reason); ok {
		log.LogAttrs(ctx, r.level, r.msg, r.attrs...)
	}
}

// leaseEnd returns the "lease ended" record of l for reason; false for a
// repeated release.
func leaseEnd(l store.LeaseLoss, reason string) (logRec, bool) {
	if l.Repeated {
		return logRec{}, false
	}
	return logRec{level: slog.LevelInfo, msg: "lease ended", attrs: []slog.Attr{slog.String("session_id", l.Released.SessionID),
		slog.String("worker_id", l.Released.WorkerID), slog.String("boot_id", l.Released.BootID), slog.Int64("epoch", l.Released.Epoch),
		slog.String("reason", reason), slog.Bool("interrupted", l.Interrupted)}}, true
}

// unlock releases s.mu and then writes the log records collected while it
// was held. Every s.mu.Lock in the package ends with it or with
// unlockKeep, so no record is written under s.mu; a handler that panics
// during the writes finds s.mu already released. A detached batch lives
// until its records are written.
func (s *Service) unlock() { s.write(context.Background(), s.unlockKeep()) }

// unlockKeep releases s.mu and returns the records collected while it was
// held, for a caller that writes them (write) after the work that follows
// its critical section.
func (s *Service) unlockKeep() []logRec {
	p := s.pend
	s.pend = nil
	s.mu.Unlock()
	return p
}

// write writes records collected earlier; s.mu not held.
func (s *Service) write(ctx context.Context, recs []logRec) {
	for _, r := range recs {
		s.log.LogAttrs(ctx, r.level, r.msg, r.attrs...)
	}
}

// later collects a record to write once s.mu is released; s.mu held.
func (s *Service) later(level slog.Level, msg string, attrs ...slog.Attr) {
	s.pend = append(s.pend, logRec{level: level, msg: msg, attrs: attrs})
}

// debug reports whether debug records are written for ctx, so hot paths
// outside s.mu build their attributes only then. Never called under s.mu:
// records collected under it use s.dbg, decided once by New.
func (s *Service) debug(ctx context.Context) bool { return s.log.Enabled(ctx, slog.LevelDebug) }

// bindingDropped collects the debug record of an in-memory lease binding
// the scheduler forgets for cause without a committed loss of its own (the
// loss, if any, is logged where it committed); s.mu held.
func (s *Service) bindingDropped(id string, h holder, cause string) {
	if s.dbg {
		s.later(slog.LevelDebug, "lease binding dropped", slog.String("session_id", id), slog.String("worker_id", h.worker),
			slog.String("boot_id", h.boot), slog.Int64("epoch", h.epoch), slog.String("cause", cause))
	}
}

// reasonNames lists Nudge reasons as lowercase names, comma-separated.
func reasonNames(rs []circusv1.WorkerEvent_Nudge_Reason) string {
	var b strings.Builder
	for i, r := range rs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strings.ToLower(strings.TrimPrefix(r.String(), "REASON_")))
	}
	return b.String()
}
