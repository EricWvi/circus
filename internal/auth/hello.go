package auth

import (
	"context"
	"log/slog"
	"sync"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
)

// HelloMetadataStore persists metadata reported by authenticated links.
type HelloMetadataStore interface {
	UpdateHelloMetadata(context.Context, store.PrincipalKind, string, store.HelloMetadata) (bool, error)
}

// RefreshHelloMetadata starts a best-effort update for an accepted Hello in
// updates. Each field uses the same byte limit as enrollment. The update
// outlives ctx by up to five seconds so a short connection still records it;
// the link handler waits for updates in Shutdown, after its connections have
// ended, so an update never outlives the server's shutdown and never delays
// a connection's teardown.
func RefreshHelloMetadata(ctx context.Context, updates *sync.WaitGroup, st HelloMetadataStore, logger *slog.Logger, principal Principal, hello *circusv1.Hello) {
	if st == nil {
		return
	}
	accepted := func(value string) string {
		if len(value) > maxFieldLen {
			return ""
		}
		return value
	}
	metadata := store.HelloMetadata{
		ClientVersion: accepted(hello.GetClientVersion()),
		OS:            accepted(hello.GetOs()),
		Arch:          accepted(hello.GetArch()),
		Hostname:      accepted(hello.GetHostname()),
	}
	if metadata == (store.HelloMetadata{}) {
		return
	}
	updates.Go(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := st.UpdateHelloMetadata(ctx, principal.Kind, principal.ID, metadata); err != nil {
			logger.WarnContext(ctx, "hello metadata update failed", "kind", principal.Kind, "identity_id", principal.ID, "error", err)
		}
	})
}

// WaitUpdates waits for the updates started in updates until ctx ends.
func WaitUpdates(ctx context.Context, updates *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() { updates.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
