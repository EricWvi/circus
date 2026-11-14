package controlplane

import (
	"context"
	"time"

	"github.com/EricWvi/circus/internal/controlplane/timers"
	"github.com/EricWvi/circus/internal/store"
)

const enrollmentCleanupPeriod = 10 * time.Minute

// seedCleanup preserves an existing claim or deadline across process restarts.
func (s *Server) seedCleanup(ctx context.Context) error {
	return s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		rows, err := tx.ListTimers(ctx, store.TimerFilter{Kind: timers.EnrollTokenExpiry, SubjectID: "purge", Limit: 1})
		if err != nil || len(rows) > 0 {
			return err
		}
		return tx.SetTimer(ctx, timers.EnrollTokenExpiry, "purge", s.now().UnixMilli())
	})
}
func (s *Server) handleTimer(ctx context.Context, claim store.Timer) error {
	switch claim.Kind {
	case timers.DeviceGrace, timers.CallWatchdog:
		return s.router.HandleTimer(ctx, claim)
	case timers.EnrollTokenExpiry:
		var purged int
		err := s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
			ok, err := tx.CompleteTimer(ctx, claim)
			if err != nil || !ok {
				return err
			}
			n, err := tx.PurgeEnrollTokens(ctx, s.now().UnixMilli())
			if err != nil {
				return err
			}
			purged = n
			return tx.SetTimer(ctx, timers.EnrollTokenExpiry, "purge", s.now().Add(enrollmentCleanupPeriod).UnixMilli())
		})
		if err == nil && purged > 0 {
			s.logger.InfoContext(ctx, "purged expired enroll tokens", "count", purged)
		}
		return err
	default:
		return s.completeUnhandledTimer(ctx, claim, "unknown timer kind")
	}
}

// completeUnhandledTimer consumes a claim that has no live handler. Completing
// it prevents an unknown kind from being reclaimed forever.
func (s *Server) completeUnhandledTimer(ctx context.Context, claim store.Timer, reason string) error {
	completed := false
	err := s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		completed, err = tx.CompleteTimer(ctx, claim)
		return err
	})
	if err == nil && completed {
		s.logger.WarnContext(ctx, "completed unhandled durable timer", "kind", claim.Kind, "subject_id", claim.SubjectID, "reason", reason)
	}
	return err
}
