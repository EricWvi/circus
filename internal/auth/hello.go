package auth

import (
	"context"
	"log/slog"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
)

// HelloMetadataStore persists metadata reported by authenticated links.
type HelloMetadataStore interface {
	UpdateHelloMetadata(context.Context, store.PrincipalKind, string, store.HelloMetadata) (bool, error)
}

// RefreshHelloMetadata schedules a best-effort update for an accepted Hello.
// Each field uses the same byte limit as enrollment.
func RefreshHelloMetadata(ctx context.Context, st HelloMetadataStore, logger *slog.Logger, principal Principal, hello *circusv1.Hello) {
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
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := st.UpdateHelloMetadata(ctx, principal.Kind, principal.ID, metadata); err != nil {
			logger.WarnContext(ctx, "hello metadata update failed", "kind", principal.Kind, "identity_id", principal.ID, "error", err)
		}
	}()
}
