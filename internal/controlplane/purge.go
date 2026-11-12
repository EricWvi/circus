package controlplane

import (
	"context"
	"time"
)

// purgeLoop deletes expired, unused enrollment tokens once at start and then
// every PurgeInterval until ctx ends.
//
// This is a stand-in for the claimed timer loop (D11 item 4, package
// timers). Moving onto it: register a handler for kind enroll_token_expiry
// that, in one Tx, calls CompleteTimer, then PurgeEnrollTokens(now), then
// SetTimer(enroll_token_expiry, "purge", now+PurgeInterval); seed the timer
// once at startup with SetTimer if ListTimers has none; then delete this loop
// and its goroutine. A per-token timer (subject = token id, due = ExpiresAt,
// set in CreateEnrollToken's Tx) would also work but buys nothing: purging
// late is harmless because UseEnrollToken already refuses expired tokens.
func (s *Server) purgeLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.PurgeInterval)
	defer t.Stop()
	for {
		s.purgeOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) purgeOnce(ctx context.Context) {
	n, err := s.st.PurgeEnrollTokens(ctx, s.now().UnixMilli())
	switch {
	case ctx.Err() != nil:
	case err != nil:
		s.logger.WarnContext(ctx, "purge expired enroll tokens failed", "error", err)
	case n > 0:
		s.logger.InfoContext(ctx, "purged expired enroll tokens", "count", n)
	}
}
